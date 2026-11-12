# ADR-0020: Model Version Binding

- Status: Accepted by workspace user on 2026-10-05 for the binding choice.
- Traces: US-0063, T-0225, TC-0183, FR-0321.

## Context

ModelClass version promotion currently tests driver registration and changes
`status.activeVersion`. Inference uses a Model's provider configuration. The
existing free-form pin does not identify an immutable candidate or select the
serving configuration. The user selected explicit candidate binding.

## Options

| Option | Benefit | Limitation |
| --- | --- | --- |
| Explicit binding | Pins configuration | Needs schema and auth integration |
| Provider ID pin | Simple routing | Does not bind endpoint or digest |

## Decision

A version pin selects an explicit candidate binding. Preserve existing pin
semantics until schema and compatibility behavior are defined. Test the exact
candidate before promotion; a generic provisioner health check cannot prove
candidate health. Failure retains the previous serving binding.

Serving authorization must bind the selected candidate configuration in addition
to the Model revision. A class status change must not redirect an already issued
capability to an unbound endpoint or silently introduce provider credentials.
Existing signature, grant, policy, usage and budget controls remain applicable.

Telemetry reports the observed active version rather than the requested pin.
This metadata does not independently prove which provider bytes were served.

## Consequences

The binding choice is resolved. Schema, canonical signed-spec digest, namespace
credential ownership, capability transition rules and candidate-specific
promotion/rollback now have local implementation and regression evidence.
Approved disposable October 13 acceptance verifies the normal PostgreSQL
migration runner and both-store promotion, rollback, protected quota accounting
and known-outcome recovery. Genuinely unknown claims remain fenced and retain
their reservations. Paid-provider, production and release acceptance are separate;
this architectural decision does not grant those approvals. See the
[scoped acceptance record](../development-plans/reviews/backlog-review-2026-10-02.md#model-candidate-local-acceptance-2026-10-13).

## Verification

Require candidate-health failure with a registered driver, exact serving binding
on success, retained binding on failure, restart/idempotence, concurrent edits,
old-capability behavior, signature and credential isolation, and truthful version
attribution. A provider liveness probe alone does not satisfy TC-0183.
