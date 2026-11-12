# SG-06 Local Acceptance

- Status: Complete for the local working tree; production activation remains blocked.
- Date: 2026-09-25.
- Scope: independent native mutation and attachment enforcement and regression evidence.
- Contract: [SG-06 requirements](security-gate-backlog.md#sg-06-finish-independent-mutation-and-attachment-regression-matrix) and [ADR-0016](../../../adr/0016-installation-trust-boundary.md).

## Capability Matrix

The [exact rendered inventory](../../../../internal/traceability/controller_native_capabilities_test.go)
fails when any native mutation grant changes. The table covers every grant in the
installation namespace (`control`) and enrolled workload namespace (`tenant`).
The [effective RBAC fixture](../../../../internal/traceability/controller_rbac_integration_test.go)
also checks an additional enrolled runtime namespace, unrelated namespaces,
named installation Secret reads, platform identity separation and scoped caching.
Temporary controller grants exist only inside isolated fixtures; the chart still
withholds controller bindings and renders a stopped controller.

Evidence keys:

- **N:** `TestControllerInstallationConfigMapAdmission` in the [native API matrix](../../../../internal/traceability/controller_admission_integration_test.go).
- **R:** `TestControllerRBACAuthorization` in the effective RBAC fixture above.
- **G:** [guard unit tests](../../../../internal/k8s/admissionguard/attach_test.go), including `TestAttachGuard*` and `TestReservedPod*`.
- **A:** `TestIndependentAttachmentAdmission` and `TestCombinedAttachmentAdmission` in the [attachment API fixture](../../../../internal/traceability/attachment_admission_integration_test.go).

| Scope and native resource | Granted mutation verbs | Independent allow/deny boundary | Evidence |
| --- | --- | --- | --- |
| Installation ConfigMaps | create, update, delete | State policy checks old and new name/classification and absence of either Helm ownership key; unrelated state cannot be adopted or deleted | N: real audit/rate producers, six state classes, immutable revisions, metadata transitions |
| Workload ConfigMaps | create, delete | State policy permits the exact managed gateway CA classification; unrelated objects and Helm-owned objects denied | N: workload CA lifecycle and old/new adoption/deletion matrix |
| Workload ConfigMap `kognition-knowledge-gateway-ca` | update | Named RBAC grant plus state policy; other workload ConfigMap updates and all ConfigMap patches denied by RBAC | N, R and exact inventory |
| Workload Secrets | create, update, patch, delete | Credentials policy requires managed Opaque credentials, rejects Helm ownership and service-account credential annotation | N: credential lifecycle, old/new adoption/deletion and merge/JSON patches |
| Installation and workload Leases | create, update | Lease policy requires the supported managed classification and rejects Helm ownership; patch/delete absent from RBAC | N: lease lifecycle, classification transitions and actual leader-election lock; R |
| Workload Deployments | create, update, patch, delete | Workload policy checks old/new management metadata, service account, token mounting and restricted Pod templates | N: safe Deployment and malicious one-field changes, metadata/adoption/delete and patch matrix |
| Workload Jobs | create, update, patch, delete | Workload policy applies to Job Pod templates, including server-defaulted labels | N: safe Job lifecycle, restricted mutations and old/new classification matrix |
| Workload Pods | create, update, patch, delete | Workload policy checks restricted specification and old/new metadata; reserved stdio Pods additionally require guard-controlled identity | N, G, A: restricted mutations, reserved creation/update and immutable invocation label |
| Workload Services | create, update, patch, delete | Service policy permits managed ClusterIP Services with scoped, matching selectors; rejects external routing and unrelated/Helm-owned objects | N: lifecycle, selector changes, published revision routing compatibility, old/new and patch matrix |
| Workload NetworkPolicies | create, update, patch, delete | Network policy requires scoped immutable selectors; sandbox metadata or selector identity requires deny-all Egress; Helm baseline cannot be adopted/deleted | N: lifecycle, mixed selector regression, baseline protection and metadata/patch matrix |
| Workload `pods/attach` | create (HTTP CONNECT) | Independent guard checks exact identity, namespace, target/options, live Pod/Job/policies and persistent baseline ownership | G, A: positive attach admission, malicious objects/options, missing policies and fail-closed outage |
| Installation and workload Events | create, patch | Namespace RBAC only; Events are untrusted, nonauthoritative telemetry and have no native content policy | N: real create/merge patch allowed, update/delete/outside create denied; R and inventory |

N's `native-rbac-boundaries` subtest checks create, update, patch, delete and
deletecollection for all nine native resource kinds across existing `control`,
`tenant` and `outside` namespaces. Outside writes and all deletecollection grants
are denied. Installation workload/Secret writes and attachment are denied.
Sensitive-subresource reviews deny exec, portforward, ephemeral-container writes,
Pod status/eviction/binding, Deployment scale/status, Job status and token creation.
The named CA update is checked separately from unnamed ConfigMap authorization.
Tests distinguish intended ValidatingAdmissionPolicy denials from RBAC denials.

## Classification And Attachment Evidence

- N checks new-object label loss, old-object adoption, both partial Helm markers and empty marker values, deletion, benign merge/JSON patches and malicious metadata patches. Nonempty unrelated labels account for Kubernetes Job defaulting instead of assuming absent labels stay absent.
- All six installation ledger/audit classifications have create/update/delete and shared metadata coverage: cognitive audit, cost, usage receipt, token reservations, tool-call reservations and tool invocations. Revision ConfigMaps additionally reject missing parent/kind labels, missing owners and foreign owner API versions in both old and new states; Kubernetes rejects immutable payload changes.
- Kubernetes rejects object UID replacement through Update and granted Patch operations. Revision owner UID changes remain allowed: owner API and labels classify a resource but do not authenticate provenance. Ordinary native owner references are not globally immutable. Dedicated enrollment is the trust boundary; no per-object tenant isolation is claimed.
- G requires the current live Job UID to match the Pod owner UID and the invocation NetworkPolicy owner. It rejects missing/wrong/deleting objects, wrong owner API/kind/controller/name/UID, invocation mismatch and incompatible policy selectors/rules. Reserved Pod invocation identity cannot change on update.
- The baseline must have a UID, no owners, exact Helm release and installation namespace, exact sandbox selector and deny-all Egress rules. Cross-release, cross-installation, absent or malformed baseline identities fail closed. Controller objects reject either Helm ownership key by presence, including empty values.
- Guard authorization uses uncached GET-only reads and a separate identity. Forbidden, timeout and cancellation failures are sanitized for Pod, Job, invocation policy and baseline reads. Tests retain positive defaulted service accounts and Linux behavior, and reject unsafe security contexts, host access, custom runtime classes, token mounting, extra/init/ephemeral containers and unsafe stream options.
- A exercises guard controls independently and with native policies installed. The combined fixture proves native Helm-adoption denial while the guard is active. Outage fails closed. Guard inability to delete Pods/read installation Secrets and controller inability to remove the webhook/read its TLS Secret remain explicit checks.
- [Chart assertions](../../../../internal/traceability/admission_guard_chart_test.go) check nonempty argument lists, exact release/install/controller/workload/baseline wiring, release-namespace annotations, fail-closed webhooks, GET-only guard grants and absent controller bindings.

## Reproduced Defects

| Defect | Reproduction and correction |
| --- | --- |
| Baseline accepted another release or installation | Guard rejection tests exposed nonempty-only ownership checks; exact configured release and installation namespace now required throughout handler/server/CLI/chart wiring |
| Partial or empty Helm markers bypassed classification | Red guard test run (1.418s) and native metadata run (18.638s); key-presence checks reject either marker on old and new objects; green reruns passed |
| Mixed sandbox selectors escaped deny-all classification | Six failing API cases (13.193s); inspect sandbox identity in metadata or selector, then require deny-all Egress; green rerun passed (13.047s) |
| Fixtures depended on missing automatic grants | Bind only rendered controller-component Roles with RBAC enabled inside envtest; require exact role counts and intended-policy denials. Final broader run exposed the older RBAC fixture's `controller binding missing`; repaired fixture passed under race (10.944s, then 6.667s after formatting) |

## Final Verification

Environment: Windows, Go 1.26.6, Helm 3.9.1, Kubernetes 1.30.3 envtest assets.
The full guard test set is selected by the expression below; the controller CLI
package compiles and its matching tests pass. Current native policy behavior and
independent/combined webhook behavior run against real envtest API servers.

```powershell
go test -race -tags=integration ./internal/k8s/admissionguard ./cmd/kognition-k8s-controller ./internal/traceability -run '^(TestAttachGuard.*|TestReservedPod.*|TestGuardConfiguration|TestController.*|TestIndependentAttachmentAdmission|TestCombinedAttachmentAdmission|TestAdmissionGuard.*)$' -count=1 -timeout=8m
```

Final result: PASS, respectively 3.809s, 1.847s and 40.820s. The formatted RBAC
fixture separately passed in 6.667s. Earlier expanded native namespace and
state/UID matrices passed under race in 19.136s and 18.819s.

Live typechecking used only task-owned `kognition-admission-security-20260923`,
explicit loopback kubeconfig, `KOGNITION_ADMISSION_ISOLATED=true` and expected
version `v1.31.1`, endpoint `https://127.0.0.1:53168`:

```powershell
go test -tags=integration ./internal/traceability -run '^TestControllerAdmissionPolicyTypeChecking$' -count=1 -v -timeout=2m
```

Result: PASS (3.535s). Each policy had generation/observedGeneration 1, zero
expression warnings and GET-confirmed cleanup with UID-preconditioned deletion.
No bindings were created for these uniquely named typechecking policies.

| Policy suffix | Observed UID |
| --- | --- |
| leases-04cab61b | 4729db20-0347-444c-864f-f64bc60e7ae8 |
| services-cba14dc0 | c043ec50-932d-4f2a-9ab0-c98a1281c4c4 |
| network-3c0d0140 | b6ba3ff9-22ec-4fc9-8109-a9b38a0caa9c |
| state-84e5e124 | ff7ed0eb-2f0a-4c05-bcb1-ea4fb6d81db3 |
| credentials-fe39592a | f41f6ba9-c502-40c9-81ca-eb9114367fd2 |
| workload-ffcb4f0f | a0129a87-e023-4b5d-a420-4ffd1d5dd4fd |

All names have prefix `security-test-kognition-control-`. Historical SG-03 live
typechecking on 1.30.3 remains historical evidence; this pass did not rerun its
controller-manager. Current 1.30.3 envtest behavior is separately identified above.

Scoped integration-tag golangci-lint reported only the two already tracked SG-11
findings: `errorlint` at `admission_fixture_integration_test.go:52` and `SA1019`
at `admission_deployed_integration_test.go:210`. No suppression or unrelated repair
was made; broad traceability lint is not green. Touched Go files are formatted and
focused editor diagnostics are clear. Documentation passes Markdown lint with the
established `MD013` allowance and relative-file link checks.

The protected umbrella archive remains unchanged, SHA256
`541F9336B2AAAD1D1D834D068CA7C2B49E656917D17C6B411BCE7CFA063BC16F`.
No dependency, historical security-review edit, archive refresh, staging, commit,
push, global context change, shared-cluster mutation or cloud operation is included.

## Acceptance Limits

Every granted native mutation/attachment capability now has an explicit enforcing
layer and regression mapping. The requirements/ADR cross-check preserves dedicated
credential namespaces, inactive bootstrap and independent runtime gates. Reused
fixture helpers cover metadata transitions without adding production dependencies.
Task traceability uses this report and test mappings; no commit was authorized.

Envtest has no kubelet or enforcing CNI. An observed CONNECT admission is not proof
of usable stdin/stdout/stderr. The saved SG-03 deployed image predates the required
`--release-name` flag; current SG-06 deployed guard acceptance is not claimed and
that image must be rebuilt before current-chart runtime rehearsal. SG-07 must prove
enforcing-CNI/full stdio behavior and SG-08 actual producer lifecycles. SG-09's
remaining disposition review and SG-10 through SG-13 final-candidate gates remain
open. SG-06 completion neither clears raw scanner findings nor authorizes activation.
