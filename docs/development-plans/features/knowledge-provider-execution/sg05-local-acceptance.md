# SG-05 Local Installation Acceptance

- Status: local SG-05 implementation and scoped acceptance complete; activation and publication remain blocked.
- Evidence date: 2026-09-24.
- Scope: working-tree installer, wrappers, recovery fixtures and operator-document parity; no final-candidate attestation.
- Contract: [installation and recovery](../../../features/knowledge-provider-execution/installation-recovery-contract.md).
- Decisions: [ADR-0016](../../../adr/0016-installation-trust-boundary.md).

## Environment And Isolation

The deployed fixture used the task-owned kind cluster
`kognition-admission-security-20260923`, its single
`kognition-admission-security-20260923-control-plane` node, Kubernetes `v1.31.1`,
and `https://127.0.0.1:53168`. Its separate kubeconfig was
`$env:TEMP/kognition-admission-security-20260923.kubeconfig`. The fixture rejects
other than the two explicitly named task-owned clusters and requires an exact
reviewed version, loopback endpoint and isolation opt-in. Global context remained
`docker-desktop`; shared clusters were not changed.

Tooling: Windows, PowerShell 7.6.6, Go 1.26.6, Helm 3.9.1 and local Kubernetes
1.30.3 envtest assets. Each deployed scenario compiled the installer and packaged
the core chart into temporary storage without rebuilding dependencies. Namespace,
kube-system and resource identities were pinned. Release/control/workload/authority
names used a random scenario suffix.

The preloaded guard image was `kognition-sg03-guard:20260924-acceptance-01`, local
Docker image ID
`sha256:40c780a8de2ed68c7dcf177340744291f55a0154cee77ebbd6318e9af8533665`,
created `2026-09-24T13:47:31.416730828Z`. The legacy fixture used
`busybox:1.37.0`. The reduced values disabled optional memory service/tool gateway
and cert-manager, used a prepared self-signed TLS Secret/CA and preloaded images,
and left admission and supported stdio enabled. Saved activation was true;
applied and stored activation remained false.

## Deployed Matrix

All six cases in the [deployed fixture](../../../../internal/traceability/installation_deployed_integration_test.go)
passed in individual or paired runs. They were not rerun as one combined matrix.

| Scenario | Release | Result and duration |
| --- | --- | --- |
| Fresh install | `sg05-07380049` | PASS with legacy case in 394.24s; revisions 1 and 2 inactive |
| Legacy upgrade | `sg05-a92c5a73` | PASS in the same run; revisions 1, 2 and 3; held finalizer blocks before shutdown/revocation |
| Failed install | `sg05-7ac797e8` | PASS 204.75s; quota rejects revision 1, reviewed retry 2, ordinary upgrade 3 |
| Failed upgrade | `sg05-ec049cbf` | PASS 278.42s; old revision 1, quota-rejected 2, reviewed retry 3, upgrade 4 |
| Certificate recovery | `sg05-29bc8966` | PASS 317.79s; Helm success followed by verifier timeout, TLS repair, explicit recovery and revision 2 |
| Interrupted verification | `sg05-788ad673` | PASS 178.31s; real installer process kill after Helm exit, pending verification recovery and revision 2 |

The certificate-recovery claim was `53638fd2-c792-47c7-845d-8b7731784584`.
The interrupted-verification claim was `ed9e0970-08db-4045-9a12-214c3e03ebec`,
journal `kognition-install-de38e4f0e38397c29834c4f5ed8d7cbd`. Recovery confirmed
verified-inactive retirement; repetition confirmed claim absence and matching
immutable receipt. A subsequent ordinary upgrade used a new claim.

The fixture verifies wrong claim UID and missing settlement confirmation cannot
mutate the claim. The ordinary upgrade checks an unrelated ConfigMap's UID,
resourceVersion and contents. Cleanup checks exact release annotations and uses
UID-preconditioned deletes; successful runs reported no cleanup errors and waited
for their tracked namespaces to disappear. This is not an inventory of all
resources left by earlier interrupted development runs.

The legacy Deployment sleeps with a minimal ConfigMap-read grant. Its held Pod
finalizer proves ordering, not actual production cleanup. No production controller
was activated. Quota failures prove admission-bootstrap failure recovery and
failed/current/last-deployed inventory review without restoring legacy privileges.

## Reproduction

Prepare the exact task-owned cluster, compatible local images and envtest assets
first. Do not change the test's cluster allowlist to target a shared environment.

```powershell
$env:KOGNITION_INSTALL_DEPLOYED_ACCEPTANCE = 'true'
$env:KOGNITION_ADMISSION_ISOLATED = 'true'
$env:KOGNITION_ADMISSION_KUBECONFIG = Join-Path $env:TEMP 'kognition-admission-security-20260923.kubeconfig'
$env:KOGNITION_ADMISSION_EXPECTED_VERSION = 'v1.31.1'
$env:KOGNITION_ADMISSION_GUARD_IMAGE = 'kognition-sg03-guard:20260924-acceptance-01'
$env:KOGNITION_ADMISSION_STREAM_IMAGE = 'busybox:1.37.0'
go test -tags=integration ./internal/traceability -run '^TestDeployedInstallationCLI/(fresh|legacy)$' -count=1 -timeout=15m -v
go test -tags=integration ./internal/traceability -run '^TestDeployedInstallationCLI/failed-install$' -count=1 -timeout=15m -v
go test -tags=integration ./internal/traceability -run '^TestDeployedInstallationCLI/failed-upgrade$' -count=1 -timeout=15m -v
go test -tags=integration ./internal/traceability -run '^TestDeployedInstallationCLI/certificate-recovery$' -count=1 -timeout=15m -v
go test -tags=integration ./internal/traceability -run '^TestDeployedInstallationCLI/interrupted-verification$' -count=1 -timeout=15m -v
```

## Focused Regression And Quality Gates

| Check | Result |
| --- | --- |
| Unfiltered installer library and CLI race suites, integration tag enabled | PASS; 11.257s / 25.987s after gzip-close error handling fix |
| Exact journal API and entrypoint race run | PASS; 38.695s; eight Make/PowerShell recorder cases, safety assertions, real API journal/settlement |
| Journal API race rerun after gzip-close fix | PASS; 18.168s |
| Pending Helm record regressions after cleanup fix | PASS; invalid ownership, status, manifest, encoding, rollback and unreviewed deletion rejected |
| Scoped installer lint | PASS; zero issues |
| Scoped installer vulnerability scan | PASS; no vulnerabilities found |
| Installer and acceptance-fixture Go formatting | PASS; `gofmt -l` returned no files |
| Editor diagnostics for the three traceability installation fixtures | PASS; no errors reported |
| Tracked SG-05 patch whitespace | PASS; `git diff --check` |
| Protected governance dependency archive | Unchanged; SHA-256 `541F9336B2AAAD1D1D834D068CA7C2B49E656917D17C6B411BCE7CFA063BC16F` |

```powershell
go test -race -tags=integration -count=1 -timeout=5m ./internal/k8s/installation ./tools/kognition-install
go test -race -tags=integration -count=1 -timeout=5m ./internal/traceability -run '^(TestInstallationJournalAPI|TestInstallationEntrypointCommands|TestInstallationEntrypointsRequireSafeWorkflow)$' -v
golangci-lint run ./internal/k8s/installation/... ./tools/kognition-install/...
govulncheck ./internal/k8s/installation ./tools/kognition-install
```

An earlier overbroad integration filter also selected
`TestIndependentAttachmentAdmission`. Three assertions failed because RBAC denied
`pods/attach` before the intended guard decision. That run is not passing evidence;
the exact SG-05 rerun above passed. Attachment acceptance remains SG-06/07, and
full-candidate regression clearance remains SG-11. Existing broader integration
lint findings are recorded in the [backlog](security-gate-backlog.md).

Markdown lint passed for the installer/activation contracts, ADR, acceptance report,
backlog, deployment and bootstrapping guides with the existing `MD013` exception.
Whole-file checks on the README, root quickstart, tutorial and sample guides still
report pre-existing formatting findings. Sample/tutorial findings do not overlap
the edited installation lines; they were not suppressed or repaired as unrelated
cleanup. Local file-link validation checked 184 destinations before adding the
report backlink: one existing tutorial link to `samples/governance/identity.yaml`
is unresolved and was confirmed present in the index before these edits. All
other checked file destinations resolve; fragment IDs were not validated.

## Operator Parity And Limits

The [README](../../../../README.md), [root quickstart](../../../../QUICKSTART.md),
[deployment guide](../../../../deploy/QUICKSTART.md),
[bootstrapping guide](../../../bootstrapping.md) and
[activation contract](../../../features/knowledge-provider-execution/activation-transaction-contract.md) now point Kubernetes
installation to the reviewed installer. Make and PowerShell require plan/freeze,
do not load credentials/images/samples, and cannot activate from inherited values.
The manual path uses the same CLI. Standalone Helm remains a separate workflow.
Automatic Kubernetes quickstart/fresh paths are blocked. Enrollment, credentials,
CRDs, certificates, sample writers and activation require separate review.

Sample installation prerequisites no longer instruct direct core-platform Helm
changes or automatic image loading. The tools sample requires egress in reviewed
values; the umbrella sample and tutorial explicitly identify combined
platform/producer deployment as unsupported during guarded installation. These
documentation corrections do not certify post-activation sample execution.

The operator contract covers fresh install, legacy drain/migration, failures,
retry, exact pending Helm storage settlement, dedicated namespace enrollment,
credential exposure, immutable retirement intent and cleanup. It does not permit
automatic rollback or finalizer deletion. API-injected lost responses and pending
Helm state are distinct from the real process kill, which happens only during
read-only verification after Helm has exited. Neither establishes settlement of
arbitrary in-flight requests; recovery requires independent settlement evidence.

SG-07 enforcing-CNI/full stdio, SG-08 production lifecycles, remaining SG-09
disposition review and SG-10 through SG-13 candidate gates remain open. Local
evidence does not approve production TLS lifecycle, all default components,
credential compromise recovery, cloud providers, actual activation or publication.
No archive refresh, scanner suppression, staging, commit, push or cloud operation
was performed for SG-05.
