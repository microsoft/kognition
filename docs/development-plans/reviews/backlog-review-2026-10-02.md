# Backlog Review, 2026-10-02

- Status: completed evidence-based status review; not a release or production certification.
- Source baseline: `896a58cff52e8e41bdd67296d8a20008f70c35ae`.
- Scope: [product backlog](../../../BACKLOG.md), [design specification](../../../PROJECT.md), linked ADRs, implementation paths, tests and retained acceptance evidence.
- Reconciled inventory: 22 epics, 90 stories, 366 distinct tasks across 370 definition rows, and 304 distinct test IDs across 314 definition rows.

Summary: **58 implemented, 22 implemented with scope corrections, 9 partial or
defective, and 1 acceptance-open story**. These classifications are scoped to the
reviewed requirements and evidence; they are not a product-completion percentage.

## Model Candidate Local Acceptance, 2026-10-13

T-0225 and TC-0183 now have approved deterministic deployed acceptance on one
fresh disposable kind/PostgreSQL target. Run identity is
`9c58b961-436c-4c98-b34e-03bc3373540b`; kube-system UID is
`96a5160d-dbfc-43e4-bcd8-f1d0ced7a8bf`. This is production reconciler code against
real stores, called by a restricted operator test Pod, not a full production
controller-manager rollout. Providers are local HTTP fixtures, not paid services.

| Check | Accepted Evidence |
| --- | --- |
| Migrations and standalone | Normal full PostgreSQL migration runner; signed v1/v2 promotion, failed marker retains v2, known accounting-outage recovery without another inference, token denial with zero calls after quota restoration, unknown 503 held at one call |
| Kubernetes | Fresh resource names and UID-pinned continuation `candidate-aabf94da`; same promotion/rollback/recovery/denial/unknown matrix through the actual API server and ConfigMap accounting stores |
| Exact accounting | Kubernetes asserts four immutable usage events and four exact settlement dispositions, matching quota counters plus one protected unknown ceiling; generic expiry reclaims zero canaries |
| Local regressions | Cost/tier/token exhaustion, overlap, fresh-reconciler recovery, concurrent edits, malformed/oversized/mismatched retained outcomes and version-1 terminal compatibility |

The initial Kubernetes attempt failed before dispatch because StoreAdapter did
not implement protected canary reservations. A public regression reproduced the
missing capability; the existing CAS quota ledger now protects canaries from
generic expiry/settlement and retains permanent exact dispositions. Signed Model
conversion already preserved signatures and was not changed. The restricted
runner uses only control-namespace get/create/update Lease rights for usage
locking. An unsupported unversioned tokenizer alias failed closed; the accepted
image uses `gpt-4-0613` and a frozen offline vocabulary, without runtime downloads.

The original standalone subtest passed even though the combined runner failed.
Only Kubernetes was continued, with fresh Model/Class/quota names and a separate
image, Pod UID and log. The completed standalone run was not replayed. One
deliberate unknown claim/reservation remains in each store. Neither is refunded,
released or treated as completed. Cleanup must retain database/ledger snapshots,
runner receipts/logs and their hash index before removing the exact owned target.

Private evidence root:
`%LOCALAPPDATA%/Temp/kognition-candidate-20261013-a94b210d`. Original
`candidate.log` and continuation `candidate-aabf94da.log` keep separate outcomes;
the earlier pre-dispatch diagnostic failure remains separate evidence.
T-0150 stays parked. This acceptance does not certify arbitrary providers,
production/Helm activation, remote CI or publication, and creates no approval to
replay retired fixtures.

### Final Verification And Cleanup

- Focused candidate and protected-ledger race regressions pass. Five focused real
	PostgreSQL race checks pass for dispatch claims, permanent exact settlement,
	protected reaper behavior and idempotent usage.
- Full `golangci-lint run ./...` and integration-tagged acceptance lint report
	zero issues. Race-enabled complete traceability/securitygate suites pass.
- Header checking reports 1429 tracked sources and zero missing notices. Required
	FR/NFR mappings pass at 110/110; NFR-plan validation passes 13 rows and 18
	mappings. Scoped patch whitespace checking passes.
- The full cross-package race/coverage attempt exceeded its bounded deadline.
	The standard package-instrumented full retry also timed out; its candidate and
	Kubernetes accounting packages passed, but `TestProviderHost_Registration`
	asserted Ready during the existing asynchronous Connected/Unknown interval.
	Partial profiles and earlier 80.6% evidence are not a current green gate.
- At this initial receipt T-0225 remained open for final verification. TC-0183
	had the qualified local deployed acceptance above. No unrelated source was
	changed to hide that result; the subsequently authorized repair is recorded
	in the closure follow-up below.
- Outcome-reviewed cleanup verified six dispatch claims, five known results,
	four usage rows and four settlement dispositions per backend, with one
	protected unknown retained in each. All runner Pods were terminated; the
	continuation UID/image matched its receipt. No claim was altered or refunded.
- Database dump, Kubernetes ledgers/resources, original/diagnostic/continuation
	logs, Pod and run receipts and a hash index are retained privately in
	`retained-4e1a746fa507413789812144e2b9b876` under the evidence root. The exact
	cluster was deleted using its explicit kubeconfig and absence was verified at
	`2026-10-13T22:17:26.6950829Z`. The global context was not changed.

| Retained Artifact | SHA-256 |
| --- | --- |
| Original combined log, standalone passed and Kubernetes failed | `90e3b5b341c82e503fc974f84894b2f85668fd84bc1a36bafff8a7d5c47bc9f5` |
| Passing Kubernetes continuation log | `043027b271ab65d715350a817ef7d7906a830bd12e9d653424a9d7baf76db550` |
| Kubernetes ledgers | `f82a173c18e7e524262d67367f3ef915fa58c0ba7594612d37a39d95581dfa0e` |
| PostgreSQL dump | `10dffab8e1f876624af18273b5d06343beb4c0bc8ce4db16a9151dbe33fd0af3` |

### Closure Follow-Up

The user explicitly approved repairing the existing CPI readiness test.
`TestProviderHost_Registration` now waits for the actual Connected/Ready snapshot,
preserving every metadata/health assertion and its ten-second deadline. Twenty
race-enabled repetitions pass; production readiness behavior is unchanged.

The next full run cleared CPI but exceeded the three-minute package cap in
PostgreSQL. Ownership checks identified its abandoned private test database;
normal `pg_ctl` shutdown stopped only that exact process and retained its files.
Verbose full PostgreSQL acceptance passed in 350.318 seconds. Its gateway-start
tests build four real commands and consumed about 240 seconds, rather than
deadlocking. No PostgreSQL source/test or repository timeout default was changed.

Final complete verification uses an eight-minute local package cap, four package
workers and the unchanged thirty-minute overall deadline. Qualified module-wide
coverage counts imported code exercised by other package tests and unions
duplicate coordinates; only generated sources are excluded by the existing
checker. The earlier complete per-package profile measured 79.3% and failed the
same gate; it is not relabeled as a passing receipt.

- Complete `go test -p=4 -count=1 -timeout=8m -race
	-coverpkg=github.com/kognition-ai/kognition/... -coverprofile=<private> ./...`
	passes in 1423.8 seconds; PostgreSQL passes in 420.924 seconds.
- Canonical authored Go coverage is 80.6%, above the unchanged 80% minimum.
	Private profile is
	`%LOCALAPPDATA%/Temp/kognition-candidate-qualified-7fc53373d0c04f8bb64a5bd7861aafaf.out`.
- Refreshed full correctness lint reports zero issues. Existing optional/tagged
	acceptance skips retain their original scope; this is not an all-profiles or
	remote-CI receipt.

T-0225 and US-0063 are closed for the approved deterministic local profile, with
TC-0183 accepted as recorded above. Cleanup and retained unknown claims remain
unchanged. Paid-provider, production/Helm activation, release and remote CI
approval remain separate; T-0150 stays parked. No retired fixture was recreated,
and no commit or push was performed.

## Implementation Follow-Up, 2026-10-05

The review findings below describe the retained October 2 baseline. Current
changes are locally reviewed source and are not release or deployment certification.

| Area | Current implementation | Remaining completion boundary |
| --- | --- | --- |
| Traceability and CI | Decorated story IDs, duplicate parent unions and unknown-story rejection repaired; fallback and activation fixtures repaired; reviewed Trivy version pinned | Remote CI still needs an enabled runner; duplicate-ID migration and semantic evidence corrections remain |
| Native checkpoint | Versioned bounded token includes full conversation and tool bindings; fresh adapter resume and busy-turn fencing tested | Legacy history-less tokens require migration; tokens contain sensitive conversation data |
| Artifact signing | Operator-pinned Sigstore verification, exact offline payload CLI and chart/Compose trust configuration tested | Independent remote artifact digest/provenance and release/deployment certification remain |
| AutoGen | Real SDK HTTP worker passes bound-principal v0.1 conformance, empty/completed checkpoint restore and namespace rejection; checkpoint-read and restored-handle races repaired | Live-provider and both-mode deployed acceptance remain; non-native CPI classes now fail explicitly before provider contact |
| Release coverage | Fifteen production Go stages including pgvector and workload proxy, plus native/AutoGen worker profiles, use canonical repositories with legacy aliases; all three chart artifacts are covered | Multi-platform image builds/publication and release approval were not executed; pgvector chart remains configuration-only |
| Model versioning | Signed candidate promotion/routing/rollback now includes durable claims, idempotent usage/audit accounting, restart fencing and concurrent-edit protection in both controller assemblies | Deployed migration/rollback and complete quota/recovery acceptance remain |
| SPIRE | Real issuer issuance/rotation and exact-peer mTLS passed with an actual AutoGen Python turn through the Go loopback transport; four gateway commands retain live Agent-UID checks | Protected enrollment/socket mounts, generated Kubernetes sidecar wiring, resource-status propagation and cross-mode rollout acceptance remain |

The user chose explicit provider/endpoint/model-ID/version-digest bindings for
canary promotion and Agent UID-bound SPIRE identities on October 5. These choices
resolve those questions; they do not complete the remaining integration work.

Verified on October 5:

- Repository-wide `go test -race -p 2 ./...` passed with bounded package concurrency, including real PostgreSQL tests.
- Complete Python agent suite: 152 passed, zero skipped in the isolated SDK env and refreshed non-root Linux artifact.
- Core Helm regression suite: 488 passed, zero failed after repairing its stale dedicated-webhook selector fixture.
- Reachable Go vulnerability scan for auth/gateway/admission/CLI: zero reachable or imported-package vulnerabilities; two unused module advisories remain.
- Optional AutoGen requirements audit: no known vulnerabilities found.
- Pgvector and workload proxy production Docker targets built after the daemon became available; the worker was rebuilt from current checkpoint/profile sources using verified offline wheels.
- Real Go-to-AutoGen-container acceptance passed bound-principal conformance and empty/completed SDK history restore without retained bearer credentials.
- Explicitly authorized disposable SPIRE issuer/agent acceptance passed real UID-bound issuance, rotation, wrong-incarnation rejection and an actual Python SDK turn through the loopback proxy to an mTLS fixture; fixture resources were removed.
- Canary purpose migration/idempotence passed against PostgreSQL; immutable receipt roundtrip, forged-writer and deletion denial passed against a real Kubernetes API server. No installed-cluster or live-database change was made.
- Final repository-wide Go race run passed after checkpoint/restore, cancellation-deadline, activation and signing-CLI repairs; integrated Go lint reported zero issues.
- The affected transport/runtime/controller/CLI vulnerability scan found no reachable or imported-package vulnerabilities; two unused-module advisories remain.
- Exact source snapshot `b155b94a99227ac72354ed9bd95af224c1f8a602` passed Trivy 0.74.0 secret scanning with zero findings. It supersedes prior code snapshot `036431260221fcc39ea2a62f8efecbb22b4793db`. The temporary index left the user's real index untouched; these final receipt-only edits are not part of that scanner receipt.

The later Go-wide run exposed a duplicated proxy package declaration and an
activation inventory still expecting six policy pairs. Both were repaired;
positive/adversarial activation tests now require seven pairs and reject omission
of the receipt protection. A one-second MCP fixture timed out under unrestricted
package load, then passed five isolated repetitions and the bounded full run.
Remaining tagged/deployed profiles, multi-platform builds, fresh exact Trivy
dispositions and disabled remote runners are not covered by this evidence.

Operational contracts and remaining boundaries:
[runtime binding profiles](../../runtime-binding-profiles.md).

## Review And Completion Reconciliation, 2026-10-06

Scope: actual tracked/new source changes, security and lifecycle controlling
paths, schemas/migrations, chart/release inventory and affected backlog claims.
The October 2 every-story matrix and its counts remain historical evidence; this
section records the current delta. This is a local source review, not a new
production installation or publication approval.

### Confirmed Findings

| Severity | Finding | Repair And Reproducing Evidence |
| --- | --- | --- |
| High | Memory helper reported a pending write as stored and hid terminal/unavailable approval states | Preserve structured gateway approval responses; actual SDK tests stop before reflection; unconfirmed HTTP 202 becomes unavailable |
| High | Delegation helper converted terminal/unavailable exchange or invocation responses into unstructured errors | Preserve barriers before HTTP-error handling; actual SDK tests verify both stages and unchanged invocation/capability identity |
| Medium | Workload proxy forwarded the wrong Trust Focus run header | Use the shared `X-Kognition-Run-ID` constant; mTLS proxy regression proves run/trace headers and authorization survive |
| Medium | SDK model requests omitted the attached W3C trace context | Inject current trace headers into the per-turn model client; actual authenticated SDK HTTP request retains the incoming trace ID |

All four findings were reproduced before repair and passed focused checks after
repair. GET polling was investigated and disproved: current governed clients use
POST retries. Informer read-after-write uncertainty was also disproved: the
manager disables cached ConfigMap/Lease/Secret reads. Neither investigation widened
the gateway allowlist or changed ledger authority.

### Completion Delta

| Story | Current Status | Evidence Boundary |
| --- | --- | --- |
| US-0022 | Implemented for accepted signing profile | Cryptographic canonical-spec verification, trust rotation and exact offline signing; independent OCI/remote provenance remains separate |
| US-0040, US-0041 | Implemented; checkpoint defect repaired | Versioned full history, quiescence, restored tool bindings and cancellation/credit regressions |
| US-0042 | Partial acceptance | Real SDK HTTP worker and public adapter verified; T-0148/T-0150 and TC-0114/TC-0116 retain production mapping/parity acceptance |
| US-0043 | Framework implemented; acceptance partial | Native and actual SDK bound-principal suites verified; TC-0119 production-provider registration acceptance remains open |
| US-0026 | Partial | Real issuer rotation, UID-bound peer validation and SDK loopback mTLS verified; protected enrollment, generated workload wiring and resource-status propagation remain open |
| US-0053 | Partial release/deployment acceptance | Fifteen Go targets and all profiles/charts are inventoried; current multi-arch publication, fresh-install and guarded GitOps receipts remain open |
| US-0054 | Implemented locally; remote enforcement blocked | Local validators exist; no new runner or live PR acceptance was authorized |
| US-0056 | Implemented; parser/mode defects repaired | Real-backlog forward/reverse fixtures and Linux execution preserved |
| US-0063 | Partial acceptance | Signed candidate promotion/routing, durable usage/audit and replay/CAS fencing verified; deployed migration/rollback and complete quota/recovery proof remain open |

The backlog now removes unsupported checked completion on these open rows and
aligns the epic summary with its detailed status. Historical completed IDs and
unrelated deferred scope are retained. Commits use existing task IDs and group
the repaired source without claiming production rollout or remote publication.

### Fresh Local Gates

- `go test -race -p 2 ./...`: passed, including PostgreSQL and current backlog/chart contracts.
- Required module-wide authored Go coverage: 81.0%, above the 80% gate; duplicate source blocks were unioned and only generated sources excluded.
- Integrated `golangci-lint run ./...`: zero issues.
- Python worker: 162 passed with zero skips on the host SDK environment and rebuilt non-root Linux artifact.
- Actual SDK HTTP container: bound-principal conformance and empty/completed checkpoint recovery passed without skips.
- Actual disposable SPIRE issuer: UID-bound issuance/rotation, wrong-incarnation denial and SDK loopback-to-mTLS turn passed; fixture cleanup completed.
- Actual Kubernetes 1.30.3 API-server controller-state admission: immutable canary receipt replay, forged-writer/deletion denial and broader writer boundaries passed without skips.
- Reviewed signing/identity/runtime/controller/CLI dependencies: no reachable or imported-package vulnerabilities; two unused-module advisories remain.
- New/updated review, signing/runtime guidance and decision records: markdownlint passed with the established MD013 allowance.

Earlier core chart assertions and purpose-migration receipts retain their exact
source/profile scope. No full manual security campaign,
fresh production install, multi-platform publication or remote runner gate was
performed or inferred from these local checks.

## Completion Rules

### Reviewed Porting Follow-Up, 2026-10-10

T-0150 remains open and US-0042 remains partial. This section supersedes the
earlier statement that the five-tool mappings and grants were unapproved.
It does not certify unchanged upstream execution, production or arbitrary
provider imports.

The reviewed renderer retains the pinned observability prompt and ordered
five SDK names, descriptions and argument schemas. The invalid 21-tool expert
remains blocked. Explicit helper opt-out keeps the parent at five tools and
the PromQL child at zero. Real SDK unconfirmed HTTP failure tests prove one
request with automatic inference retries disabled.

The real bridge uses namespace-confined Kubernetes reads, Prometheus query,
governed no-retry child AgentJobs and owned Grafana operations. Local tests
cover all eleven Grafana actions, scope checks and nested uncertainty. Those
tests do not substitute for live differential provider acceptance.

The exact worker `194ac8532233a87c5e381d5d6a1522eacb570cc8bd8e17bbf15bb6db91db8908`
passed 186 restricted nonroot Linux tests with zero skips. Bounded startup
probes cover shared and revision workers without changing native steady-state
health checks. The installed matrix passed in 369.763 seconds; external
provider binding was explicitly skipped, and cached ingester dependencies
retain their earlier current-source caveat.

The real Tool Gateway exposed integer-envelope decoding failure: canonical
JSON represents resource version 36910 as an exponent, which cannot decode
directly into `int64`. The repair keeps strict canonical validation, then
decodes the validated original envelope. Signed revision 10/36910 and duplicate
field regressions pass. Owner-approved gateway digest
`b4e7d90ee78a8ddf21b7149ae536b9961bae19db61ae429a2de4ea62576fc5c9`
rolled out on Deployment UID `72a1c055-5ed2-4905-88fa-84bd31da9ff1`.

Five separately approved intentional sessions have failed and remain fenced.
The last session completed real Kubernetes resource reading and API discovery,
but `GeneratePromQLTool` stopped on the gateway's fail-closed host-clock
`TIME_ERROR`. No child model call followed. Final provider counters are ten
requests, nine accepted parent calls, one rejection and zero child calls.
This is partial evidence, not the expected complete workflow/accounting proof.

Explicitly approved real NTP synchronization returned synchronized outer,
kind-node and nonroot-image probes, but did not prevent clock denial during
actual gateway requests. Later syscall evidence below identifies periodic
clock steps; this earlier point-in-time evidence did not explain them.
No manual UNSYNC clearing, clock-health bypass or paid-session retry occurred.
Expired Grafana tokens were replaced only with separate owner approval and
retained credential-free receipts; no raw key enters these documents.

After the owner started Windows Time and successfully resynchronized it,
the service reported Leap Indicator 0 and a current NTP source. A separately
approved fresh preparation used cluster UID
`2532a1df-4f4a-48fb-b09d-996e58639455`, endpoint `https://127.0.0.1:62690`,
and the same reviewed controller, worker, corrected gateway and bridge bytes.
Its bounded read-only Linux clock preflight failed: 1,163 of 6,000 readings
returned `TIME_ERROR` with UNSYNC set. Short healthy probes therefore cannot
certify clock stability. Docker uses the WSL2 kernel; read-only commands to
both WSL distributions also timed out. No application install, activation,
Secret provisioning or session submission followed. The new node is stopped
with its unconsumed plan and failed preflight retained. Shared Docker/WSL
maintenance or a healthy native Linux host remained an unmet environment
prerequisite at that point; the initial preparation did not perform maintenance.

The owner subsequently approved state-preserving Docker/WSL maintenance and
mirrored networking with DNS tunneling and the firewall enabled. All 58 recorded
container identities, running states and restart policies were restored; the
retained porting fixtures stayed stopped. Ubuntu then received real NTP packets
from Cloudflare and Google, resolving the previously unanswered NAT-path probes.
The temporary NTP reply firewall rule did not help and was removed. Existing
`SenseNdr` packet monitoring, the Cisco VPN and managed policies were untouched.
The owner also aligned Windows Time with the same peers; its original source and
the original WSL configuration are retained for reviewed rollback.

Reachable NTP did not establish continuously healthy kernel time discipline.
The separately isolated Linux syscall trace retained 69 of 69 events and showed
`initd` calling `settimeofday` at monotonic times 2972.774526, 3002.779513 and
3032.820139 seconds, intervals of 30.004987 and 30.040626 seconds. Its latter two
steps coincide with the finite probe's UNSYNC transitions. `chronyd` and
`systemd-timesyncd` also called `clock_adjtime` between steps. The trace probe
failed on six of 6,000 readings. This identifies a periodic VM-level clock writer
as a concrete acceptance blocker; it does not certify a supported way to disable
that writer. No supervisor process was killed, vendor binary patched, UNSYNC flag
manually cleared or production clock-health check weakened. Closing T-0150 still
requires a supported environment repair or an independently disciplined Linux
acceptance host, followed by the real workflow and remaining acceptance evidence.

The owner abandoned the dedicated Hyper-V Linux acceptance route on 2026-10-10
and requested shutdown of only VM `81b4952d-13e9-4999-870b-8cd99c046a56` to
release its assigned memory. The guest booted, but captured logs show no DHCP
IPv4 lease or route; DNS failure prevented Chrony installation. Host-to-guest SSH
failed before authentication in both standard and administrator sessions. An
enabled MDM Public-profile block includes port 22, but the exact denying filter
was not identified. The private disk capture and policy evidence remain retained
under `%LOCALAPPDATA%/Temp/kognition-porting-linux-vm-20261010`. Shutdown requires
the owner's administrator session and is not yet verified. Work continues in
the existing environment; clock-health protections and pending acceptance gates
remain unchanged. No new application activation or provider session occurred.

Normal exact-UID producer deletion retained all five failed jobs and seven
claimed encrypted recovery records. Original immutable authority
`porting-startup-safe-20261010154248`, executable, chart and render recovered
the target as `recovered-inactive`. The fixture node and NTP daemon are stopped.
Historical stopped fixtures, reservations, claims, credentials and PVCs remain
retained. The original authority bytes were never overwritten for the gateway
delta.

Current-source verification: 35 focused gateway tests pass; affected CLI,
runtimeauth, dispatcher, model/tool gateways, executor, bridge contracts and
acceptance-driver Go race suites pass; affected-package lint reports zero issues.
Completion remains blocked on the live five-tool session, reference comparisons,
grant revocation, nested uncertainty/no-replay and complete accounting evidence.
No completion mark, all-green claim, push or production/cloud operation is made.

Pre-commit checks against an exported staged snapshot pass the affected Go race
suites, traceability harness compilation, 103 focused SDK tests with zero skips,
Go lint and the redacted staged-file Gitleaks scan. Fresh `govulncheck` reports
12 reachable standard-library advisories against Go 1.26.6; some also affect
`golang.org/x/net` v0.58.0. An unmodified HEAD snapshot reports the same 12
reachable advisory IDs. The scanner lists Go 1.26.9 and x/net v0.60.0 fixes.
These existing dependency risks remain unresolved; local source commits do not
certify release security, deployment readiness or completed live acceptance.

Retained evidence root:
`%LOCALAPPDATA%/Temp/kognition-porting-startup-20261010/all-startup-final`.
Fresh preparation and clock preflight:
`%LOCALAPPDATA%/Temp/kognition-porting-clock-fixed-20261010`.

| Evidence | SHA-256 |
| --- | --- |
| Raw installed matrix | `ddc1a0a59c2ec2a038585fcb604c50e2e6b2566fd251d5f2eb79aeb4e98a6696` |
| Final observed five failed jobs | `563a1022cf66e3fd90d876c349e6468296d63dfba85eaff8db7e78247bb3b033` |
| Final observed provider counters | `e06d98d84cbd6f7484e4bb2545602208ac704ca68b03d28f8ceb3182230f6692` |
| Normal UID-bound drain and retained claims | `5797c010192bd809b1f9992423246f10614f875d641cbee2aa5b3c0a52496ef8` |
| Original-authority inactive recovery | `dd5bb740cf4b7f8b814f1c9e3fabb3d51888d456d6214f69d6f8e73f9a07d560` |
| Fresh preparation plan, not installed | `bb8f21c841d2ecdff2ac887f19d931abe61418c4e349f94fc0334b0c9b9f6aed` |
| Fresh render, not activated | `56a8427a20a44cbbc7428b63b55a9b10b5deebdc545781cb35b5cdc9f161934f` |
| Failed 6,000-sample Linux clock prerequisite | `682e003a28080c1889cf77a4a54a86582426736f1328afecc677401d6d5de087` |
| Mirrored-networking container-state restoration | `634c7725ea68d89d38af14966a841b0db602e9f062a74641bddf3fcb0ebc004e` |
| Isolated time-syscall writer trace | `e6e9769bfe73c3647ae23450208ff7f4b0cc3e581a1ec80efd90484263d31357` |
| Failed strict clock check accompanying writer trace | `107de4a17c5dd8e63ee8619e0556d2dfd90a86bdbcb0d2c8b11077bebc4bf90c` |

### Workload Identity Status Foundation, 2026-10-10

T-0085/T-0086 remain partial. The owner selected platform-managed SPIRE;
Kognition does not install the issuer or alter its enrollment policy.

- The existing verified source now exposes metadata for exactly the current
Agent incarnation. Another namespace, name or recreated UID receives no
evidence. Certificate material remains inside the source.
- Optional AgentIdentity certificate status records UID, incarnation URI,
validity, source generation and fixed readiness reason codes. The opt-in
reconciler checks automatic ownership and current Agent UID before reading
evidence, rejects malformed/expired/future evidence, and polls every five
seconds. Source outage or missing configuration clears readiness or removes
the certificate-status object. Legacy URI/phase behavior is preserved.
- API JSON/deep-copy/conversion and Kubernetes status updates have regressions.
Unchanged status is a no-op; UTC normalization prevents write churn. Signed
generation metadata avoids Kubernetes managed-fields rejection of uint64.
- The Kubernetes test client now registers AgentIdentity's existing status
subresource. The test creates spec-only data, then publishes status separately;
fake-client create behavior is not proof of API-server status filtering.
- Affected local race suites, focused persistence/adapter checks, Helm lint and
repository traceability pass. The umbrella core chart archive is refreshed;
the unrelated governance archive is preserved. Scoped new-code lint reports
zero issues before the final reason-code hardening.
- The installed v0.21.0 generator failed on existing package deep-copy/schema
constructs. The narrow AgentIdentity CRD and deep-copy changes were updated
explicitly and tested. No generator upgrade or unrelated schema repair occurs.

Security review for this local slice: status is observational, never an
authentication or authorization credential. Only a trusted controller-side
reader is eligible for opt-in projection. Stale ownership, invalid validity,
counter overflow and unexpected issuer diagnostics fail closed. No tenant
reporting endpoint, private-key export, issuer installation or dependency is
added. Existing live peer-verification and runtime clock checks are unchanged.

The resumed local slice adds a status handler, mTLS server factory and bounded
fixed-origin client. Only one pinned controller SVID may read metadata; the client
pins the current Agent incarnation and compares reported validity with the actual
TLS leaf. The response permits exactly seven public fields within 8 KiB, rejects
duplicate/unknown/missing/null fields, and never caches successful readiness.
Real local mTLS tests cover rotation, stale UID, forged validity, malformed and
oversized responses, redirect denial and issuer outage. All six affected identity
package race suites pass. No listener is opened by the factory or enabled by
production configuration.

Protected Workload API delivery, production listener/reader registration, issuer
policy acceptance and both-mode deployed verification remain open. The new
constructors are not enabled by production entrypoints.
No deployed SPIRE operation, application activation, commit or push was performed
for this slice. Local tests do not close the full T-0085/T-0086 acceptance scope.

#### CSI and Production Status Integration, 2026-10-11

This update supersedes the earlier unwired-production statement above. The owner
selected the platform-managed `csi.spiffe.io` driver. T-0085/T-0086 remain partial
until installed enrollment and both-mode acceptance are approved and recorded.

- Both production controllers optionally load a strict 64 KiB/128-entry
operator registry at startup. Registrations pin namespace, Agent name, UID and
HTTPS origin; malformed/duplicate bindings fail before issuer contact.
- The proxy executable binds a separate pinned-controller mTLS listener.
Partial listener setup fails closed. Bind failures release earlier listeners;
cancellation shuts down both listeners. The executable readiness probe uses the
existing source-health endpoint with a one-second production deadline.
- Both controller charts mount the Workload API and registry read-only.
Generated shared/revision Agent pods isolate the CSI socket to a non-root,
read-only, capability-dropped proxy and pin the current Agent UID. Unchanged
configuration is stable; reconciliation repairs stale UID metadata, preserves
unrelated sidecars and removes only the proxy/socket on disablement.
- Status Service port 8097 has a separate controller-only runtime ingress rule.
Controller egress targets only managed Agent pods in the configured runtime
namespace. Managed SPIRE settings require enforced networking, a digest-pinned
proxy image and four distinct pinned HTTPS gateway peers.
- Rendered CEL tests accept only the exact protected CSI/mount shape. Writable
CSI, custom attributes, secret references, other drivers, hostPath and agent/init
socket mounts fail. A reproduced CEL equality issue allowed an identical init
container; explicit init/ephemeral mount rejection fixes it.
- All ten affected package race suites and the full local traceability suite
pass. Wider tests caught an early Service-port update bypassing existing owner
annotation repair; the same update now repairs both. Test scheduling tolerance
is separate from the unchanged production readiness deadline. Both local chart
archives were regenerated from current sources.

Security assessment: the implementation preserves metadata-only resource status,
live peer verification, token/privilege restrictions and normal identity-bound
cleanup. Operator registry integrity, independent incarnation attestation, CSI
socket permissions and actual gateway SPIRE serving must be established by the
platform. Multi-replica status represents the authenticated endpoint's certificate.
No issuer installation, deployed-cluster change, dependency, commit or push was
performed in this update. Existing repository-wide publication/security findings
are not cleared by these focused tests.

Remaining acceptance requires a separately approved isolated platform profile:
verify controller/workload enrollment and mounts, both-mode status projection,
issuance/rotation/outage/reconnect, wrong/recreated UID rejection, and deployed
Tool/Memory requests. Use a new dedicated revision when activating SPIRE because
its invocation Service ports are immutable. Protect the registry ConfigMap with
operator-only writes and explicitly replace/restart it for UID changes; no tenant
endpoint registration or implicit reuse is allowed.

#### Scoped Migration Disposition Review

The owner explicitly approved this prerequisite review during installed identity
acceptance. All 67 source/chart migration SQL bodies match their own
`c5837ae^` revision after exact Microsoft/MIT header and line-ending
normalization. No SQL statement or credential literal was added. Source/chart
differences in migrations 001 and 004 are inherited and were not rewritten.
The initial disposition preserved the parallel removal of migration 020's header.
On 2026-10-13 the owner explicitly approved required-header restoration on all
28 affected tracked first-party files. Every result matched the exact header-only
transformation byte-for-byte; functional Go/UI/SQL bodies remained unchanged.
Both migration 020 copies now carry the required Microsoft/MIT comments, and
only the core chart archive was refreshed; the governance archive remained
byte-identical.

The previous reviewed payload digest was
`98c1003239ce09b60501f846692097144bab92f7b7fc1582b039be189fcaf3d3`.
The header-restored 34-key Helm SQL payload has SHA256
`fdfc909439a0319018e2faa7a11b43548940068fd833aeee053e1d29c0baed0d`.
The exact KSV-0109 finding concerns SQL reservation/preparation token identifiers,
not a credential value. Update only the content pin after this equivalence
review; keep ConfigMap identity/labels, exact finding message, key count,
provenance, freshness and other findings fail-closed. A future content change
still requires a new review and now reports its observed digest.

#### Approved Installed Identity Acceptance

T-0085/T-0086 and TC-0068/TC-0069 are closed for the implementation and this
explicitly approved local profile. This section supersedes earlier pending
installed-acceptance statements. Production readiness remains outside scope.

| Boundary | Accepted Profile |
|---|---|
| Fixture ownership | Run `6c580a11-f6b4-4a11-a1aa-a80314e534e6`; kube-system UID `9d7577a2-25cb-48b5-b1b8-cfdf6076895f`; exact one-node kind name, ownership label and private loopback kubeconfig verified before operations |
| Platform | Kubernetes 1.31.1, Cilium 1.18.3, SPIRE 1.15.3 and standard `csi.spiffe.io` driver 0.2.9 |
| Enrollment | Independent PSAT node attestation, verified kubelet certificate, exact Pod UID/container-name selectors; tenant labels alone do not establish enrollment |
| Workloads | Separate non-root/read-only CSI-mounted proxy Pods; no workload hostPath, privilege escalation or API token |
| Status | Shared production reconciler and authenticated registry against real Kubernetes/PostgreSQL stores; validity/generation, rotation, outage/reconnect and normal recreated-UID rejection |
| Gateways | Production Tool/Memory handlers and `ServeGatewayHTTP`, real SPIRE TLS, live stores, stable binding keys and normal Gate 3 preparation/claim/replay accounting |
| Targets | Deterministic local Tool plus read-only Memory search; one Tool effect and one authorized Memory target call per mode; wrong peers and revoked grants denied before targets |

The installed wrong-peer test reproduced a missing Memory peer check: a valid
same-domain gateway SVID could use an Agent capability. Adding the shared
`RequireAgentWorkloadPeer` check after loading the live Agent fixes it. The same
deployed test then passes for both stores; legacy capability-only tests pass.
The earlier legacy-only gateway fixture was corrected to production Gate 3
wiring rather than changing terminal-receipt compatibility behavior.

Normal Kubernetes identity cleanup exposed a last-finalizer/status-write conflict.
The adapter now stops after removing the final finalizer from a deleting object;
the regression and installed Kubernetes recreation continuation pass. No
finalizer or approval status was forced. Standalone recreation succeeded in the
earlier combined run; that combined run failed only on the subsequently fixed
Kubernetes cleanup path, so its evidence is recorded separately below.

| Recorded Run Suffix | Evidence | SHA256 |
|---|---|---|
| `0f60c1a3` | Both-store status rotation, issuer outage/readiness loss and verified reconnect; passed | `3c48f152fe4028cc0c03e17fcdb56bafc6084f349869b266c0332c8834320d86` |
| `217ea7d0` | Normal full-schema migration after approved SQL review and initial-schema baseline/backup; passed | `207101225c5e27cb9f3af6328d06b0b953b3f0653119c03acb6772bc38fe749c` |
| `8a340f0e` | Standalone recreation passed; combined run failed on Kubernetes cleanup, retained without relabeling | `6baf18f22e1d6afefc89d8b79218b3ee0b2bb07d691829bf424377344731915f` |
| `81c731c0` | Kubernetes-only recreation continuation; passed | `d7d4c5653070409ea217dc875e90816bf373ae0ca75e2c1552c7012e449d9221` |
| `77309b82` | Memory wrong-peer security defect reproduced in both stores; failed before the fix | `7fc0c64d3f1dd98484bb5a59bfc6e931de5647a44e25ad39b6aeca5230fe8e78` |
| `4e7ab6e0` | Final both-store Tool exchange/execution/replay, Memory search, real-certificate wrong-peer and revoked-grant checks; passed | `94d2fc11b8503eb4e4eaa0731c52e8557e888fe8d651405eb02780dc0866661c` |

All 12 affected package race suites, full local traceability/security-gate suites
and default/integration-tag scoped new-code lint pass. No dependency was added.
The migration disposition retains its exact-input and negative controls.

The fixture's stale bootstrap bundle caused an initial reconnect failure; only
the authoritative issuer's public bundle was refreshed. Verified reconnect then
passed without weakening certificate or clock checks. The reusable platform
manifest includes the standard scoped `k8sbundle` notifier. Application keys and
SVID certificate bodies are not exported to status or this report.

Scope limits: installed status checks execute shared production reconciliation
directly, not a complete Helm controller release. Gateway checks run production
handlers in a CSI-mounted trusted operator fixture with multiple explicitly
enrolled test roles; they do not certify inter-container role separation.
Generated pod/admission/network contracts retain local tests; cross-pod policy
enforcement, arbitrary platform enrollment and multi-replica rollout require
deployment-specific acceptance. Earlier actual SDK transport evidence remains
separate. No cloud/production activation or paid model calls occurred.

Cleanup first verified zero active/uncertain executions and preparations in both
stores, then retained the database, Kubernetes approvals/tombstones/quota tickets,
receipt ledgers and a hash index under the private run directory. The exact
owned cluster was deleted and its absence verified. UTC cleanup recorded
`2026-10-13T12:33:13.9664539Z`; no global kubeconfig/Docker/Git context was changed.
Retained artifacts stay outside the repository; no commit or push occurred.

#### Local Verification Refresh, 2026-10-13

The current repository-wide Go race/coverage run passes. Authored statement
coverage is 80.6%, exceeding the unchanged 80% CI threshold. The first broad run
reproduced two failures caused by the embedded PostgreSQL fixture stopping at
migration 032. Applying existing migrations 033/034 in that test-only setup and
clearing recovery rows between tests fixes `TestModelCanaryUsagePurposePersistsOnce`
and `TestRuntimeRecoveryDurableCASAndExpiry`; their focused rerun and the full
race run pass. No production schema or live database was altered.

The 286 focused editor-runner tests, full new-code lint and current integration
test compilation/opt-in guards pass. Installed tests were skipped locally by
their explicit guards; those skips are not new installed acceptance. The
previous real-SPIRE receipts remain separate, and the fixture stays deleted.

After owner-approved header-only restoration, exact body equivalence supports
reuse of functional results. Affected header/chart/SQL gates were rerun; all
disposition negative controls remain active. The core archive was repackaged,
while the governance archive stayed byte-identical. The private full coverage
profile is `kognition-verification-20261013-c733.out` in the local temporary
directory. Remote CI, release findings and arbitrary deployment acceptance are
not cleared by this local refresh.

T-0225 has started locally. The existing signed-candidate public reconciliation
test now covers exact-limit cost and model-tier exhaustion. Both deny without a
provider call or usage charge, retain the prior serving binding and remain denied
after reconciler restart. Atomic token reservation/settlement and uncertain
recovery remain open; no paid call, deployed operation, commit or push occurred.

### Native-Only Provider Registration Acceptance

TC-0119 is accepted for the explicitly approved local native-only-to-AutoGen
configuration-registration profile. T-0150 remains blocked on unchanged upstream
tool semantics and grants. This section does not expand production, hot-upgrade,
arbitrary-provider or external service authority.

The new fixture `kognition-registration-20261008` has cluster UID
`e6ad595a-72c4-441f-9939-a7dc3c452f67`. Its native-only controller has no AutoGen
registration, yet completes a real local inference with eighteen tokens and one
model call. A distinct AutoGen job fails with no configured adapter image before
an SDK Pod template or another model call. Its original authority then recovers
inactive, and the installer claim retires normally.

The reviewed reset removes only exact-UID unused bootstrap classes and the
disposable release, retaining CRDs, database/PVCs, operator credentials and
receipts. Registered/native renders differ only by the three AutoGen/TLS
environment settings and CA mount. Both use controller digest
`55bedbcdf3ee707873d9d7d68e9f1fda65d92336ba5095e1f3eda82774cbd740` and worker
digest `a301425f09e87c62b22709559225b269fd1d69902f9abc27bdc97c806b27bcb5`.
Independent exact-target authorities are not reused between these configurations.

The registered installed matrix passes in 283.039 seconds, with external-provider
binding explicitly excluded and cached ingester dependencies/current source.
It compiles from frozen source `03dacb4`, matching the deployed controller:
parallel working-tree capability edits removed `CandidateBinding` while a
dispatcher reference remains and therefore do not compile. That build failure is
retained separately; unrelated edits are not reverted or certified by these
receipts. The task-only CLI/dispatcher snapshot over the latest committed baseline
passes both race suites and affected-package lint.

Actual registered dispatch creates the selected SDK worker and completes the
same local input with eighteen tokens and one model call. The public Runtime
adapter connects to that generated worker using the real operator CA and Service
hostname over TLS. Health/capabilities, new session, foreign-principal rejection,
empty checkpoint/fresh restore and acknowledged cancels pass with no model
capabilities requested. The exact selected image separately passes the shared
streaming/cancellation/checkpoint/backpressure conformance suite. Those receipts
keep their deployed versus container scope; neither is substituted for the other.
Owned resources finalize through their real controllers, original registered
authority recovery passes, and the node is stopped with records retained.

Security review of the import boundary: `apply` now rejects foreign/malformed
explicit API versions before resource lookup or contact, retaining the existing
omitted-version path. The separately hash-bound, bounded, strict offline planner
does not initialize clients, import executable providers, print opaque tool
configuration, create grants or emit apply-ready resources. The pinned upstream
commit is `b133e628c9534da4a68bf1e3d035426b9a768d9b`. Its 21-tool expert exceeds
the release's twenty-tool limit and remains blocked. The schema-valid five-tool
observability render has source hash
`185978317a9ee40cec7023e729f03cf468192967cdc4f260555820fd772016f7`; byte-identical
template provenance is retained and no upstream script runs. Real mappings for
Kubernetes discovery, Prometheus/PromQL and Grafana dashboard management remain
unapproved. Preserved prompts are untrusted input, not authority to run those
tools. No stub or synthetic-tool substitution closes T-0150.

Evidence roots: `%LOCALAPPDATA%/Temp/kognition-registration-kind-20261008` and
`%LOCALAPPDATA%/Temp/kagent-porting-20261008`. Receipts retain actual clock
timestamps, including the October 9 UTC registration authority. No push,
production/cloud action or mutation of prior stopped fixtures occurred.

| Evidence | SHA-256 |
| --- | --- |
| Canonical registered activation target | `650c117b15419348e9e8e4f84809a30eac7fd14026f29c0cc1cc48ea099a8193` |
| Raw registered installed matrix | `61ea81c0f8dd1bbf4d4ca6de48a3261315b668663fa103733a53a9c1529a88ad` |
| Actual native-only execution | `674c33421c34cf9e5057a40f93fcea81fccb90a4fb1bc771c0da704903292d1a` |
| Actual unregistered SDK rejection | `83423bee1e1f244de97ec94d225f75521b6ea08ff0f61c27dacd33badfcb0b92` |
| Native original-authority recovery | `b433a31c6e507ba090587a5e16f5747b2b1e8cb01fd9de4dffa8753ee1ec1105` |
| Actual registered SDK execution | `8da96015f03a48c640b5104eab6c833090b0ffb28d73749d74240488aa8c1726` |
| Actual deployed public Runtime lifecycle | `e657192a4b5f34a8e882a61d942e42c46f8168e22c0d7952288b64758aed630e` |
| Exact selected image conformance | `c9c7f6da65e66947f0df640ceac0c277bb26c0643625d06749c049a134aaf9ce` |
| Registered original-authority recovery | `873c1608a21b0a05497810a8109c282e5d0035f57f80dea9e5c3e99965c2e7f8` |

### Installed Governed Runtime Acceptance

This section supersedes earlier pending local installed-runtime statements below.
Scope is the explicitly approved disposable Kubernetes/native/AutoGen profile;
it does not certify production, external providers, arbitrary kagent imports,
general additional-provider registration, SPIRE enrollment or publication.

The installed Memory write exposed a producer defect: the Python helper omitted
the caller-stable `X-Kognition-Memory-Invocation-Id` required by the real gateway.
The public tool regression failed before repair. The helper now binds a UUID to
session capability and canonical payload, preserves it across pending/unknown
outcomes, rejects mismatched returned IDs, and stops protected unconfirmed writes
with an unavailable barrier. Store/recall also propagate existing trace headers.
Tests cover identity isolation, terminal/new writes, HTTP/transport uncertainty
and actual SDK approval barriers. Gateway authorization and replay checks are
unchanged.

Acceptance used `kognition-memory-20261008`, cluster UID
`9ef84a95-119d-40a1-8a4c-ae1614a5f995`, with exclusive Cilium 1.17.8,
cert-manager 1.19.1, reviewed CRDs, fresh operator-managed Secrets/TLS/key ring,
and completed migrations through 034. Retained fixture timestamps report October
8; the receipts below retain those actual timestamps. Synthetic trust score 90
was explicitly owner-initialized, not measured or status-forged. Effective policy
comes from TrustProfile; an Agent's declared autonomy class cannot bypass the
cold-start supervised barrier.

Verified behavior:

- Corrected worker digest `a301425f09e87c62b22709559225b269fd1d69902f9abc27bdc97c806b27bcb5` passes all 173 tests with zero skips on host and non-root Linux, plus actual public SDK Runtime container conformance.
- Fresh exact-target installed matrix passes in 291.690 seconds: native policy probes, Cilium isolation/additive-denial, controller attach, concurrent streams/cancellation, production stdio, ingestion, runtime/Service lifecycle and Secret rotation. External provider-binding is explicitly outside the enabled profile; the ingester uses cached dependencies/current source, not a clean dependency build.
- Production-generated SDK Agent invokes the real Tool Gateway once, stores through Memory Policy Gateway, and recalls the exact marker with score 1. Output is `GOVERNED_EFFECTS_OK`, usage 72 tokens, four model calls and three runtime tool calls.
- Both class-switch directions preserve exact Agent UID/spec/image/input except runtime class. Native and SDK retain the same governed operation order/output/usage. Shared Memory correctly retains each intentionally written note; differing recall cardinality is not hidden.
- Actual controller and worker Pods both disappear. A fresh public dispatcher decrypts the live terminal record and reconstructs output/usage/three calls with zero worker/provider contacts. Effect counters do not change during loss, readback or restored rollout.
- Revoking the live ModelGrant fails a distinct job with `ModelAccessDenied` before dispatch. Final counters remain twelve model calls and three Tool effects for the three intentional successful sessions; no denied-session effect occurs.
- Four actual recovery records, including two terminal records, contain no plaintext effect markers. Unknown-effect fences from the earlier fixture remain retained. Expired approval and failed attempts were never replayed, status-forged or promoted into passing receipts.
- The synthetic TrustProfile and owned runtime producers are removed normally. Both fixtures pass recovery through their original immutable authorities/executables, are certified inactive, and their node containers are stopped with encrypted/accounting records retained. No push or production/cloud operation occurs.

Retained evidence root: `%LOCALAPPDATA%/Temp/kognition-memory-kind-20261008`.
The original stopped fixture and its uncertain-session fences remain under the
separate October 6 evidence root. Runtime source/test links:
[Memory caller](../../../runtime/agent/memory.py),
[public Memory regressions](../../../runtime/agent/test_memory.py), and
[actual SDK barriers](../../../runtime/agent/test_autogen_graph.py).

| Evidence | SHA-256 |
| --- | --- |
| Canonical fresh activation target | `84e5a596279b2e003763a180d6862a21b77506eb15074e9f137696d14071c05d` |
| Raw passing installed matrix | `7aaf51c31786eb0960a67edb7df9a66ed9caa40ea44daf6f1f342fd3b71675f2` |
| Installed governed SDK before loss | `8cfb93f48a730ff05868d27e032d0a034faef5b755d47d8601231264a36520ba` |
| Installed native governed run | `07c1ac0b175eaeb08e11eff7fee81ab5ddf136ce310421fc468f43331a9527ce` |
| Native-to-AutoGen class-only run | `3471f134f48c2ce479e6668de1628905e0b308f4b022fa3769e4e7143c255c71` |
| Actual controller/worker-loss readback | `f7daa93d24117951f373ec1723f6ebc0f1f205f27f2db6de1c53f96d244a5ccb` |
| Structured class-only comparison | `2013dc27c04434c9463059a5e3447e7e3f29b7132d0e9da757337aa678e6a8fc` |
| Actual live ModelGrant revocation | `9fe4b00508fb83cc6300f8e827b2df4169db5fbdcfa6769b2a09bc3655e72926` |
| Original-authority final inactive recovery | `8256e0c24f2c3da81a99918fe376fd3ba0f49ce98e52815db25360c766f6f2dc` |

Completion reconciliation: T-0148 is complete for its implemented lifecycle and
approved local installed scope; TC-0114 and TC-0116 carry that explicit scope.
T-0150, TC-0119, US-0042 and MVP-5 retain broader import/registration acceptance.
Historical every-story counts and prior gate receipts are not recomputed or
silently re-certified by this delta.

### Encrypted Recovery Follow-Up, 2026-10-06

ADR-0024 is implemented locally: per-workspace AES-256-GCM checkpoint envelopes
authenticate the full runtime/Agent/instance/generation/configuration/expiry
binding. Strict key/envelope parsing, hot operator key reads, retained read keys,
removed-key denial and bounded payload/retention checks pass. PostgreSQL migration
034 and Kubernetes ciphertext records implement prepared/claimed/terminal/retired
CAS. Claims precede bound restore and Step; terminal persistence precedes successful
completion and worker release. Unknown outcomes never authorize paid replay.

The real PostgreSQL lifecycle/expiry test and actual Kubernetes 1.30.3 admission
test pass, including foreign-writer/deletion rejection. The actual SDK container
passes production execution and encrypted terminal readback after controller and
worker loss without a second inference. Periodic leadership-scoped expiry purges
terminal ciphertext only, retaining unresolved claims and minimal replay fences.

Reviewed packaging correction: canonical SQL migrations 031 through 034 are now
included in the chart. They contain schema/purpose definitions, not credentials.
The exact static-SQL verifier pins 34 keys and digest
`414282907b0517faef6e2ebfe5404f36ea1be85a5745d8c861476df4f0435df7`;
metadata/content/provenance mutation tests still reject unreviewed input. This
source-classifier review does not import a historical target risk approval.

Guarded fixture preparation exposed a required Kubernetes Memory migration hook
that the installer rejects. The user authorized resolving that contract. Kubernetes
now renders a normal restricted, content-named migration Job without API-token
mounting; standalone hooks remain unchanged. Chart assertions and the guarded
inactive-plan parser pass. No installer or activation check was weakened.

Current local gates: final complete Go race suite, 80.7% authored coverage,
integrated lint, 165 worker tests, 495 chart assertions, backend CAS/expiry, actual
API admission and actual SDK crash/release checks pass. Bootstrap/migration
identity and archive fixtures were repaired without weakening their contracts.
These final candidate receipts remain separate from deployment acceptance.

The approved disposable kind cluster is `kognition-recovery-20261006`, using a
separate kubeconfig in `%LOCALAPPDATA%/Temp/kognition-recovery-kind-20261006`.
Its inactive `installation-plan.json` passed the supported plan/render parser;
plan SHA256 is `779b59bf3429fdbb0163eee6d9c0610d55fd0247b6129b36a5d1efe6843851ea`.
Exact target/security approval, operator material and activation remain pending.
No controller/workload was activated there and no existing cluster was changed.
Installed Agent/Tools/Memory and runtimeClass-only parity remain open; container
and envtest evidence do not close that requirement.

### T-0148 Installed Fixture Progress, 2026-10-07

The separately approved disposable target now passes guarded inactive bootstrap
with PostgreSQL migrations, operator Secrets, worker HTTPS and the actual
`admissionGuard.stdioIsolationProfile=cilium` profile. Its current plan SHA256 is
`460d4c4dfae1f3864e8cb3a4be41c31c6b1687cc62365b8349e842cc70feae1e`.
The cognitive controller remains at zero replicas. Activation preparation passed;
genuine approval and installed Agent/Tools/Memory parity remain pending.

Real reviewed-target native-policy, deployed attachment and production stdio
acceptance passed in 228.520 seconds. This exercised first-command egress denial,
additive allows, concurrent isolated streams, guard permission and TLS-stall
failures, policy UID replacement, cancellation/deadline and held-finalizer cleanup
recovery. Temporary producer grants and workloads were confirmed absent.
The harness intentionally replaces policy UIDs during negative cases, so fresh
activation preparation must bind the final live target before owner approval.

The reviewed-target producer run passed in 134.383 seconds: restricted ingestion,
generation/restart/reset, deliberate read-only-root failure, KnowledgeBase
finalization, authenticated native Agent dispatch, two stable Service revisions,
Service finalization and credential rotation/deletion with restoration. The
optional provider-binding case was skipped and remains uncertified. The ingester
uses current source and Go transport over a cached dependency image with a passing
`pip check`; this evidence does not establish a clean dependency build.

Fixture execution found a chart defect that discarded the Agent Communication
Gateway image digest. The shared image helper now receives that digest; the
rendered Deployment regression fails before the repair and passes for both digest
precedence and tag-only compatibility afterward. The refreshed core archive,
full Go race suite, integrated traceability lint and 495 chart assertions pass.
Separately reviewed CRDs remain an explicit prerequisite; installer checks were
not relaxed. Imported images also require verified digest-addressable CRI aliases.
T-0148 and TC-0114/TC-0116/TC-0119 remain partial until their deployed acceptance
criteria pass. This fixture evidence does not authorize production activation.

#### Installed Runtime And Recovery Findings

The owner separately approved bounded fixture activation. A real native AgentJob
returned `INSTALLED_RUNTIME_PARITY_OK` with 18 tokens and one observed local model
call. Full Agent-spec comparison proved that the subsequent switch changed only
`spec.runtime.class`. The AutoGen invocation exposed a production assembly panic:
the disabled concrete plugin host became a nonnil interface containing a nil
pointer. Assembly now injects a true nil selector. A public Dispatch regression
reproduces the panic before the repair and passes afterward; enabled plugin
selection and native dispatch regressions remain green.

Guarded recovery also exposed an approval equality defect. Journal serialization
normalizes RFC3339 `+00:00` timestamps to `Z`; internal Go timezone representation
then made `reflect.DeepEqual` reject unchanged approval instants. Recovery and
completed-transaction checks now compare timestamp instants while retaining exact
approval identity, target, approver, version, report hashes, test versions and gate
inventory. A public Recover regression covers the valid round trip and nine
changed-approval denials before journal mutation.

The retained old executable required an independently owner-approved, exact
UID/resource-version/old-data-bound journal representation repair. Read-only Go
verification proved unchanged approval instants and every nonapproval audit field;
the immutable authority record and consumed approval were preserved. Original-tool
recovery then revoked grants and confirmed controller replicas zero. Task-owned
runtime resources completed real finalization; no finalizer or status was forced.
The three unused built-in autonomy classes required separate exact-UID cleanup
approval before the unchanged installer drain could pass.

The corrected inactive candidate uses controller digest
`8f75dbf75decbc5b897f835f5b4afd5e5ef503fb8cab0d4f375e3df087aa2daa`
and plan SHA256
`68076a1d192e99ebdd3dea3a0b559550f2e6481dfb68b2893b8bd8849e1316d8`.
Full Go race and integrated lint pass for both repairs. Fresh live acceptance and
owner approval remain required before reactivation. AutoGen Tool/Memory effects,
encrypted installed recovery and full runtime parity remain open.

The refreshed combined candidate run retained a failed held-finalizer assertion:
default client throttling returned a pre-deadline rate-limit error after the exact
terminating Job was observed. The reviewed-target harness now uses the activation
CLI's bounded 50-request/second, burst-100 budget. The unchanged attachment,
isolation, timeout and cleanup matrix then passed in 155.837 seconds. Failed raw
output remains preserved; producer positives in that failed combined run do not
make the whole run passing. Fresh owner review must use the passing attachment
receipt and explicitly scoped producer evidence.

The disposable CNI setup subsequently exposed a leftover kindnet host address
`10.244.0.1/32` colliding with Cilium's allocation of that address to a guard Pod.
The owner approved removing only that stale address and obsolete daemonset and
recreating DNS under Cilium. After real rollout/health checks, the unchanged full
applicable matrix passed in 336.545 seconds; optional provider binding remained
skipped. A fresh exact-target approval activated only the corrected fixture.

Native execution again passed on the corrected controller with one local model
call and 18 tokens. AutoGen then failed closed before effects with HTTP 412:
generated gateway-backed Pods omitted `KOGNITION_NAMESPACE`, although protected
configuration checks included it. The environment producer now supplies the actual
Agent namespace. A public generated-workload regression reproduces the mismatch
before the repair and compares startup settings exactly against the binding after
it. Affected race/lint checks pass; updated installed SDK execution remains pending.

### T-0148 Implementation Progress, 2026-10-06

Production configured AutoGen selection now uses the public Runtime adapter,
with stable controller session/instance binding, persisted activation gating,
fresh step capabilities, live normalized usage, replay fencing and acknowledged
cancellation. The actual SDK container passed this production dispatcher path
under the race detector without a second inference on retry. Native dispatch
regressions remain green. Generated worker TLS uses the separately user-approved
operator HTTPS profile in ADR-0023; no installed-cluster change was performed.

Final local checks: repository-wide Go race suite, integrated lint, 165 host/Linux
worker tests, public SDK container conformance/recovery and 491 chart assertions
passed. Current controller/worker restart ownership fails closed as uncertain;
automatic durable checkpoint recovery, bounded retention/reclamation and full
deployed Agent/Tools/Memory parity remain open. T-0148 is not full acceptance.

| Classification | Meaning |
| --- | --- |
| Implemented | Production path and behavior-focused tests exist for the stated scope; current deployment acceptance is recorded separately |
| Partial implementation | Some stated acceptance criteria lack a production path or complete wiring |
| Design required | A contract or architectural decision must be resolved before implementation |
| Validation required | Implementation exists; a required measurement, deployed scenario, manual attestation or fresh evidence remains missing |
| External blocker | Completion depends on administrator access, infrastructure, provider availability or an authorized owner decision |
| Deferred | Explicit future scope; preserve it without silently making it an MVP blocker |
| Status correction | Ledger wording conflicts with newer implementation or evidence |

A checked box, traceability reference, helper implementation or package PASS alone
does not establish production completion. Historical evidence retains its exact
source, profile and environment limits. This review does not rerun the full
security campaign or certify all deployment targets.

## Confirmed Status Drift

### Packaging And Source Publication

The root backlog's September 18 security checkpoint predates the scoped
SG-01 through SG-13 remediation and the October 2 change-based policy. The
[current security review](../features/knowledge-provider-execution/security-review-code.md)
records completed local control acceptance and a scoped source-publication review.
The retained publication receipt verifies the same `896a58c` on both remotes.

Keep these states separate:

- Source publication: complete on origin and mirror.
- Release certification: not granted by source publication.
- New KSV-0049 ConfigMap-create risk: open for release/deployment disposition.
- Production activation and external-provider acceptance: not granted.
- Origin hosted CI: blocked by Enterprise runner policy, before any steps executed.
- Mirror CI: completed with failure on the reviewed baseline. Traceability, ordinary tests, integration tests and the security scan failed; the build was skipped. Proto, lint, Python, watchdogs and the change planner passed.

Receipt: `%LOCALAPPDATA%/Temp/kognition-webhook-delta-20261002/publication-receipt.json`.
The security backlog's "authorized push pending" wording also needs a dated status
correction; its historical scanner approvals must remain unchanged.

## Confirmed Completion Gaps

### High: Signed-Artifact Verification Is A Presence Check

Affected: US-0022, T-0074/T-0075, TC-0059/TC-0060, FR-0305.

[The production signature validator](../../../internal/admission/signature_validator.go#L49)
allows any nonempty signature string. The production admission chain registers
this validator. There is no artifact-digest verification, signer trust check,
Sigstore bundle verification or cosign invocation in that path. Its positive
test accepts `sha256:abc123`, which establishes string presence rather than a
verified signature.

A temporary Go-overlay probe with `requireSignedArtifacts=true` and signature
`not-a-cryptographic-signature` failed its rejection assertion: the validator
allowed the Tool. Product source was not changed.

Classification: **partial implementation; design and implementation required**.
Define artifact identity, allowed signers/trust roots, verification mechanism,
offline/online behavior, timeout/failure semantics and both-mode admission wiring.
Then test forged, wrong-digest, untrusted, expired and unavailable-verifier cases.
Keep opt-in scope; mandatory signing remains separately deferred.

### High: Published Image Set Does Not Cover The Chart

Affected: US-0053, T-0186/T-0187/T-0188, TC-0141, FR-0800.

[Release publishing](../../../.github/workflows/release.yml#L45) contains eight Go
image targets plus runtime and ingester jobs. It omits the default-enabled Tool
Gateway and Memory Policy Gateway, and optional Model Gateway, Agent Communication
Gateway and knowledge-pgvector provider targets. It publishes names such as
`apiserver` and `k8s-controller`, while [chart defaults](../../../deploy/helm/kognition/values.yaml)
use `kognition-apiserver` and `kognition-k8s-controller`. Local builds and deliberate
image overrides do not prove that a tagged release supplies the chart's expected
image set.

Classification: **partial release implementation**, separate from completed local
packaging/source publication. Define one image-name/target inventory, publish the
required components, test each supported render profile against it, and require
the approved release checks before artifact publication. Do not trigger a release
or change registries during this review.

### High: Traceability Completion Is Not Reliable

Affected: US-0054 through US-0056, FR-0900 through FR-0902, all derived coverage claims.

- [The trace parser](../../../tools/trace#L92) matches headings beginning with `#### US-...`, but several real headings begin with a completion marker. `bash tools/trace US-0013` reports no tasks, tests, FRs or NFRs even though the story defines them. The mirror CI trace-rendering step also fails.
- Four duplicated task IDs: T-0236, T-0237, T-0238 and T-0244.
- Nine duplicated test IDs: TC-0194 through TC-0198, TC-0245 through TC-0247, and TC-0299. TC-0194 has three definitions.
- All 370 task definition rows carry completion markers, including workload identity descriptions that explicitly leave issuance/rotation pending.
- TC-0153 is specified as a 10K-record cascade latency case, but [its current tagged test](../../../internal/controller/identity_isolation_test.go#L1130) checks admission of invalid references.
- [The NFR plan](../../../BACKLOG.md#5-nfr-verification-plan) maps RPO/RTO to TC-0026. Its [test](../../../internal/controller/workflow_reconciler_test.go#L848) constructs a second reconciler over the same in-memory store; it does not restore a backup, simulate persisted data loss or measure recovery time.
- PROJECT's forward-slot registry is still empty, and its OPEN master list retains naming and other decisions resolved elsewhere. Evaluation US-0085 traces narrative sections instead of the mandatory FR/NFR/DIFF/MVP/RISK IDs.

Classification: **parser implementation, identifier-migration design and semantic
evidence correction required**. Preserve existing IDs and historical references;
use explicit disambiguation/aliases and a reviewed migration instead of renumbering
old definitions. A matching tag or test-function name is not acceptance proof.

### High: Workload SVID Identity Is Not Implemented

Affected: US-0026, T-0085 through T-0087, FR-0401.

[The reconciler](../../../internal/controller/identity_reconciler.go#L36) explicitly
does not issue cryptographic SVIDs. [Identity status](../../../api/v1alpha1/identity.go#L74)
only contains Phase. Automatic identity ownership and signed session capabilities
are implemented; they must not be described as workload SVID issuance/rotation or
agent-to-tool/memory mTLS verification. Provider-channel mTLS is a separate feature.

Classification: **partial implementation; issuer/rotation/trust contract design
required before implementation**. Define standalone and Kubernetes issuer wiring,
trust domains, certificate status, rotation/revocation, unavailable-issuer behavior
and authenticated identity propagation through the data planes.

### Medium: Native Checkpoint Loses Conversation State

Affected: US-0040/US-0041, T-0141/T-0143, TC-0110, FR-0600.

[Native Checkpoint/Resume](../../../internal/runtime/native/native.go#L175) delegates
to the session manager. [The serialized checkpoint](../../../internal/runtime/session.go#L171)
does not contain the native runtime's conversation map, and Resume allocates a new
session ID without restoring that map.

A public-interface overlay probe performs a turn, checkpoints, resumes and sends
a second question. The second model call contains only that new question; system
instructions, the first question and the first answer are lost. The rejection
assertion failed. This is an observed defect, not just a missing test tag.

Classification: **implementation repair required**, with a behavior-focused
regression for same-process and fresh-adapter resume, conversation/tool state,
checkpoint compatibility and invalid tokens. Do not infer equivalent behavior in
the separate HTTP/Python runtime from this reference-adapter defect.

### Medium: AutoGen Production Integration Is Missing

Affected: US-0042/US-0043, T-0147 through T-0155, MVP-9.

[The adapter](../../../internal/runtime/autogen/autogen.go#L18) requires an AgentClient.
The reviewed implementation and call sites provide only
[a fake client](../../../internal/runtime/autogen/autogen_test.go#L19); adapter
construction is found in tests rather than a real AutoGen production client/factory.
The packaging ledger itself defers the AutoGen/bridge image.

Classification: **adapter logic implemented; real-client, deployment and conformance
integration still required**. Validate a real second runtime, not an adapter fed
prebuilt events. Decide the supported AutoGen version/transport and runtimeClass
selection before implementing the client and shipping its image.

### Medium: Model Canary Promotion Is Metadata-Only

Affected: US-0063, T-0225, TC-0183.

[Version promotion](../../../internal/controller/model_class_reconciler.go#L354)
checks whether the provider name is registered, then changes ActiveVersion. It
does not probe the candidate model version before promoting it. Driver removal
can trigger its rollback branch, but that is not the declared candidate-health
failure scenario.

Classification: **partial implementation**. Define candidate-version/provider
binding and real promotion criteria, then wire candidate health evaluation,
serving-version changes and rollback. Keep existing class lifecycle and ordinary
health-probe completion separate from this missing contract.

### Gateway Authority Follow-Ups Have Qualified Local Closure

Affected: SEC-F1-002/SEC-F1-003, runtime-autonomy deployment acceptance.

**Local repair follow-up, 2026-10-14:** Direct gateway human-decision and
reconciliation forgery now fails at resource admission with explicit authority
configuration. Lease RBAC and native admission distinguish the control binding
lock from tenant record locks and forbid out-of-scope writes, takeover and
deletion. The full affected local race suites and fresh disposable API-server
negative tests pass. Activation requires the additional enforcement pair.
The broader findings remain open for execution-owner/transition review,
controller-manager typechecking and exact deployment acceptance. See the
[scoped security review](../features/gateway-authority-hardening/security-review-code.md).

**Isolated live follow-up, 2026-10-18:** Kubernetes 1.34.11 acceptance now passes
actual controller-manager typechecking for all nine policies, actual Pod-bound
gateway credentials, rendered Lease/RBAC boundaries and all three planes'
production-validator claim/finalization/recovery transitions. Evidence was
retained before removing the successful target. An earlier partial Communication
claim remains retained without replay. Full gateway HTTP/provider workflows,
replica concurrency and the exact production assembly remain outside this
profile; SEC-F1-002/SEC-F1-003 remained open at that checkpoint.

**Qualified deployed follow-up, 2026-10-19:** Both findings are closed for the
deterministic isolated gateway boundary profile. Two real replicas per gateway
passed Tool, Memory store/delete and cross-workspace Communication workflows
with fail-closed verified-TLS admission, exact owner/fence receipts and one
synthetic provider effect per operation. Real PostgreSQL mutation receipts and
both-replica replay passed. Communication claimed before single exact-capability
issuance and denied reissuance; its binding repair retains source authority and
signs the exact remote AgentService. The earlier pause-only Communication claim
was reviewed and retained unchanged without replay by owner choice. Saved
evidence precedes removal of the successful targets and separately approved
failed sandboxes. Full controller activation/enrollment, paid/provider-specific
acceptance and broader release-risk dispositions remain separate.

The paragraph below retains the original pre-repair finding and scope:

The current [Tool Gateway role](../../../deploy/helm/kognition/templates/tool-gateway.yaml#L38),
[Memory Policy Gateway role](../../../deploy/helm/kognition/templates/memory-policy-gateway.yaml#L38)
and [Communication Gateway role](../../../deploy/helm/kognition/templates/agent-communication-gateway.yaml#L45)
still grant cluster-wide Lease get/create/update without resourceNames and direct
RuntimeApproval status writes. Application state-machine checks do not constrain
a compromised gateway identity independently.

Classification: **security design/implementation and exact owner disposition still
required before broader production acceptance**. Reconcile intended transition
ownership, namespace/name scoping and enforcement with ADR-0014. This review does
not declare that all direct status writes are unintended, or expand an old scanner
approval. The newer webhook KSV-0049 create-ConfigMap risk remains separately open.

## Inventory And Evidence Limits

The full definitions were read and inventoried. There are 90 unique stories and
22 epic memberships. Explicit source/test references were found for 294 of the
304 distinct test IDs; duplicate IDs can contaminate those mappings. The absence
of an explicit ID in a test is not absence of implementation: Solution behavior,
for example, has named tests without the old TC IDs.

Temporary evidence is under
`%LOCALAPPDATA%/Temp/kognition-backlog-review-20261002`: `inventory.json`, the
signature/checkpoint overlay probes and `negative-probes.jsonl`. Both negative
probes produced the expected behavioral failures. The repository contains only
this review document as a new artifact, not those virtual test files.

## Review Coverage

All 90 story definitions, the remaining-work ledger, NFR/MVP matrices, PROJECT's
OPEN/deferral registers and specialized security follow-ups were read and
reconciled. Implementation/test references were inventoried for every story;
high-risk completion claims received direct controlling-path inspection and
focused probes. The latest mirror CI logs supplement historical test receipts.
This is not a line-by-line review of every implementation file or an execution of
every declared acceptance case. Findings identify which completion claims lack
adequate implementation or acceptance proof. No production source was changed.

## Every-Story Classification

Legend: **I** = implemented for the current stated scope; **S** = implemented with
an important scope correction; **P** = partial implementation or confirmed defect;
**V** = required acceptance remains open. I/S rows retain historical and current
package evidence, not a blanket all-green sign-off. Evidence owners below are
the corresponding production/test areas, not proof inferred from filenames alone.

| Epic | Story | Assessment | Evidence owner and remaining boundary |
| --- | --- | --- | --- |
| 0001 | US-0001 | I | API handler and PostgreSQL CRUD/watch/concurrency tests; HA/DR NFRs separate |
| 0001 | US-0002 | I | Controller queue/requeue and leader-election tests; measured HA separate |
| 0001 | US-0003 | I | Production admission chain and API/webhook integration tests |
| 0001 | US-0004 | S | Finalizers, conditions and revision APIs implemented; revision retention and Workflow revision wording need separate scope |
| 0001 | US-0005 | I | Namespace resolver, grants and production cross-namespace admission |
| 0002 | US-0006 | I | Agent reconciliation, defaulting, revisions and dispatch |
| 0002 | US-0007 | I | AgentInstance dispatch/status/retry/cleanup; duplicated T-0236..0238 must be disambiguated |
| 0002 | US-0008 | I | AgentJob lifecycle, completion and retry budget |
| 0003 | US-0009 | S | Durable reconciliation and WorkflowRun execution; Temporal-shaped event log and RPO/RTO are not demonstrated |
| 0003 | US-0010 | I | Bounded emergent DAG mutation; P10 refinements remain deferred |
| 0004 | US-0011 | I | Prompt hashing, history and referencing-agent drift |
| 0004 | US-0012 | S | Internal/env and Kubernetes SecretRef paths; canonical Vault requirement is not implemented and is rejected by admission |
| 0005 | US-0013 | S | Provision/reclaim and memory policy scope implemented; linearizable/CRDT/lock claims need supported-backend semantics and acceptance |
| 0005 | US-0014 | S | Claims/index runs/schedules, ingestion, refresh and query boundaries implemented; provider security blockers and live external acceptance remain |
| 0005 | US-0015 | I | Session/provenance behavior implemented in current backends; stronger cross-writer guarantees not inferred |
| 0005 | US-0016 | I | Background summarization, history and provenance behavior |
| 0005 | US-0017 | I | Memory-write redaction and RTBF cascade; workload-specific enterprise policy acceptance separate |
| 0006 | US-0018 | I | Tool lifecycle, digest checks and OpenAPI compilation |
| 0006 | US-0019 | S | Dispatcher and controller-owned OCI sandbox paths; generic process/sidecar/VM claims need supported-tier boundaries |
| 0006 | US-0020 | I | Approval tool/broker and workflow approval semantics; runtime approval is a separate durable resource |
| 0006 | US-0021 | I | Model/provider routing and health in the supported provider set |
| 0006 | US-0022 | P | Nonempty signature accepted without cryptographic verification; negative probe confirms gap |
| 0006 | US-0057 | S | ToolClass schema and lifecycle; TC-0163 validates a sidecar class, not actual sidecar provisioning |
| 0006 | US-0058 | I | Required-tool readiness, health and OCI exec probe runner; PROJECT FR-0310 is stale |
| 0006 | US-0059 | S | Stdio/SSE/HTTP, Browser and class-approved Shell paths implemented; VM and gateway-created sandbox expansion are not this scope |
| 0006 | US-0086 | S | Resilience/coordination/conformance implemented; historical enforcing-CNI acceptance scoped, current E2E recertification separate |
| 0006 | US-0087 | S | OpenAI-compatible chat authorization implemented; embeddings/streaming/other gateway-provider modes need their own scope |
| 0007 | US-0023 | I | Stable endpoint and immutable revision pinning in both modes; weighted split deferred |
| 0007 | US-0024 | I | HTTP/unary-gRPC goal routing and dedicated Kubernetes gateway |
| 0008 | US-0025 | I | Workspace boundary/default deny; tenant-scale/noisy-neighbor measurements separate |
| 0008 | US-0026 | P | Automatic URI identity implemented; SVID issuance, rotation and agent mTLS not implemented |
| 0008 | US-0027 | I | AgentRole/Binding cognitive authorization |
| 0008 | US-0028 | I | Workflow communication and exact AgentService delegation; not external A2A federation |
| 0008 | US-0029 | S | Live revocation/provenance denial implemented; TC-0153 does not prove the 10K-record latency requirement |
| 0008 | US-0030 | I | Tool/Model grants and rate limits; signing and workload SVID remain separate gaps |
| 0009 | US-0031 | I | Policy aggregation, admission evaluation and defaults |
| 0009 | US-0032 | I | Production OPA/policy denial, audit and input-constraint wiring |
| 0009 | US-0033 | I | Quota/limits and normalized provider usage projections |
| 0009 | US-0034 | I | Reservations, terminal settlement and cooperative runtime termination |
| 0009 | US-0035 | I | Production scheduler selection and same-Agent ready-replica placement |
| 0009 | US-0060 | S | ADR-0014 Tool/Memory/Communication Gate 3 complete; Model Gate 3 and costThreshold need new contracts |
| 0009 | US-0061 | I | Observed trust scoring, promotion/demotion, stabilization and persisted evidence |
| 0009 | US-0062 | I | Authenticated AutonomyReview decisions, TTL and promotion fencing |
| 0010 | US-0036 | I | Runtime/readiness probes and routing exclusion |
| 0010 | US-0037 | I | Replay-safe canary, bounded output/accounting and ADR-0012 closure ledger |
| 0010 | US-0038 | I | Restart ladder, rollback, model degradation and quarantine |
| 0011 | US-0039 | I | Agent horizontal scaling and stabilization; 100-agent measurement separate |
| 0012 | US-0040 | P | Session/stream/cancel/credits exist; native checkpoint loses conversation state |
| 0012 | US-0041 | P | Native model/tool/memory reference adapter exists; checkpoint restore defect remains |
| 0012 | US-0042 | P | AutoGen adapter uses fake AgentClient; real client/factory/deployment absent in reviewed paths |
| 0012 | US-0043 | P | Conformance framework exists; fake AutoGen conformance is not proof of two deployable runtimes |
| 0012 | US-0044 | I | Pgvector memory CRUD/vector adapter and PostgreSQL tests |
| 0012 | US-0045 | S | Protobuf host/SDK, reconnect and production Runtime selection implemented; extra Memory/Tool/Model consumers need their own rows |
| 0013 | US-0046 | I | Solution schema/validation and named tests; missing TC ID tags are bookkeeping, not missing implementation |
| 0013 | US-0047 | S | Deterministic multi-resource compilation/execution exists; p95 less than 60s not established by functional test |
| 0013 | US-0048 | I | Goal revision history and rollback; named tests lack legacy TC IDs |
| 0013 | US-0049 | I | Workspace inheritance and bounded overrides in both modes |
| 0014 | US-0050 | I | OTel/redaction and core Events projection; scenario lineage acceptance retains its recorded scope |
| 0014 | US-0051 | S | Cost/audit/monitoring implemented; API attempts are not committed mutations, gateway authority risks remain |
| 0014 | US-0052 | I | CloudEvents and tenant event/trace/cost retention; revision ConfigMap retention not included |
| 0015 | US-0053 | P | Local charts/images/install work exists; release image inventory, guarded GitOps and current release acceptance incomplete |
| 0016 | US-0054 | I | Commit/PR validation exists; origin runner policy prevents enforcement there |
| 0016 | US-0055 | S | Syntactic coverage checker exists; semantic evidence/unique IDs/OPEN closure not proven |
| 0016 | US-0056 | P | Trace executable mode fails Linux CI; decorated headings lose tasks/tests/requirements |
| 0017 | US-0063 | P | Class lifecycle exists; candidate-version health/serving canary promotion is not implemented as claimed |
| 0017 | US-0064 | I | Health degradation/recovery and fallback in supported scope |
| 0017 | US-0065 | I | CPI registry/socket discovery/health/lifecycle; provider acceptance scoped |
| 0017 | US-0066 | I | Four-dimension CPI conformance framework; PROJECT FR-0613 deferred marker is stale |
| 0017 | US-0067 | S | Built-in CPI drivers exist; provider validation and live scope must match actual provisioners |
| 0018 | US-0068 | I | Native types, generated CRDs/deepcopy and code generation |
| 0018 | US-0069 | I | Conversion round trips/fuzz coverage; duplicate TC IDs need correction |
| 0018 | US-0070 | I | Controller-runtime adapters; deployed target/version evidence separate |
| 0018 | US-0071 | S | Resource webhooks/cert-manager implemented; independent server replaces single-process assumption |
| 0018 | US-0072 | S | Controller binary implemented; resource admission and native guard deliberately run separately |
| 0018 | US-0073 | S | CRD/chart/RBAC modes implemented; chart rendering is inactive, reviewed installation/activation required |
| 0018 | US-0074 | I | kubectl plugin commands and backend paths |
| 0019 | US-0075 | I | Output capture, templates, errors and bounds |
| 0019 | US-0076 | I | CEL step/edge gating and evaluation bounds |
| 0019 | US-0077 | I | Parallel dispatch, throttling, failFast and fan-in |
| 0019 | US-0078 | I | WorkflowRun separation, concurrency/history and parameter merging |
| 0019 | US-0079 | I | Retry/backoff/timeout/fallback |
| 0019 | US-0080 | S | Deterministic tools implemented; historical OpenAPI/HTTP-only note predates newer transports |
| 0019 | US-0081 | I | Per-run shared memory injection and retention |
| 0019 | US-0082 | I | Approval subresource/quorum/TTL and CRD ApprovalDecision path |
| 0019 | US-0083 | I | Workflow spans, restart-safe metrics and timeline; duplicate TC IDs need migration |
| 0020 | US-0084 | I | Server embedding, BM25/RRF, tags and no-match honesty; indexed lexical scale is optional follow-up |
| 0021 | US-0085 | S | Agent/unpinned and explicit Agent/AgentService-targeted Solution evaluations; pinned-Agent/full-workflow targets unsupported |
| 0022 | US-0088 | I | Four-plane normalized decisions and cross-mode query API |
| 0022 | US-0089 | I | Read-only explorer, Trust Focus, safe rendering and configured observability links |
| 0022 | US-0090 | V | Narrated TC-0316 open; current clean-three-run/fallback proof needs candidate/profile scope; runbook contract fails CI |

## Design And Implementation Queue

These are decomposition recommendations, not new task IDs or approval to implement
optional capabilities. Resolve scope/ADR conflicts before allocating new IDs;
reuse existing IDs only for their original definitions.

| Priority | Work | Design needed | Implementation or verification needed |
| --- | --- | --- | --- |
| First | Restore required CI | Preserve activation/security contracts and distinguish dispositions from suppression | Trace executable mode/parser, webhook fixture inventory, runbook contract alignment, Trivy setup failure; origin needs Enterprise-approved runners |
| First | Real signature verification | Artifact/digest binding, trusted signers, verifier and offline behavior | Cryptographic verification and forgery/wrong signer/digest/unavailable-verifier tests |
| First for production | Workload identity | Issuer/trust domain, SVID lifecycle, status/rotation/revocation and both-mode propagation | Issuance, rotation, mTLS and negative/recovery acceptance |
| First for production | Deployment-specific activation and provider acceptance | SEC-F1-002/003 have qualified isolated closure; target enrollment, authoritative provider storage and release risks still need target-specific acceptance | Approved candidate/target and residual-risk dispositions; full provider TLS/routing/authorization/quota/audit acceptance |
| Next | Native checkpoint defect | Token compatibility, state ownership and bounded persistence | Conversation/tool state restoration and fresh-process resume regression |
| Next | Real second runtime | AutoGen version, transport, runtimeClass factory, image and lifecycle | Real AgentClient/runtime deployment plus checkpoint/cancel/credits and conformance |
| Next | Model version canary | Candidate artifact identity, health criteria, serving switch and rollback | Real candidate probe/promotion gate and serving-version recovery |
| Next for release | Image/chart release inventory | Canonical registry/repository names and required profile targets | Missing image targets, matching chart references, render-to-published-target checks and release gates |
| Next for release | Guarded GitOps installation | Automated prune/selfHeal versus writer freeze, inactive apply and explicit approval/activation | Supported integration and live sync/recovery acceptance; Argo Application alone is insufficient |
| Before broader autonomy claims | Model Gate 3 and costThreshold | Model approval boundary, normalized cost unit, reservation and protected-action binding | Dedicated rows and conformance before implementation; preserve completed ADR-0014 slice |
| Before stronger memory claims | Shared memory modes | Supported linearizable/CRDT/lock semantics, backend guarantees or explicit deferral | Multi-writer enforcement and concurrency/failure acceptance; schema fields are insufficient |
| Scope decision | Vault backend | Keep canonical FR-0201 requirement or explicitly defer it | Vault adapter/auth/rotation/error behavior if adopted; current admission rejects vault |
| Scope decision | Extra plugins and execution tiers | Memory/Tool/Model consumer contracts and sidecar/VM ownership | Production consumers/provisioners and deployed tests if adopted |
| Scope decision | Expanded evaluation targets | Pinned-Agent snapshots and whole-workflow/Solution evaluation | Explicit target support and promotion/regression acceptance if adopted |
| Scope decision | Revision retention | Retention, rollback dependencies, ownership and immutability safety | Dedicated task/tests; T-0185 does not cover revision ConfigMaps |
| Required acceptance | Presenter/current demo | Supported profile and storyboard claims | Narrated less-than-1:55 TC-0316 attestation, clean repetitions and fallback proof without manual repair |
| Required acceptance | NFR qualification | Workloads, sample windows, failure models and reproducible measurements | Load/latency/tenant isolation/backup-restore evidence below |
| Bookkeeping | Traceability/design ledger | Collision migration and resolved/deferred ownership | Semantic mappings, forward-slot registry, canonical FR status and operational links |

## NFR Acceptance

A mapped TC need not measure the stated number. Retain functional evidence, but
obtain a profile-bound report before marking the numeric/operational target verified.

| NFR | Target | Assessment and next evidence |
| --- | --- | --- |
| NFR-0001 | 99% CP availability | Failure/leader tests exist; operational availability/HA measurement required |
| NFR-0002 | Reconcile p95 less than 10s | Convergence exists; representative 50-Agent percentile measurement required |
| NFR-0003 | Cold-start p95 less than 5s | Lifecycle tested; measured first-Step distribution required |
| NFR-0004 | 100 concurrent agents | Replica/autoscale tests are not a 100-agent load run; capacity/failure evidence required |
| NFR-0005 | 10 Workspaces | Isolation implemented; explicit ten-workspace simultaneous acceptance needed |
| NFR-0006 | 50 tools per Agent | Functional case exists; disambiguate TC-0194 and scope proof to real resolution |
| NFR-0007 | RPO 1h | Reconciler restart is not data recovery; backup/loss/restore-point report required |
| NFR-0008 | RTO 30min | TC-0026 does not time disaster restoration; timed restore/operational recovery required |
| NFR-0009 | Solution p95 less than 60s | Functional answer is not a percentile benchmark; define scenario/provider/approval timing |
| NFR-0010 | Revocation less than 5s | Denial/cascade exists; 10K-record latency case is incorrectly mapped and open |
| NFR-0011 | Probe interval at most 5min | Schedule slots implemented; retain interval/timeout proof for runtime/profile |
| NFR-0012 | Durable write before effect | Ordering/failure tests exist; preserve supported-plane scope and distinguish API attempts from commits |
| NFR-0013 | Noisy-neighbor isolation | Quota denial exists; tenant-load impact and queue-fairness measurement required |

MVP rows need corresponding qualifications: MVP-9 lacks real AutoGen production
conformance, MVP-6 lacks its p95 measurement, and MVP-3/4 must not imply SVID-backed
cryptographic provenance. MVP-10 appears in epic/story traces but is absent from
the root MVP success map and PROJECT's MVP-1..9 matrix; reconcile this registry gap.

## Current CI Blockers

Mirror run `37052465333` on the reviewed commit completed with failure. Exact logs
are retained under the temporary review evidence directory.

| Gate | Exact observed failure | Classification |
| --- | --- | --- |
| Traceability and ordinary tests | tools/trace is not executable on Linux | Git-mode/CLI implementation repair |
| Ordinary and integration tests | Runbook lacks expected fragment `three sequential runs with unique IDs` | Documentation/contract-test alignment; preserve the actual clean-run requirement |
| Integration | Built prepare/dry-run fixture cannot find `security-test-kognition-webhook` ServiceAccount | Fixture lacks new mandatory resource-admission inventory; do not bypass candidate checks |
| Security | Trivy v0.65.0 setup action fails; Run Trivy step is skipped | Installation prerequisite failed, not a current repository scan or disposition result; exact setup cause needs follow-up |
| Build | Skipped after prerequisite failures | No fresh CI build acceptance |
| Origin | Enterprise disables hosted runners; zero job steps executed | External administrator/approved-runner dependency |

Proto, lint, Python, both watchdogs and the change planner passed. The remote unit
and tagged suites contain many passing package results, but they are not globally
green. No new repository-scanner result should be inferred from the failed setup.

## Preserve Deferred Scope

The design defers SessionSet/AgentDaemon/AgentCronJob/AgentClass, weighted
AgentService routing/affinity, priority/preemption/taints/affinity/disruption
budgets, event-driven/runtime/model scaling, extra runtime/backend/workflow
adapters, kagent/tool-registry shims, SolutionBundle, MCP KB resources/model
sampling, A2A federation, cross-workspace Memory/Tool grants, external event buses,
graph memory/federation, multi-runtime Agents and workflow P10 refinements.
Generic types/helpers do not prove a complete production contract; these are not
all current MVP blockers.

The deferral list is itself stale: Evaluation, UI, production naive scheduling and
four-dimension CPI conformance have shipped in scoped forms. Vault is the reverse
case: present in a canonical MVP requirement, absent from implemented backends.
Reconcile contradictions rather than planning completed work again.

## Recommended Order

Updated 2026-10-06 after local commits through `b9dcb34`. Signing, native
checkpoint, trace CLI, real SDK foundations and release inventory are implemented
and locally verified; do not schedule their original repair work again. The
[current backlog queue](../../../BACKLOG.md#43-next-work-queue-2026-10-29) controls
the next implementation and its closure evidence.

1. Complete T-0148 production AutoGen Runtime factory/lifecycle integration, retaining native compatibility and exact session/capability binding. Then exercise T-0150, TC-0114, TC-0116 and TC-0119 through the actual control-plane selection path.
2. Complete T-0085/T-0086 protected SPIRE enrollment, generated socket/sidecar wiring and resource-status propagation after confirming socket delivery and registration ownership. Accepted UID/Go transport decisions do not authorize installed-cluster changes.
3. Resolve T-0225's remaining quota/recovery contract and obtain approval for exact-candidate deployed migration/rollback acceptance. Preserve paid-call replay fencing and previous serving bindings.
4. Before rollout, resolve gateway transition/Lease authority and exact release-risk dispositions, enable/rerun remote CI, and finish T-0186/T-0187 publication plus T-0189 guarded GitOps acceptance. Local source commits do not provide these receipts.
5. Produce profile-bound NFR measurements and presenter-attested TC-0316; allocate optional contract extensions only after separate approval.

Dependency check: the required foundations are committed on local `main`. The
read-only origin PR lookup could not resolve the repository under the current
CLI identity, so remote unmerged-PR dependencies are unverified, not confirmed
absent. No remote push, merge, runner change or deployment was performed.

This review proposes work/status corrections. It does not approve new product
scope, rename stable IDs, alter historical attestations, waive scanner findings,
change Kubernetes targets or commit/push implementation fixes.
