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
| `workflows` + verb `use` is authorized with the submitter's identity before a run is accepted. | [workflow_run_submission.go](../../../../internal/k8s/webhook/workflow_run_submission.go) |
| `agents` + verb `invoke` is authorized for the workflow executor's service account. It is not checked for people. | [workflow_executor_target.go](../../../../internal/k8s/controller/workflow_executor_target.go) |
| `runtimeapprovals/decision` with verbs `approve` or `reject`, and `runtimeapprovals/reconcile` with verb `reconcile`, are authorized by `TokenReview` + `SubjectAccessReview`. | [runtime_approval_decision.go](../../../../internal/k8s/webhook/runtime_approval_decision.go), [runtime_approval_reconciliation.go](../../../../internal/k8s/webhook/runtime_approval_reconciliation.go) |
| `ApprovalDecision.spec.approver` must equal the requesting user; `spec` is immutable. | [approval_decision_validator.go](../../../../internal/admission/approval_decision_validator.go) |
| An `AutonomyReview` decision requires `status.decidedBy` to equal the requesting user. | [autonomy_review_validator.go](../../../../internal/admission/autonomy_review_validator.go) |
| CLI and Studio chat in Kubernetes mode lists Pods, opens `pods/portforward`, and reads the runtime token from a Secret. | [k8sclient.go](../../../../cmd/kognitionctl/cmd/k8sclient.go), [portforward.go](../../../../cmd/kognitionctl/cmd/portforward.go) |
| The Kubernetes-mode CLI resolves the caller with `SelfSubjectReview`. | [k8sclient.go](../../../../cmd/kognitionctl/cmd/k8sclient.go) |

The `portforward` row matters most: interactive chat is not a least-privilege
action today (section 9, G1).

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
6. **Caller authority, not controller authority.** Anything a controller does for a
   user is authorized as that user first.
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
| `grant` | Delegation and credential binding; high risk | AgentRole, AgentRoleBinding, CrossNamespaceGrant, ExternalAgentGrant, MemoryGrant, ModelGrant, SecretRef, ToolGrant |
| `governance` | Policy, quota and trust configuration | AgentCommunicationPolicy, AgentPolicy, CognitiveAdmissionPolicy, CognitiveLimitRange, CognitiveQuota, TrustProfile |
| `decision` | Human decisions and their records | ApprovalDecision, AutonomyReview, RuntimeApproval |
| `platform` | Platform-owned namespaced kinds | ExternalAgent, Workspace |
| `class` | Cluster-scoped classes | AutonomyClass, KnowledgeBaseClass, MemoryClass, ModelClass, ToolClass |
| `system` | Internal controller state; no human write access | AgentIdentity, KnowledgeBindingRecord (cluster-scoped), RuntimeApprovalQuotaTicket, RuntimeApprovalTombstone, TrustProfileIndex |

Totals: config 15, knowledge 3, submit 3, runtime 4, data 1, grant 8, governance 6,
decision 3, platform 2, class 5, system 5 = 55.

Notes that affect grants:

- `Tool` and `Model` are `config` but high impact: a Tool can launch a process or
  reach a network target. Runtime use is still gated by `ToolGrant`/`ModelGrant`
  and signature policy; granting `kognition-builder` does not grant use.
- `SecretRef` is `grant`, not `config`: it lets platform components read a named
  backing Secret.
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
| `kognition-viewer` | RoleBinding | R on `config`, `knowledge`, `governance`, `grant`, `platform` |
| `kognition-run-viewer` | RoleBinding | R on `submit`, `runtime`, `decision` |
| `kognition-data-reader` | RoleBinding | R on `data` |
| `kognition-builder` | RoleBinding | W on `config`; R, `create` and `delete` on `evaluationruns` |
| `kognition-curator` | RoleBinding | W on `knowledge` |
| `kognition-consumer` | RoleBinding | `create` on `workflowruns` and `agentjobs`; verb `use` on `workflows`; verb `invoke` on `agents` |
| `kognition-operator` | RoleBinding | everything in `kognition-run-viewer`; verb `reconcile` on `runtimeapprovals/reconcile` |
| `kognition-approver` | RoleBinding | R on `runtimeapprovals`; R and `create` on `approvaldecisions`; verbs `approve`, `reject` on `runtimeapprovals/decision` |
| `kognition-autonomy-reviewer` | RoleBinding | R on `autonomyreviews`, `trustprofiles`; `update`, `patch` on `autonomyreviews/status` |
| `kognition-policy-admin` | RoleBinding | W on `governance` |
| `kognition-grant-admin` | RoleBinding | W on `grant` |
| `kognition-access-admin` | RoleBinding | R on `roles`, `rolebindings`; W on `rolebindings`; verb `bind` on `clusterroles` limited by `resourceNames` to the namespace-bound `kognition-*` roles (not `kognition-class-viewer` or `kognition-platform-admin`) |
| `kognition-auditor` | RoleBinding | R on every namespaced class except `data` (`config`, `knowledge`, `submit`, `runtime`, `grant`, `governance`, `decision`, `platform`, `system`) |
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
  top-level `create`, so creating a `WorkflowRun` is permitted namespace-wide and
  the `use` check is what restricts which workflow may run.

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
| Use a workflow | resource `workflows`, verb `use`, name = workflow | workflow submission admission, with the submitter's identity |
| Invoke an agent | resource `agents`, verb `invoke`, name = agent | workflow executor target authorization (today, for the executor only) |
| Approve or reject a runtime approval | resource `runtimeapprovals/decision`, verbs `approve`, `reject`, name = approval | decision endpoint (`TokenReview` + `SubjectAccessReview`) |
| Reconcile a runtime approval | resource `runtimeapprovals/reconcile`, verb `reconcile` | reconciliation endpoint |
| Vote on a workflow approval gate | resource `approvaldecisions`, verb `create`; `spec.approver` = requesting user | admission |
| Decide an autonomy review | resource `autonomyreviews/status`, verbs `update`, `patch`; `decidedBy` = requesting user | admission |

These verbs are not endpoints that Kubernetes creates for a CRD. A CRD supports
only the `status` and `scale` subresources. The checks work because Kognition asks
the cluster a `SubjectAccessReview` about a name it defines. That is a valid use of
RBAC, but it means a bare `kubectl` user cannot "do" these verbs: a Kognition
component must expose the operation and perform the check.

The `invoke` verb in `kognition-consumer` is granted for forward compatibility with
the action API and for the executor-targeted check; it does not by itself let a
person chat with an Agent (G1).

The decision and reconcile endpoints treat a `SubjectAccessReview` evaluation error as
"authorization unavailable" and never as an allow, even when the review also says
`allowed`. This matches the workflow-use and executor checks. Kubernetes can report an
evaluation error alongside an allow (for example when an unrelated binding references a
missing role), so a dangling binding for an approver makes the decision endpoints
return 503 until it is repaired; failing closed is the deliberate trade-off.

The decision endpoint accepts bearer tokens through `TokenReview`. Client
certificate or exec-plugin users who cannot present a bearer token are not covered
by that path; closing this is the purpose of the later aggregated action API.

## 8. Separation of duties and threat considerations

| Risk | Treatment |
| --- | --- |
| Builder grants itself access | Grants and `SecretRef` are only in `kognition-grant-admin`; the builder role excludes them. Constraining what a grant-admin may delegate is open (ADR-0031 OD-2). |
| Self-approval: a person holds consumer and approver and approves their own run | RBAC cannot express this. Verify whether admission already compares submitter and approver; if not, add the check or require roles to be held by different people. Task in EPIC-0025. |
| Confused deputy: a controller creates resources from a user-authored object (`Solution`, `MemoryClaim`, `KnowledgeBaseIndexSchedule`, `AgentAutoscaler`, workflow collaboration) | Review each controller-mediated path: the resources it creates must be limited to what the author could create. Task in EPIC-0025. |
| Namespace-wide approval eligibility | `create` on `approvaldecisions` cannot name a gate. Eligibility is per namespace. Narrower eligibility is a separate feature. |
| Wildcard growth | Roles list explicit resources. A new CRD is added to a role only through the catalog. |
| Bulk delete | `deletecollection` is excluded from every shipped role. |
| Secret read as privilege escalation | No shipped role grants `secrets`. See G1. |
| Over-broad `bind` | `kognition-access-admin` limits `bind` with `resourceNames` to the namespace-bound roles. It cannot bind the cluster-bound roles: a RoleBinding to one still grants its namespaced rules (for example write access to `ExternalAgent` and `Workspace`), which would let a namespace administrator delegate platform-owned rights. Found by the real-API-server tests; Kubernetes' own escalation checks still apply to everything else. |
| Stale authority after revocation | Kubernetes evaluates RBAC per request. Group membership freshness depends on the operator's authentication integration and token lifetime and is documented, not guaranteed instant. |

## 9. Known gaps

| ID | Gap | Treatment |
| --- | --- | --- |
| G1 | Interactive chat needs `pods` list, `pods/portforward` and `secrets` get. A least-privilege end-user role cannot cover it. | Documented as privileged and not shipped as a role. Fixed by the action API (FR-0416), a later stack. |
| G2 | Approval decisions accept bearer tokens only. | Documented. Action API stack. |
| G3 | Audit and trust evidence in ConfigMaps or an HTTP query API is not governed by CRD RBAC. | `kognition-auditor` covers CRD evidence only. Design later (OD-5). |
| G4 | No deny and no field-level RBAC. | Mitigated by classes, subresource actions and admission. |
| G5 | Hosted Studio has no per-user identity design. | Design in the plan for FR-0417; implementation later. |
| G6 | No command shows a person what they may do. | `kognitionctl` access check using `SelfSubjectRulesReview` (FR-0415). Advisory only. |

## 10. Qualification

The existing test tiers apply ([testing strategy](../../../testing.md)). Evidence
must come from a real Kubernetes API server with RBAC enabled, not only fakes.

| Layer | What it proves |
| --- | --- |
| Go unit | Every CRD is classified exactly once; classes match the CRD scope; roles generated from the catalog contain no wildcard, `deletecollection`, `secrets` or `*/status` outside the named exception; the catalog and chart output are in sync |
| Helm | Rendered roles match the catalog; no bindings and no aggregation labels by default; a switch disables the roles; no role references a kind that is not installed |
| Envtest | With RBAC enabled: for each role, bind a test principal and check allowed and denied requests for every kind and verb in the matrix, including cluster-scoped classes, `resourceNames` scoping of `use`, and the status exception. The aggregated built-in roles (`admin`, `edit`, `view`) are empty in envtest because no controller-manager populates them, so escalation against them is verified in the kind tier |
| Kind | Real users and service accounts: end-to-end negative tests (no grant, wrong namespace, wrong kind), `access-admin` cannot bind a non-Kognition role, approval and autonomy-decision identity binding, revocation observed on the next request |
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
