# SG-09 Operator Contract And Finding Reconciliation

- Status: SG-09 local contract review complete; publication and production activation remain BLOCKED.
- Date: 2026-09-25.
- Scope: approved trust decisions, historical finding reconciliation and five operator walkthroughs; no new deployment or scanner attestation.
- Decision: [ADR-0016](../../../adr/0016-installation-trust-boundary.md).
- Checklist: [security gate backlog](security-gate-backlog.md).

The approved boundary and operating contract are documented. No additional
residual risk is accepted by this report. SG-10 through SG-13 remain required for
the exact publication candidate; production acceptance is a separate decision.
The ADR complements ADR-0009, ADR-0010 and ADR-0015 rather than replacing their
execution, durability or provider contracts. Its existing approval covers
two-phase activation, dedicated credential trust domains and the pinned opt-in
Cilium isolation model, not blanket scanner exceptions.

## Historical Finding Reconciliation

The [historical review](security-review-code.md) remains unchanged. Its filesystem
scan reported four findings in the expanded core chart archive; its Kubernetes
render reported the two KSV-0056 findings and KSV-0109. Absence of KSV-0041 in that
render did not clear credential exposure. The following rows reconcile both
scopes without rewriting raw reports or declaring a current scan successful.

| Original finding / target | Current disposition and control evidence | Residual scope and owner |
| --- | --- | --- |
| KSV-0041, CRITICAL, controller Secret management in archived `clusterrole.yaml` | Historical cluster-wide grant design replaced by enrolled namespace Roles, named installation Secret GET, denied Secret list/watch and separate guard identity. [SG-06](sg06-local-acceptance.md) verifies authorization/admission; [SG-08](sg08-local-acceptance.md) verifies runtime copies, rotation and source deletion. ADR-0016 explicitly approves the dedicated namespace credential trust model. | Enrolled Secret GET remains allowed and bypasses admission; entire named installation Secret contents are readable. Controller compromise can expose this whole credential domain. Installation security owner approves enrollment; SG-12 must bind any remaining finding disposition to the exact candidate. No generic Secret finding suppression is accepted. |
| KSV-0056, HIGH, controller Service management in archive and Kubernetes render | Namespace-limited grants plus independent old/new-object admission replace reliance on application ownership checks. [SG-03](sg03-local-acceptance.md) and SG-06 cover native denials; SG-08 covers legitimate Service creation, stable identity across revisions, replacement protection and cleanup. | Required mutation rights remain in dedicated workload namespaces. Controller/chart maintainers supply exact-candidate evidence; installation security owner owns any remaining SG-12 exception. Baseline presence is not acceptance. |
| KSV-0056, HIGH, controller NetworkPolicy management in archive and render | Independent admission protects persistent baselines and restricts invocation policy mutation. [SG-07](sg07-local-acceptance.md) proves administrator-owned Cilium deny survives additive Kubernetes allows, policy replacement/deletion and established streams; SG-08 covers legitimate producer policy lifecycles. | Generic Kubernetes NetworkPolicy alone is insufficient. Local evidence is limited to the pinned Cilium profile. Authorized administrators can remove enforcement; other CNI/topology combinations require new acceptance. Installation security owner owns the boundary; SG-12 owns exact finding disposition. |
| KSV-0109, HIGH, migration ConfigMap in archive and render | Historical reviewed false positive: static migration SQL token-budget names and empty-string constraints, not credential values. [Exact verifier](../../../../tools/securitygate) retains the finding and checks payload and metadata; canonical SQL SHA256 is `01fadf1322da74e76e78787f0e2113580682b1352e6366605339564531da42e6`. | This is not a ConfigMap-wide or rule-wide exemption. SG-10 refreshes only the core archive; SG-12 must rerun the verifier against proven current inputs and reject changed content, metadata or extra findings. Controller/chart maintainers provide evidence for security-owner review. |
| Installation ConfigMap/Lease/credential tampering described in the historical namespace assessment | Independent state, lease and credential admission restricts controller mutations, including old/new identity changes and unrelated installation objects. SG-01 verifies exact trusted inventory; SG-03/SG-06 exercise native API enforcement. The authority namespace is outside controller write authority. | Labels are classification, not provenance. Effective inherited permissions, independent administrator ownership and a trusted render remain prerequisites. Administrator bypass or authority compromise is outside controller-only protection and requires incident review. |
| Unguarded startup, legacy upgrade and attachment gaps | [SG-05](sg05-local-acceptance.md) supplies guarded inactive installation/recovery; [drain](../../../features/knowledge-provider-execution/drain-verification-contract.md), [bootstrap verification](../../../features/knowledge-provider-execution/bootstrap-verification-contract.md) and [activation](../../../features/knowledge-provider-execution/activation-transaction-contract.md) contracts define separate authority and mutation. SG-06/SG-07 exercise attachment restrictions and usable stdio; SG-08 exercises restricted producers. | A writer freeze is external coordination, not fencing. DELETE success is not absence; reads are not transactional revocation. Existing streams are not retroactively closed by admission. Synthetic approval, saved activation values or readiness never authorize production startup. |

Historical statements that CNI, upgrade and producer validation were outstanding
are accurate for their snapshots; the linked later reports supply bounded new
evidence. The historical statement that no residual risk was accepted is narrowed
only by the later explicit ADR decisions. It does not become approval for all
remaining findings. Historical archive parity and scanner results do not attest
the current source or eventual packaged candidate.

## Authority And Operator Walkthroughs

The installation security owner approves the enrollment/trust boundary and any
explicit residual-risk disposition. A separately authorized administrator owns
namespace enrollment, guard/admission/baseline configuration, authority records
and exact-target activation. Controller and application identities cannot
self-enroll by labeling objects. Credential issuers own revocation; data owners
approve migration and incident integrity decisions. These are responsibility
roles, not evidence that a named person has approved a deployment.

Each walkthrough below is a contract review against existing evidence, not a newly
executed deployment or incident exercise. The [installation contract](../../../features/knowledge-provider-execution/installation-recovery-contract.md)
contains the actionable preparation, enrollment and compromise procedures.

### Fresh Install

Review explicit static kubeconfig, cluster/namespace UIDs, chart/values/render
hashes, digest-pinned images, credentials, CRDs and certificates. Provision the
separate administrator-owned authority namespace and dedicated workload trust
domains. Freeze all writers; use the reviewed installer, not direct Helm mutation.
Its output must be verified inactive with zero controller replicas and no grants.
Collect exact state and genuine acceptance before separate activation approval.

Review result: SG-05's fresh fixture proves the inactive CLI/Helm path under its
reduced local profile. It does not prove production certificate lifecycle,
optional default components or production approval. Missing prerequisites stop
the procedure; no example command authorizes activation.

### Legacy Upgrade

Inventory old identities, effective grants, producers, finalizers, invocation
policies and retained data. Stop new work, complete finalization or reviewed data
migration while required old cleanup authority remains, then observe controller
stop and revoke grants. Verify actual absence and drain before admission changes.
Recreate namespaced resources for migration; do not treat namespace labels or a
values change as migration. Bootstrap stays inactive even if saved activation was
true. Keep the freeze on failure; do not restore grants through Helm rollback.

Review result: SG-05's held-finalizer fixture verifies ordering, not production
data migration. SG-08 supplies actual producer cleanup evidence, but neither is
a universal migration acceptance. Retained work or unresolved identity blocks
progress; arbitrary finalizer deletion is not an escape path.

### Failure And Retry

Preserve original artifacts, journal, claim UID and freeze. Stop all earlier
executors and independently establish outstanding request settlement before
installer recovery. Inspect exact pending Helm storage before using paired
settlement flags. Recovery never restores old grants; distinguish verified
inactive from recovery-required. A retirement-intent receipt is not proof that
claim deletion completed. Review failed/current/last-deployed inventory before
issuing a new plan; never replay Helm apply or use automatic rollback.

Review result: SG-05 covers quota, certificate readiness and a real process kill
after Helm exit during read-only verification. API-injected delayed/lost responses
cover different cases. Activation recovery has a separate journal protocol:
unresolved Create intent can remain recovery-required indefinitely. Installer
settlement flags are not an activation override or proof of request completion.

### Namespace Enrollment

The security owner first reviews every workload and credential in the proposed
dedicated namespace; reject mixed unrelated credentials. The administrator
reviews exact UID, grants and baseline/CNI coverage and follows a frozen,
drained, inactive installation change before new acceptance/activation. Removing
enrollment first requires finalization or explicit migration, not merely editing
the values list. A same-name namespace replacement requires new identity review.

Review result: SG-06 authorizer/admission evidence and SG-01 exact inventory checks
support this boundary; SG-08 proves tested producer namespace/identity handling.
This is not a newly executed add/remove namespace migration. Authority to edit
values or labels alone cannot establish the required approval and live evidence.

### Credential Compromise

Follow the installation contract's incident procedure: freeze writers, preserve
evidence, contain compromised identities/processes with trusted administrator
authority, retain independent network/admission controls, and revoke exposed
material at each issuer. Include named installation sources, all enrolled
credentials and copies accessible to the compromised identity. Verify old
credentials fail and replacements work; terminate stale consumers and sessions.
Do not retain compromised HMAC material as a routine previous-key overlap.

Review result: SG-08's source-deletion behavior intentionally preserves existing
copies, so deletion alone is not revocation. SG-07 shows established streams can
continue after policy change, so admission/RBAC changes alone are not containment.
A compromised controller is not a trusted finalizer; emergency containment may
strand work that must be reconciled by trusted operators. Security-owner review,
data-integrity checks and fresh acceptance precede restart. Issuer revocation and
a real compromise exercise have not been validated locally by SG-09.

## Commands, Defaults And Compatibility

The installer contract retains the existing `deploy-kubernetes` wrappers with
`INSTALL_PLAN` and `INSTALL_WRITERS_FROZEN=true`, or the equivalent
`kognition-install bootstrap --plan ... --writers-frozen`. Recovery requires
`--executors-stopped`, `--requests-settled` and exact `--claim-uid`; optional Helm
settlement requires both `--helm-record-uid` and `--helm-record-sha256`.
These confirmations are operator attestations, not automatically inferred facts.

| Operation | Effect and authority boundary |
| --- | --- |
| Drain / bootstrap verification | Reads and non-persisting server dry runs as documented; no repairs, grant restoration or activation. External writer freeze remains required. |
| Installer `bootstrap` | Mutates reviewed installation state in ordered stop/drain/revoke/bootstrap stages; successful endpoint is verified inactive. |
| Installer `recover` | Mutates only supported exact-journal recovery and explicitly reviewed pending Helm settlement; operator confirmations do not establish request settlement themselves. |
| Activation `prepare` | Verifies inactive state and emits target evidence; does not create approval or activate. |
| Activation `activate --dry-run` | Non-persisting verification; does not consume approval, execute behavioral probes or prove rollout. |
| Activation `activate` | Consumes approval and journals changes, probes, grants and rollout; requires separate fresh exact-target acceptance and authorization. Not authorized by this report. |
| Activation `recover` | Separate journal-bound recovery under the activation contract; unresolved mutation intent can block indefinitely. No generic retry or installer-flag bypass. |

The activation contract keeps `prepare`, `activate --dry-run`, mutating `activate`
and `recover` distinct. It requires explicit kubeconfig, freeze and reviewed
target; genuine gate reports and short-lived approval are required for activation.
This Markdown report is not an `SG-09.json` activation report. A future authorized
administrator must create fresh evidence bound to the exact prepared target,
report hashes and approval window. Existing local reports cannot be recycled as
fresh deployment authorization.

The chart renders Kubernetes controller replicas zero and omits controller grants
even when saved `k8sController.activated=true` is supplied. The declared
`k8sController.replicaCount: 1` is not authorization to run that controller.
`admissionGuard.stdioIsolationProfile` defaults to `network-policy`; Cilium
isolation is opt-in, not a claim about all default installations. Current-policy controller-manager checks
cover Kubernetes 1.31.1; Kubernetes 1.30.3 has envtest behavior and older
controller-manager evidence, not a current-policy deployed rerun. Full stdio/CNI
acceptance covers only Kubernetes 1.31.1 with Cilium 1.17.8, single-node IPv4 VXLAN,
Kubernetes IPAM and kube-proxy replacement disabled. No version range, other CNI,
multinode topology, IPv6 or production environment is certified by this report.

## Remaining Gates And Approvals

| Open boundary | Responsible owner and required action |
| --- | --- |
| Current publication candidate | Maintainers complete SG-10 packaging/parity and SG-11 full tests, coverage, lint and dependency checks; SG-08's 22 scoped lint findings remain recorded, not waived. Subsequent source changes require affected reruns. |
| Current scanner findings | Maintainers supply SG-12 input/output provenance and exact finding evidence. Installation security owner explicitly approves any additional residual exception at rule, target and candidate-hash scope; no such new approval is recorded here. |
| Production identity and credentials | Installation security owner and credential issuers validate issuance, rotation, revocation, incident response and protected authority administration for the actual deployment. |
| Provider and query guarantees | Provider/application owners validate deployed authorization, endpoint and database TLS/pool behavior, query quota accounting and durable success audit where still open in the historical review. Local TLS/SQL/HTTP fixtures and source overlays do not establish these guarantees. |
| Production operations | Cluster/data owners establish writer freeze, request settlement, migration/backup/restore, certificate renewal, rate limits and the chosen CNI topology. Security owner reviews resulting exact-target acceptance before activation. |
| Publication | SG-13 validates owned staging against the tested tree and obtains explicit publication authorization. Local contract completion does not authorize commit, push or cloud operations. |

## Review Validation

Local command checks on 2026-09-25 passed: 21 top-level tests, no failed or skipped
tests, including all eight PowerShell/Make recorder scenarios. These checks use
local fixtures, chart rendering and command shims, not a deployment. Executed
from the repository root:

```powershell
go test ./tools/kognition-install ./tools/check-admission-activation ./internal/traceability -run '^(TestCommandRequiresExplicitInputs|TestPolicyCommandRequiresFreezeAndManifest|TestDrainCommandModes|TestDrainCommandReadOnly|TestCommandRejectsMissingManifest|TestTransactionCommandRequiredInputs|TestTransactionCommandFailureReport|TestPostRendererRejectsMissingTrustedPlan|TestInstallationEntrypointsRequireSafeWorkflow|TestAdmissionBootstrapWithholdsControllerPrivileges|TestActivationParsesBootstrapChart|TestAdmissionGuardChart|TestAdmissionGuardChartRejectsUnsafeConfiguration|TestAdmissionGuardSandboxBaseline|TestAdmissionGuardNetworkPolicy|TestActivationCiliumIsolationInventory)$' -count=1 -timeout=3m -v
go test ./tools/kognition-install -run '^(TestHelmEnvironmentCannotOverrideTarget|TestInvalidInvocationHasNoEffects|TestHelmSettlementFlags|TestFailedHistoryReview)$' -count=1 -timeout=2m -v
go test ./internal/traceability -run '^TestInstallationEntrypointCommands$' -count=1 -timeout=2m -v
```

The last command used the existing explicit `KOGNITION_INSTALL_SHELL_ACCEPTANCE=true`,
`KOGNITION_INSTALL_MAKE` and `KOGNITION_INSTALL_BASH` configuration. Without the
opt-in it skips and must not be reported as acceptance. All commands exited zero.
The first command returned package times 1.257s, 1.520s and 33.701s; installer
argument/history checks took 1.069s and shell checks 30.411s.

Documentation checks passed for 113 local destinations, two fragments and ASCII
additions, with existing Unicode preserved. Markdown lint uses the established
MD013 long-line allowance: the four feature documents pass; the security guide
retains four pre-existing MD060 table-spacing findings outside the added section,
with no new findings. Editor diagnostics and backlog state checks pass.
The five walkthroughs distinguish read-only checks,
mutation, operator attestations and remaining human approval. The historical
review, protected governance archive and both externally edited Go files retain
their pre-review SHA256 values. Existing SG-08 evidence does not automatically
attest source changes made after its tested snapshot. Expensive deployed suites,
scans and the whole-candidate quality gate were not rerun for this documentation
review.

Historical review SHA256 at the start of this reconciliation:
`7907DB1479FB124F448688CB73AF3BA68B4A8BB63815685AFFF5A717142BE53D`.
Protected governance archive SHA256:
`541F9336B2AAAD1D1D834D068CA7C2B49E656917D17C6B411BCE7CFA063BC16F`.
Neither file is an SG-09 edit. No runtime code, cluster resource, archive, scanner
report or approval record is changed by this documentation review.
