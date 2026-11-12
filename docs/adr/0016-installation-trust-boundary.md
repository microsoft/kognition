# ADR-0016: Installation Trust Boundary

- Status: Accepted.
- Date: 2026-09-24; Cilium isolation decision added 2026-09-25.
- Approval scope: the three decisions below were approved in conversation;
  production activation remains blocked by outstanding security gates.
- Approval source: [agreed boundaries](../development-plans/features/knowledge-provider-execution/security-gate-backlog.md#agreed-boundaries).

## Context

The controller needs credential and native-resource lifecycle permissions for
Agent, KnowledgeBase and supported stdio work. Admission can constrain mutations
and attachment, but cannot restrict Secret GET. Admission changes made while an
old privileged controller runs can expose an unguarded transition. Revoking grants
before finalization can strand legacy workloads and data cleanup.

ADR-0009 defines exec sandbox isolation and ADR-0010 defines durable stdio
invocations. Neither defines installation activation or namespace credential
trust. ADR-0015 defines provider boundaries and namespace-local credentials;
this decision complements it without changing provider protocols or data retention.
These accepted ADRs remain in force and are not superseded.

Standard Kubernetes NetworkPolicy allows are additive. SG-07 reproduced sandbox
egress despite a deny-all baseline when another policy allowed egress. The user
approved a Cilium-backed profile to supply independently controlled deny priority.

## Priorities And Requirements

1. Keep authority and isolation enforceable independently of a compromised
   controller: admission cannot narrow Secret GET, and additive allows must not
   reopen sandbox egress under the supported profile.
2. Preserve supported operation with verifiable transitions: drain/finalize with
   old authority, bootstrap inactive, and retain working stdio with explicit
   evidence before any separate activation.

## Decisions

1. Use mandatory two-phase activation: install admission with the controller
   stopped and grants absent; verify exact inactive state and applicable acceptance
   evidence; then authorize a separate explicit activation transaction.
2. Treat enrolled workload namespaces as dedicated credential trust domains.
   Controller compromise can expose credentials in those namespaces. Keep
   unrelated workloads, identities and credentials outside the enrollment set.
3. Offer an explicit Cilium isolation profile with an administrator-owned
  `CiliumNetworkPolicy` in every enrolled workload namespace. Its `egressDeny`
  targets all entities for classified stdio sandboxes. Bind it into activation
  inventory, spec/ownership verification and UID/hash snapshots. Keep the runtime
  guard uncached and GET-only on Pods, Jobs and Kubernetes NetworkPolicies.

These choices prioritize independent enforcement while preserving verified
activation and usable stdio. This record does not approve remaining residual
findings, production producer acceptance, production deployment or incomplete gates.

## Alternatives And Tradeoffs

| Choice | Benefit | Cost or reason not selected |
| --- | --- | --- |
| Single-step Helm activation or inherited activation values | Fewer operator steps | Cannot establish guarded admission before privileges/startup; rejected by mandatory two-phase approval |
| Separate verified bootstrap and explicit activation | Checks admission and evidence before granting authority; selected | Requires protected approval state, external writer freeze and explicit recovery |
| Mixed workload namespaces with ownership-based credential isolation | Fewer namespaces | Secret GET bypasses admission and labels do not narrow RBAC; incompatible with the approved trust boundary |
| Dedicated enrolled workload namespaces | Makes credential exposure and enrollment review explicit; selected | More namespace administration; does not isolate credentials from a compromised controller inside the domain |
| Standard NetworkPolicy baseline only | Portable and compatible with existing stdio | Fails priority 1 under additive allows; retained default is not SG-07 certified |
| Opt-in Cilium deny profile | Meets priority 1 against tested additive allows and priority 2 through verified working streams; selected | Requires administrator-controlled Cilium CRDs/configuration and version-specific deployed acceptance |

Separate bootstrap meets both priorities; single-step activation fails independent
authority checks despite fewer operational steps. Dedicated namespaces make the
priority 1 credential exposure boundary explicit and preserve priority 2 producers;
mixed namespaces fail that boundary even if producers can run.

## Operational Contract

- Keep the installation namespace control-plane-only. Independently protect
  admission and persistent sandbox baselines from controller mutation.
- Store approvals, installation claims, journals and receipts in a separate
  administrator-owned authority namespace outside controller write authority.
- Maintain an external freeze of every release/admission/workload writer. Neither
  sequential checks nor optimistic Kubernetes updates constitute fencing.
- Drain, finalize or explicitly migrate old work with required old privileges
  available, then stop the old controller, revoke grants and verify drain before
  changing admission. Do not silently adopt objects or delete finalizers/data.
- Use exact reviewed artifacts, namespace/cluster identities and live inventory.
  On failure preserve evidence and freeze; never roll back old grants/activation.
- Keep supported stdio enabled. [SG-07 local acceptance](../development-plans/features/knowledge-provider-execution/sg07-local-acceptance.md)
  certifies the pinned single-node IPv4 Cilium 1.17.8/Kubernetes 1.31.1 profile.
  Actual producer lifecycles remain a separate SG-08 requirement.
- Protect Cilium policies/configuration from controller and unrelated writers.
  Generic `network-policy` mode lacks additive-allow certification. Verification
  is point-in-time: administrator removal of Cilium deny or disabled enforcement
  is outside the guarantee. Established streams are not retroactively revoked by
  admission; independent CNI isolation protects their network access.

The [installation/recovery contract](../features/knowledge-provider-execution/installation-recovery-contract.md)
defines current commands, flags, inventory and request-settlement responsibilities.
The [activation contract](../features/knowledge-provider-execution/activation-transaction-contract.md)
defines evidence binding, freshness, consumption and compensation. Administrator
bypasses remain outside the supported protocol; restricting administrators and
protecting authority-state authenticity are operational obligations.

## Consequences

Automatic quickstart/reset cannot safely resume Kubernetes producers. Operators
must prepare deterministic artifacts and explicitly review migration, recovery
and activation. Failed partial releases require new inventory review. Immutable
retirement intent is evidence of intent, not proof an outstanding request settled.
These costs preserve the supported lifecycle rather than disabling stdio or
removing finalization permissions without a replacement.
