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
