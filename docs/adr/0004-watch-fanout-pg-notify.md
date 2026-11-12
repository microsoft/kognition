# ADR-0004 — Multi-replica watch fan-out via PostgreSQL LISTEN/NOTIFY

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Kognition core team

## Context

The standalone API server exposes a watch endpoint (SSE) so clients and
controllers receive create/update/delete events for resources. The PostgreSQL
store delivered these events through an **in-process** subscriber registry: each
write called `notify()` directly, dispatching to Go channels held by watchers in
the *same* process.

This has two failure modes once the deployment is more than a single process:

1. **Cross-process blindness.** The API server and the controller-manager are
   separate processes with separate stores. A watcher on the API server never
   saw writes made by the controller-manager (e.g. status updates), and vice
   versa.
2. **No horizontal scale.** Running more than one API server replica behind a
   load balancer meant a watcher connected to replica A missed events produced
   by replica B.

The database already had the raw material: a trigger emits
`pg_notify('resource_changes', …)` on every row change, and a `StartNotifyListener`
existed that `LISTEN`ed and re-dispatched. But it was only started by the
controller-manager, had **no reconnection logic** (a dropped connection silently
stopped all delivery until process restart), and — where it *was* active — the
direct write-path `notify()` still fired too, double-delivering a process's own
writes to its local watchers.

## Decision

Make **PostgreSQL `LISTEN/NOTIFY` the single, shared source of watch delivery
whenever the listener is active**, and make the listener resilient.

1. **Start the listener in the API server too** (not just the controller
   manager), so SSE watchers see events from every process and replica. Startup
   failure falls back to local-only delivery with a warning rather than aborting.

2. **Single-source delivery.** While the listener is active, the write-path
   `notify()` is suppressed (`notifyLocal`): delivery for *every* process,
   including the one that made the write, happens via the shared `resource_changes`
   channel. This yields uniform, exactly-once delivery across all replicas and
   removes the double-delivery of local writes. Without an active listener (unit
   tests, or a single process with no DB trigger) direct dispatch is retained.

3. **Reconnect with backoff.** The listener loop reconnects on connection loss
   with exponential backoff and full jitter (250 ms → 30 s), re-issuing `LISTEN`,
   until the context is cancelled.

4. **DELETE payloads preserved.** Because deletes are soft-deletes, the
   pg_notify path fetches the tombstone row so DELETE watch events still carry the
   last-known object, matching the previous write-path behavior.

5. **Connection hygiene.** The listener issues `UNLISTEN` before returning a
   pooled connection, so a recycled connection does not retain subscription state
   or leak buffered notifications to later callers.

## Consequences

**Positive.**

- Correct watch semantics with multiple API-server replicas and across the
  API-server / controller-manager process boundary — a prerequisite for HA.
- Exactly-once delivery per event, identical on every replica (no special-casing
  the originator).
- A dropped database connection self-heals instead of silently wedging watches.

**Negative / to accept.**

- Local writes now round-trip through the database before reaching same-process
  watchers, adding a small latency versus the previous in-memory path. Acceptable
  and uniform across replicas.
- During a database outage, writes fail anyway; while the listener is
  reconnecting, watchers may miss events and should re-list on reconnect (standard
  watch semantics).
- Delivery correctness depends on the `resource_changes` trigger existing, which
  the migrations create; the listener is only started against a migrated database.

## Alternatives Considered

1. **Keep in-memory fan-out; document single-replica only.** Rejected: precludes
   HA for the API server and never fixes cross-process blindness.
2. **Keep both paths (direct + pg_notify) and de-duplicate in the watch stream by
   resourceVersion.** Rejected: adds a dedup window with its own memory/TTL edge
   cases; single-sourcing is simpler and uniform.
3. **External pub/sub (Redis, NATS).** Rejected for now: adds an operational
   dependency the platform otherwise does not require. PostgreSQL is already a
   hard dependency of standalone mode, so `LISTEN/NOTIFY` reuses it.

## Related

- Implementation: `internal/store/postgres/store.go`
  (`StartNotifyListener`, `runListener`, `notifyLocal`),
  `cmd/kognition-apiserver/main.go`, migration `001_initial_schema.sql`.
- Operations guide: [docs/security.md](../security.md) (HA notes).
