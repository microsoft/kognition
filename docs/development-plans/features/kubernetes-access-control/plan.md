# Kubernetes access control: roles for people and automation

> **Status:** Proposed design. Nothing in this plan is shipped. The roles,
> catalog and checks described here do not exist until the tasks in
> [EPIC-0025](../../../../BACKLOG.md) land. Reader-facing documentation is added
> with the implementation, not before it.
> **Decision record:** [ADR-0031](../../../adr/0031-kubernetes-human-access-control.md).
> **Requirements:** FR-0410..FR-0419 in [PROJECT.md](../../../../PROJECT.md).
> **Scope:** Kubernetes mode only. Standalone compatibility, the aggregated action
> API and Agent capability authorization are separate (section 11).

## 1. Problem and decision

In Kubernetes mode, Kognition resources are CRDs, so the cluster already
authenticates and authorizes every request. What is missing is a maintained
answer to "what should this person be allowed to do with Kognition?":

- The chart grants permissions only to Kognition's own controllers and gateways
  ([ClusterRole template](../../../../deploy/helm/kognition/templates/clusterrole.yaml)).
- Operators must hand-write RBAC across 55 kinds and discover custom actions by
  reading code.
- Some kinds are dangerous to grant casually (grants, `SecretRef`, policy, memory
  content), and some kinds are not human-writable at all (controller state).

The decision: define Kubernetes `ClusterRole`s for Kognition and let operators bind
them to their existing users, groups and service accounts. Kognition does not
store human roles, evaluate bindings, or integrate identity providers.

## 2. Boundary of responsibility

| Concern | Owner |
| --- | --- |
| Authenticating a person (OIDC, certificates, managed-cloud integration) | Platform operator, through the cluster |
| Turning directory users, groups or cloud roles into Kubernetes users and groups | Platform operator |
| Choosing the cluster's authorizer chain | Platform operator |
| Defining Kognition ClusterRoles and the actions they name | Kognition |
| Binding a role to an enterprise group in a namespace | Platform operator or workspace administrator |
| Enforcing admission and runtime rules RBAC cannot express | Kognition |
| What an executing Agent may do | Kognition (`AgentRole`, grants, gateways); unchanged |

Kognition tests against Kubernetes contracts (RBAC objects, `SubjectAccessReview`,
`SelfSubjectRulesReview`). It adds no provider-specific code and no provider
qualification gate. A binding subject is whatever string the cluster's
authenticator produces; this plan uses neutral examples.

## 3. Current state (verified in source)

| Fact | Where |
| --- | --- |
| The chart installs roles only for controllers, webhooks and gateways. No human-facing role exists. | [templates](../../../../deploy/helm/kognition/templates/) |
| 55 CRDs, all in `kognition.dev`: 49 namespaced and 6 cluster-scoped (five classes and `KnowledgeBindingRecord`). 40 define a `status` subresource. | [CRDs](../../../../deploy/helm/kognition/crds/) |
| `workflows` + verb `use` is authorized with the submitter's identity, but only for submissions marked as requiring acceptance (the protected path). Ordinary `WorkflowRun` creation is not checked for `use` (G7). | [webhook.go](../../../../internal/k8s/webhook/webhook.go), [workflow_run_submission.go](../../../../internal/k8s/webhook/workflow_run_submission.go) |
| `agents` + verb `invoke` is authorized for the workflow executor's service account (the operator-bound executor identity of ADR-0029), not for the submitting person. The person's authority is the `use` check at submission; target invocation runs under the executor's authority. | [workflow_executor_target.go](../../../../internal/k8s/controller/workflow_executor_target.go) |
| `runtimeapprovals/decision` with verbs `approve` or `reject`, and `runtimeapprovals/reconcile` with verb `reconcile`, are authorized by `TokenReview` + `SubjectAccessReview`. | [runtime_approval_decision.go](../../../../internal/k8s/webhook/runtime_approval_decision.go), [runtime_approval_reconciliation.go](../../../../internal/k8s/webhook/runtime_approval_reconciliation.go) |
| `ApprovalDecision.spec.approver` must equal the requesting user; `spec` is immutable. | [approval_decision_validator.go](../../../../internal/admission/approval_decision_validator.go) |
| An `AutonomyReview` decision requires `status.decidedBy` to equal the requesting user. | [autonomy_review_validator.go](../../../../internal/admission/autonomy_review_validator.go) |
| CLI and Studio chat in Kubernetes mode reads the `Agent`, then creates an `AgentInstance` and polls it (`get`). It does not read Secrets or use `pods/portforward`. No shipped human role can `create` an `AgentInstance` (it is a `runtime` kind), and `invoke` is not checked for people on this path. The `pods`, `pods/portforward` and `secrets` helpers in `k8sclient.go` serve a runtime-direct variant that no active command or Studio route calls. | [agent_instance_invoke.go](../../../../cmd/kognitionctl/cmd/agent_instance_invoke.go), [chat.go](../../../../cmd/kognitionctl/cmd/chat.go), [k8sclient.go](../../../../cmd/kognitionctl/cmd/k8sclient.go) |
| An `AgentJob` is expanded by its reconciler without any check on who created it: `spec.agentSpec` is copied into a new `Agent` (authoring), and `spec.agentRef` is turned into an `AgentInstance` (invocation). `create` on `agentjobs` therefore carries authoring and invocation authority (G9). | [agent_job_reconciler.go](../../../../internal/controller/agent_job_reconciler.go) |
| `SecretRef` with the `internal` backend carries the credential value in an object annotation, so reading the object reads the credential. | [secret_ref.go](../../../../internal/gateway/secret_ref.go) |
| The Kubernetes-mode CLI resolves the caller with `SelfSubjectReview`. | [k8sclient.go](../../../../cmd/kognitionctl/cmd/k8sclient.go) |

Two rows matter most. Interactive chat is not a least-privilege action today
because no human role can create an `AgentInstance` and no check binds `invoke` to
that creation (G1). And `SecretRef` must not be readable through broad read roles
(section 5).

## 4. Principles

1. **Reuse the cluster.** One permission system for people: Kubernetes RBAC.
2. **Least privilege by construction.** Explicit resources and verbs only. No
   wildcards, no `deletecollection`, no `secrets`, no `pods/exec` or
   `pods/portforward`.
3. **Classification is mandatory.** A catalog assigns every kind exactly one access
   class; roles are generated from it; a test fails if a CRD is unclassified.
4. **Status belongs to controllers.** Human roles never write `*/status`, with one
   named exception (section 6).
5. **Inert and composable.** Roles carry no bindings and are not aggregated into
   built-in roles by default. People combine roles; no role implies another.
6. **Authority ceiling for user-selected capability.** When a user selects a
   capability (a definition, a delegation or a target), the user is authorized for
   that selection with their own identity before it is accepted. Two things are not
   a violation: execution under an operator-bound identity that the operator
   explicitly configured (the ADR-0029 workflow executor), and bounded
   controller-owned provisioning that follows from an object the user may create
   (for example the `MemoryVolume` a `MemoryClaim` causes). Both must be named,
   bounded and reviewed (section 8).
7. **Explicit non-guarantees.** RBAC cannot express deny, field-level, or
   per-object `create` restrictions. Where needed, use a separate class, an action
   subresource or admission, and say so.

## 5. Access classes

Every kind belongs to exactly one class. Classification is by the consequence of
granting access, not by where the kind sits in the API.

| Class | Meaning | Kinds |
| --- | --- | --- |
| `config` | Author-managed definitions | Agent, AgentAutoscaler, AgentService, Evaluation, EvaluationDataset, IntentGateway, KnowledgeBaseClaim, MemoryClaim, Model, ModelAutoscaler, ModelInvocationProfile, Prompt, Solution, Tool, Workflow |
| `knowledge` | Curated knowledge and indexing (can spend provider budget) | KnowledgeBase, KnowledgeBaseIndexRun, KnowledgeBaseIndexSchedule |
| `submit` | Requests that start work | AgentJob, EvaluationRun, WorkflowRun |
| `runtime` | Controller-owned execution records; may contain prompts and results | AgentInstance, Memory, MemoryVolume, ToolInvocationRun |
| `data` | Stored content | MemoryRecord |
| `grant` | Delegation of capability; high risk | AgentRole, AgentRoleBinding, CrossNamespaceGrant, ExternalAgentGrant, MemoryGrant, ModelGrant, ToolGrant |
| `credential` | Credential binding; reading the object can read the credential | SecretRef |
| `governance` | Policy, quota and trust configuration | AgentCommunicationPolicy, AgentPolicy, CognitiveAdmissionPolicy, CognitiveLimitRange, CognitiveQuota, TrustProfile |
| `decision` | Human decisions and their records | ApprovalDecision, AutonomyReview, RuntimeApproval |
| `platform` | Platform-owned namespaced kinds | ExternalAgent, Workspace |
| `class` | Cluster-scoped classes | AutonomyClass, KnowledgeBaseClass, MemoryClass, ModelClass, ToolClass |
| `system` | Internal controller state; no human write access | AgentIdentity, KnowledgeBindingRecord (cluster-scoped), RuntimeApprovalQuotaTicket, RuntimeApprovalTombstone, TrustProfileIndex |

Totals: config 15, knowledge 3, submit 3, runtime 4, data 1, grant 7, credential 1,
governance 6, decision 3, platform 2, class 5, system 5 = 55.

Notes that affect grants:

- `Tool` and `Model` are `config` but high impact: a Tool can launch a process or
  reach a network target. Runtime use is still gated by `ToolGrant`/`ModelGrant`
  and signature policy; granting `kognition-builder` does not grant use.
- `SecretRef` is its own `credential` class, not `config` or `grant`: it lets
  platform components read a named backing Secret and, with the `internal` backend,
  stores the credential value in an annotation. Any role that can read the object
  can read the credential, and RBAC cannot hide an annotation. Only
  `kognition-grant-admin` receives it. `kognition-viewer` and `kognition-auditor` do
  not, so an auditor cannot inspect `SecretRef` objects until credential values
  leave the object (G8).
- `Solution` is `config`, but a controller creates resources from it. It needs the
  confused-deputy review in section 8.
- `MemoryRecord` is isolated in `data` because RBAC cannot hide fields.
- `EvaluationRun` is `submit` because it consumes provider budget; the builder
  role receives it as a named exception because evaluating is part of authoring.

## 6. Role catalog

Verbs: **R** = `get`, `list`, `watch`. **W** = R plus `create`, `update`, `patch`,
`delete`, because a role that can change a kind but not read it is not usable with
declarative tools. No role includes `deletecollection`. No role writes `*/status`
except `kognition-autonomy-reviewer`, whose decision path is a status update by
design. Roles bound with a RoleBinding contain only namespaced kinds. "Class"
means the kinds in section 5.

| Role | Bind with | Grants |
| --- | --- | --- |
| `kognition-viewer` | RoleBinding | R on `config`, `knowledge`, `governance`, `grant`, `platform` (not `credential`) |
| `kognition-run-viewer` | RoleBinding | R on `submit`, `runtime`, `decision` |
| `kognition-data-reader` | RoleBinding | R on `data` |
| `kognition-builder` | RoleBinding | W on `config`; `create` and `delete` on `evaluationruns` |
| `kognition-curator` | RoleBinding | W on `knowledge` |
| `kognition-consumer` | RoleBinding | `create` on `workflowruns` and `agentjobs`; verb `use` on `workflows`; verb `invoke` on `agents` |
| `kognition-operator` | RoleBinding | everything in `kognition-run-viewer`; verb `reconcile` on `runtimeapprovals/reconcile` |
| `kognition-approver` | RoleBinding | R on `runtimeapprovals`; R and `create` on `approvaldecisions`; verbs `approve`, `reject` on `runtimeapprovals/decision` |
| `kognition-autonomy-reviewer` | RoleBinding | R on `autonomyreviews`, `trustprofiles`; `update`, `patch` on `autonomyreviews/status` |
| `kognition-policy-admin` | RoleBinding | W on `governance` |
| `kognition-grant-admin` | RoleBinding | W on `grant` and `credential` |
| `kognition-access-admin` | RoleBinding | R on `roles`, `rolebindings`; W on `rolebindings`; verb `bind` on `clusterroles` limited by `resourceNames` to the namespace-bound `kognition-*` roles. **A privileged namespace-level delegator**: see the rules below |
| `kognition-auditor` | RoleBinding | R on every namespaced class except `data` and `credential` (`config`, `knowledge`, `submit`, `runtime`, `grant`, `governance`, `decision`, `platform`, `system`) |
| `kognition-class-viewer` | **ClusterRoleBinding** | R on `class` |
| `kognition-platform-admin` | **ClusterRoleBinding** | W on `class` and `platform`; R on `system` including `knowledgebindingrecords` |

Rules about the catalog:

- A role never implies another. A person who builds, runs and reads results holds
  `kognition-builder`, `kognition-consumer`, `kognition-run-viewer` and, to pick
  classes, `kognition-class-viewer`.
- `kognition-platform-admin` is not cluster-admin and does not include builder,
  grant or approval rights. It must be combined deliberately.
- The `system` class is never writable by a human role. `knowledgebindingrecords`
  is cluster-scoped, so reading it needs a ClusterRoleBinding.
- Cluster-scoped classes cannot be reached by a RoleBinding. Builders need
  `kognition-class-viewer` bound with a ClusterRoleBinding to choose a class.
- `kognition-platform-admin` writes `platform` kinds cluster-wide because it is
  bound with a ClusterRoleBinding. A namespaced platform administrator is composed
  by binding a narrower role the operator defines; this plan does not ship one.
- Per-object scoping works for custom action verbs and subresources through
  `resourceNames` (for example `use` on one workflow). It does not work for
  top-level `create`, so creating a `WorkflowRun` is permitted namespace-wide.
  Which workflow a run may start is restricted by the `use` check, and today that
  check runs only for protected submissions (G7). Until every `WorkflowRun` creation
  is authorized, a per-workflow `use` grant is not a boundary against a person who
  can create an ordinary `WorkflowRun`.
- `kognition-access-admin` is a privileged delegator, not a low-risk role. Native
  RBAC lets a principal with `bind` on a ClusterRole grant it without holding its
  permissions, so an access-admin can bind any listed role, including
  `kognition-grant-admin` and `kognition-approver`, to themselves or anyone else in
  the namespace. `resourceNames` limits which roles can be bound, not which
  combinations, so it does not preserve separation of duties inside the list.
  Treat the role like namespace-admin over Kognition authority: bind it to few,
  trusted, audited subjects, and separate duties with the operator's own controls
  (for example admission policy on RoleBindings).
- `kognition-access-admin` can also revoke any RoleBinding in its namespace. `W`
  includes `delete`, and Kubernetes' `bind` and subset checks apply to creating and
  changing a binding, not to deleting one. The role can therefore remove bindings it
  could not create, including the bindings platform components rely on in a managed
  namespace. This is availability and revocation authority, not an escalation: the
  role still cannot create a binding for a role outside its allowlist, and cannot
  add itself to one by editing. Revocation is part of the job (removing access
  granted earlier), so `delete` stays. The authority is qualified, not removed:
  bind the role only where namespace-wide revocation is acceptable, and treat
  platform bindings as reconciled state (a Helm upgrade or GitOps sync restores
  them). Restricting deletion to approved role references needs an admission policy
  owned by the operator or the optional hardening in T-0471 (G10).
- `kognition-consumer` holds `create` on `agentjobs`, which is only safe with
  caller-identity checks at submission: `invoke` on the named Agent for
  `spec.agentRef`, and `create` on `agents` for an inline `spec.agentSpec`, so that
  a consumer cannot author through a job or run an Agent outside a scoped `invoke`
  grant. These checks are a prerequisite for shipping the permission (T-0470, G9).

### Recommended bundles

| Who | Roles |
| --- | --- |
| Agent developer (Diego) | viewer, class-viewer, builder, consumer, run-viewer |
| Platform engineer (Patricia) | viewer, class-viewer, platform-admin, access-admin, operator |
| Governance officer (Gabriela) | viewer, policy-admin, grant-admin, autonomy-reviewer, auditor |
| On-call (Sam) | viewer, run-viewer, operator, class-viewer |
| Knowledge engineer (Karim) | viewer, curator, class-viewer |
| Business approver | approver, run-viewer |
| Application user | consumer, run-viewer |

These are examples for documentation. They are not roles.

### Binding example

Subject names are whatever the cluster's authenticator produces; the group below
is illustrative.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ai-builders-team-a-dev
  namespace: team-a-dev
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kognition-builder
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: ai-builders
```

## 7. Custom actions contract

| Action | RBAC rule | Checked by |
| --- | --- | --- |
| Use a workflow | resource `workflows`, verb `use`, name = workflow | workflow submission admission, with the submitter's identity, for protected submissions only today (G7) |
| Invoke an agent | resource `agents`, verb `invoke`, name = agent | workflow executor target authorization, for the executor identity only; not checked for a person creating an `AgentInstance` (G1). Required for `AgentJob` submission by admission (T-0470, G9) |
| Author through a job | resource `agents`, verb `create`, for an inline `AgentJob.spec.agentSpec` | admission (T-0470, G9) |
| Approve or reject a runtime approval | resource `runtimeapprovals/decision`, verbs `approve`, `reject`, name = approval | decision endpoint (`TokenReview` + `SubjectAccessReview`) |
| Reconcile a runtime approval | resource `runtimeapprovals/reconcile`, verb `reconcile` | reconciliation endpoint |
| Vote on a workflow approval gate | resource `approvaldecisions`, verb `create`; `spec.approver` = requesting user | admission |
| Decide an autonomy review | resource `autonomyreviews/status`, verbs `update`, `patch`; `decidedBy` = requesting user | admission |

These verbs are not endpoints that Kubernetes creates for a CRD. A CRD supports
only the `status` and `scale` subresources. The checks work because Kognition asks
the cluster a `SubjectAccessReview` about a name it defines. That is a valid use of
RBAC, but it means a bare `kubectl` user cannot "do" these verbs: a Kognition
component must expose the operation and perform the check.

The `invoke` verb in `kognition-consumer` is granted for the executor-targeted check
and for `AgentJob` submission (T-0470); it does not by itself let a person chat with
an Agent. Chat creates an `AgentInstance`, which no shipped human role can do (G1).

The decision endpoint accepts bearer tokens through `TokenReview`. Client
certificate or exec-plugin users who cannot present a bearer token are not covered
by that path; closing this is the purpose of the later aggregated action API.

## 8. Separation of duties and threat considerations

| Risk | Treatment |
| --- | --- |
| Builder grants itself access | Grants and `SecretRef` are only in `kognition-grant-admin`; the builder role excludes them. Constraining what a grant-admin may delegate is open (ADR-0031 OD-2). |
| Self-approval: a person holds consumer and approver and approves their own run | RBAC cannot express this. Verify whether admission already compares submitter and approver; if not, add the check or require roles to be held by different people. Task in EPIC-0025. |
| Confused deputy: a controller creates resources from a user-authored object (`Solution`, `MemoryClaim`, `KnowledgeBaseIndexSchedule`, `AgentAutoscaler`, workflow collaboration) | Classify each controller-mediated path. Bounded controller-owned provisioning that follows from an object the author may create is allowed (a builder's `MemoryClaim` makes the reconciler create a `MemoryVolume`, which no human role may create). A user-selected definition or delegated capability must not exceed what the author could select. Review every path against this rule. Task in EPIC-0025. |
| Job submission launders authority: a consumer with `create` on `agentjobs` authors an Agent through `spec.agentSpec` or runs any Agent through `spec.agentRef` | Admission authorizes the requesting user with `invoke` on the named Agent and `create` on `agents` for an inline spec, fail closed; the controller's own service account, which creates jobs for Solutions, Workflows and Evaluations after their own checks, is the only exemption (T-0470, G9). |
| Revocation of platform bindings: access-admin deletes a RoleBinding it could not create | Kubernetes does not restrict deletion by role reference. Documented and qualified as namespace-wide revocation authority; platform bindings are reconciled state; optional admission hardening in T-0471 (G10). |
| Namespace-wide approval eligibility | `create` on `approvaldecisions` cannot name a gate. Eligibility is per namespace. Narrower eligibility is a separate feature. |
| Wildcard growth | Roles list explicit resources. A new CRD is added to a role only through the catalog. |
| Bulk delete | `deletecollection` is excluded from every shipped role. |
| Secret read as privilege escalation | No shipped role grants `secrets`. Active chat paths do not read Secrets. `SecretRef` objects can carry credential values, so they are in the `credential` class (G8). |
| Over-broad `bind` | `kognition-access-admin` limits `bind` with `resourceNames`; Kubernetes' own escalation checks still apply. Both limits are partial: `bind` lets the holder grant a listed role without holding it, `resourceNames` does not keep duties separate within the list, and a principal that already holds all of a role's permissions can bind it without `bind` (the subset case). Access-admin is a privileged delegator (section 6). |
| Stale authority after revocation | Kubernetes evaluates RBAC per request. Group membership freshness depends on the operator's authentication integration and token lifetime and is documented, not guaranteed instant. |

## 9. Known gaps

| ID | Gap | Treatment |
| --- | --- | --- |
| G1 | Chat creates an `AgentInstance` and polls it, and no shipped human role can create one. Granting `create` and `get` on `agentinstances` would not be least privilege: `invoke` is not checked for people on that path, and `get` exposes every caller's prompts and results in the namespace because RBAC cannot scope to the creator. | No chat role is shipped. Fix by checking `invoke` with the caller's identity on `AgentInstance` creation and scoping result visibility, or by the action API (FR-0416); both need separate approval. |
| G2 | Approval decisions accept bearer tokens only. | Documented. Action API stack. |
| G3 | Audit and trust evidence in ConfigMaps or an HTTP query API is not governed by CRD RBAC. | `kognition-auditor` covers CRD evidence only. Design later (OD-5). |
| G4 | No deny and no field-level RBAC. | Mitigated by classes, subresource actions and admission. |
| G5 | Hosted Studio has no per-user identity design. | Design in the plan for FR-0417; implementation later. |
| G6 | No command shows a person what they may do. | `kognitionctl` access check using `SelfSubjectRulesReview` (FR-0415). Advisory only. |
| G7 | `use` on `workflows` is checked only for protected `WorkflowRun` submissions. An ordinary `WorkflowRun` create bypasses the per-workflow restriction. | Documented. A task in EPIC-0025 decides and implements authorizing every `WorkflowRun` creation with the submitter's identity. |
| G8 | `SecretRef` with the `internal` backend stores the credential in an annotation, so readers of the object read the credential. | `credential` class held only by grant-admin. Auditor and viewer cannot read it. Moving credential values out of the object is a follow-up that would allow broader reads. |
| G9 | `create` on `agentjobs` lets the creator author an Agent (inline `spec.agentSpec`) and invoke any Agent in the namespace (`spec.agentRef`) with no caller-identity check, so the consumer role can bypass builder rights and scoped `invoke` grants. | Admission authorizes the requesting user: `invoke` on the named Agent, `create` on `agents` for an inline spec, failing closed, with the controller's own service account as the only exemption (T-0470). Prerequisite for shipping the consumer role's `agentjobs` permission. |
| G10 | `kognition-access-admin` can delete any RoleBinding in its namespace, including bindings platform components rely on; Kubernetes' `bind` checks do not cover deletion. | Documented as namespace-wide revocation authority. Optional admission hardening in T-0471. |
| G11 | `webhook.failurePolicy` defaults to `Fail` but the schema accepts `Ignore`. With `Ignore`, every authorization-in-admission check fails open while the webhook is unavailable. | Reject `Ignore` for the validating webhook, or require an explicit opt-out documented as unsupported for production (T-0472). Proposed in the ADR review section. |

## 10. Qualification

The existing test tiers apply ([testing strategy](../../../testing.md)). Evidence
must come from a real Kubernetes API server with RBAC enabled, not only fakes.

| Layer | What it proves |
| --- | --- |
| Go unit | Every CRD is classified exactly once; classes match the CRD scope; roles generated from the catalog contain no wildcard, `deletecollection`, `secrets` or `*/status` outside the named exception; the catalog and chart output are in sync |
| Helm | Rendered roles match the catalog; no bindings and no aggregation labels by default; a switch disables the roles; no role references a kind that is not installed |
| Envtest | With RBAC enabled: for each role, bind a test principal and check allowed and denied requests for every kind and verb in the matrix, including cluster-scoped classes, `resourceNames` scoping of `use`, and the status exception |
| Kind | Real users and service accounts: end-to-end negative tests (no grant, wrong namespace, wrong kind), `access-admin` cannot bind a role that adds permissions it does not hold, such as `cluster-admin` or a Secret-reading role, while binding a role whose permissions it already holds is permitted (the subset case), approval and autonomy-decision identity binding, revocation observed on the next request |
| Traceability | Each requirement links to a story and test case; `tools/trace` renders the chain |

For every test record: cluster version and authorizer configuration, principal
type, the exact resource, subresource, verb, namespace and name, the
`SubjectAccessReview` result and the actual HTTP result. A `404` or successful
discovery is not evidence that an authorization boundary works. Denied operations
must also show that no work started.

## 11. Staging and what is out of scope

| Stack | Content | Status |
| --- | --- | --- |
| 1 | This design, backlog, catalog and classification test, chart roles, envtest and kind verification, reader and operator documentation, the access-check command, hardening reviews | In scope (EPIC-0025) |
| 2 | Aggregated action API for invoke, approvals and cancellation | Separate approval and security review required |
| 3 | Hosted Studio per-user identity | After stack 1; design in this plan, implementation separately approved |
| 4 | Standalone-mode alignment | Separate follow-on |
| 5 | Auditor access to non-CRD evidence | After OD-5 |

Out of scope everywhere in this plan: identity-provider configuration, directory
synchronization, provider-specific authorization models, and any change to Agent
capability authorization.
