# ADR-0031: Kubernetes human access control

- Status: Proposed.
- Date: 2026-11-27.
- Scope: Kubernetes-mode authorization of people and automation that call the
  Kognition API (CLI, local Studio, hosted Studio, controllers acting for a user).
  Standalone-mode compatibility, the aggregated action API and agent capability
  authorization are out of scope; see [Out of scope](#out-of-scope).
- Design: [Kubernetes access control plan](../development-plans/features/kubernetes-access-control/plan.md).
- Requirements: FR-0410..FR-0419 in [PROJECT.md](../../PROJECT.md).
- Builds on: [ADR-0003](0003-apiserver-authentication.md) (standalone
  authentication), [ADR-0029](0029-workflow-execution-boundaries.md) (executor
  authority) and [ADR-0030](0030-external-agent-outbound-a2a.md).

## Context

In Kubernetes mode Kognition resources are CRDs served by the Kubernetes API
server, so authentication, authorization and audit already belong to the
cluster ([security guide](../security.md#deployment-modes-and-the-security-baseline)).
The repository does not yet define *what* a person may do with Kognition's 55
resource kinds:

- The Helm chart ships roles only for Kognition's own controllers and gateways.
  An operator must hand-write RBAC for every human, with no guidance on which
  kinds are safe to grant together.
- Four custom authorization checks already exist and are undocumented as a
  contract: `use` on `workflows`, `invoke` on `agents`, `approve` and `reject`
  on `runtimeapprovals/decision`, and `reconcile` on `runtimeapprovals/reconcile`.
- Two human decision paths are bound to the authenticated caller by admission:
  `ApprovalDecision.spec.approver` must equal the requesting user, and an
  `AutonomyReview` decision requires `status.decidedBy` to equal the requesting
  user.
- Interactive Agent chat from the CLI and Studio creates an `AgentInstance` and
  polls it. No shipped human role can create one, and `invoke` is not checked for
  people on that path. Granting `create` and `get` on `agentinstances` would let
  any holder run any Agent in the namespace and read every caller's results,
  because RBAC cannot scope to the creator. (An unused runtime-direct variant
  needs `pods/portforward` and `secrets` get; it is not offered.)

Enterprises connect identity providers to Kubernetes themselves (OIDC, managed
cloud integrations, certificates) and bind the resulting users and groups to
Roles. Kognition must fit that model without owning it.

## Decision

1. **Native Kubernetes RBAC is the authorization model for people in
   Kubernetes mode.** Kognition defines `rbac.authorization.k8s.io/v1`
   ClusterRoles for its resource kinds and actions. Operators grant them with
   ordinary RoleBindings and ClusterRoleBindings to `User`, `Group` and
   `ServiceAccount` subjects. Kognition adds no user, group or human-role store
   and does not evaluate bindings itself; the cluster's configured authorizers
   decide.
2. **The boundary is the Kubernetes contract.** Kognition consumes the
   authenticated Kubernetes principal and authorization decisions. How an
   identity provider, claims, cloud IAM or directory groups become Kubernetes
   users and groups is the platform operator's responsibility. Kognition ships no
   provider-specific logic and treats no provider as a release gate.
3. **Every shipped kind has exactly one access class**, and the shipped roles are
   generated from that classification. A new CRD cannot be added without
   classifying it. Roles list resources and verbs explicitly: no wildcards, no
   `deletecollection`, and no write access to `*/status` except
   `autonomyreviews/status`, because an autonomy decision is a status update whose
   `decidedBy` admission binds to the caller.
4. **Roles are inert and composable.** The chart installs the ClusterRoles with
   no bindings. They are not aggregated into Kubernetes' built-in `view`,
   `edit` or `admin` unless the operator opts in. Workspace access is a
   RoleBinding in the workload namespace; a workspace is one namespace in this
   design.
5. **Separate duties.** No shipped role combines authoring definitions, creating
   delegation resources (grants, `SecretRef`, `AgentRole`), and deciding
   approvals. `kognition-platform-admin` is not an exception. The one named
   overlap is `kognition-autonomy-reviewer`, which decides through
   `autonomyreviews/status`. Controller-owned status is writable only by
   controllers. `kognition-access-admin` is a privileged delegator: native RBAC
   lets a holder of `bind` grant any listed role, so separation of duties across
   subjects is the operator's responsibility.
6. **Custom actions are real RBAC permissions.** The existing `use`, `invoke`,
   `approve`, `reject` and `reconcile` checks are a documented, stable contract
   expressed as RBAC rules, and any new action follows the same pattern.
7. **Authority ceiling.** A user-selected capability (a workflow, a grant, a
   definition) is authorized against the user's own identity with a
   `SubjectAccessReview` before it is accepted. Two cases are not violations and
   must stay named and bounded: execution under an operator-bound identity the
   operator configured (the ADR-0029 workflow executor, whose target invocation is
   authorized as the executor while submission is authorized as the caller's
   `use`), and controller-owned provisioning that follows from an object the user
   may create (a `MemoryClaim` causing a `MemoryVolume`). The `use` check is
   enforced only for protected `WorkflowRun` submissions today; authorizing every
   creation is a tracked gap. A hosted Studio forwards a credential the cluster
   accepts for each user and never runs user requests as a shared privileged
   identity. Broad impersonation is not a default.
8. **Delegation resources are privileged.** Kubernetes `bind` and `escalate`
   protections do not cover Kognition grants. They are a separate role
   (`kognition-grant-admin`). `SecretRef` is a separate `credential` class
   because its object can contain the credential; viewers and auditors cannot read
   it. How creation of a grant is constrained is an open decision (OD-2).

## Open decisions

| ID | Question | Proposed default | Blocks |
| --- | --- | --- | --- |
| OD-1 | How does an end user chat with an Agent, given that no human role can create an `AgentInstance` and `invoke` is not checked for people on that path? | No chat role until either admission checks `invoke` with the caller's identity on `AgentInstance` creation and result visibility is scoped, or the aggregated action API exists (separate, separately approved stack). Until then chat is documented as an unscoped privileged action. | FR-0416 |
| OD-2 | How is creation of a grant constrained so a grant-admin cannot delegate more than they hold? | Admission authorizes the creator with a `SubjectAccessReview` on the target before accepting the grant. | FR-0413 |
| OD-3 | Install roles by default? | Yes, inert, with a Helm switch to disable. | Chart task |
| OD-4 | Hosted Studio identity mode. | Per-user credential forwarded to the Kubernetes API; impersonation requires its own ADR. | FR-0417 |
| OD-5 | How does an auditor read evidence stored outside CRDs (ConfigMap ledgers, HTTP trust query)? | Define in a later design; the first auditor role covers CRD evidence only. | FR-0415 |

OD-1 and OD-2 sit on privileged delegation boundaries, so they are worked through
below with options, a recommendation, and the exact decision the owners are asked
to make. Both are recommendations for platform and security-owner review. Nothing
here is accepted until they approve it.

### Proposal for OD-2: constraining who may create a grant

**Problem.** `kognition-grant-admin` can create `ToolGrant`, `ModelGrant`,
`MemoryGrant`, `ExternalAgentGrant`, `CrossNamespaceGrant`, `AgentRole` and
`AgentRoleBinding`. Nothing records that the creator may delegate the thing it
names, so a role that can write grants can delegate any Tool, Model, Memory or
cross-namespace share in its namespace, and `CrossNamespaceGrant` accepts `to: "*"`
and `name: "*"`. Kubernetes closes the same hole for RBAC with the `bind` and
`escalate` checks; Kognition grants have no equivalent.

| Option | How it works | Strengths | Weaknesses |
| --- | --- | --- | --- |
| A. Do nothing in admission; treat grant-admin as trusted | Rely on binding the role to few subjects. | No code. | A custom role with `create` on a grant kind can delegate anything. No way to scope a grant author to some Tools or Models. Contradicts the least-privilege goal. |
| B. Require `get` on the target | Admission checks the creator can read the named object. | Simple. | Every viewer can read, so it constrains nothing. |
| C. A dedicated `grant` verb on the target (recommended) | Admission asks the cluster whether the creator may `grant` on the named target, for example verb `grant` on `tools/<name>`. | Mirrors Kubernetes `bind`. One verb across kinds. Scopes with `resourceNames` like `use` and `invoke` already do. Fails closed. Reuses the proven `SubjectAccessReview` pattern. | New verb to document and maintain. The shipped role must carry it, or it would break today's behavior. |
| D. The creator must hold the granted permission (Kubernetes `escalate` model) | Check the creator holds each permission the grant confers. | Exact for `AgentRole` rules (resources and verbs). | Tool `invoke`, Model use and Memory access are not human RBAC actions, so there is nothing to hold. Needs new verbs per kind anyway. |
| E. Two-person approval for grants | An `ApprovalDecision`-style gate on grant creation. | Strongest separation. | New workflow and latency; a different product feature. |

**Recommendation: C.** Admission authorizes the *requesting user* on the target
before accepting the grant, failing closed on any evaluation error, anonymous user
or unresolvable target. The controller's own service account is the only exemption.

| Grant | Required, in the grant's namespace |
| --- | --- |
| `ToolGrant` | verb `grant` on `tools/<toolRef>` |
| `ModelGrant` | verb `grant` on `models/<modelRef>` |
| `MemoryGrant` | verb `grant` on `memoryclaims/<memoryClaimRef>` |
| `ExternalAgentGrant` | verb `grant` on `externalagents/<ref>` |
| `AgentRole` | verb `grant` on `agentroles/<name>`; for each rule, the creator must also hold the listed verbs on the listed kinds (option D applies here because the rules are RBAC-shaped) |
| `AgentRoleBinding` | verb `grant` on `agentroles/<roleRef>` |
| `CrossNamespaceGrant` | verb `grant` on each named kind and name in `from`; a wildcard name or `to: "*"` requires the verb cluster-wide (no namespace) |

The check runs on create and on any update that changes the target, and not on
delete, so revocation stays available. `kognition-grant-admin` is generated with
`grant` on those kinds in its namespace, so the shipped role keeps working; an
operator narrows it by binding a custom Role with `resourceNames`. Existing grants
are untouched. The plan's `grant` class and the catalog gain the verb, with the
catalog test enforcing that no other role holds it.

**What the owners are asked to decide.**

1. Approve option C and the verb name `grant`.
2. Confirm the shipped `kognition-grant-admin` holds `grant` namespace-wide by
   default (no regression) and that narrowing is the operator's job.
3. Confirm wildcard and cross-namespace shares require the cluster-wide verb.
4. Confirm `AgentRole` also applies the hold-the-permissions rule.

**Residual after C.** It prevents delegating what the author may not delegate. It
does not stop a trusted grant-admin who holds `grant` from granting it to the wrong
Agent; that is what the grant review process, not RBAC, is for. It also does not
separate authoring from granting for a subject bound to both roles, which is a
bundle choice (see the recommended bundles).

**Implementation (T-0452).** An admission hook beside the Solution and AgentJob
authorizers, the verb added to the catalog and chart, unit and handler tests, and a
real-API-server test, written first. `AgentRole` rule resources are Kognition kind
strings, so the hook maps them to plural resource names and rejects any it cannot
map (fail closed).

### Proposal for OD-1: end-user chat with an Agent

**Problem.** CLI and Studio chat create an `AgentInstance` and poll it. No shipped
role can create one, `invoke` is not checked for a person on that path, and RBAC
cannot limit a reader to the instances they created. Granting `create` and `get`
as-is would let any holder run any Agent and read every caller's prompts and
results in the namespace.

| Option | How it works | Strengths | Weaknesses |
| --- | --- | --- | --- |
| A. Ship nothing; document chat as privileged | Status quo. | No risk added. | Least-privilege chat is impossible through the CLI or Studio. |
| B. Admission `invoke` check on `AgentInstance` creation, no read access | Require `invoke` on the Agent, grant `create` only. | Closes the authority gap. | The client cannot read the answer without `get`; unusable. |
| C. Admission `invoke` check plus `create` and `get`, with namespace-wide visibility accepted and documented (recommended now) | A new, narrowly named role `kognition-chat` holds `create` and `get` on `agentinstances`; admission requires `invoke` on the named Agent for the caller. | Small, reuses the proven check, and fits the model: the namespace is the isolation boundary, and `run-viewer` and `auditor` already read every instance there. Per-Agent scoping works through `resourceNames` on `invoke`. | Anyone holding `kognition-chat` can read other holders' chat prompts, outputs and tool-call details in the same namespace. Isolation between people needs separate namespaces. |
| D. Aggregated action API (the staged stack) | A server filters by the caller's identity. | True per-caller isolation, and client-certificate parity for approvals. | Large; separate approval and security review. |

**Recommendation: C now, D as the long-term answer.** C delivers least-privilege
chat without waiting for the extension API, but only if the owners explicitly accept
its exposure. D remains the way to get per-caller result isolation and is staged
separately.

Requirements if C is approved:

- Admission checks verb `invoke` on the named Agent for the requesting user on
  `AgentInstance` creation, failing closed. The controller service account is
  exempt (it creates instances for Agent replicas and jobs after their own checks).
  The same check already protects `AgentJob`.
- `kognition-chat` is a separate role, not part of `kognition-consumer`, so the
  result-visibility exposure is an explicit, opt-in binding.
- The role is added to the catalog with its own tests, and the documentation states
  plainly that chat prompts, outputs and tool-call details are readable by every
  holder of `kognition-chat`, `kognition-run-viewer` and `kognition-auditor` in the
  namespace.

**What the owners are asked to decide.**

1. Accept namespace-wide visibility of chat output and tool-call detail for holders
   of the role, or require D before shipping any chat role.
2. If accepted, approve a separate `kognition-chat` role.
3. Confirm per-person isolation is delivered by namespace separation until D exists.

**Implementation (T-0468).** The `invoke` check on `AgentInstance`, the role and its
envtest matrix, and the documentation, written first. If the owners require D, this
task is replaced by US-0121.

### Review of these proposals against Kubernetes practice

The proposals were checked against the Kubernetes documentation and the code.

**Confirmed.**

- *A custom verb checked with a `SubjectAccessReview` is the established pattern.*
  The RBAC documentation defines `bind` and `escalate` this way: creating or
  updating a binding needs all the permissions of the role **or** the `bind` verb
  on it ([RBAC, privilege escalation prevention][rbac]). A `grant` verb is the
  same idea for Kognition grants, and `use`, `invoke`, `approve` and `reconcile`
  already work this way here.
- *`CrossNamespaceGrant` already follows the Gateway API `ReferenceGrant` model.*
  The owner of the referenced object creates the grant in its own namespace
  ([ReferenceGrant][refgrant]). Here the resolver reads grants from `spec.from`,
  so the grant must live in the owning namespace.
- *Result isolation cannot be done with RBAC alone.* `resourceNames` limits
  requests that name an object (and `list` or `watch` only with a matching
  `metadata.name` field selector); it cannot limit top-level `create`, and RBAC has
  no notion of "objects this user created".

**Refinements adopted in the recommendations above.**

1. **Require `spec.from` to equal the grant's namespace.** Today a mismatch is
   merely inert. The check should state it, so the verb is always evaluated in the
   namespace that owns the target.
2. **Wildcards are broader than the model they resemble.** `ReferenceGrant` has no
   wildcard namespace and names each peer. `to: "*"` and `name: "*"` need the
   cluster-wide `grant` verb (as proposed), and the owners should also decide
   whether to keep them at all (decision 2 below).
3. **Wildcard roles inherit custom verbs.** The RBAC documentation warns that a
   wildcard verb grants any custom verb added later. An operator role with
   `verbs: ["*"]` on `kognition.dev` would hold `grant`, `use`, `invoke` and
   `approve`. The documentation must say so; the shipped roles already forbid
   wildcards by test.
4. **Chat results are more than prompts.** `AgentInstance.status` carries
   `output` and `toolCallsDetail`, which holds bounded tool arguments and results.
   The visibility the owners accept for option C includes those. The spec a person
   can set is small (`agentRef`, `revision`, `agentRevision`, `sessionContext`),
   `status` is a subresource and is not writable, and `CognitiveQuota` admission
   already applies on create, so the write side adds little beyond `invoke`.
5. **A fifth option for OD-1 exists.**

| Option | How it works | Strengths | Weaknesses |
| --- | --- | --- | --- |
| E. Per-creator read grant | Admission stamps the creator on the instance. A controller creates a Role with `get` limited by `resourceNames` to that instance, and a RoleBinding to the creator. The chat role holds `create` only. | Per-caller isolation with native RBAC and no extension API. | The controller must be able to create Roles and RoleBindings, which is the delegation surface this ADR restricts. Propagation delay before the first read. `list` and `watch` stay unavailable. Needs its own security review. |

   E sits between C and D. It is worth choosing only if the owners reject
   namespace-wide visibility but do not want to wait for D. It is not the
   recommendation, because it adds a privileged controller path for a feature C
   delivers with an existing check.

**A decision the proposals leave to the owners: the admission mechanism.** The
checks in T-0452 and T-0468 depend only on fields of the object being admitted, so
they can be written either as validating webhook hooks (as T-0469 and T-0470 are) or
as `ValidatingAdmissionPolicy` with the CEL `authorizer` variable, which is stable
since Kubernetes 1.30 and runs in the API server without a webhook
([ValidatingAdmissionPolicy][vap]).

| | Webhook hook (current) | ValidatingAdmissionPolicy |
| --- | --- | --- |
| Fits the existing chain, shared fail-closed tests and the controller exemption | Yes | The exemption is a username baked in by the chart |
| Can read another object (a `Solution` naming an `AgentService`) | Yes | No |
| Depends on webhook availability | Yes (mitigated by `failurePolicy: Fail`) | No |
| Kubernetes floor | None added | 1.30 |
| Proof on a real API server | Hook unit tests plus a SAR-backed envtest | Real enforcement in envtest |

   The recommendation is to **keep one house style for now and use the webhook
   chain**, because T-0469 needs object lookup, the chain and its tests exist, and a
   second mechanism raises the review burden. Record a follow-on to move
   field-only checks (`AgentJob`, grants, `AgentInstance`) to
   `ValidatingAdmissionPolicy` once the supported Kubernetes floor is declared as
   1.30 or later. The owners may prefer to start with policies; the checks are small
   enough that either choice is reversible.

**A gap found by this review: webhook `failurePolicy` is operator-settable.** The
chart defaults `webhook.failurePolicy` to `Fail`, but the value schema accepts
`Ignore`. With `Ignore` every authorization-in-admission check (the new
Solution, AgentJob, grant and chat checks, and the existing approval validators)
silently fails open while the webhook is unavailable. Recommendation: the
validating webhook that carries these checks must be `Fail`; reject `Ignore` in the
schema, or require an explicit, named opt-out that the documentation calls
unsupported for production (plan G11, T-0472).

**A small fidelity gap.** The Solution, AgentJob and (proposed) grant reviews send
the user name, groups and extra values but not the user `uid`, which the existing
workflow `use` review does send. Authorizers keyed on `uid` would not see it.
Propagate it (T-0473).

### How to proceed

Work that does not depend on the owners can start now. The two decisions unblock the
rest.

| Step | Depends on | Status |
| --- | --- | --- |
| T-0472 fail-closed webhook `failurePolicy`, T-0473 propagate `uid` | Nothing | Ready |
| T-0467 authorize every `WorkflowRun` creation | Nothing | Ready |
| US-0118 documentation, US-0119 access-check command, US-0120 kind end-to-end | Nothing (the documents state only verified behavior) | Ready |
| OD-3 install default, confirm with the maintainer | Maintainer | Open, implemented as proposed |
| OD-2 decision, then T-0452 | Platform and security owners | Blocked |
| OD-1 decision, then T-0468 (or US-0121) | Platform and security owners | Blocked |
| Mechanism decision (webhook or policy) for T-0452 and T-0468 | Platform owners | Open, default webhook |

**Decisions requested from the owners, in one place.**

1. OD-2: approve option C, the `grant` verb, shipped `kognition-grant-admin` holding
   it namespace-wide, cluster-wide verb for wildcard shares, and the
   hold-the-permissions rule for `AgentRole`.
2. OD-2 sub-decision: keep `to: "*"` and `name: "*"` in `CrossNamespaceGrant`, or
   deprecate them in favor of explicit lists as `ReferenceGrant` does.
3. OD-1: accept namespace-wide visibility of chat output and tool-call detail for
   `kognition-chat` holders, choose option E, or require D first.
4. Admission mechanism for T-0452 and T-0468: webhook chain (default) or
   `ValidatingAdmissionPolicy`, and the Kubernetes floor if policies are chosen.
5. `failurePolicy`: reject `Ignore` for the validating webhook.

[rbac]: https://kubernetes.io/docs/reference/access-authn-authz/rbac/#privilege-escalation-prevention-and-bootstrapping
[refgrant]: https://gateway-api.sigs.k8s.io/reference/api-types/referencegrant/
[vap]: https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/

## Consequences

**Positive.**

- Reuses the identity and authorization integrations enterprises already operate.
  No Kognition identity-provider or directory code.
- Existing checks (`use`, `invoke`, approvals) become a documented contract
  instead of incidental behavior.
- A classification test makes unclassified or over-granted kinds a build failure.
- Roles can be reviewed, diffed and applied through GitOps like any RBAC.

**Negative / to accept.**

- Kubernetes RBAC has no deny rules and no field-level authorization. Where a
  resource mixes sensitive and non-sensitive fields, access to the kind exposes
  both; such kinds are placed in a separate class (`data`) instead of being
  filtered.
- `resourceNames` cannot restrict top-level `create`. Per-object restrictions on
  creating requests need subresource actions or admission checks.
- `bind` restriction by `resourceNames` denies additional, unauthorized
  permissions, not every non-Kognition role: native RBAC also lets a principal bind
  a role whose permissions it already holds. A strict allowlist of bindable roles
  needs a separate admission restriction owned by the operator.
- `kognition-access-admin` can bind any listed role to anyone in the namespace,
  including grant and approval roles.
- Until OD-1 is resolved, least-privilege end-user chat is not achievable through
  the CLI or Studio. This is a documented limitation, not an accepted
  permanent state.
- Authorization semantics differ by cluster configuration. Native RBAC is the
  contract; clusters that add or replace the authorizer chain must be qualified
  by their operator.

## Alternatives considered

| Option | Assessment |
| --- | --- |
| Kognition-owned role store for people (extend `ControlPlaneRole`) | Rejected for Kubernetes mode. It duplicates the cluster's authorizer, loses enterprise integrations and audit, and its allow/deny semantics differ from RBAC. Remains the standalone model. |
| Argo CD-style application ACL | Rejected. It is a parallel policy engine with its own default floor and deny semantics; it would not compose with cluster identity and audit. |
| Map IdP groups to ServiceAccounts in the server (Argo Workflows SSO RBAC) | Rejected as the default. It replaces the caller's identity with a shared one and loses per-user audit. |
| Aggregate Kognition permissions into built-in `view`/`edit`/`admin` by default | Rejected as default. It silently widens existing cluster roles; offered as opt-in. |
| Only document RBAC examples; ship no roles | Rejected. Without a maintained catalog every operator re-derives it, and drift from new kinds goes unnoticed. |
| Use a proxy that holds one privileged identity for all users | Rejected. It removes per-user authorization and audit. |

## Out of scope

- **Standalone mode.** Its `ControlPlaneRole` model is unchanged. Compatibility is
  a separate follow-on.
- **Aggregated action API.** A Kubernetes extension API for `invoke`, approvals
  and similar actions is staged after the native roles and needs its own approval
  and security review.
- **Agent capability authorization.** `AgentRole`, grants and gateways govern
  what an *Agent* may do and are unchanged.
- **Identity provider configuration.** Owned by the platform operator.

## References

- Kubernetes RBAC and authorization: <https://kubernetes.io/docs/reference/access-authn-authz/rbac/>,
  <https://kubernetes.io/docs/reference/access-authn-authz/authorization/>.
- API aggregation and delegated authorization:
  <https://kubernetes.io/docs/tasks/extend-kubernetes/configure-aggregation-layer/>.
- Flux multi-tenancy (native RBAC with per-tenant execution identity):
  <https://fluxcd.io/flux/installation/configuration/multitenancy/>.
- Argo Workflows authentication modes and SSO RBAC:
  <https://argo-workflows.readthedocs.io/en/latest/argo-server-auth-mode/>,
  <https://argo-workflows.readthedocs.io/en/latest/argo-server-sso/>.
- Custom Roles bound to externally authenticated groups, as documented by managed
  Kubernetes offerings:
  <https://learn.microsoft.com/azure/aks/kubernetes-rbac-entra-id>,
  <https://cloud.ibm.com/docs/containers?topic=containers-understand-rbac>.
