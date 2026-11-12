# Drain Verification Contract

- Scope: SG-02 pre-bootstrap drain and effective RBAC grant checks.
- Updated: 2026-09-24.
- Release gate: BLOCKED. A drain result does not authorize bootstrap changes or activation.

## Target And Authority

The installation administrator supplies an explicit, trusted kubeconfig, the
installation namespace, the dedicated controller service account and Deployment,
and the complete set of dedicated workload namespaces. Derive this inventory from
the reviewed installation and proposed render, including `managedNamespaces` and
nonempty `agentNamespace`. Include former enrollment during migration; do not hide
legacy work by omitting its namespace. Namespace entries must be valid, unique,
nonempty and separate from the installation namespace. The `default` controller
service account is unsupported.

Confirm the kubeconfig server and context before running. Never rely on global
context or `KUBECONFIG`. The administrator needs cluster-wide LIST of Pods,
RoleBindings and ClusterRoleBindings, GET of referenced shared ClusterRoles and
the controller Deployment, and LIST of the seven native producer kinds in every
target namespace. The command does not obtain or grant these privileges.

This is an RBAC check for the named service-account identity and its standard
groups, not a universal authorization audit. Operators must rule out additional
authorizers, impersonators, stolen credentials or external identities that can
act as writers. Existing legacy controller identities need their own binding and
producer inventory, revocation and migration decisions. A check of the new
identity cannot attest that an old identity has been retired.

## Freeze And Drain Order

1. Record the cluster, release, identities, namespaces, image/render inputs, maintenance owner and start time. Stop new submissions and schedules. Freeze GitOps/Helm automation, custom controllers, operators, autoscalers, users and scripts that can recreate producers, resume the controller, change enrollment or restore grants. Suspend their desired-state sources as well as current instances; deleting a child does not freeze its owner.
2. Inventory legacy work and state using the checklist below. Let approved in-flight work finish, finalize or migrate while the old controller still has deliberately retained drain permissions and existing admission protection. Do not change admission resources yet. Do not remove finalizers merely to make a check pass.
3. Stop the controller, wait for current-generation zero replica status and removal of its Pods, and remove approved legacy producers after their owners have been quiesced. Suspended CronJobs and completed Jobs still count as producers in workload namespaces. Terminating Pods still block.
4. Revoke old controller RoleBindings and non-discovery ClusterRoleBindings, including grants through inherited groups and old identities. Do not delete shared bindings blindly: replace broad grants with reviewed narrow subjects where needed, preserving unrelated users' access. Keep all grant-restoring automation frozen.
5. Run the drain-only command. On any failure, remain inactive, resolve the specifically reported object or permission under the approved migration decision, and rerun. A fresh install may have no controller Deployment or admission resources; that is valid for drain-only mode.
6. After the other security gates permit it, apply the reviewed inactive admission bootstrap and run structural and behavioral verification. Keep the freeze in place. Rerun drain and all required checks immediately before the separately authorized activation step. The [SG-04 transaction](activation-transaction-contract.md) owns evidence-bound activation and explicit recovery; SG-05 owns install/upgrade wrappers. The drain-only command does not perform either operation.
7. Resume external writers only after authorized activation and its postconditions pass. On interruption, unexpected writes, target changes or uncertain freeze ownership, invalidate prior results and restart verification while inactive. Do not restore legacy privileges through rollback against changed admission state.

The freeze must remain continuously effective from completed drain through the
activation decision. The check uses sequential reads, not a transaction or lock.
UID/generation consistency checks detect some controller replacement races but
cannot prevent a later write. Custom resources and external desired-state systems
are not discoverable from the native producer scan. If their writers cannot be
identified and frozen, preflight is not operationally complete even with exit 0.

## Legacy Inventory

Record namespace, kind/name, UID, owner UID chain, finalizers, identity, desired and
observed state, dependencies, approved action, responsible owner and completion
evidence for each item. Labels alone are not ownership proof.

| Inventory | Required decision before bootstrap changes |
| --- | --- |
| Pods and all seven native producer kinds | Finish/cancel/migrate owned work; preserve outputs; remove approved producers and wait for Pods to disappear. The sole stopped-controller ReplicaSet exception is described below. |
| Kognition custom resources, active runs, warm pools, retries and scheduled work | Record pending/finalizing work and disable sources that would recreate native resources. Reconcile or explicitly migrate state before removing required drain privileges. |
| Controller Deployments, service accounts and old direct/inherited grants | Map old and new identities, stop all old controllers, revoke old grants and prevent reconciliation from restoring them. A shared/default legacy identity requires an explicit separation plan. |
| ConfigMaps, Secrets and Leases | Classify installation state, revisions, checkpoints, credentials and leader leases. Record retain/migrate/delete decisions and references; do not log Secret values. Resolve ownership/finalizers and credential consumers before removal or rotation. |
| Services and NetworkPolicies, including legacy random-name stdio policies | Map backing workloads and invocation ownership. Retain required protection until work ends; explicitly migrate or remove obsolete owned objects. Do not adopt unrelated objects by relabeling. |
| PVCs, external data, publication/index state and cleanup obligations | Record retention, backup, migration and finalization decisions. Native drain success does not certify data migration or external cleanup. |
| Unrelated resources inside proposed dedicated workload namespaces | Stop enrollment until their owner approves relocation or another namespace is selected. Never delete unrelated work to satisfy the dedicated-namespace contract. |

Retained state is not automatically safe or compatible with the new controller.
The administrator must review its migration separately. Drain-only verification
does not read Secret contents, assess state schemas or perform any migration.

## Read-Only Command

Run from the repository after replacing placeholders with reviewed values:

```powershell
go run ./tools/check-admission-activation --drain-only --kubeconfig <explicit-kubeconfig> --namespace <installation> --controller-service-account <controller-sa> --controller-deployment <controller-deployment> --workload-namespaces <workload-a,workload-b>
```

Do not combine `--drain-only` with `--manifest`. Without drain-only mode, use the
[bootstrap verification invocation](bootstrap-verification-contract.md#invocation-and-limits).
The command uses only GET/LIST in drain mode. It never scales, deletes, repairs,
revokes, grants or activates anything. Requests have a 15-second timeout and
verification a two-minute deadline. Application exit codes are 0 for success,
1 for verification/configuration failure and 2 for argument failure; `go run`
can wrap a nonzero application exit. Invalid target names fail closed.

Success explicitly says admission and activation are NOT verified. Preserve the
target inventory, timestamp, command, exit status and output with the freeze and
migration record. No output is an activation token or long-lived approval.

## Exact Checks

- Reject every Pod in workload namespaces, including unlabeled, completed and terminating Pods. In the installation namespace reject Pods using the controller identity through either `serviceAccountName` or deprecated `serviceAccount`.
- Scan Deployments, ReplicaSets, StatefulSets, DaemonSets, Jobs, CronJobs and ReplicationControllers. Reject all in workload namespaces and controller-identity producers in the installation namespace. Inspect CronJob nested templates and both service-account fields. Unrelated installation workloads may remain.
- Permit a missing controller only for fresh drain preflight. An existing controller must be non-deleting, explicitly scaled to zero, have zero current/updated/ready/available replicas and exact observed generation. Its Deployment LIST snapshot must match the GET UID/generation and remain stopped.
- Permit a controller-identity ReplicaSet only in the installation namespace, owned by the stopped current Deployment's exact name and nonempty UID through an `apps/v1` Deployment controller reference. Require non-deleting state, explicit zero desired/current replicas and a present exact positive observed generation. Optional ready/available/fully-labeled/terminating counts must be absent or zero. Reject malformed counts and stale, active, foreign-owned or deleting sets.
- Reject every RoleBinding granting the controller access anywhere, regardless of role name or current role contents. This intentionally conservative rule also rejects empty or dangling roles; there is no namespaced role exception that could later gain permissions.
- Resolve applicable ClusterRoleBindings through direct ServiceAccount/User subjects and `system:authenticated`, `system:serviceaccounts`, and the installation's service-account group. Ignore unrelated subjects. Only the exact discovery/basic-user/public-info-viewer/service-account-issuer-discovery role names are candidates for an exception, and GET their actual rules. Reject missing roles, malformed/unexpected references and aggregation rules.
- Permit only reviewed GET discovery/health/version/OpenAPI/issuer endpoints and CREATE self-access/self-rules/self-identity reviews. Issuer discovery includes the two exact public paths and their trailing-slash variants, not issuer wildcards. A system-looking role with Secret access or other extra rules fails.
- Propagate every GET/LIST failure, including forbidden, missing referenced role, cancellation and deadline expiry. Do not interpret inaccessible state as an empty inventory.

## Validation Evidence

- [Drain regressions](../../../internal/k8s/activation/drain_test.go) and [existing verifier tests](../../../internal/k8s/activation/verify_test.go): native producer/alias matrix, stopped ReplicaSet ownership/status, terminating Pods, controller snapshot changes, actual referenced grants, direct/inherited/unrelated subjects, invalid targets and read/list failures.
- [Command tests](../../../tools/check-admission-activation/main_test.go): incompatible/missing flags, explicit kubeconfig despite an invalid environment default, fresh success, retained Pod denial, forbidden list denial and GET-only HTTP traffic.
- [Isolated API fixture](../../../internal/traceability/activation_integration_test.go): Kubernetes 1.30.3 default shared roles remain intact; fresh preflight passes; real replica/identity defaulting, active/unobserved/foreign-owned denial and stopped ReplicaSet success pass without persisted drain mutations.

Rollout and policy type-check status in that API fixture are simulated. The
[SG-03 report](../../development-plans/features/knowledge-provider-execution/sg03-local-acceptance.md) records separate actual controller-manager
and deployed admission/stream validation; SG-07 and
SG-08 retain CNI, stdio and production producer acceptance. SG-09 must finalize
the durable trust decision. See the [security backlog](../../development-plans/features/knowledge-provider-execution/security-gate-backlog.md)
for the still-blocked release and exact completion commands.
