# Activation Transaction Contract

- Status: SG-04 implemented for local review; production activation remains BLOCKED.
- Updated: 2026-10-23.
- Code: [transaction](../../../internal/k8s/activation/transaction.go), [recovery](../../../internal/k8s/activation/recovery.go), [command](../../../tools/check-admission-activation/transaction.go).

Local [SG-07](../../development-plans/features/knowledge-provider-execution/sg07-local-acceptance.md), [SG-08](../../development-plans/features/knowledge-provider-execution/sg08-local-acceptance.md) and
[SG-09](../../development-plans/features/knowledge-provider-execution/sg09-operator-contract-review.md) evidence is recorded for its stated
scope. It does not supply fresh, exact-target production acceptance or approval.
Synthetic reports and Deployment readiness never satisfy those gates. SG-05 owns
installation/upgrade wrappers. This contract does not authorize activating an
installation; remaining candidate and deployment gates still apply.

## Prerequisites And Authority

An administrator reviews the exact rendered bootstrap manifest, chart archive,
digest-pinned images, release and enrolled namespaces. Follow the
[trusted-render contract](bootstrap-verification-contract.md) and
[drain contract](drain-verification-contract.md). The command hashes chart bytes;
it does not render the archive or automatically establish provenance.

Maintain an external writer freeze throughout preparation, approval and activation,
including GitOps, autoscalers and custom producers. Existing installations must
drain work, stop the controller and revoke old grants before admission changes.
Sequential API reads and optimistic updates are not a transactional cluster lock.

Use a separate administrator-owned authority namespace outside installation and
workload namespaces. Approval, permanent consumption claims, journals and archives
reside there. Controller write authorization checks include named objects and
inherited groups; allowed or indeterminate results fail closed. Administrators must
restrict other writers and namespace administration. Approval authenticity comes
from this authority, not signatures. Enrolled credentials are controller-trusted;
admission cannot restrict Secret GET.

Reviewed-target Agent acceptance reads the existing named runtime Secret without
rotating, deleting or restoring it. It verifies that the source UID, resource
version and data remain unchanged, including for immutable Secrets. Source
rotation and deletion probes belong to the separate disposable mutable-credential
fixture; do not count those unrun probes as target-specific acceptance.

## Command Workflow

Build once and retain the exact executable, manifest, chart and explicit kubeconfig
for activation and recovery. Rebuilding changes the executable digest.

```powershell
go build -o ./bin/check-admission-activation.exe ./tools/check-admission-activation
$activationArgs = @(
    '--manifest', './reviewed/bootstrap.yaml',
    '--chart', './reviewed/kognition.tgz',
    '--kubeconfig', './reviewed/target.kubeconfig',
    '--release', 'reviewed-release',
    '--authority-namespace', 'activation-authority',
    '--replicas', '1',
    '--writers-frozen'
)
./bin/check-admission-activation.exe prepare @activationArgs
```

Substitute administrator-reviewed artifacts and an explicitly authorized target.
`prepare` verifies inactive/drained structural state and returns the exact target
in JSON `result`; it does not issue approval. After genuine acceptance and approval,
use mode `activate` with the same arguments plus `--approval <record-name>` and
`--dry-run` for nonpersisting validation. Dry-run neither consumes approval nor runs
behavioral probes or proves rollout. Removing `--dry-run` authorizes mutation and
is prohibited while acceptance gates remain unfinished.

Reports contain `mode`, `passed`, `result` and optional `error`. Exit codes are zero
for success, one for execution failure and two for invalid arguments. All modes
require explicit kubeconfig, positive replicas and writer-freeze confirmation.
Requests and transactions are bounded. Legacy verification/drain modes remain
read-only and separate.

The transaction CLI sets a 15-second request timeout and a client-side budget of
50 requests/second with burst 100. This supports repeated exact inventory checks
across enrolled namespaces; it does not skip checks, extend approval validity or
remove the five-minute activation deadline. A client rate-limit deadline error is
a failed attempt, not successful rollout. Retain its diagnostics and compensation
result before deciding whether a new attempt is appropriate.

## Approval And Reports

The administrator creates an immutable, ownerless ConfigMap with five `data`
strings: `approval.json`, `SG-03.json`, `SG-07.json`, `SG-08.json`, `SG-09.json`.
Unknown JSON fields and trailing values are rejected.

| Record | Required fields |
| --- | --- |
| Approval | `version`, `approver`, `issuedAt`, `expiresAt`, `target`, `gates` |
| Gate entry keyed by SG identifier | `reportSHA256`, `testVersion`, `acceptedAt` |
| Acceptance report | `version`, `gate`, `targetSHA256`, `testVersion`, `acceptedAt`, `passed`, `findings`, `evidence` |

Both versions are `kognition.dev/activation/v1`; timestamps use RFC3339. Approval
must be issued no later than now, remain unexpired and last at most 30 minutes.
Each gate must have been accepted within the 30 minutes preceding issuance.
Reports require `passed: true`, no findings, nonempty source-evidence SHA256 hashes,
matching test version/time and exact-byte report SHA256. Source hashes are
administrator attestations: the tool does not load/authenticate source artifacts.

The [target schema](../../../internal/k8s/activation/approval.go) binds kube-system
UID, endpoint/server version, release, installation/workload/authority namespaces,
controller/guard identities, controller Deployment, replicas, manifest/chart hashes,
executable digest, image digests and live object UIDs/generations/content hashes.
Values affect the rendered-byte hash. API-managed fields and controller Deployment
revision metadata are excluded; unrelated annotations and ownership still matter.

For the resource-admission NetworkPolicy, preparation recognizes the two ownership
annotations Helm adds after rendering only when their release and installation
namespace match the reviewed candidate and its managed-by label is `Helm`.
All other labels, annotations, owner references and policy content remain exact.
Live ownership annotations remain included in the object hash and approval target.
Direct and GitOps application without those Helm annotations remains supported.

When enabled, the independent resource-admission Deployment, Service, service
account, RBAC, network policy and mutating/validating webhook configurations are
also included in the target inventory. Its container image must be digest-pinned;
its readiness is checked during activation rollout. The sandbox guard and resource
admission server remain distinct identities with distinct privileges.

The Cilium profile may also include one explicitly enabled
`<guard-prefix>-kubernetes-api` CiliumNetworkPolicy in the installation namespace.
The validator requires its exact release ownership, six named control-plane
components, `host`/`kube-apiserver` entities and TCP 443/6443 ports. It rejects
additional peers, ingress, policy specs or broader selection. The object joins
the exact UID/generation/content inventory; it does not replace or relax any
workload namespace's stdio deny policy. Older safety executables reject it.
See the [pipeline-free GitOps procedure](../../../deploy/GITOPS.md).

Report `targetSHA256` is lowercase SHA256 of Go
`json.Marshal(activation.ActivationTarget)`, using exported
[TargetSHA256](../../../internal/k8s/activation/evidence.go). Hashing pretty-printed
command output or differently serialized JSON does not implement this contract.
Use the helper with the exact prepared target. Synthetic approvals are not acceptance.

## Transaction And Audit

Activation validates reports, approval and current snapshot, then atomically creates
an immutable `approval-used-<SHA256(recordUID)>` claim, followed by a separate
installation journal Create. These writes are not one atomic transaction.
Uncertain Create/journal failure can consume approval without starting work. Never
automatically retry it. Deleting a journal does not remove its permanent claim.

The journal key binds cluster UID, installation namespace and controller account.
`audit.json` contains full approval, approval UID, stage, start/update times, failure,
original failed stage, created binding UIDs, `resourceJournalVersion: 1` and
`resources`. Each resource records `apiVersion`, `kind`, namespace, exact name,
`probe`, optional UID and optional `rejected`. The tool persists intent before
Create, then persists the returned UID or definite API rejection. It never retries
the same resource Create within a transaction. An empty UID without rejection
means the request outcome is unresolved. Optimistic updates reject stale resource
versions. The journal stores current state and failure provenance, not an
append-only history of every transition.

The transaction executes six exact-identity behavioral probes per enrolled
workload namespace, repeats
snapshot/approval checks, rereads the record and creates only reviewed grants.
Inventory and exact grant checks precede scaling. Rollout requires current policy
status, exact controller/guard/resource-admission readiness, unchanged inventory
and fresh approval.
Grants are checked again before recording `active`.

| Stage | Interpretation |
| --- | --- |
| `claimed`, `behavior-verification`, `granting:<namespace>/<name>`, `granted:<binding-key>`, `starting`, `rollout` | In progress; interruption needs explicit recovery |
| `active` | Grants and rollout verified; identical repeat performs read-only checks |
| `failed-inactive` | Compensation resolved all resource intents, removed owned probes/grants and confirmed zero replicas and drain |
| `recovery-required` | Cleanup or persistence uncertain; inactivity is not confirmed |
| `recovering` | Explicit revocation/drain in progress |
| `recovered-inactive` | Confirmed inactive; another attempt requires fresh approval |

An active repeat requires original record UID/full approval, artifacts/options,
unchanged inventory, exact grants and healthy rollout. It does not replay mutation.
Approval expiry does not revoke already active state; drift still rejects a repeat.
This command is not a continuous authorization monitor.

## Failure And Recovery

Failure after claim triggers bounded compensation with an independent context:
reconcile durable grants and probes, delete with UID/resourceVersion preconditions,
scale the original controller UID to zero and verify drain. Probe cleanup removes
grants, then fixtures, then dry-run protection policies/bindings. Unresolved probe
intents or failed cleanup retain protections. Foreign/replaced objects, absent
transaction markers and foreign owner references are preserved and reported.
Legacy grants are never restored.

Every created resource carries `kognition.dev/activation-transaction` equal to
the installation journal UID. Recovery can resolve a lost Create response only by
observing that exact owned object and persisting its UID before deletion. NotFound
resolves cleanup only when the journal already contains a UID. A pending Create
with no observed object stays `recovery-required` indefinitely: neither a timeout,
successful drain nor stopping the executor proves that a sent API request has
finished. A crash after intent persistence but before sending Create is also
indistinguishable from an in-flight request. There is no automatic retry or override
for this uncertainty. A fresh recovery invocation reloads persisted journal state
after an uncertain journal update.

Process death cannot run compensation. Terminate every earlier activation/recovery
executor before recovery and maintain the writer freeze. Journal updates do not
fence paused executors. Run mode `recover` with the original arguments, approval
and `--executors-stopped`. Recovery forbids dry-run and ignores expiry/report
freshness so expired approval cannot prevent revocation. Original executable,
artifacts, cluster/server/options, record UID and full approval remain required.
Journals without resource-journal version 1 cannot certify completion, inactive
recovery or retirement for another attempt. Keep such journals and their artifacts;
automatic recovery does not reconstruct an incomplete historical probe inventory.

After recovery, a fresh attempt archives the audit in immutable
`activation-history-<journalUID>` before UID/resourceVersion-preconditioned journal
deletion. Conflicting/mutable archives block the attempt. Consumption claims remain.
Retain authority records: deleting them bypasses the supported protocol.

A compensated `failed-inactive` journal is not authorization to submit a different
transaction. Run explicit recovery with the original executable, artifacts and
approval before preparing a new target and obtaining fresh approval. A changed
executable, including a client-throughput fix, changes the target digest. Keep the
old executable for recovery and retain both the failed and successful receipts.

The chart omits controller bindings. Reviewed inactive values must leave the
legacy `k8sController.replicas` override at zero; a nonzero render is rejected by
the installer and verifier even when `activated=false` is supplied. Saved
`activated=true` alone does not authorize activation. Administrator RBAC/replica
edits, custom renders and legacy chart rollback are unsupported activation paths. The
[installation and recovery contract](installation-recovery-contract.md) defines
SG-05's guarded upgrade ordering and prohibits automatic rollback.

## Verification Boundary

[Cleanup tests](../../../internal/k8s/activation/transaction_test.go) and the
[API lifecycle fixture](../../../internal/traceability/activation_transaction_integration_test.go)
cover mismatch/expiry, missing gates, drift, replay, interrupted durable state,
lost responses, grant/rollout/journal failures, recovery, repeats and a compiled CLI
prepare/dry-run. [Resource journal tests](../../../internal/k8s/activation/resource_journal_test.go)
cover delayed persistence after NotFound, failed/lost journal updates, Create retry
rejection, collisions, foreign/replaced resources and retained protections. API
tests cover a delayed permanent grant and fresh-client recovery after interruption
at policy, policy-binding, fixture, Role and RoleBinding creation. Local cleanup and
returned-UID persistence are deliberately unavailable during injected interruption.
API defaulting, RBAC and persistence are real; acceptance/readiness are synthetic.
Interrupted state is injected, not a process-kill acceptance test.

The [backlog](../../development-plans/features/knowledge-provider-execution/security-gate-backlog.md) records validation and remaining gates.
The original SG-04 completion included no archive refresh, staging, commit, push,
cloud operation or real activation. A separate 2026-10-02 local reinstall used
fresh exact-target evidence and explicit owner approval, recovered a compensated
timeout through this protocol, and completed activation. That local result does
not certify production activation, cloud deployment, or earlier candidate hashes.
