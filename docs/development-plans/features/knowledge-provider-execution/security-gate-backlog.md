# Security Gate Backlog

- Status: SG-11, SG-12 and SG-13 retain their historical scope. The October 20 gateway-state repairs and 92 exact local finding dispositions are CLOSED WITH OWNER APPROVAL for the recorded candidate. Raw scanner output remains unchanged; production/target approval and remote CI remain separate.
- Updated: 2026-10-20.
- Scope: local security validation and publication of the current knowledge-provider and Kubernetes hardening candidate.
- Source assessment: [Security review](security-review-code.md).
- Ownership: controller/chart maintainers implement; the installation security owner approves trust and residual-risk decisions. Individual assignees are not yet assigned.

This backlog records the security-remediation campaign and its control baseline.
The user lifted the source-push freeze on 2026-10-02 with "ok, push to origin and
mirror" and subsequently approved the [change-based policy](../../../development-gates.md).
That authorization does not grant release certification or deployment approval.
Local completion applies only to
the recorded candidate and verified commit, not arbitrary later working-tree
changes. Documentation of a task alone does not mark its implementation complete.

## Later Changes

The approved process revision supersedes blanket repetition of this campaign for
routine development. Select checks by affected control inputs, preserve usable
baseline receipts and review new privilege/finding deltas. A changed global tree
hash invalidates its exact attestation without invalidating every unaffected check.

The [planner](../../../../tools/securitygate/impact.go) and CI publish required-check
guidance and control input fingerprints. They do not grant clearance or import
old risk approvals. Unknown impact and changes to assurance rules require review.
Existing CI stays required. Broad artifact-release validation and the guarded
installer/exact-target activation transaction remain separate requirements.

Historical freeze/blocker statements in dated completion evidence describe those
original runs. The current source-publication policy above governs later work.

### Current Permission Delta Review

The [October 20 closure](../gateway-authority-hardening/finding-closure-2026-10-20.md)
repairs the gateway-to-migration ConfigMap escalation across all enabled
writers, isolates Tool/Communication receipts and retains legacy uncertainty
without replay. It records 333 minimum-version API results, eight affected
race suites, actual deployed acceptance and immutable cleanup. Fresh matched
all-writer profiles have 23 occurrences each; all 92 receive exact owner-approved
local dispositions bound to proposal
`b8de8eedcf80f16fa85da29f151bc462e77b434cc6aff572285573c473f17f08`.
This supersedes open local disposition statements in the dated paragraphs below.
Raw scan exits remain 1; no production or publication waiver is inherited.

The [October 19 audit-boundary review](security-review-code.md#webhook-audit-creation-boundary-2026-10-19)
records native audit-only creation, unchanged webhook RBAC, mandatory state-policy
identity, minimum-version API/race tests and actual two-replica gateway workflows.
Matched fresh scans of four render profiles each retain 21 findings, including
three HIGH, with zero added/removed finding/source fingerprints and no detected
secrets. KSV-0049 remains visible; arbitrary ConfigMap creation is independently
mitigated, while forged audit content and exact target risk acceptance remain
explicit. The local source checkpoint was not pushed; no remote CI receipt or
new SG-12/SG-13 release clearance is inferred.

The following paragraph preserves the October 2 checkpoint:

The [source-publication review](security-review-code.md#source-publication-delta-review-2026-10-02)
covers the independent resource-webhook permissions and executable candidate
`1e043c5`. Seven affected Go packages passed race checks with 217 passing test
events, no failures and no skips; core chart/archive parity passed.

Fresh scans of the four affected Kubernetes render profiles retain 146 identical
baseline finding/source occurrences and add four medium KSV-0049 occurrences for
the new installation ConfigMap-create grant. No added high/critical finding and no
secret were detected. Raw scanner exits remain 1; exact previous approvals were
not imported. The new medium risk stays open and explicit for release/deployment
owner review. Source-publication review completion does not claim release
certification, remote CI success or production activation approval.

## Agreed Boundaries

- Use mandatory two-phase activation: admission-only bootstrap, verification, then explicit controller activation.
- Before upgrading an existing installation, stop new work, drain affected workloads, and revoke old controller grants before changing admission resources.
- Enrolled workload namespaces are dedicated to Kognition-managed workloads and credentials. Controller compromise may expose credentials inside that trust domain. Unrelated namespaces remain outside it.
- Admission cannot constrain Secret GET. Required credential access must be bounded through identity, namespace enrollment, and RBAC.
- Preserve supported stdio execution. Disabling stdio or removing required lifecycle permissions without a replacement is not closure.
- Use the approved opt-in Cilium-backed stdio isolation profile for additive-allow resistance; its deny policy and CNI configuration remain administrator-owned.
- Perform local checks only. Do not mutate shared `desktop` or `kognition-cilium-e2e` clusters, change global Kubernetes context, or perform cloud operations.
- No blanket scanner suppression, unrelated cleanup, new branch, commit, or push as part of documenting this backlog.

The architecture choices above were approved in conversation and are recorded
in [ADR-0016](../../../adr/0016-installation-trust-boundary.md). This approval is not
blanket acceptance of remaining findings or evidence gaps.

## Current Evidence

| Area | Current implementation and evidence | Remaining limitation |
| --- | --- | --- |
| Bootstrap | SG-05 guarded wrappers, inactive install/upgrade, exact recovery and six deployed scenarios pass; chart ignores saved activation | Local 2026-10-02 target activated after fresh reviewed evidence; production and other targets remain separate |
| Manifest verifier | SG-01 complete: trusted-render contract, sealed/validated inventory, strict current-generation type-check status, normalization and drift checks pass focused unit/race/API gates | SG-04 binds activation to evidence; equality does not authenticate a render |
| Drain verifier | SG-02 complete: explicit read-only command, seven producer kinds and aliases, strict stopped-controller/ReplicaSet checks, actual shared role inspection, failure matrices and documented writer freeze/migration inventory | Sequential reads require an externally maintained freeze; SG-05 wrappers enforce ordering, not production finalization acceptance |
| Latest focused regressions | Activation, verifier CLI, guard and selected traceability suites passed under the race detector on 2026-09-24 | Focused SG-01 through SG-04 evidence, not full-candidate acceptance |
| API defaulting fixture | Public six-policy behavior/cleanup, lost Create response, live binding/policy/controller drift and fresh/drained checks passed on isolated Kubernetes 1.30.3 with default shared grants intact | Rollout/type-check status in envtest is simulated; actual runtime evidence is separate |
| Independent admission | SG-03 deployed evidence retained; SG-06 complete: exact native capability inventory, namespace/write/subresource matrix, classification and attachment regressions, independent and combined controls pass; SG-07 deployed the rebuilt current guard | Historical SG-06 image caveat is resolved by SG-07; local producer lifecycle evidence is recorded separately in SG-08 |
| Restricted producer lifecycles | SG-08 complete locally: actual ingestion, authenticated Agent dispatch, service revisions, Secret rotation/deletion, provider binding/finalization and negative/recovery matrix pass; [report and raw evidence](sg08-local-acceptance.md) | Tested source-overlay runtime images and reconstructed producer recovery only; no clean dependency build, live manager restart, cloud acceptance or production activation certification |
| Stdio and network isolation | SG-07 complete locally: full deployed matrix, actual concurrent streams, independently enforced Cilium deny, additive allows, policy races, drift rejection and cleanup recovery pass | Pinned single-node IPv4 Cilium 1.17.8 / Kubernetes 1.31.1 only; administrator mutation, other profiles and production producers are not certified |
| Kubernetes versions | SG-03 six-policy controller-manager typechecking passed on 1.30.3 and 1.31.1; current SG-06 policies reran on 1.31.1 with current-generation warning-free status and confirmed cleanup, plus 1.30.3 envtest behavior | Historical 1.30.3 controller-manager result is not a current-policy rerun; no production activation approval |
| Trust and operator contract | SG-09 complete locally: [finding reconciliation and five scenario reviews](sg09-operator-contract-review.md), enrollment authority, credential-compromise response and current command/default parity | No additional risk approval, fresh target activation report, production incident exercise or final scanner disposition |
| Packaging | Core archive refreshed again for independent admission on 2026-10-02; governance archive's authorized timestamp refresh retained all 11 member payloads | Changed core payload needs affected parity/configuration checks; timestamp-only evidence is scoped to compared payloads |
| SG-11 candidate validation | CLOSED for tree `1a2fd85548628e3828f6703d24d9024d8d3751fa`; full Go gates, 81.2% coverage, lint, Python/browser checks, audits, 19 builds, Docker/memory checks, no-skip producer acceptance and six installation scenarios pass; [closure evidence](sg11-local-acceptance.md) | Local candidate acceptance only; live external/cloud acceptance, SG-12 and SG-13 remain separate |
| SG-12 scans and dispositions | CLOSED with exact owner-approved dispositions; six profiles, 162 hash-bound occurrences and zero detected secrets; approval renewed for local commit tree `bc818b4178b2b3cdf1cb90c9c2eeb99b28eddfbf` | Residual risks and raw scanner exit 1 remain; renewed evidence expires 2026-10-03 01:50:13 UTC; no remote or production approval |
| SG-13 local commit | CLOSED by explicit scope revision on 2026-10-02; commit `261c5038f1c578bdc493b5bd4cb5fb7160fc0b88` matches the validated staged tree and preserves protected work | Later source push is authorized and pending affected-change review; it does not expand this historical attestation |

## Finding Closure Map

| Original finding or expanded risk | Required closure | Tasks |
| --- | --- | --- |
| KSV-0041: Secret management | Verify namespace-limited grants, named installation reads, guard identity separation, rotation/cleanup, and the approved dedicated-namespace credential trust contract | SG-02, SG-06, SG-08, SG-09, SG-12 |
| KSV-0056: Service management | Verify independent old/new-object mutation restrictions and allowed Service reconciliation; prevent unguarded activation or upgrade | SG-01 through SG-06, SG-08, SG-12 |
| KSV-0056: NetworkPolicy management | Verify independent mutation restrictions, persistent baseline ownership, actual CNI isolation, and additive-policy handling | SG-03 through SG-08, SG-12 |
| KSV-0109: migration ConfigMap | Retain raw finding and validate only the exact reviewed static SQL payload and metadata; reject changed content or extra findings | SG-10, SG-12 |
| Installation-state tampering | Verify independent ConfigMap/Lease/credential admission, including unrelated objects and old/new identity changes | SG-01, SG-03, SG-06 |
| Unguarded startup, attachment, or legacy work | Complete drain, bootstrap, verification, controlled activation, upgrade, and recovery; prove actual stdio remains usable | SG-02 through SG-08 |

Scanner findings can remain visible even when compensating controls are verified.
Each disposition must identify the exact rule, target, candidate hash, control
evidence, residual risk, and approving owner where acceptance is necessary.
Neither baseline presence nor absence from one render clears a finding.

## Execution Order

For the original remediation campaign, all tasks were required unless the
installation security owner changed scope explicitly. For later changes, use the
approved affected-control policy above. P0 denotes a security-control or acceptance
blocker; P1 denotes a required final release gate, not optional work.

Task IDs are stable references, not execution sequence. The table is ordered by
dependencies. This security-remediation backlog traces to the finding closure map
and agreed boundaries above; it does not invent feature story or requirement IDs.

| ID | Task | Priority | State | Dependencies |
| --- | --- | --- | --- | --- |
| SG-09 | Record trust decisions and operator contract | P0 | Complete locally; exact final dispositions recorded in SG-12 | None; foundational approval precedes activation design |
| SG-01 | Complete bootstrap manifest verification | P0 | Complete | None |
| SG-02 | Complete drain and effective-grant verification | P0 | Complete | None |
| SG-03 | Verify live admission behavior and compatibility | P0 | Complete | SG-01, SG-02 |
| SG-06 | Finish independent mutation and attachment regression matrix | P0 | Complete locally | SG-01 |
| SG-07 | Prove stdio and network isolation on a local CNI | P0 | Complete locally for pinned Cilium profile | SG-03, SG-06 |
| SG-08 | Prove restricted production producer lifecycles | P0 | Complete locally for tested runtime images | SG-03, SG-06 |
| SG-04 | Bind explicit activation to verified state | P0 | Local implementation complete; reviewed local activation performed on 2026-10-02 | SG-01, SG-02, SG-03, SG-07, SG-08, SG-09 |
| SG-05 | Implement safe install, upgrade, and recovery entry points | P0 | Local implementation and scoped acceptance complete | SG-04 |
| SG-10 | Refresh core packaging and generated artifact parity | P1 | Complete locally; exact-candidate attestation remains SG-11 | SG-01 through SG-09 |
| SG-11 | Run exact-candidate functional and quality gates | P1 | Closed for recorded local candidate | SG-10 |
| SG-12 | Refresh security scans and exact dispositions | P1 | Closed with exact owner-approved local dispositions | SG-10 |
| SG-13 | Review exact staging and record the cleared local commit | P1 | Closed locally by explicit scope revision; no push required | SG-11, SG-12 |

SG-01, SG-02 and SG-03 are complete for the working tree. SG-04 local implementation
and scoped validation are complete; SG-05 local installation acceptance is complete.
SG-06's independent mutation and attachment matrix is complete for the working tree.
SG-07's deployed stdio/Cilium acceptance is complete for the pinned local profile.
SG-08's restricted producer lifecycle acceptance is complete for the tested local
runtime images; [evidence and limits](sg08-local-acceptance.md) include the combined
deployed run, final provider rerun and scoped race checks. The 2026-10-01 SG-11
run supersedes the historical lint blockers and closes producer acceptance for its
recorded local candidate.
Production activation is not granted by this backlog. The isolated local target
was activated on 2026-10-02 through fresh exact-target evidence and owner approval;
see the [activation contract](../../../features/knowledge-provider-execution/activation-transaction-contract.md) and
[reviewed-target test scope](../../../testing.md#reviewed-local-target-acceptance).
SG-09's approved boundaries, historical finding
reconciliation and operator scenario review are complete locally; no additional
residual-risk approval is implied. SG-07 and SG-08 use an isolated acceptance
harness with explicit temporary grants after admission preflight; they must not
depend on an unfinished production activation shortcut. This avoids a dependency
cycle while keeping production activation blocked.

SG-11 is **closed for the recorded local candidate**; [acceptance evidence and
limits](sg11-local-acceptance.md) include clean offline image builds and no-skip
producer execution. SG-12 is **closed with exact owner-approved dispositions**;
[the decision](sg12-finding-dispositions.md) binds all six reports and 162
occurrences to that same candidate, with renewed exact approval for the final
local commit tree. SG-13 is **closed locally by explicit user scope revision** on
2026-10-02. The user originally removed push and remote-SHA verification from its
completion criteria while retaining the freeze, then explicitly authorized source
publication. That later authorization remains pending; it does not extend the
historical candidate's scanner dispositions or grant production activation.

## Implementation Tasks

### SG-01: Complete Bootstrap Manifest Verification

**Code:** [Verifier](../../../../internal/k8s/activation/verify.go),
[unit tests](../../../../internal/k8s/activation/verify_test.go),
[API fixture](../../../../internal/traceability/activation_integration_test.go),
[chart tests](../../../../internal/traceability/admission_guard_chart_test.go).

**Required implementation:**

- Run the existing API fixture first. Fix any defaulting or dry-run comparison failures against actual Kubernetes behavior.
- Require a present, well-formed, current-generation `status.typeChecking`; reject expression warnings and malformed status fields. Define how supported policy conditions affect readiness without requiring conditions Kubernetes does not emit.
- Validate exact supported GVKs, one baseline per enrolled namespace, policy-to-binding pairing, webhook identity, guard service account, and RoleBinding subjects.
- Make the trusted-render contract explicit. Validate exported `Manifest` inputs or restrict construction so callers cannot bypass inventory checks. Comparing live state to an unsafe desired manifest is not sufficient authorization.
- Preserve the non-mutating contract: server-side dry runs must not activate the controller or persist repairs.

**Required tests:** complete bootstrap success; absent/stale/malformed type-check status; warnings; duplicate/missing/wrong-version inventory; mispaired bindings; wrong guard subject; missing CA; spec/ownership drift; deleted objects; forbidden reads; cancellation and timeout. Verify Service and Deployment defaults without over-normalizing dangerous changes. Ensure each drift case restores its fixture so later cases cannot pass because an earlier fault remains.

**Done when:** focused unit/race and real-API tests pass, each rejection has a discriminating assertion, and the positive fixture remains inactive. Synthetic status evidence is labeled separately from SG-03's real controller-manager evidence.

**Completion evidence (2026-09-24):**

- [Trusted-render and verification contract](../../../features/knowledge-provider-execution/bootstrap-verification-contract.md) defines administrator-reviewed input, digest limits, exact inventory, status rules, normalization, dry-run semantics and evidence boundaries.
- [Manifest regressions](../../../../internal/traceability/activation_manifest_test.go) reject malformed/unsafe inventory, wrong versions, identities, narrowed bindings, webhook bypasses and exported-manifest mutation through public entry points.
- [Status regressions](../../../../internal/traceability/activation_status_test.go) cover absent, malformed, stale and future generation/type-check/condition fields, warnings, and supported positive status forms. Intercepted status responses are synthetic.
- Unit/chart check passed: `go test -count=1 -timeout=120s ./internal/k8s/activation ./tools/check-admission-activation ./internal/traceability -run 'TestActivation|TestVerify|TestParse|TestAdmissionBootstrap|TestAdmissionGuard|TestRun'`. The command package had no matching names; its full suite passed in the following unfiltered race run.
- Full verifier/command race check passed: `go test -race -count=1 -timeout=120s ./internal/k8s/activation ./tools/check-admission-activation`.
- Focused chart/status race check passed: `go test -race -count=1 -timeout=120s ./internal/traceability -run 'TestActivation|TestAdmissionBootstrap|TestAdmissionGuard'`.
- API check passed with local 1.30.3 envtest assets: `go test -tags integration -count=1 -timeout=5m ./internal/traceability -run '^TestActivationAPIDefaultingAndDrift$' -v`. Real defaulting, explicit allocation/CA preservation, live drift, deletion, forbidden/missing reads, cancellation, timeout and dry-run errors are checked. Restored fixtures pass again; before/after snapshots and zero controller replicas establish no persisted repair or activation.
- Touched Go files were formatted with `gofmt`; focused editor diagnostics were clear. The contract and backlog pass Markdown lint with only the established long-line allowance (`MD013`).

At SG-01 completion the API fixture simulated rollout/type-check status and removed
its isolated issuer-discovery grant. SG-02 supersedes that grant limitation with
default grants intact. Simulated status still does not prove live controller-manager
typechecking, guard TLS reachability, stdio or CNI behavior; those remain SG-03,
SG-07 and SG-08. No final-candidate
scan, archive refresh, staging, commit or push is included in SG-01 completion.

### SG-02: Complete Drain And Effective-Grant Verification

**Code:** [Verifier and producer scan](../../../../internal/k8s/activation/verify.go),
[drain tests](../../../../internal/k8s/activation/drain_test.go),
[operator command](../../../../tools/check-admission-activation/main.go).

**Required implementation:**

- Expose a drain-only pre-bootstrap command that does not require admission resources to exist. Retain explicit kubeconfig, bounded requests, nonzero failure exits, and no implicit mutation.
- Cover all seven native producer kinds in workload namespaces and controller-identity producers in the installation namespace. Review deprecated service-account aliases and server-defaulted forms.
- Limit the ReplicaSet exception to the stopped, current controller Deployment's UID and observed stopped state. Reject active, stale, foreign-owned, and deleting/recreating producers as appropriate.
- Validate effective shared grants from actual referenced roles, including inherited groups; do not trust system-looking names. Fail closed on read/list failures and unexpected references.
- Define the operator freeze for custom controllers, GitOps reconcilers, autoscalers, and other writers that can recreate resources or restore grants between checks. Recheck immediately before activation.
- Inventory legacy workload/state migration needs. Require explicit operator decisions for drain/deletion; do not delete unrelated objects automatically.

**Required tests:** every producer kind; terminating/unlabeled Pods; CronJob nested templates; stopped owned ReplicaSet success; wrong UID, stale generation, omitted replicas, active ReplicaSet denial; missing deployment on fresh install; unrelated installation workload success; malformed/missing roleRef and role; read/list failure injection; actual supported Kubernetes default roles; unrelated subjects; direct User/SA and inherited group grants.

**Done when:** fresh install and drained upgrade preflight pass, adversarial producers/grants fail, and the documented freeze closes the operational race assumptions without claiming transactional API reads.

**Completion evidence (2026-09-24):**

- [Drain verification contract](../../../features/knowledge-provider-execution/drain-verification-contract.md) defines explicit target inventory, legacy identity/state migration, externally maintained writer freeze, drain/revocation order, read-only invocation, bounded requests, failure exits and immediate recheck. It prohibits automatic deletion of unrelated objects and does not claim transactional reads.
- Producer regressions cover all seven kinds and both service-account fields, including nested CronJobs, unrelated installation workloads and terminating Pods. Controller shutdown fields and GET/LIST replacement/restart/disappearance fail closed. Stopped ReplicaSet tests cover current ownership, exact observed generation, absent/malformed/active counts, deletion and foreign references.
- Grant regressions cover actual referenced rules, malformed/missing role references, missing roles, all direct and inherited subjects, unrelated subjects, and cancellation/deadline/forbidden errors on every required GET/LIST path. Exact issuer endpoints include the trailing-slash variants observed on Kubernetes 1.30.3; no issuer wildcard or Secret rule is allowed.
- Public command HTTP tests verify explicit kubeconfig selection, mode separation, GET-only traffic, fresh success, retained Pod denial and forbidden LIST denial. Drain mode requires no admission resources and never activates or repairs state.
- Full activation/command race gate passed: `go test -race -count=1 -timeout=120s ./internal/k8s/activation ./tools/check-admission-activation` (19.628s / 2.905s).
- Bootstrap/guard regression race gate passed: `go test -race -count=1 -timeout=120s ./internal/traceability -run 'TestActivation|TestAdmissionBootstrap|TestAdmissionGuard'` (30.245s).
- Isolated real-API gate passed with local 1.30.3 envtest assets: `go test -tags integration -count=1 -timeout=5m ./internal/traceability -run '^TestActivationAPIDefaultingAndDrift$' -v` (15.790s). All default shared grants remain intact. Fresh drain, server-defaulted replicas and service-account alias, active/unobserved/foreign-owned denial, stopped owned ReplicaSet success and nonpersistence are verified alongside SG-01 regressions.
- Five touched Go files were formatted with `gofmt`; focused editor diagnostics were clear. Operator contracts and backlog pass Markdown lint with only `MD013` disabled and relative-link validation.

SG-02 is complete for the working tree, not a staged candidate attestation or
operational drain of an installation. The documented freeze is an operator
prerequisite, not an enforced lock. Runtime admission, CNI/stdio, durable trust ADR,
activation, upgrades and final-candidate release gates remain open. No shared
cluster mutation, activation, archive refresh, staging, commit or push occurred.

### SG-03: Verify Live Admission Behavior And Compatibility

**Code:** [Operator verifier](../../../../tools/check-admission-activation/main.go),
[policy enforcement tests](../../../../internal/traceability/controller_admission_integration_test.go),
[typechecking tests](../../../../internal/traceability/controller_admission_typecheck_integration_test.go),
[attachment tests](../../../../internal/traceability/attachment_admission_integration_test.go).

**Required implementation:**

- Add bounded behavioral preflight alongside structural checks. Exercise intended allow and deny cases under the exact controller identity without installing persistent broad grants.
- Verify all six live policies and bindings are active; establish that denials come from the intended control, not merely missing RBAC permission.
- Verify both guard webhooks through the deployed Service and serving certificate. Check trusted CA, service DNS identity, endpoint availability, and guard-to-API access.
- Run actual controller-manager typechecking at the supported minimum and selected local acceptance version. If 1.30 cannot be supported, explicitly approve and consistently change the minimum instead of silently skipping it.
- Keep probes uniquely named, ownership/UID guarded, time bounded, and cleaned up without touching existing objects.

**Required tests:** intended allow/deny per policy; absent binding; Ignore instead of Fail; weakened action; wrong controller match; wrong CA/DNS; unavailable guard; API denial/timeouts; current-generation warning-free typechecking on supported versions; proof that successful verification leaves grants and replicas unchanged.

**Done when:** a saved local control-plane report identifies versions, identities, objects, probe outcomes, and cleanup results. Envtest-only readiness or a webhook admission allow does not satisfy runtime acceptance.

**Completion evidence (2026-09-24):**

- [Local acceptance report](sg03-local-acceptance.md) records explicit cluster versions/endpoints, controller and guard identities, runtime image provenance, probe/typecheck object UIDs, outcomes, cleanup and commands.
- [Behavioral verifier](../../../../internal/k8s/activation/behavior.go) provides bounded exact-identity allow/deny probes with temporary exact-name grants, dry-run-only protection, ownership recovery and ordered UID-guarded cleanup. CLI opt-in requires `--policy-behavior --writers-frozen` and emits success/failure JSON.
- [Deployed acceptance](../../../../internal/traceability/admission_deployed_integration_test.go) and [real attachment fixture](../../../../internal/traceability/admission_deployed_attach_integration_test.go) passed on 1.30.3: both Service/TLS webhook paths, CA/DNS/endpoint faults, guard API denial and real timeout, followed by restored kubelet stdin/stdout. Controller replicas remained zero and grants unchanged; all tracked objects and namespaces were confirmed deleted.
- Actual six-policy controller-manager typechecking passed on minimum 1.30.3 and selected 1.31.1 with current-generation warning-free status and GET-confirmed cleanup. Focused unit, CLI, guard, chart and envtest race gates passed.
- Positive RBAC propagation failures remain conservative failures requiring cleanup confirmation and a complete rerun after convergence. Ambiguous Create ownership requires exact-marker inspection; neither is accepted as verification success.

SG-03 completion does not close enforcing-CNI/full stdio acceptance, broader
mutation/producer tests, trust ADR, activation, upgrades or final-candidate gates.
Overall status remains BLOCKED. No shared cluster/cloud mutation, scanner
suppression, archive refresh, staging, commit or push was performed.

### SG-04: Bind Explicit Activation To Verified State

**Code:** [Activation command](../../../../tools/check-admission-activation/main.go),
[activation transaction](../../../../internal/k8s/activation/transaction.go),
[chart values](../../../../deploy/helm/kognition/values.yaml),
[inactive controller template](../../../../deploy/helm/kognition/templates/k8s-controller-deployment.yaml).

**Required implementation:**

- Define an operator-controlled activation transaction: rerun drain/structural/behavior checks, verify applicable acceptance evidence, then grant the reviewed privileges and start the controller.
- Bind evidence to the cluster identity, release/namespace, enrolled namespace set, controller/guard identities, rendered manifest and chart hashes, image digests, relevant live UIDs/generations, and tool/test versions. A printed manifest hash alone is insufficient.
- Reject stale or mismatched evidence and define evidence lifetime. Store authoritative approval outside controller-writable state; document the administrative trust boundary.
- Separate preparation from activation so inherited `activated=true` or an ordinary upgrade cannot accidentally restore privileges. Define direct-Helm administrator bypass as an explicit unsupported/manual responsibility if it cannot be technically prevented.
- On failure, do not grant privileges or start work. If partial activation occurs, stop/revoke safely and record the failed stage; do not restore an unsafe legacy configuration automatically.

**Required tests:** changed values/images/namespaces/cluster/UID; expired evidence; missing runtime gate; concurrent drift; replay; interrupted activation; RBAC apply failure; rollout failure; repeated invocation; no-op success; dry-run mode. Use local fixtures and fault injection before installed-release tests.

**Done when:** activation cannot follow from a Boolean alone, failures confirm inactive state or explicitly report recovery-required uncertainty, and audit identifies exact authorization and failed stage.

**Local completion evidence (2026-09-24):**

- [Transaction contract](../../../features/knowledge-provider-execution/activation-transaction-contract.md) defines commands, approval/report schemas, separate administrator authority, freshness, replay protection, audit, compensation and recovery limits.
- Approval binds exact cluster/release/namespaces, images, object UIDs/generations and rendered/chart/executable hashes to all SG-03/07/08/09 reports. The chart always withholds controller bindings and renders zero replicas.
- Unit/fault tests cover mismatches, authority writes, immutable claims, stale journals, archive collisions, uncertain consumption, foreign-binding preservation and failure-stage provenance.
- The [API lifecycle fixture](../../../../internal/traceability/activation_transaction_integration_test.go) covers persistence/defaulting/RBAC, built CLI prepare/dry-run, active no-op, drift before scaling, replay, interrupted-state recovery, lost responses and grant/rollout/journal/cleanup failures. Acceptance/readiness are synthetic; no installed-release or process-kill acceptance is claimed.
- The [independent review](../../reviews/sg04-independent-review.md) found uncertain grant creation and interrupted probe inventory gaps. Both now use durable resource intent before Create and persisted UID before deletion. Pending absence blocks inactive success; legacy journals cannot certify completion or retirement. Tests cover delayed writes, failed/lost journal updates, foreign/replaced objects, collisions, retry rejection and all five probe interruption stages with fresh-client recovery.
- Full activation and CLI race suites passed after recovery fixes: `go test -race -count=1 -timeout=3m ./internal/k8s/activation ./tools/check-admission-activation` (16.402s / 7.986s).
- Both real-API fixtures and selected chart regressions passed with local Kubernetes 1.30.3 envtest assets: `go test -race -tags=integration -count=1 -timeout=10m ./internal/traceability -run 'TestActivation|TestAdmissionBootstrap|TestAdmissionGuard'` (194.957s). After repairing the new interruption-test lint finding, the transaction API race fixture passed again (158.544s).
- Scoped lint passed with zero issues: `golangci-lint run --max-same-issues=0 --max-issues-per-linter=0 ./internal/k8s/activation/... ./tools/check-admission-activation/...`. `govulncheck ./internal/k8s/activation ./tools/check-admission-activation` found no vulnerabilities. Changed Go files are formatted; focused editor diagnostics are clear.
- Earlier operator documentation and archive checks remain recorded; the recovery-fix review records current document validation and archive preservation.
- At SG-04 completion, SG-05 wrappers and SG-09's ADR were still missing. SG-05 evidence below and ADR-0016 supersede those implementation gaps; SG-07/08 acceptance and remaining SG-09 disposition review stay open. Archive refresh and final staging remain deferred; unrelated user files are preserved.

**Existing fixture lint findings retained for SG-11:** The broader integration-tag
lint command includes two earlier SG-03 fixture findings:
[unwrapped GET error](../../../../internal/traceability/admission_fixture_integration_test.go#L52)
(`errorlint`) and [deprecated Endpoints API](../../../../internal/traceability/admission_deployed_integration_test.go#L210)
(`SA1019`). Neither is suppressed or accepted as final-candidate clearance. The
GET error formatting should preserve both error chains; the deployed readiness
fixture needs an EndpointSlice migration validated on both supported local
versions. These remain assigned to the controller/chart maintainers under SG-11,
outside SG-04's activation implementation. Broad traceability lint is not green.

### SG-05: Implement Safe Install, Upgrade, And Recovery Entry Points

**Code:** [PowerShell deployment](../../../../make.ps1),
[Make deployment](../../../../Makefile),
[Helm notes](../../../../deploy/helm/kognition/templates/NOTES.txt),
[deployment guide](../../../../deploy/QUICKSTART.md).

**Required implementation:**

- Route supported entry points through the two-phase workflow. Force bootstrap inactive even with Helm reuse-values and old saved activation settings.
- Existing-release order: freeze producers, drain/finalize or explicitly migrate owned legacy work, stop controller, revoke old grants, verify drain, update admission, verify bootstrap, activate explicitly, then resume producers.
- Migrate old controller identities and randomly named stdio policies without silently adopting unsafe objects. Preserve required finalization while privileges are still deliberately available during the controlled drain stage.
- Handle install/upgrade interruption, certificate delay, migration failure, and Helm rollback. Do not let atomic rollback restore old controller privileges against changed admission state.
- Separate local security rehearsal from deployment helpers that load cloud credentials or touch shared clusters. Require explicit target selection and ownership checks.
- Finalize the SG-09 operator contract against the implemented commands, flags, and recovery behavior; include documentation parity in this task's acceptance evidence.

**Required tests:** recorded-command ordering in both shells; reuse-values true; fresh install; live old release upgrade; existing invocations; finalizers; legacy identity bindings; admission update failure; activation failure; retry/recovery; rollback. Assert old grants disappear before admission changes and no unintended resource is deleted.

**Done when:** both entry points and the documented manual installer flow have matching safe semantics and a repeatable isolated upgrade/recovery acceptance log. Direct Helm mutation is an unsupported Kubernetes bypass.

**Local completion evidence (2026-09-24):**

- [Acceptance report](sg05-local-acceptance.md) records the six real CLI/Helm cases: fresh, legacy, failed install, failed upgrade, certificate recovery and actual post-Helm process interruption. It includes exact cluster/version/image, releases, claims, commands, results and evidence limits.
- [Installer contract](../../../features/knowledge-provider-execution/installation-recovery-contract.md) defines strict reviewed plans, artifact/target snapshots, identity/UID/hash ownership, external freeze, finalization-before-revocation, inactive apply, post-render checks, immutable retirement intent and explicit settled-request recovery. Neither bootstrap nor recovery activates or rolls back.
- Both wrappers and the direct CLI share the implementation. Recorded shell tests reject missing plan/freeze and propagate errors. Automatic Kubernetes quickstart/fresh is blocked; credentials, images, namespace preparation and samples are separate writers.
- Unfiltered library/CLI race suites with integration enabled, exact real-API journal and wrapper race checks, scoped lint and scoped vulnerability checks passed. Saved activation remains inactive. Failure and cleanup tests reject unreviewed deletion and claim substitution; ordinary upgrade preserves an unrelated ConfigMap.
- Operator guides match the implemented commands and retain separate activation approval. ADR-0016 records only the previously approved trust choices. Finalization fixtures do not substitute for SG-08 production lifecycles, and local kind does not satisfy SG-07 enforcing-CNI/full stdio acceptance.

SG-05 closure is local and scoped. Overall status remains BLOCKED. The report
retains the accidentally selected attachment-test failures and broader lint gaps;
no whole-repository acceptance, production activation or publication is claimed.

### SG-06: Finish Independent Mutation And Attachment Regression Matrix

**Code:** [Admission templates](../../../../deploy/helm/kognition/templates),
[guard](../../../../internal/k8s/admissionguard/attach.go),
[guard tests](../../../../internal/k8s/admissionguard/attach_test.go),
[policy integration tests](../../../../internal/traceability/controller_admission_integration_test.go).

**Required implementation:**

- Review old/new-object checks for ConfigMaps, Secrets, Leases, Services, NetworkPolicies, Pods, Jobs, and other permitted workload kinds. Fix only reproduced gaps; labels alone must not establish trusted provenance.
- Verify unrelated installation state cannot be modified, workload mutation/attachment cannot occur in the installation namespace, and unrelated namespaces remain inaccessible.
- Test policy classification transitions, label removal/addition, owner changes, UID replacement, immutable invocation identity, subresources, deletion, and attempts to convert unrelated objects into managed objects.
- Verify persistent baseline protection and exact intended installation ownership. Review whether merely nonempty Helm release annotations allow cross-release confusion.
- Preserve guard GET-only identity, uncached authorization reads, sanitized failures, and exact exec/attach restrictions. Harden chart argument assertions against empty argument lists and assert baseline configuration explicitly.

**Required tests:** positive production-shaped objects plus one-field malicious mutations; all controller write verbs and relevant subresources; cross-release guard/baseline objects; missing/wrong-owner/deleting Job and policies; forbidden/timeout/cancelled reads; defaulted objects; guard outage; restricted security context variants. Run policy and guard controls together as well as independently.

**Done when:** an explicit allow/deny matrix maps every granted native mutation/attachment capability to independent enforcement and regression evidence, with no unreviewed classification bypass.

**Local completion evidence (2026-09-25):**

- [Acceptance report and capability matrix](sg06-local-acceptance.md) map every granted native mutation and attachment capability to RBAC, native policy, guard or Kubernetes validation and named regression evidence.
- Exact baseline release/installation ownership is wired through handler, server, CLI and chart. Reproduced partial/empty Helm ownership and mixed sandbox NetworkPolicy selector bypasses are fixed with red/green regressions.
- Real API matrices cover all native write verbs across installation, enrolled and existing unrelated namespaces; sensitive subresources; six state classes and immutable revisions; old/new metadata, adoption/deletion, merge/JSON patches and UID replacement. Owner classification remains distinct from authenticated provenance under ADR-0016.
- Guard tests cover live owner/UID/invocation identity, restricted/defaulted objects, exact attach options, missing/deleting/wrong policies, sanitized forbidden/timeout/cancelled reads and outage. Native policies and the independent guard pass both separately and together.
- Final focused guard/CLI/chart/native/RBAC/attachment race suite passed (3.809s / 1.847s / 40.820s). Current six-policy controller-manager typechecking passed on isolated 1.31.1 with zero warnings, current observed generations and UID-guarded cleanup (3.535s). Current native API behavior uses 1.30.3 envtest.
- Formatting, focused diagnostics, documentation lint and relative links are checked. Scoped integration lint retains only the two previously recorded SG-11 fixture findings; no whole-repository quality clearance is claimed. The protected umbrella archive hash is unchanged.

SG-06 closure is local and scoped. Current-source webhook fixtures do not attest
the historical deployed guard image or kubelet/CNI behavior. SG-07, SG-08 and the
remaining SG-09 disposition review remain open; production activation stays blocked.

### SG-07: Prove Stdio And Network Isolation On A Local CNI

**Code:** [Attachment guard](../../../../internal/k8s/admissionguard/attach.go),
[guard chart and baseline](../../../../deploy/helm/kognition/templates/admission-guard.yaml),
[attachment integration fixture](../../../../internal/traceability/attachment_admission_integration_test.go).

**Required implementation:**

- Add a reproducible acceptance harness on a task-owned local cluster with a verified NetworkPolicy-enforcing CNI. Plain kind networking is not sufficient evidence.
- Exercise real controller-to-Pod stdio through Kubernetes streaming and kubelet, not only admission CONNECT. Verify expected stdin/stdout/stderr and denial of unsupported TTY/target/options.
- Prove guard TLS/service/API connectivity with the rendered policies enabled. Keep the guard usable while workloads remain isolated.
- Address additive NetworkPolicies: inventory and prohibit conflicting allow policies in the dedicated sandbox, or enforce equivalent isolation through an independently controlled mechanism. Merely finding one deny policy is insufficient.
- Test baseline persistence, per-invocation policy creation ordering, deletion/cleanup races, and failure recovery. Document administrative mutation assumptions and any remaining time-of-check/time-of-use limits.

**Required tests:** positive stdio exchange; blocked external and cross-workload egress against reachable local control endpoints; injected additive allow policy; missing/deleting baseline; per-invocation policy replacement; guard outage; stream cancellation/timeouts; cleanup failure; concurrent invocations. Include a positive network control to show that blocked traffic is due to enforcement rather than an unreachable destination.

**Done when:** streams work and forbidden traffic is observably blocked under the supported CNI/configuration, including additive-policy and race scenarios. Save CNI version/configuration, image digests, commands, exit codes, and cleanup evidence.

**Local completion evidence (2026-09-25):**

- [Acceptance report](sg07-local-acceptance.md) records the pinned task-owned cluster, runtime image identities, effective Cilium configuration, reproduction commands, exact outcomes and administrative limits.
- The full deployed matrix passed in 166.579s with current guard image, six native policies, rendered guard NetworkPolicy, Service/TLS faults, real stdin/stdout/stderr, concurrent streams, cancellation and deadlines.
- Reproduced additive-allow bypass is addressed by the approved independent Cilium deny profile. Reachable Pod-network and host-network controls stayed blocked from sandboxes through baseline and invocation-policy deletion/replacement, including active streams.
- Activation rejects malformed Cilium inventory and live weakened rules/ownership drift; the guard retains uncached GET-only authorization with no Cilium permissions.
- Held-finalizer cleanup failure was observed before fresh-context recovery confirmed absence. UID-guarded cleanup removed temporary grants and all fixtures; final bootstrap remained inactive with zero controller replicas.
- Focused chart/activation/guard suites and diagnostics passed. Scoped lint retains only the two existing SG-11 fixture findings; the protected archive hash is unchanged.

This closes SG-07 locally, not SG-08 production producer lifecycles or production
activation. Generic `network-policy` mode is not additive-allow certified; no
multi-node, IPv6 or Internet-destination acceptance is claimed.

### SG-08: Prove Restricted Production Producer Lifecycles

**Code:** [Dispatcher](../../../../internal/k8s/dispatcher/dispatcher.go),
[ingestion controller](../../../../internal/k8s/controller/kb_ingester.go),
[service reconcilers](../../../../internal/k8s/controller/service_reconcilers.go),
[controller manager](../../../../internal/k8s/controller/manager.go).

**Required implementation:**

- Run actual Agent, ingestion, service, Secret rotation, and cleanup producers under independent admission using the deployed controller identity.
- Verify restricted ingestion as UID 1000 with read-only filesystem, required writable paths, dependency access, and real completion. Do not infer image compatibility from manifest validation.
- Exercise generation changes, cancellation, retries, binding publication, finalizers, Job/Pod UID replacement, and deterministic invocation-policy cleanup.
- Validate Secret access in installation and enrolled namespaces, including custom source names; verify unrelated Secret GET/list/watch denial and supported rotation/deletion behavior.
- Review recently changed namespace isolation and identity tests against current producer behavior before modifying them; preserve unrelated user changes.

**Required tests:** successful dispatch/ingestion/service lifecycle; source Secret rotation and deletion; namespace rejection before provisioning; stale bindings; concurrent cleanup; non-root runtime failure; API conflict/forbidden/timeout; restart recovery. Use local PostgreSQL and local service fixtures where required, never live cloud services.

**Done when:** allowed production paths reconcile and clean up under the exact restrictions, negative boundaries hold, and no supported lifecycle requires bypassing admission or weakening the sandbox.

**Closure (2026-09-25):** [SG-08 local acceptance](sg08-local-acceptance.md)
records the combined deployed PASS (154.283s), final provider image PASS (98.110s),
actual authenticated dispatch, restricted ingestion, binding cleanup, service
revisions, Secret rotation/deletion, UID protection, failure and recovery matrix,
and passing scoped race suites. Recovery reconstructs producer objects against
persisted Kubernetes state; it does not certify a live controller-manager restart.
Agent/ingester source-overlay images are runtime evidence, not clean dependency
build certification. Installation unrelated Secret GET and all Secret list/watch
denials hold; enrolled GET remains intentionally allowed by ADR-0016. Exact-candidate
lint, packaging, scans, publication and production activation remain blocked by
their separate gates.

### SG-09: Record Trust Decisions And Operator Contract

**Documents:** [ADR directory](../../../adr), [security guide](../../../security.md),
[deployment guide](../../../../deploy/QUICKSTART.md), [security review](security-review-code.md).

**Required implementation:**

- Add an ADR after checking existing decisions for overlap. Record two-phase activation, dedicated workload namespace trust, installation separation, and the rejected reliance on application-only ownership checks.
- State that enrolled credentials are controller-trusted and Secret GET bypasses admission. Define who can enroll namespaces and who can change guard/admission/baseline configuration.
- Document new install, upgrade, drain, evidence collection, activation, rollback/recovery, and cleanup, including the administrative freeze and the distinction between read-only verification and mutation.
- Reconcile stale security-review statements without erasing historical evidence. Link each original finding to its current exact disposition and supporting task evidence.
- Keep local publication clearance separate from production deployment acceptance. Do not imply live cloud, certificate lifecycle, quota/audit, or provider acceptance was completed by local fixtures.

**Required validation:** scenario walkthrough for fresh install, legacy upgrade, failure/retry, namespace enrollment, and credential compromise; relative-link checks; consistency with actual flags/defaults and supported Kubernetes/CNI versions.

**Done when:** the approved boundary and intended operating contract are durable and unambiguous before activation design proceeds. Any additional residual-risk acceptance identifies its owner and exact scope. Final command-level documentation parity is a required SG-05 deliverable, not a circular prerequisite for this foundational decision.

**Completion evidence (2026-09-25):**

- [ADR-0016](../../../adr/0016-installation-trust-boundary.md) retains the approved two-phase activation, dedicated credential trust and later Cilium isolation decision, alternatives and administrator authority. No additional approval is invented.
- [SG-09 review](sg09-operator-contract-review.md) maps each historical finding to current controls, bounded evidence, residual scope and responsible owner. It reviews fresh install, legacy upgrade, failure/retry, namespace enrollment and credential compromise.
- [Installer contract](../../../features/knowledge-provider-execution/installation-recovery-contract.md) now defines enrollment authority and trusted-administrator incident containment, issuer revocation, copied credential exposure, established-stream limits and recovery stop conditions. The security guide distinguishes these from ordinary maintenance.
- Installer/activation arguments, read-only boundaries, guarded entrypoints, inactive chart defaults and Cilium inventory checks pass: 21 top-level tests, including all eight shell recorder scenarios, with no skips. The report records exact commands and scope.
- Relative links/fragments, ASCII additions and scoped diagnostics pass. With the established MD013 allowance, the four feature documents pass Markdown lint; the security guide has no new lint findings and retains four pre-existing MD060 table-spacing findings outside the added section. The historical review, governance archive and externally edited source files remain unchanged by SG-09.
- This is local documentation and command-parity closure, not a fresh target activation report, production incident exercise or publication clearance. Final scanner dispositions remain SG-12; SG-10 through SG-13 and separate production acceptance are still required.

### SG-10: Refresh Core Packaging And Generated Artifact Parity

**Artifacts:** [Core chart](../../../../deploy/helm/kognition),
[umbrella chart archives](../../../../samples/helm-umbrella/charts),
[generation configuration](../../../../buf.gen.yaml).

**Required implementation:**

- Package only the core chart with `helm package deploy/helm/kognition --destination samples/helm-umbrella/charts`. Do not run `helm dependency build`.
- Verify normalized archive members match source and render both bootstrap and activated Kubernetes configurations, including custom identities/namespaces and standalone compatibility.
- Recheck generated bindings with the pinned generator versions and expected source-relative layout; settle literal drift fixtures and clean-target behavior without deleting tracked generated files.
- Record the protected governance archive's before/after hash and normalized members. Do not regenerate, edit, or stage it.

**Required tests:** core archive/source parity; strict chart lint and render assertions; pinned regeneration parity; intentional literal drift rejection; clean/build behavior on the supported shells.

**Done when:** source, generated code, and packaged core chart represent the same candidate and the governance archive remains unchanged by this work.

**Completion evidence (2026-09-25):**

- [SG-10 packaging report](sg10-packaging-parity.md) records tool versions, commands, final core hash, generated layout and before/after protected inventory.
- Complete core archive/source parity passes after core-only packaging. The protected governance archive retains its raw SHA256 and all 11 normalized members; no dependency build or staging occurred.
- Strict Helm 3.19.0 lint/render matrix passes 476 assertions. Twelve focused installation-render tests pass for inactive bootstrap, saved activation values, custom identities/namespaces, standalone compatibility and Cilium inventory.
- Pinned temporary protobuf regeneration matches all 11 source-relative outputs; all six parity/drift fixtures and Buf lint pass. Both real shell clean/build fixtures preserve generated bytes and execute successfully.
- Go formatting and touched-file diagnostics pass. This closes SG-10 locally only; SG-11, SG-12, SG-13 and separate production acceptance remain required.

### SG-11: Run Exact-Candidate Functional And Quality Gates

**Status (2026-10-01): CLOSED for the recorded local candidate.**

- [Closure report](sg11-local-acceptance.md) records execution, resolved blockers, skip dispositions, integrity and scope limits.
- Tested tree: `1a2fd85548628e3828f6703d24d9024d8d3751fa`.
- Candidate archive SHA256: `307b56d259ccc8b54b3ca7d1d52d974ac36f380da7e139cd9207b2a2e40d3054`.
- Evidence root: `%LOCALAPPDATA%/Temp/kognition-sg11-20261001-producers`; `closure.json`, `candidate.json`, `producer-acceptance.ps1`, `evidence/*.result.json` and matching logs retain commands, times, exits, hashes and test-level execution.
- Passed: all applicable local gates, including full Go race/integration, 81.2% coverage, both lint modes, Python/browser suites, audits, 19 builds, generator/shell/chart checks, Docker sandbox execution and memory replay.
- Former blockers closed: the provider fixture is included; clean offline agent/ingester images pass dependency checks; deployed producer lifecycle has 17 passing parent/subtest events and zero skips, including stdio, ingestion, provider and agent cases. All six installation scenarios pass. Policy typechecking passes on actual 1.30.3 and 1.31.1 control planes.
- Preserved caveats: container-to-PyPI TLS remains unreliable; verified host wheel downloads and clean offline installs are the recorded build procedure. One Docker timeout remains in the evidence, superseded by three serial passes of each unchanged case. Live external API/model/provider acceptance remains outside local scope.
- Integrity attestation passed before these status documents were updated. Closure applies to the tested candidate, not an automatically recertified working/staged tree. SG-12 has separate exact owner-approved dispositions; SG-13 subsequently validated the local commit and closed under the user's 2026-10-02 local-only scope revision. Remote publication remains frozen.

**Required implementation:**

- Build a fresh candidate from explicitly owned paths; record its tree hash before testing. Do not reuse earlier temporary checkouts or coverage as current evidence.
- Run the full Go race/coverage suite and the repository coverage gate (at least 80%); run integration tests with explicit envtest assets and task-owned local PostgreSQL. Confirm exit codes and intended test execution, including skips.
- Run agent and ingester Python suites separately using bounded runners. Run applicable browser, shell, chart, generator, and changed-surface tests from repository requirements.
- Run Go formatting/diagnostics, pinned lint, `govulncheck`, dependency audits for both Python requirement sets, and builds for all 19 previously identified binary targets. Confirm the target inventory rather than silently omitting a target.
- Repair only candidate-related failures. Record unrelated failures explicitly; do not claim all-green while a required gate fails or is skipped.

**Required evidence:** command, cwd, tree hash, tool versions, prerequisites, start/end time, exit code, passed/failed/skipped counts, coverage result, and artifact hashes. A coverage file's existence or a truncated terminal echo is not proof of success.

**Done when:** every applicable required gate passes on the exact candidate, coverage meets the threshold, and there are no unexplained skipped acceptance tests or background commands with unknown results.

### SG-12: Refresh Security Scans And Exact Dispositions

**Status (2026-10-01): CLOSED WITH EXACT OWNER-APPROVED DISPOSITIONS for the recorded local candidate.**

- Scanned SG-11 tree `1a2fd85548628e3828f6703d24d9024d8d3751fa`, archive SHA256 `307b56d259ccc8b54b3ca7d1d52d974ac36f380da7e139cd9207b2a2e40d3054`: filesystem/archive extraction, standalone, bootstrap, activated, configured optional resources and packaged core chart bootstrap.
- [Finding review and owner decision](sg12-finding-dispositions.md) cover exactly 162 occurrences. Proposal SHA256: `54a439a523eba9afed3690d03b9aacd465c73b5a2ca5df3ccb0e3b388a3d22bb`. The user self-attested security-owner authority and explicitly approved these exact local dispositions and conditions.
- Evidence: `sg12/inputs.json`, `sg12/reports-sg12-closure/`, `sg12/disposition-proposal.json`, `sg12/owner-approval.json`, `sg12/disposition-validator-tests.json` and `sg12/closure.json` under `%LOCALAPPDATA%/Temp/kognition-sg11-20261001-producers`.
- Dispositions: 151 exact residual-risk acceptances, five newly reviewed KSV-01010 SQL false positives, one non-imported/non-reachable OpenPGP advisory disposition, and five pre-existing verified KSV-0109 false positives. No secrets were detected. Required privileges and gateway writer/credential-domain risks remain present; they are accepted at exact local scope, not eliminated.
- The completion check validated report/input/source/finding/evidence hashes, expected candidate, approval scope and freshness. Ten adversarial controls reject absent or changed approval/evidence, unexpected findings, secrets and expired scans.
- Raw scanner and strict SQL verifier exits remain 1; owner-approved disposition validation exits 0. Reports are unchanged, and the automated SQL exception was not broadened.
- Freshness expires 2026-10-02 22:55:53 UTC. Refresh scans and affected dispositions after expiry or candidate/configuration changes. No production, activation or publication approval is implied; SG-13 still requires exact staged-tree clearance.

**Code/evidence:** [Security gate verifier](../../../../tools/securitygate),
[security review](security-review-code.md), [packaged charts](../../../../samples/helm-umbrella/charts).

**Required implementation:**

- Scan the fresh candidate filesystem/archive and explicitly rendered Kubernetes configurations for vulnerabilities, secrets, and misconfigurations. Default standalone rendering omits controller resources and cannot cover this gate.
- Cover bootstrap and activated profiles, configured identities/namespaces, and relevant optional resources. Record scanner/database versions, commands/options, input hashes, output hashes, timestamps, and raw exit codes.
- Bind raw reports to the exact scanned tree. Keep findings intact; do not rewrite reports or use path-wide/rule-wide exclusions.
- Revalidate only the exact static-SQL false positive with the existing verifier and canonical payload hash `01fadf1322da74e76e78787f0e2113580682b1352e6366605339564531da42e6`. Reject metadata/content drift and extra findings.
- For remaining RBAC findings, attach current authorizer/admission/runtime evidence and the precise approved trust boundary. If automated disposition support is required, implement per-finding, provenance-bound verification with adversarial tests; do not broaden the SQL exception.

**Required tests:** SQL credential injection, extra/duplicate keys/documents, labels/annotations, changed finding metadata, unexpected RBAC findings, stale report, wrong input hash, changed archive, and missing provenance. Verify strict failure for every undispositioned HIGH/CRITICAL finding and every detected secret.

**Done when:** each original and newly discovered finding is mitigated or explicitly dispositioned at exact scope, raw scanner failures are distinguished from a verified disposition result, and the security owner can audit all evidence against the candidate hash.

### SG-13: Review Exact Staging And Publish Only After Clearance

**Status (2026-10-02): CLOSED LOCALLY BY EXPLICIT USER SCOPE REVISION; LATER SOURCE PUBLICATION AUTHORIZED AND PENDING.**

- Scope revision: the user explicitly instructed **"you can close it, even without the push."** SG-13 now closes on the verified local commit; push and verification of both remote SHAs are excluded from this task's closure. They are not claimed as completed or automatically authorized later.
- The earlier source-push freeze was subsequently lifted by **"ok, push to origin and mirror"**. Publication now follows [affected-change validation](../../../development-gates.md); historical local closure is preserved and does not certify later candidates.
- Verified local commit: `261c5038f1c578bdc493b5bd4cb5fb7160fc0b88`, parent `352d690503030601791c65431e8698224e03a482`, tree `bc818b4178b2b3cdf1cb90c9c2eeb99b28eddfbf`. All 427 committed paths match the cleared inventory; the eight remaining protected paths were excluded and their working contents preserved.
- Final local validation and receipt: `%LOCALAPPDATA%/Temp/kognition-sg13-local-20261001`, including `local-validation.json`, `staging-attestation.json`, `sg12/closure.json` and `local-commit-receipt.json`. The receipt's earlier "NOT CLOSED" wording reflects the original remote-publication requirement and is superseded only by this scope decision; its technical evidence is retained unchanged.
- Fresh scans and renewed owner approval bind archive SHA256 `5eee79832a65e72b3e54873393f5c6af9da6dc1008cd6615b002d75a9ecb821e` and proposal SHA256 `81e075cbbc57b5251d743789632d63f1bbb0d57dac3da11d3c20a17aab34785d`. The 162 findings retain their reviewed risks and raw scanner failures. Evidence expires 2026-10-03 01:50:13 UTC; the local completion record is not a perpetual security clearance.
- The SG-11/SG-12 snapshot inherited an `errors.Is` cleanup in protected [admin_server_test.go](../../../../cmd/kognition-knowledge-pgvector/admin_server_test.go). Preserving the committed version in a publication preview reproduces two `errorlint` failures. The tested snapshot therefore cannot be published unchanged while excluding that delta.
- The initial user decision was **"Keep it protected; leave publication blocked"**. The user subsequently explicitly selected **"Yes, include that cleanup and resume SG-13"**, superseding that restriction only for the existing `errors` import and two `errors.Is` comparisons already present in the SG-11-tested blob. Any other edit to that file still requires approval; all other protected paths remain excluded.
- Initial Git state: `main` at `352d690503030601791c65431e8698224e03a482`, existing index tree `29b6d7cbe991254db39aab7b04339fc0ab0583d6`, 168 already-staged paths. Isolated preview indexes preserved the initial index until authorized scoped staging. The final staged tree was verified equal to the cleared candidate before committing; protected working files were not changed.
- The validated deletion of `deploy/helm/kognition/templates/clusterrolebinding.yaml` must be represented explicitly in any future candidate inventory. A disposable-index check confirmed it remains deleted; starting from HEAD without that deletion would reintroduce a controller binding.
- Historical protected-boundary preview `8398462dc963b8bd58c54d2fbffc9d5cd59f6e5e` was rejected and was never approved or staged. It excluded the test cleanup and therefore differed from validated tree `1a2fd85548628e3828f6703d24d9024d8d3751fa` in that test and the post-validation security/closure documents. The subsequently authorized publication candidate includes the exact tested cleanup; its executable content must match the SG-11 candidate, with only reviewed documentation differences. The final candidate hash, scan binding and remote publication receipt are recorded in the SG-13 evidence, not inferred from this historical preview.
- Evidence: `%LOCALAPPDATA%/Temp/kognition-sg13-20261001`, including `candidate.json`, `evidence/protected-boundary-lint.log` and `deletion-check.json`. Historical SG-11/SG-12 closure remains limited to its recorded candidate; it does not clear this publication preview.
- The scope conflict was resolved by the explicit narrow authorization and the exact local commit was validated. Do not waive lint or reuse mismatched scanner approvals. Later authorized source publication requires affected checks, reviewed deltas and verification of both remote `main` SHAs.

**Historical completion criteria under the revised local scope:**

- Review staged and unstaged changes separately; stage only owned candidate paths. Never use blanket staging in this dirty worktree.
- Confirm the validated candidate tree equals the staged tree. Any code, chart, dependency, generated artifact, or security-document change invalidates affected evidence and requires the relevant reruns.
- Inspect binaries, archives, generated files, credentials, local infrastructure files, and accidental test output. Confirm protected paths remain excluded and unchanged by this work.
- Update the security review verdict only after SG-01 through SG-12 meet their definitions of done; record the final finding dispositions and evidence locations.
- Under the current local-commit authorization, commit directly on `main` only when all required gates are green. Follow the required `T-####` title convention and Copilot co-author trailer; do not invent an external work-item reference.
- Push and remote-SHA verification were excluded from the historical local closure. Later source publication is now authorized under the revised policy; never force-push or rewrite history to conceal a partial publication.

**Required validation:** explicit staged-path review; protected-file hash check; staged/validated tree equality; no detected secrets in scoped scans; clean required diagnostics; final local commit review. These checks are recorded for the local commit above. Remote SHA checks are not part of the revised closure.

**Done when:** the exact cleared candidate is committed locally, the commit tree matches the validated staged tree, and protected/unrelated files remain outside the commit except for the explicitly authorized test cleanup. Met by commit `261c5038f1c578bdc493b5bd4cb5fb7160fc0b88`; SG-13 is CLOSED. Later source publication is separately authorized and pending. This status update does not alter the recorded tested tree or claim revalidation of later working-tree changes.

## Protected Paths

The following paths are excluded from this candidate unless the user explicitly
changes ownership/scope. Existing user changes must not be reverted:

The only current exception is the already-tested `errors.Is` cleanup in
`cmd/kognition-knowledge-pgvector/admin_server_test.go`, explicitly authorized
for SG-13. Its complete content must match the validated candidate blob; no
additional changes to that file are authorized.

```text
.azure/infrastructure-plan.json
.azure/insights.json
.azure/validate-status.json
.hackaton2026/project-long-description.md
cmd/kognition-knowledge-pgvector/admin_server_test.go
cmd/kognition-knowledge-pgvector/main_helpers_test.go
cmd/kognition-knowledge-pgvector/wait_for_shutdown_test.go
providers/pgvector/pool.go
samples/helm-umbrella/charts/kognition-governance-0.1.0.tgz
coverage/
test-results/
temporary checkouts, reports, logs, and generated test output
```

Reconfirm ownership and current hashes before staging. Historical hashes alone do
not prove that a user's later changes should be discarded.

## Initial Verification Commands

These commands describe future implementation checks. They were not run as part
of writing this backlog. Execute from the repository root on the intended
candidate; prerequisites and exit codes must be recorded.

```powershell
go test -count=1 -timeout=120s ./internal/k8s/activation ./tools/check-admission-activation ./internal/traceability -run 'TestVerify|TestParse|TestCommand|TestActivationParses|TestAdmissionBootstrap'
go test -race -count=1 -timeout=120s ./internal/k8s/activation ./tools/check-admission-activation
$env:KUBEBUILDER_ASSETS = Join-Path $env:LOCALAPPDATA 'kubebuilder-envtest/k8s/1.30.3-windows-amd64'
go test -tags integration -count=1 -timeout=5m ./internal/traceability -run '^TestActivationAPIDefaultingAndDrift$' -v
go test -p=1 -count=1 -timeout=10m -race -coverpkg=github.com/kognition-ai/kognition/... -coverprofile=coverage.out ./...
go test -tags integration -count=1 -timeout=15m ./...
```

Confirm assets exist and supply required local database settings without logging
secrets. Save and restore environment changes. Full-control-plane tests require
an explicit task-owned kubeconfig; never rely on the current context. Integration
tests that skip due to missing assets or kubeconfig do not satisfy the gate.

## Completion Record

Update this record as tasks close. A passing code test alone cannot close a task
that also requires runtime acceptance, operator documentation, or owner approval.

| Task | Candidate/tree hash | Test or acceptance evidence | Residual risk/approval | Completed by/date |
| --- | --- | --- | --- | --- |
| SG-01 | Working tree only; final staged-tree attestation remains SG-11 | Focused unit/race/API commands and contract linked in SG-01 above | Synthetic readiness; former grant-removal limitation superseded by SG-02; runtime and activation remain blocked | GitHub Copilot / 2026-09-24 |
| SG-02 | Working tree only; final staged-tree attestation remains SG-11 | Full activation/CLI race, bootstrap regression race, real API default-grant/fresh/upgrade checks and drain contract linked above | Operator freeze required; sequential reads are not transactional; overall security gate BLOCKED | GitHub Copilot / 2026-09-24; ready for review |
| SG-10 | Local core SHA256 recorded in report; final tree attestation remains SG-11 | [Packaging, generator, chart and shell evidence](sg10-packaging-parity.md) | Governance unchanged; scans, full candidate gates and activation clearance remain open | GitHub Copilot / 2026-09-25 |
| SG-11 | `1a2fd85548628e3828f6703d24d9024d8d3751fa` | [Local closure](sg11-local-acceptance.md); required checks pass, coverage 81.2%, no-skip producer acceptance | Closed for recorded local candidate; external production and final staged-tree limits retained | GitHub Copilot / 2026-10-01 |
| SG-12 | `1a2fd85548628e3828f6703d24d9024d8d3751fa` | [Exact dispositions and approval](sg12-finding-dispositions.md); six reports, 162 occurrences and ten negative checks | Closed with explicit owner-approved local residual risks; raw scans still fail; 24-hour freshness and candidate limits retained | GitHub Copilot verification; workspace user owner approval / 2026-10-01 |
| SG-13 | Commit `261c5038f1c578bdc493b5bd4cb5fb7160fc0b88`; tree `bc818b4178b2b3cdf1cb90c9c2eeb99b28eddfbf` | [Local completion and scope revision](#sg-13-review-exact-staging-and-publish-only-after-clearance); exact tree equality, protected-path and commit checks pass | CLOSED locally; later source publication authorized under affected-change policy, remote verification pending | GitHub Copilot verification; user scope revision / 2026-10-02 |
| Other tasks | See individual completion evidence above; pending where not recorded | See individual task sections | Overall gate remains BLOCKED | See individual task sections |

Production-only concerns already listed in the security review, including live
cloud provider acceptance, operational certificate lifecycle, and outstanding
provider authorization/quota/audit integration, remain tracked deployment blockers.
This local backlog does not silently clear them. Before final sign-off, explicitly
map each such concern to either this candidate's required local gate or a retained
production blocker; any scope change requires owner approval.
