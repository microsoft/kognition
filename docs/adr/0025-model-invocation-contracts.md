# ADR-0025: Model Invocation Contracts

- Status: Accepted for implementation by the workspace user.
- Date: 2026-10-22.
- Related: [ADR-0008](0008-model-gateway.md),
  [ADR-0015](0015-provider-integration-boundaries.md),
  [ADR-0020](0020-model-version-binding.md).

## Context

Model deployment names are opaque aliases. Different deployments and API versions
require different token-limit fields, sampling settings and tool/reasoning
combinations. Model-family conditionals in Go and Python hide these differences,
change explicit intent and diverge between standalone and gateway execution.

## Options

| Option | Benefits | Costs |
| --- | --- | --- |
| Model-name conditionals | Preserves an immediate compatibility path | Breaks aliases, duplicates behavior, requires runtime changes |
| Free-form parameter maps | Small initial API change | No omission semantics, weak validation, inconsistent consumers |
| Provider-specific profile resources | Strong provider validation | Duplicates common intent and prevents cross-provider reuse |
| Generic profiles and adapter-owned contracts | Reusable intent, explicit compatibility, deterministic validation | Requires resource registration, revision binding and adapter conformance |

## Decision

Introduce a namespace-local `ModelInvocationProfile` for portable defaults,
requirements and hard limits. `Model.invocationProfileRef` selects the profile;
`Model.invocationContract` selects an explicit versioned contract implemented by
the selected CPI adapter. Provider identity, protocol, model alias and invocation
intent remain distinct concepts. No generic transformation language or arbitrary
provider parameter dictionary is introduced.

Profiles describe chat or embedding operations. Adapters own supported contracts,
wire-field mappings and response formats. Unsupported protocols, contracts and
parameter combinations fail before dispatch. This change does not implement a
Vertex adapter or imply support for Responses, streaming or every provider's
reasoning extensions.

Resolve defaults only for omitted settings. Explicit incompatible or unknown
generation parameters are rejected, not removed, clamped or retried with weaker
behavior. Completion-token limits include any reasoning tokens counted by the
selected contract and remain separate from aggregate invocation budgets.

Profile specs are immutable. Model references pin their reviewed spec digest,
which also becomes part of the signed Model spec. Sessions pin the
namespace-local profile UID and spec digest; model-version candidates
additionally bind those values. Deletion
and same-name replacement cannot silently redirect a session or candidate.
Authorization remains live and credentials remain at the existing model boundary.
Profile session capabilities use a distinct audience to fail closed against
older gateways that cannot enforce the contract.

Unprofiled Models retain the existing legacy compatibility path during migration.
Profile-backed execution never inspects deployment-name prefixes. Standalone,
gateway, runtime, candidate and embedding consumers must resolve the same declared
contract, or reject unsupported execution explicitly.

## Implementation Plan

1. Add API types, schemas, conversion, CRUD/CLI registration and admission.
2. Add shared typed resolution and adapter-owned versioned contracts.
3. Bind profiles into model validation, session capabilities and candidates.
4. Wire model consumers and preserve omitted runtime settings.
5. Migrate Azure sample generation and add provider-neutral examples.
6. Verify aliases, strict rejection, wire payloads, legacy behavior, profile
   replacement, API parity, package builds and chart packaging.

## Consequences

Compatible model changes become declarative. A genuinely new protocol still
requires an adapter release. Profiles add a namespaced dependency and read
permission to existing authorized control-plane components. Installing schemas,
RBAC or updated images requires the existing guarded installation and separate
activation workflow; this implementation does not modify a running environment.
