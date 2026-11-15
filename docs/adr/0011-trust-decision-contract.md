# ADR-0011: Trust Decision Contract

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** Kognition core team

## Context

Kognition persists Tool and Memory evidence through CognitiveAuditLog, but
producer-specific `details` values do not provide one stable contract for
operators or the Trust Focus dashboard. The Demo demonstration needs to
show why a request was allowed, denied, replayed, or revoked without exposing
credentials, prompts, provider responses, or unrestricted arguments.

## Decision

Trust Decision v1 is the normalized, read-only evidence payload stored in
`AuditEntry.details` for controlled Tool and Memory operations. The canonical
machine-readable definition is
`api/schemas/trust-decision-v1.schema.json`.

Each payload includes:

- `schemaVersion: 1`, decision ID, and decision timestamp.
- Closed `plane`, `outcome`, and `reasonCode` values.
- Workspace, identity, session, invocation, resource, authorization, approval,
  execution, and trace references when that evidence exists.
- A bounded operator-facing reason and bounded source resource references.

The v1 plane enum contains `tool`, `memory`, `model`, and `communication`.
P0 producers emit only `tool` and `memory`; the remaining values reserve a
stable expansion path without changing the schema version.

Producers write an allowed decision before the target side effect. Denied,
revoked, and replay decisions are written without forwarding to the target.
The normalized payload supplements existing audit fields and does not replace
legacy consumers of `AuditEntry`.

## Query API

The dashboard exposes `GET /api/trust-decisions`. Standalone mode forwards to
`GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/trust-decisions` on
the API server. Kubernetes mode queries immutable audit ConfigMaps in the
configured Kognition control namespace.

The dashboard route requires the `namespace` query parameter. Both routes
accept `agentIdentity`, `sessionId`, `plane`, `outcome`, `since`, `until`, and
`limit`. An omitted time range covers the previous 24 hours. The default limit
is 100 and the maximum is 500. Results use `timestamp DESC, id DESC` ordering
and return `{apiVersion, items, truncated}`. Invalid filters return a bounded
client error. Malformed audit payloads and ConfigMap page overflow return an
actionable server error that identifies the invalid entry or asks the caller
to narrow the filter.

## Redaction Rules

The schema is closed at every object boundary. It has no fields for
credentials, tokens, secrets, raw prompts, model responses, unrestricted tool
arguments, or raw memory content. Producers may include only:

- Canonical resource references with kind, namespace, name, and version.
- Identifiers, capability/session references, closed evidence statuses, and
  bounded safe reason text.
- Approval decision references and trace/span identifiers.

Tool argument constraints may be named by a preconfigured constraint reference
only. They must never include an argument value or an unrestricted argument
payload. Memory evidence may name a volume or record reference but must never
include stored value content.

## Consequences

**Positive.** Trust Focus, the query API, PostgreSQL, and Kubernetes ConfigMap
audit stores share one versioned evidence contract. Closed values make filters
and summaries deterministic. The schema and tests prevent accidental addition
of sensitive fields.

**Negative.** Producers must map local failures to the contract reason codes.
Unrecognized producer detail is excluded from Trust Focus until a later schema
version accepts it.

## Alternatives Considered

1. **Render producer-specific audit details directly.** Rejected because the
   UI would need to infer policy conclusions and could expose sensitive data.
2. **Create a separate trust-decision store.** Rejected for P0 because it
   duplicates write-ahead audit durability and weakens trace correlation.
3. **Allow arbitrary extension properties.** Rejected because they invalidate
   redaction guarantees and make the presentation inconsistent.