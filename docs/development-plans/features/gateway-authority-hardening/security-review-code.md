# Gateway Authority Hardening

- Mode: code review and isolated live Kubernetes authority acceptance.
- Date: 2026-10-20.
- Status: SEC-F1-002/SEC-F1-003 retain qualified local closure. The [gateway-state escalation and exact local scanner dispositions](finding-closure-2026-10-20.md) are also closed for their recorded candidate. Production activation and release acceptance remain separate.
- Baseline: `b5ab6c4` on `docs/enterprise-reference-architecture`.

## Findings And Repairs

| Boundary | Finding | Local Repair And Evidence |
| --- | --- | --- |
| Human decisions | A configured gateway lifecycle writer could forge Pending to Approved with structurally valid actor/audit fields through direct status mutation, or fabricate missing evidence on legacy human terminal records. Regression cases reproduced both defects before repair. | Explicit decision authority denies lifecycle-only Approved/Rejected transitions, human actor/audit creation or drift, Pending evidence poisoning and human reconciliation fields/dispositions. Public validator and Kubernetes handler tests preserve legitimate acknowledgement, timestamp canonicalization, claim, completion and automatic NotRequired initialization. |
| Authority configuration | Lifecycle-writer membership did not distinguish the authenticated decision server from gateways. | `KOGNITION_RUNTIME_APPROVAL_DECISION_SERVICE_ACCOUNT` selects the dedicated resource-webhook identity; missing configuration denies human decisions. The chart configures the same identity in both admission assemblies. Existing decision/reconcile HTTP TokenReview, exact SubjectAccessReview and forbidden-runtime-subject checks remain unchanged. |
| Execution ownership | A Tool Gateway could complete a Memory claim, and another Pod sharing the same ServiceAccount could finalize a retained claim. Regression cases reproduced both gaps. | Explicit plane accounts constrain lifecycle/execution writes. Authenticated admission Pod name/UID extras bind claims to the live gateway Pod, ServiceAccount and running ContainerID through the uncached reader. Claimant identity, process nonce, fence and termination proof remain immutable; missing identity/read capability fails closed. |
| Lease authority | Gateway ClusterRoles permitted arbitrary Lease read/create/update. The independent Lease policy matched only the controller. | Tenant reads/updates name only `kognition-runtime-approval-records`; control coordination is namespaced. CREATE remains separate because RBAC cannot constrain it with `resourceNames`. A mandatory fail-closed gateway policy limits namespaces and coordination names, validates old/new ownership and rejects adoption, Helm ownership and deletion. |
| Activation | An added enforcement pair must not become optional or be replaced to satisfy a count. | Bootstrap requires nine policy/binding pairs and the exact gateway policy identity. Omission and renamed-pair tests fail; existing activation negative controls remain intact. Only the core chart archive is refreshed; governance bytes are preserved. |
| Cross-workspace Communication | Deployed exact-capability exchange returned unavailable because generic binding validation required the AgentService target to share the source workspace. The action-validation projection also omitted the plane; an older HTTP test substituted a same-workspace builder. | Preserve the plane and allow a remote namespace only for a Communication AgentService target. Source identity/authorization constraints, immutable target facts and MAC authentication remain enforced. Red/green binding coverage, public admission checks and the real HTTP request builder cover same/cross-workspace requests and denied scope drift. |

## Trust Boundaries

- The decision authority writes human state only after the existing authenticated HTTP endpoint performs its exact decision/reconciliation authorization checks. This trust is distinct from gateway lifecycle authority.
- Gateways retain creation, expiry, evidence acknowledgement and execution-state responsibilities. This repair does not redefine the execution claimant or reassign an uncertain operation.
- Binding-key coordination is in the control namespace. Approval retention/quota-ticket coordination remains in each declared workspace; moving every Lease into the control namespace would break the existing path.
- Existing reservations, replay fences, keys, receipts, finalizers and retained evidence are not released, rewritten or shortened.

## Local Verification

- Full affected admission, webhook, controller command, activation/security/traceability and Kubernetes coordination-store race suites pass.
- Thirty rendered gateway Lease allow/deny cases cover all three writers, legitimate control/workspace coordination, foreign names/namespaces, unowned takeover, Helm ownership and deletion.
- A fresh envtest Kubernetes 1.30.3 API server passes the complete native admission matrix, including the gateway Lease subtest with deliberately permissive fixture RBAC. This proves the native admission denial, independently of role denial. The owned environment shuts down normally; no kubeconfig or existing cluster is used.
- Default and integration-tagged correctness lint report zero issues. Handler conversion/user propagation and actual `RuntimeApproval/status` webhook registration are covered.
- Final affected race verification includes complete admission, webhook, controller command, activation, traceability/securitygate and Kubernetes controller/cost packages. Header checking reports 1446 tracked sources with zero missing notices; FR/NFR mapping remains 110/110 and the NFR plan validates 13 rows/18 mappings.
- At the initial local checkpoint, controller-manager policy-status typechecking was skipped without a separately approved isolated target. The live acceptance below supersedes that gap. No full-module coverage refresh, scanner/risk disposition, paid-provider or production rollout receipt is claimed here.

## Execution-Owner Follow-Up

The chart configures `KOGNITION_RUNTIME_APPROVAL_TOOL_SERVICE_ACCOUNT`,
`KOGNITION_RUNTIME_APPROVAL_MEMORY_SERVICE_ACCOUNT` and
`KOGNITION_RUNTIME_APPROVAL_COMMUNICATION_SERVICE_ACCOUNT` explicitly. Disabled
gateways configure no plane authority; missing namespace or duplicate
cross-plane accounts fail closed. Generic lifecycle-writer membership does not
grant another plane's creation, acknowledgement or execution authority.

The Kubernetes handler preserves authenticated `authentication.kubernetes.io/pod-name`
and `authentication.kubernetes.io/pod-uid` values. Gateway claim/finalization writes
require one nonempty value for each and a live claimant-reader capability. The
dedicated webhook has only Pod GET in the control namespace; it verifies the Pod
UID, non-deleting state, ServiceAccount, plane-specific running container and
retained ContainerID. A replaced Pod or restarted container cannot finalize the
old claim.

Admission enforces preparation/evidence/expiry prerequisites, initial fencing,
gateway terminal ordering and immutable owner/recovery facts. It rejects skipped
claims, premature claimant fields, Uncertain finalization, terminal rewrites and
forged gateway recovery. The dedicated human authority may reconcile only an
evidence-bound retained Uncertain claim, not perform ordinary finalization.
Automatic NotRequired initialization, gateway acknowledgement, prepared claim,
successful finalization and expired-at-claim denial remain covered positive paths.

These checks do not independently authenticate a process nonce inside one live
container. Such processes share the same Kubernetes workload credential, and an
admission Pod-status read is not atomic with container lifecycle changes. The
nonce/fence remains retained evidence, not proof that an uncertain effect did not
occur. Legacy credentials lacking authenticated Pod extras fail closed for owner
writes; do not bypass the check to enable rollout.

Local public-validator, real webhook conversion, live Pod-reader and production
configuration regressions pass. The later live profile below verifies actual
controller-manager status, Pod credentials and the trusted recovery surface.
Complete deployed gateway workflows still require separate qualified acceptance.

## Container Boundary And Controller Recovery

On 2026-10-15 the workspace owner selected **keep the container as the trust
boundary**. Processes sharing one live container credential remain one trusted
principal for this profile. The alternative, separate per-process credentials or
a trusted transition broker, was considered and deferred; introducing it would
require a separately reviewed authority protocol and platform enrollment.
This clarifies ADR-0014's existing Pod/container recovery identity and does not
grant production rollout approval or independent process-nonce authentication.

| Option | Benefit | Cost Or Residual Risk | Disposition |
| --- | --- | --- | --- |
| Container/workload credential boundary | Preserves the existing Pod-bound credentials, runtime protocol and exact-container recovery model. | Same-container credential compromise cannot be distinguished as a separate process; live reads are not one transaction with container lifecycle. | Explicitly selected by the workspace owner for this profile. |
| Separate per-process authority | Could distinguish independently authenticated processes if issuance, proof and transition ownership are designed together. | New credentials or a broker, lifecycle/enrollment and deployment acceptance; a caller-provided nonce alone is insufficient. | Deferred; no implementation or deployment implied. |

The controller's retained-claim bypass was reproduced: direct admission allowed
it to fabricate ordinary completion of a gateway claim. It now cannot finalize
or reassign such a claim. Its recovery write may change only Claimed to Uncertain
and add the independently verified canonical termination reference. Existing
claimant, fence, preparation, disposition and prior proof remain unchanged.

Both admission and the recovery reconciler use the uncached store termination
reader, restricted to the control namespace. The reader checks the exact Pod UID
and ContainerID across regular, init and ephemeral container statuses; incomplete
identity, read errors or a still-present incarnation deny proof. Admission
compares the caller's reference with the independently computed reference. A
cached omission or a forged string is not termination authority. The webhook's
Pod permission is control-namespace GET/LIST only, never mutable or cluster-wide.

Controller pre-claim writes are restricted to canonical initialization and expired
preparation reaping; the controller cannot manufacture a gateway claim. Explicit
inline human authority is evaluated as evidence-bound human reconciliation and
does not revive ordinary finalization. The existing in-process workflow Tool
core is not silently rerouted or given a new claimant authority.

Public admission tests cover fabricated controller completion and claim creation, missing/live/
unavailable/mismatched proof and recovery-side fact rewrites, plus the valid
proof-backed uncertainty transition, expired reaping and explicit inline human
reconciliation. Reconciler tests deliberately omit live
Pods from the cached client while retaining the authoritative reader, and cover
all container-status classes. No uncertainty transition releases a reservation,
refunds an operation or authorizes execution replay.

Final complete affected race suites pass for admission, webhook, controller
command, Kubernetes controller, traceability and securitygate. Full correctness
lint and integration-tagged traceability lint report zero issues. This is a
local source/compatibility receipt, not fresh module-wide coverage or deployment
certification.

## Historical Isolated Acceptance Attempt

The workspace owner approved fresh deterministic local acceptance on 2026-10-15,
using a disposable kind target and private kubeconfig, without cloud calls,
paid inference, existing-cluster changes or retired-fixture replay.

- The render-only `TestControllerAdmissionTypecheckInventoryMatchesChart` passes
	for all nine native policies. This does not establish controller-manager
	typechecking status.
- The acceptance harness parses after adding bounded creation and retained
	creation output for future attempts. The interrupted attempt predates that
	logging change and has no creation-output receipt.
- Creation was interrupted before a kubeconfig or cluster UID was obtained.
	A bounded inspection of the exact recorded node timed out. Its existence and
	readiness remain unverified; no acceptance phase or gateway operation ran.
- Private ownership/configuration files and their interruption receipt are
	retained. No creation retry, cleanup, unrelated Compose termination or Docker
	restart was performed. SEC-F1-002/SEC-F1-003 remain open.

Resume only after the exact partial target can be inspected and its ownership
independently verified. Do not infer node absence, fabricate readiness or replay
creation from this interrupted record. Retain acceptance evidence before any
owned cleanup.

## Live Authority Acceptance, 2026-10-18

After Docker Desktop recovered, a separately owned kind target on Kubernetes
1.34.11 completed the local authority profile. The shared Desktop cluster was
only inspected and received no acceptance fixtures.

| Gate | Actual Result |
| --- | --- |
| Controller-manager policy typechecking | All nine rendered policies reached their observed generation with zero expression warnings. Exact UID cleanup passed. |
| Gateway credentials and Lease enforcement | All three real Pod-bound ServiceAccount tokens authenticated with the expected Pod extras. Rendered chart RBAC and the native policy permitted legitimate control/workspace locks and denied human verbs, foreign scope, unowned/Helm takeover and deletion. |
| RuntimeApproval lifecycle and ownership | All three planes passed create, initialization, human decision, acknowledgement, preparation, exact-container claim and completion through the Kubernetes API and production validating handler. Human forgery, cross-plane and same-account foreign-Pod completion, changed nonce, wrong container, controller claim/finalization and fabricated recovery were denied. |
| Recovery and reconciliation | Each actual claimant Pod was deleted with its exact UID. The authoritative reader rejected proof while live, then produced the canonical absence proof. Mismatched proof failed, matching controller uncertainty passed, and evidence-bound human NotExecuted reconciliation passed. |
| Local regression checks | Race-enabled CRD parsing, nine-policy inventory, rendered Lease matrix and independent status-webhook registration passed. |

Successful run: `6f579c15-9fbc-404a-b91b-bfc87fab1542` with cluster UID
`4806f1da-b91a-47eb-85bb-fab2fa08ba4f`. The evidence index SHA256 is
`ce4df59d4d308147f1583ae537ae1a7a5cf3baaaa92472d080448ca8a3c84b5b`.
The RuntimeApproval log SHA256 is
`cd730058e77d926dcb6dbf823be30c2755258380996718d2d284975fc4e5d4a8`.
Private snapshots retain object UIDs, claimant/fence/proof facts, chart/CRD and
binary hashes, full logs and process evidence. The passed target was removed
after evidence verification at 2026-10-18T15:58:47Z.

Earlier failures are retained rather than relabeled as acceptance. Two failed
preclaim targets were snapshotted and removed without changing their prepared
approval status or finalizers. A third partial run completed Tool and Memory
recovery but encountered a revoked credential in a later cross-plane test. Its
Communication claim and exact target remain retained under run
`c28dd56f-a48a-4e31-926f-5072f5ad11ef`, with evidence index SHA256
`3510a182ad2043e721e583a39e6e388d075d30675fc8ee71238fd2b1dc6872d9`.
It was not replayed, reconciled or discarded. The corrected passing run uses
fresh identities and finishes all authorization checks before deleting Pods.

Scope: the runner uses production validation and authoritative readers behind
a loopback TLS webhook inside the isolated node. Gateway credentials and RBAC
are real, but gateway HTTP servers, external providers, physical effects,
duplicate-effect prevention, production webhook deployment, replica concurrency
and admission/container-lifecycle atomicity are not certified by this matrix.
The fixture performed no provider operation. Same-container credential sharing
remains the selected residual risk.

## Qualified Deployed Acceptance, 2026-10-19

SEC-F1-002/SEC-F1-003 are closed for the deterministic isolated gateway boundary
profile. The earlier authority/recovery matrix and the deployed workflows below
provide complementary evidence; this disposition does not approve production
activation or the broader release findings.

| Gate | Verified Result |
| --- | --- |
| Image loading | kind v0.31 rejects containerd's migrated v4 configuration. The owned direct-import path verifies archive SHA256, exact OCI digest, complete/unpacked content and independent CRI identity. It changes neither containerd configuration nor shared clusters. Seven loader regression cases pass. |
| Production assembly | Two actual replicas of each Tool, Memory Policy and Agent Communication Gateway run with chart roles, two resource-webhook replicas and two admission-guard replicas. Every webhook fails closed; the API server and decision client use verified local CA trust. All nine native policies report their observed generation with zero warnings. |
| Tool | Capability exchange, pending execution, forbidden gateway self-approval, authorized decision, concurrent invocation and durable replay pass with exactly one non-idempotent synthetic provider effect. |
| Memory store/delete | Both operations pass pending denial, authorized decision, two-replica execution, replay through both replicas and wrong-volume denial. Real chart-pinned PostgreSQL runs the complete embedded migrations and retains Succeeded mutation receipts with cleared preparation fields. Each operation produces exactly one provider effect. |
| Communication | Cross-workspace exchange denies missing grants and parks before capability issuance. After the authorized decision, exactly one replica issues the exact capability; reissuance is denied. Concurrent dispatch and both durable replays produce one provider effect; substituted messages are denied. |
| Retained owners | All four approvals are Completed with fence 1 and exact unchanged gateway Pod UID/container ownership. Provider identity and restart checks keep the effect counts authoritative. Final effects are Tool=1, Memory store=1, Memory delete=1, Communication=1. |
| Local regression | Complete affected binding, admission, gateway, webhook, Kubernetes store/recovery, activation and controller-command race suites pass. The 17 focused cross-workspace cases, chart/authority traceability, CRD/TLS checks and integration-tagged correctness lint pass. |
| Evidence lifecycle | Cleanup uses a byte-verified disposable kubeconfig copy and preserves the sealed original. Three regressions cover success, failure and interruption. All fifteen indexed artifacts and 1160 candidate sources remain exact after actual cluster deletion. |

The [acceptance harness](../../../../e2e/gateway-authority-acceptance.ps1) provides
`Production` for the complete profile and `ProductionTool` for the focused Tool
path. Application images are offline builds of the recorded source. The
PostgreSQL prerequisite is the exact chart-pinned
`pgvector/pgvector@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b`
artifact, prefetched before the offline build/import phase.

The fixture provisions trusted AgentIdentity data through a tenant-only test
Role. SubjectAccessReview proves this grants no controller Pod, Secret or Lease
creation. The controller remains inactive; full activation/enrollment and
arbitrary namespace onboarding are not certified. Synthetic downstream
providers deliberately expose repeated effects rather than deduplicating them.
This profile does not certify a real paid provider, provider-specific storage,
NFR load/latency, admission/container-lifecycle atomicity or same-container
process isolation.

Checkpoint preparation applies `gofmt` to the two Tool race regressions. This
test-formatting delta changes their source bytes relative to the sealed run;
their behavior and all production/chart/harness inputs remain unchanged.
The focused regressions and Tool package race suite are rerun before the local
checkpoint. Historical deployed source/artifact hashes below remain unchanged.

Successful sealed run: `5a1e2438-676e-4bc9-be1c-1af2c1b76fd9`; cluster UID:
`949fb595-4bdf-4686-b37c-6123193a7dc8`.

| Retained Artifact | SHA256 |
| --- | --- |
| Evidence index | `20e05388c93a8d57de51d524e39e9ad5caf3797d82ec92af76aa63f9c3c21b7d` |
| Deployed workflow log | `bd65a15d8ba30ea9e40985d945acc3349edd0324c29df11d8b30c547d55284f5` |
| Source inventory | `d6ee1dee5d166df2315fd164ac994605b2012bb2c2e2710cb3ff14dde1464b6d` |
| Rendered assembly | `59b2b451929e8a6e8197e2e3bf25342f998c01a78d4a2f0ce8f16aa97b972334` |
| Linux acceptance binary | `a7782b78a58aac5b34c097977b7663633c6448842dcaab2f5b64e3b48be5b6e0` |

Private retained records contain source/artifact/image hashes, policy status,
object identities, logs and PostgreSQL receipt/result hashes. The full passed
target was removed at 2026-10-19T18:13:43Z; complete post-cleanup byte verification
passed at 18:13:45Z. The earlier passed Tool-only and full-workflow targets were
also removed. Their original indexes remain pre-cleanup receipts: kind changed
only the operational private kubeconfig. Separate per-run difference reports
verify all other immutable artifacts without rewriting those indexes. The
disposable-copy repair supersedes that evidence-bookkeeping defect.

Four newly created failed
targets were separately reviewed and removed with owner approval. Three had no
approvals or tickets. The partial workflow target required additional explicit
approval for its three Completed synthetic approvals and matching Live
record-retention tickets; its sealed evidence remains. No invocation was
replayed, reconciled, refunded or rewritten to enable cleanup.

The older Communication claim under run
`c28dd56f-a48a-4e31-926f-5072f5ad11ef` received its separate review. All eleven
retained artifact hashes match. Its claimant is the unchanged pause-only
admission fixture with zero restarts, no provider deployment and no active
acceptance runner. The owner explicitly chose **retain unchanged without
replay**. The claim remains Claimed; no termination, reconciliation, reservation
release or cluster removal occurred. The private review receipt SHA256 is
`98692d2e1874b60fa662164ec02cccb28942ca88efeb8dc022b683bce279c56a`.

Require enabled fail-closed resource admission. Disabled/Ignore webhooks are
not certified. Preserve the selected container trust boundary and all unrelated
retained unknowns. Do not reuse retired fixtures, infer a refund from a timeout,
or resume parked T-0150 work. No cloud or paid inference operation, shared
Desktop cluster mutation or global-context change occurred during acceptance.
The owner-approved local checkpoint `f9e17d0` was created afterward; nothing was
pushed. The later [webhook audit-boundary mitigation](../knowledge-provider-execution/security-review-code.md#webhook-audit-creation-boundary-2026-10-19)
has separate deployed and scanner evidence without granting production approval.