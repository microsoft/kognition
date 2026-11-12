# SG-08 Local Producer Lifecycle Acceptance

- Status: SG-08 complete locally for the tested profile; production activation remains BLOCKED.
- Date: 2026-09-25.
- Scope: actual production producer code against task-owned Kubernetes v1.31.1 with independent admission and exact rendered controller grants.
- Contract: [SG-08](security-gate-backlog.md#sg-08-prove-restricted-production-producer-lifecycles), [ADR-0016](../../../adr/0016-installation-trust-boundary.md).

The combined deployed acceptance passed in **154.283s**, including restricted
ingestion, deliberate root-write failure, provider binding and cleanup, authenticated
Agent dispatch, service revision transitions, and Secret rotation/deletion. The final
provider fixture was rebuilt after lint fixes and passed focused deployed acceptance
in **98.110s**. Required negative and recovery cases pass in the scoped unit/race suites.
No lifecycle required bypassing admission or weakening the ingestion sandbox.

This is local lifecycle closure. SG-09 through SG-13, final-candidate packaging,
whole-repository quality clearance, and production activation remain outstanding.

## Execution Boundary

- Cluster: `kognition-sg07-20260925`; explicit kubeconfig `$env:TEMP/kognition-sg07-20260925.kubeconfig`; API `https://127.0.0.1:63564`.
- Profile: single-node IPv4 Kubernetes v1.31.1, Cilium 1.17.8; network enforcement evidence remains in [SG-07](sg07-local-acceptance.md).
- Combined run: `24ffd718`; namespaces `sg03-control-24ffd718` and `sg03-work-24ffd718`.
- Producer identity: `system:serviceaccount:sg03-control-24ffd718:sg03-24ffd718-controller`.
- Temporary grants: rendered controller ClusterRole, installation/workload Roles, and installation quota-ledger Role, without ad hoc producer permissions.
- Current producer source runs in the local test process using that identity. Workloads, admission guard, PostgreSQL, model fixture and provider execute in the cluster. This is not a deployed controller-manager process restart test.
- Both successful runs verified controller replicas zero, unchanged bootstrap grants, and absence of both temporary namespaces after cleanup.
- No shared cluster mutation, global context change, cloud operation, commit, push, branch creation or protected archive regeneration occurred.

## Acceptance Matrix

| Requirement | Discriminating evidence |
| --- | --- |
| Restricted ingestion | UID/GID 1000, read-only root returning EROFS, writable `/tmp`, readable input, absent service-account token; actual ingestion persisted one document/chunk with eight-dimensional embedding in local PostgreSQL |
| Retry and generation | Generation 1, same-generation retry, generation 2; reconstructed launchers preserved the existing Job UID; Reset confirmed individual Job/Pod/input Secret absence and repeated Reset succeeded |
| Non-root failure | Attempted `/app` write failed with EROFS; production launcher reported `IngestFailed`; Reset removed failed Job, Pod and input |
| Binding publication | Real Knowledge SDK server over mTLS committed known content; persisted binding matched generation, provider, class UID/generation, configuration UID/digest/GVK/name, Delete policy and operation identities |
| Provider finalizer | Reconstructed production reconciler wired the real private cleaner; provider confirmed exact committed-operation cleanup; KB, Job, Pod and input Secret became absent |
| Agent dispatch | Actual authenticated production `Dispatch` and `Status` completed session `23be748b-6017-436c-ac17-a8e447949458` against the deployed Agent runtime and local model fixture |
| Service lifecycle | Revisions 1 and 2 became Ready; published Service UID `1ec3b234-3d5f-43e8-a897-b5e0f6fe4f72` remained stable; in-cluster DNS readiness succeeded; finalizer removed the endpoint |
| Source credentials | Custom installation Secret name `sg08-controller-credential`; rotation updated copied value without changing copied UID; source deletion blocked new revision creation while retaining materialized credentials |
| Secret boundary | Installation unrelated GET denied; list/watch denied in installation and enrolled namespaces; enrolled GET intentionally allowed by the approved credential trust-domain contract |
| Namespace boundary | Installation-namespace Agent/revision provisioning rejected before any creation |
| UID and ownership | Replacement Job/Secret/Deployment/Service/policy and stale Pod/completion tests preserve foreign identities; deletion uses UID preconditions; pending foreground Deployment deletion retains revision policies |
| Failure recovery | Typed Forbidden/Conflict/Timeout Reset failures preserve resources; a fresh launcher retries successfully; deterministic concurrent Reset passes with no resources remaining |
| Cancellation and stale state | Caller cancellation, interrupted stdio transport, bounded cleanup failure, held finalizers, stale completion identity and mismatched provider binding regressions pass |
| Restart recovery | Fresh launcher/dispatcher/store/reconciler objects reuse Kubernetes state and complete retries/finalizers; no claim of live manager process restart, informer restart or provider restart durability |

Independent admission cannot restrict Secret GET. The SG-08 Secret requirement is
interpreted under the explicitly approved enrolled-namespace trust domain, not as
an assertion that unrelated enrolled Secrets are unreadable.

## Fixes And Regression Evidence

- Service ownership checks reject foreign published Services before mutation/deletion; dispatcher cleanup uses UID preconditions.
- Revision cleanup waits for foreground Deployment absence before deleting its network policies.
- Ingestion completion honors cancellation and requires the expected successful Job-owned ingester Pod; stale provider confirmations cannot advance committed state.
- Stdio timeout normalization and fresh bounded cleanup contexts retain policy until workload deletion settles.
- The first combined run failed in **180.328s** because Agent cleanup incorrectly required zero Services across the namespace, including the unrelated provider fixture. Cleanup now checks recorded Agent Deployment/Service identities, rejects replacement UIDs, and still requires Agent Pod absence. The identical combined command then passed.
- Provider fixture lint fixes use typed protobuf cloning and a context-aware listener. A separate image tag and deployed rerun validate those final source changes.
- The final stdio cleanup type-assertion guard passed focused race tests after its edit.

## Commands And Results

The commands and image identities below record the historical September profile.
For the current pinned Go 1.26.10 fixture and separate-cluster credential
boundary, see [the later qualification](local-release-gitops-qualification-2026-10-22.md#isolated-agent-and-provider-follow-up).
Do not retarget this standalone mutable-credential fixture at an existing
qualified installation or widen its webhooks to admit unmanaged namespaces.

The optional current `autogen-runtime` child requires
`KOGNITION_ADMISSION_PRODUCERS=true` and a reviewed immutable
`KOGNITION_ADMISSION_AUTOGEN_IMAGE`, alongside the existing producer/profile
inputs. Run its operator-side probe with access to real cluster Service DNS on a
separate disposable cluster; a workstation port-forward does not cover this path.
See [the Kubernetes AutoGen TLS follow-up](local-release-gitops-qualification-2026-10-22.md#kubernetes-autogen-tls-follow-up)
for the measured profile, protected-ledger teardown and remaining gates.

Run from the repository root with the installed Go 1.26.6 toolchain. The deployed
harness requires the task-owned cluster and locally loaded images below.

```powershell
$env:KOGNITION_ADMISSION_KUBECONFIG="$env:TEMP/kognition-sg07-20260925.kubeconfig"
$env:KOGNITION_ADMISSION_GUARD_IMAGE='kognition-sg07-guard:20260925-01'
$env:KOGNITION_ADMISSION_STREAM_IMAGE='busybox:1.37.0'
$env:KOGNITION_ADMISSION_EXPECTED_VERSION='v1.31.1'
$env:KOGNITION_ADMISSION_ISOLATED='true'
$env:KOGNITION_ADMISSION_CNI='cilium'
$env:KOGNITION_ADMISSION_PRODUCERS='true'
$env:KOGNITION_ADMISSION_INGESTER_IMAGE='kognition-sg08-ingester:20260925-01'
$env:KOGNITION_ADMISSION_DATABASE_IMAGE='pgvector/pgvector:pg16-kind'
$env:KOGNITION_ADMISSION_AGENT_IMAGE='kognition-sg08-agent:20260925-01'
$env:KOGNITION_ADMISSION_PROVIDER_IMAGE='kognition-sg08-provider:20260925-01'
go test -tags=integration ./internal/traceability -run '^TestDeployedAdmissionPreflight$/^production-ingestion$' -count=1 -timeout=10m -v
$env:KOGNITION_ADMISSION_PROVIDER_IMAGE='kognition-sg08-provider:20260925-02'
go test -tags=integration ./internal/traceability -run '^TestDeployedAdmissionPreflight$/^production-ingestion$/^provider-binding$' -count=1 -timeout=10m -v
```

| Check | Result |
| --- | --- |
| Combined deployed command above | PASS 154.283s; root failure 5.07s, provider 13.78s, Agent/service 62.51s |
| Final provider command above | PASS 98.110s; run `9e6111d8`; provider child 13.74s |
| `go test ./internal/k8s/controller ./internal/k8s/dispatcher ./internal/controller ./internal/k8s/knowledge ./pkg/pluginsdk ./e2e/fixtures/sg08-provider -count=1 -timeout=10m` | PASS all five suites; fixture compiled with no dedicated unit tests |
| `go test -race ./internal/k8s/controller ./internal/k8s/dispatcher ./internal/controller ./internal/k8s/knowledge ./pkg/pluginsdk -count=1 -timeout=10m` | PASS all five suites: 25.029s, 15.522s, 11.462s, 4.108s, 5.482s |
| `go test -race ./internal/k8s/controller -run 'Test.*StdioInvocation' -count=1 -timeout=3m` | PASS 10.941s after final cleanup guard edit |
| `go test -tags=integration ./internal/traceability -run '^TestControllerRBAC' -count=1 -timeout=5m` | Earlier scoped PASS 38.725s |
| `golangci-lint run ./e2e/fixtures/sg08-provider/...` | PASS, zero issues |
| Uncapped lint command below | FAIL: 22 findings outside SG-08 edits; retained for SG-11, no suppressions added |
| `govulncheck ./internal/k8s/controller ./internal/k8s/dispatcher ./internal/controller ./internal/k8s/knowledge ./pkg/pluginsdk ./e2e/fixtures/sg08-provider` | Zero reachable vulnerabilities and zero affected imported packages; four advisory-bearing required modules not reached by this scan |

```powershell
golangci-lint run --max-same-issues=0 --max-issues-per-linter=0 --build-tags=integration ./internal/k8s/controller/... ./internal/k8s/dispatcher/... ./internal/controller/... ./internal/k8s/knowledge/... ./pkg/pluginsdk/... ./internal/traceability/... ./e2e/fixtures/sg08-provider/...
```

The 22 remaining lint findings comprise 15 error-wrapping findings in CPI,
authorization, workflow, runtime-approval and existing admission fixture code,
plus seven staticcheck findings in existing integration/dispatcher tests,
runtime-approval lease code and the deployed harness Endpoints API. This report
does not claim package-wide lint clearance or final-candidate security clearance.

## Image Provenance

| Image tag | Observed runtime identity |
| --- | --- |
| `kognition-sg07-guard:20260925-01` | `sha256:97457dfb2d0a9dbb94161493f7b2399bdb611ebac721cfb5efa050e99b843dd5` |
| `kognition-sg08-ingester:20260925-01` | `sha256:e1979be6e693d753c49bcc44a8ae0fefea77697b7536baabe3546e97bc6998a6` |
| `kognition-sg08-agent:20260925-01` | `sha256:79d5dadc4f12fa84204c8e0804c7625098bc6971f4cc9428c344dea31a76be58` |
| `pgvector/pgvector:pg16-kind` | `sha256:8e5355e9ff399a002fa46148399a1ac22fb3e9b2d390f857296e6da6b5559ba1` |
| `kognition-sg08-provider:20260925-01` | `sha256:8fa610d6da402f1370b109f147311d032966f438eb53a67099212284a03b9509` |
| `kognition-sg08-provider:20260925-02` | `sha256:d5fdad325261cfb0ffad14bfae05837b258fec12d123f1bc8733d3bf12aa9380` |

Agent and ingester images overlay current source onto cached dependency images.
Dependency compatibility was checked, but clean ingester dependency builds were
blocked by package-download TLS failures. These runs certify the observed local
images, not clean dependency builds. The guard image is the SG-07 artifact; current
producer fixes execute in the test process. Final provider image build and load:

```powershell
docker build --quiet -f e2e/sg08-provider.Dockerfile -t kognition-sg08-provider:20260925-02 .
kind load docker-image kognition-sg08-provider:20260925-02 --name kognition-sg07-20260925
```

## Preserved Evidence

Raw outputs are retained privately and excluded from the current published tree.
The hashes below identify the historical receipts, not current-candidate passing
results. The original source reports are not rewritten or relabeled as passing.

| Raw output | SHA256 |
| --- | --- |
| Combined acceptance, private receipt | `1F832644271147B94EEC35AF0E791AE724EAA48DEF7330AC511A8A3CB359F294` |
| Reproduced cleanup assertion failure, private receipt | `58B1FD412E1608D8BC241A99A6E291813642D1D34C8D398FA3B7BE7680016293` |
| Final provider acceptance, private receipt | `ABA3CCA09CC4396091730248C6C3DB2ED6C668254570E2CA82952B7314EC890D` |
| Uncapped scoped lint, private receipt | `17385970E47CA688443F83D5AF58EE40D5ADD997062245CBCF87A3B23FAE7066` |

Final provider cleanup confirmed Job `628db845-60cc-45ea-8a2e-31a9016990e4`,
Pod `71307dfb-2281-415b-b6de-e0a8d267c76e`, and input Secret
`daee2fc7-1ca7-44c4-82ce-e20889fd23bb` absent. The exact backend operation was
`sg03-work-9e6111d8/kb-ingest-sg08-provider-1`, committed at 18:46:53 UTC and
cleaned at 18:46:57 UTC. Namespace deletion was additionally confirmed.

## Quality Disposition And Limits

- C1/C2: local SG-08 requirements and ADR-0016 trust boundaries are met by the matrix above; no new permission or sandbox exception was introduced.
- C3/C4/C6: scoped suites, race checks, deployed positives and explicit failures cover lifecycle behavior; package-wide lint remains a recorded SG-11 blocker.
- C5/C8/C9: existing Kubernetes client, SDK, ownership and fixture patterns are reused; no dependency was added for SG-08. The dependency scan is scoped, not repository-wide clearance.
- C7: this report and the backlog provide traceability; commits and branches were not authorized.
- The local provider uses an AzureSearchBackend schema as a configuration fixture and makes no Azure calls. It is not a durable production provider or a cloud acceptance test.
- Legacy pgvector finalization alone does not prove external deletion; the separate SDK provider case proves exact-operation cleanup.
- Authenticated dispatch uses an exact-Pod port-forward; published Service DNS is tested for readiness, not an authenticated end-to-end invocation through DNS.
- Controller-runtime emitted a logger-initialization warning. Windows port-forward shutdown emitted a connection-aborted diagnostic; the actual cleanup assertions and commands passed. Both messages remain in raw evidence.
- Other Kubernetes/CNI profiles, clean image builds, live manager restart, certificate lifecycle, production availability and final staged-tree attestation are not certified here.