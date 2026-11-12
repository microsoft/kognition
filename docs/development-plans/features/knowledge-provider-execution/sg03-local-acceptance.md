# SG-03 Local Admission Acceptance

- Status: SG-03 complete for the working tree; overall security gate BLOCKED.
- Date: 2026-09-24.
- Scope: bounded behavioral preflight, deployed guards, actual typechecking and cleanup on task-owned local clusters.
- This report is incremental acceptance evidence, not approval to activate, deploy or publish a candidate. SG-04 owns freshness, replay prevention and evidence-bound activation.

## Targets And Provenance

| Target | Server | Explicit endpoint | Acceptance |
| --- | --- | --- | --- |
| `kognition-sg03-minimum-20260924` | v1.30.3 | `https://127.0.0.1:50797` | Deployed guard, six policies, kubelet stream, actual typechecking |
| `kognition-admission-security-20260923` | v1.31.1 | `https://127.0.0.1:53168` | Actual six-policy typechecking |

The harness requires `KOGNITION_ADMISSION_ISOLATED=true`, an explicit kubeconfig,
an exact expected server version and a loopback HTTPS endpoint. Shared clusters
and global context were not changed. Go 1.26.6 ran the checks. The deployed test
used the production `k8s-controller` Dockerfile target, not a mock webhook server.

| Image | Recorded identity |
| --- | --- |
| Guard tag | `kognition-sg03-guard:20260924-acceptance-01` |
| Guard OCI index and observed runtime digest | `sha256:40c780a8de2ed68c7dcf177340744291f55a0154cee77ebbd6318e9af8533665` |
| Guard platform manifest | `sha256:193064000b2d8b9a212eea1102692c31b713a95f6d4483803b7002bcfffcee87` |
| Guard image config | `sha256:159282764e9362658df731b66f5f40c236cb9d9d094d86f67fa50f8a717d6507` |
| Builder, `golang:1.26.6-alpine` | `sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83` |
| Runtime, `gcr.io/distroless/static-debian12:nonroot` | `sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab` |
| Stream image, `busybox:1.37.0`, OCI index | `sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e` |
| Stream observed runtime image ID | `sha256:30ecbe1509090b38ef3260f3609d64d8261230464eaa4d235901db496e9647fb` |

## Deployed Outcomes

The passing deployment used release `sg03-b2e55267`, installation namespace
`sg03-control-b2e55267` and workload namespace `sg03-work-b2e55267`. Controller
impersonation was exactly
`system:serviceaccount:sg03-control-b2e55267:sg03-b2e55267-controller`, with its
service-account and authenticated groups. The separate guard identity was
`sg03-control-b2e55267/sg03-b2e55267-admission-guard`.

All policy names and binding names have prefix
`sg03-b2e55267-sg03-control-b2e55267-`. Fixture suffix is
`70405e82-3df0-42bb-bd39-704bcb8d2ad4`. State and leases are in the installation
namespace; the other four are in the workload namespace.

| Policy suffix | Object name prefix before fixture suffix | Object UID | Allow / exact deny / cleanup |
| --- | --- | --- | --- |
| state | `kognition-cost-` | `56093125-4f0d-4a49-b36b-da9ba6539a27` | PASS / PASS / confirmed |
| leases | `kognition-tool-grant-rate-` | `c30dc7ab-71da-4cf4-b2fe-4b5c00e085b5` | PASS / PASS / confirmed |
| credentials | `kognition-admission-probe-` | `1b93e911-7591-4903-9fe1-4fed789f821e` | PASS / PASS / confirmed |
| workload | `kognition-admission-probe-` | `c673642f-a210-4332-ada7-bf3b9087687c` | PASS / PASS / confirmed |
| services | `kognition-admission-probe-` | `be30f259-974f-408f-a640-28efab32d19f` | PASS / PASS / confirmed |
| network | `kognition-admission-probe-` | `33c9db0b-fc94-4e61-83c6-93afbb9c37e4` | PASS / PASS / confirmed |

| Deployed scenario | Discriminating result |
| --- | --- |
| Sandbox through Service/TLS | Restricted Pod allowed; hostPID Pod denied by `sandbox.guard.kognition.dev` |
| Wrong CA | TLS rejected with `certificate signed by unknown authority` |
| Wrong Service DNS | Certificate valid for actual guard Service, not `wrong-b2e55267.sg03-control-b2e55267.svc` |
| No endpoints | Fresh `empty-b2e55267` Service failed with connection refused |
| Missing invocation policy | `attach.guard.kognition.dev` denied with `cannot verify attachment NetworkPolicy` |
| Guard API permission removed | Exact attach webhook denied with `cannot verify attachment Pod` |
| Real guard API TLS stall | Exact attach webhook failed with `context deadline exceeded`, within the asserted 12-second bound |
| Recovery | Restored current fields and verified the actual Service path after each fault |
| Actual kubelet stream | Exact stdin/stdout exchange `sg03-b2e55267\n`, empty stderr, via SPDY under controller impersonation |
| Final bootstrap | Controller replicas remained zero; temporary grants removed; original reviewed grants unchanged |

The stream Pod `kognition-stdio-invocation-b2e55267-ks4l7` had UID
`eb87cc5c-4349-4f34-8a23-0aafbcdce828`; its Job UID was
`47be8e06-a47f-415a-9532-b36a800b8624` and invocation policy UID was
`48170232-91c5-40c6-8f1f-e0e48bb397d1`. The restricted credential-free fixture
used no service-account token. This proves a real stream, not enforcing CNI.

Both guard Pods reported the runtime digest above: suffix `2m2bg`, UID
`aa615a1c-0af7-480e-b084-a106c39b2883`, and suffix `5kp2j`, UID
`668970fe-47f0-40fe-ae90-e7b5ff37f847`, under ReplicaSet prefix
`sg03-b2e55267-admission-guard-784bdb485c`.

Cleanup confirmed absence of all tracked fixtures, TLS Secret, bootstrap
resources, policies/bindings, temporary attach Role/RoleBinding, stalled-API
Pod/Service and both namespaces. Deletes used UID preconditions and bounded GET
confirmation. Workload namespace UID was `2895118f-80a9-4da8-9683-d3d611721df9`;
installation namespace UID was `ccd01eb3-eb52-4662-a8ac-d6bc4167e914`.

## Actual Typechecking

Every row had generation 1, observedGeneration 1, zero expression warnings and
GET-confirmed deletion. Names below follow prefix `security-test-kognition-control-`.
These results came from actual controller-managers, not injected status.

| Server | Name after prefix | UID |
| --- | --- | --- |
| v1.30.3 | leases-72881715 | `4e67ad02-1aa4-4c8f-a0d8-bbc4517ef406` |
| v1.30.3 | services-7ad26bb2 | `646c8c43-49b3-4124-b171-5b541bc3dea0` |
| v1.30.3 | network-fc04d650 | `5e49b2f9-c20d-4c02-a6ad-1cc125072b18` |
| v1.30.3 | state-dda53952 | `2393b306-2303-4c23-a34c-1367bc6af706` |
| v1.30.3 | credentials-f87bdc59 | `19a6de51-65bf-4166-bdda-9a5c3823a5f6` |
| v1.30.3 | workload-3e8c363e | `228128a3-918c-43be-9e48-2fad4ed56ffd` |
| v1.31.1 | leases-346363ea | `694d0f72-3e3e-4eea-9ca9-d331de79e34c` |
| v1.31.1 | services-950f012d | `6e79d61c-fcb1-4f92-bdb9-7986d8984a36` |
| v1.31.1 | network-11bb6878 | `372bfcdd-f83c-436d-af76-8fb0ea977db0` |
| v1.31.1 | state-ab266dc3 | `fc14c979-5d37-4cae-891d-da82627ca712` |
| v1.31.1 | credentials-8095e57a | `93b33b96-bab0-47c9-9a2a-ac76ba4ebf92` |
| v1.31.1 | workload-9bde91b4 | `6f895329-0d48-4517-8b9c-b312fcf48e86` |

## Reproduction And Regression Gates

Set `KOGNITION_ADMISSION_KUBECONFIG` to the explicit task-owned kubeconfig,
`KOGNITION_ADMISSION_EXPECTED_VERSION` to its exact server version and
`KOGNITION_ADMISSION_ISOLATED=true`. Deployed acceptance additionally requires
`KOGNITION_ADMISSION_GUARD_IMAGE` and `KOGNITION_ADMISSION_STREAM_IMAGE` above,
available in that cluster. Do not target a shared cluster.

```powershell
go test -tags integration -count=1 -timeout=8m ./internal/traceability -run '^TestDeployedAdmissionPreflight$' -v
go test -tags integration -count=1 -timeout=3m ./internal/traceability -run '^TestControllerAdmissionPolicyTypeChecking$' -v
go test -race -count=1 -timeout=3m ./internal/k8s/activation ./tools/check-admission-activation ./internal/k8s/admissionguard
go test -tags integration -race -count=1 -timeout=5m ./internal/traceability -run 'TestActivation|TestAdmissionBootstrap|TestAdmissionGuard|TestAdmissionFixtureOwnership' -v
```

- Deployed acceptance passed: 68.21 seconds test, 69.181 seconds package. Subsequent changes added tests and formatted Go; production guard behavior did not change after its image build.
- Minimum-version typecheck passed in 3.62 seconds, including cleanup. Its combined command failed an earlier deployed fixture's exact baseline annotation check. The fixture was corrected by marking the expected render before sealing it; production equality was preserved. The separate deployed rerun above passed.
- Selected-version typecheck passed: 3.43 seconds test, 4.342 seconds package, including cleanup.
- Full race suites passed: activation 13.915 seconds; CLI 4.490 seconds; guard 7.575 seconds.
- Focused traceability race gate passed in 62.900 seconds, including real-API defaulting/drift in 32.48 seconds. Envtest uses local 1.30.3 assets and simulates rollout/type-check status only in that fixture.
- Regression coverage includes absent binding, Ignore, Warn, wrong controller match, lost Create response, UID replacement, cleanup failure, exact policy/binding attribution, positive permission denial, missing enforcement, dry-run updates and conflict retry.
- CLI tests cover paired opt-in flags, early failure JSON, success/failure report fields and report-write errors. Report success is not activation approval.
- SG-03 Go files were formatted; focused editor diagnostics were clear. No new dependencies, scanner suppression, archive refresh, staging, commit or push was performed.

## Operator Contract And Limits

Run behavioral preflight only with the externally maintained writer freeze from
the [drain contract](../../../features/knowledge-provider-execution/drain-verification-contract.md):

```powershell
go run ./tools/check-admission-activation --manifest <trusted-render.yaml> --kubeconfig <explicit-kubeconfig> --policy-behavior --writers-frozen
```

Behavioral mode intentionally creates temporary owned fixtures, exact-name UPDATE
grants and UPDATE-only dry-run protection, then removes them. It does not activate
the controller. Requests are bounded to 10 seconds, verification to two minutes,
and independent cleanup to one minute. Successful results require positive allow,
the exact intended policy and binding denial, cleanup, and final structural/drain
verification. Negative probes cannot pass on missing RBAC permission.

Positive Forbidden errors fail immediately, including RBAC propagation delay.
After confirming cleanup and stable RBAC, rerun the entire preflight while keeping
the freeze. This may cause a conservative rejection, never false acceptance.
Conflict retries are bounded and reread the same UID. The CLI emits a timestamped
JSON report with endpoint, version, controller, manifest SHA256, probe UIDs,
allow/deny/cleanup outcomes and errors; invalid arguments return 2 and verification
or output failure returns 1.

Creation recovery uses unique ownership markers and UIDs; AlreadyExists never
adopts an object. A lost response still fails verification even when a recovery
GET finds the owned object for cleanup. If ownership cannot be resolved, inspect
the exact reported marker for a late commit before rerunning; do not delete by
name alone. Cleanup removes grants before fixtures and protections last. A failed
grant or fixture cleanup retains protections and fails the run.

The writer freeze remains an operator prerequisite, not a transactional lock.
SG-06 retains the broader mutation matrix; SG-07 retains enforcing-CNI, additive
policy/race and full stdio operational acceptance; SG-08 retains production
producer lifecycles. SG-09 retains the durable credential trust ADR. Secret GET
is not constrained by admission. SG-04/SG-05 and final-candidate gates remain open.
