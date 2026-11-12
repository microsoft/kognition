# SG-11 Local Candidate Acceptance

- Status: CLOSED for the recorded local candidate. SG-12 closed with exact owner-approved dispositions; SG-13 closed locally by user scope revision on 2026-10-02. Push freeze remains active.
- Date: 2026-10-01.
- Candidate tree: `1a2fd85548628e3828f6703d24d9024d8d3751fa`.
- Candidate archive SHA256: `307b56d259ccc8b54b3ca7d1d52d974ac36f380da7e139cd9207b2a2e40d3054`.
- Evidence root: `%LOCALAPPDATA%/Temp/kognition-sg11-20261001-producers`.
- Completion validator and record: `verify-closure.ps1` and `closure.json` under that evidence root.

## Results

| Required check | Recorded result |
| --- | --- |
| Full Go race suite | PASS; 7,099 passing test events, zero failures |
| Full Go integration suite | PASS; 7,698 passing test events, zero failures; explicit envtest assets and disposable PostgreSQL |
| Authored coverage | 81.2%, above the 80% threshold |
| Formatting and lint | PASS; normal and integration-tag whole-repository lint |
| Dependency checks | PASS; govulncheck, npm audit and both Python requirements audits |
| Python suites | PASS; agent 126, ingester 74, watchdog 11 |
| Browser suites | PASS; dashboard 30 and Studio 37, across desktop/mobile |
| Binary builds | PASS; all 19 inventoried binaries, with artifact hashes |
| Chart, generator and shells | PASS; chart regression/parity, pinned generation, traceability and explicit two-shell acceptance |
| Docker sandbox execution | PASS; all three functional cases repeated three times serially with unchanged deadlines |
| Memory restart replay | PASS; exact-candidate migrations, disposable database and confirmed fixture cleanup |
| Admission typechecking | PASS; all six policies on actual Kubernetes 1.30.3 and 1.31.1 control planes |
| Deployed producer lifecycle | PASS; 17 passing parent/subtest events, zero failures, zero skips |
| Guarded installation | PASS; fresh, legacy, failed install, failed upgrade, certificate recovery and interrupted verification; zero skips |

Go event counts include parent and subtest events, not unique user scenarios.
Every required result has its command, candidate identity, timestamps, exit code
and log hash in `evidence/`. The completion validator checks these records,
required executions, build hashes, coverage and source integrity before closure.

## Closed Blockers

- Narrow ignore exceptions include the [kind configuration](../../../../e2e/sg07-kind.yaml), [provider Dockerfile](../../../../e2e/sg08-provider.Dockerfile) and [provider source](../../../../e2e/fixtures/sg08-provider/main.go) in the candidate. Other local E2E files remain excluded.
- Python image builds use fresh `python:3.12-slim` bases and clean offline dependency installation. Linux wheels were downloaded over verified HTTPS from the host, hashed and installed with `pip --no-index`; both images pass `pip check`. No TLS verification was disabled and no old runtime-image source overlay was used.
- The ingester transport and provider binaries were built from the frozen candidate. Evidence-local Dockerfiles record the offline build procedure; `producer-image-provenance.json` binds image identities, wheels, binaries and recipes to the candidate. This validates those clean offline builds, not the still-failing container-to-PyPI network path.
- `production-stdio`, `production-ingestion`, provider binding/finalization and agent-runtime acceptance all ran with explicit opt-ins and exact image references. The formerly missing producer cases no longer skip.

Acceptance reused only the identity-verified task cluster
`kognition-aae08983-a26a-4402-ba6b-1ebfad89979a` (Kubernetes 1.31.1,
Cilium 1.17.8). Image archives were checksum-verified across transfer and imported
for linux/amd64. Minimum-version policy typechecking used a separate fresh
`kognition-sg11-min-20261001-1a2fd855` cluster. No working installation was changed.

## Skips And Superseded Attempts

- Broad-suite Docker, memory, deployed acceptance and Windows shell-wrapper skips are covered by the explicit passing runs above. `closure.json` maps every skip to its evidence or retained external-test requirement.
- Two dashboard and one Studio skips are intentional desktop exclusions of mobile-only cases; those cases execute in the mobile project.
- Live standalone API smoke tests require a running seeded API/chat target. Live Foundry and Azure Search tests require cloud credentials. They remain deployment/production acceptance outside this local-only gate, not implied passes.
- One Docker stdio run timed out at its existing ten-second limit during concurrent validation. Its failure log is retained. The unchanged test binary subsequently passed every Docker case three consecutive times serially without increased deadlines or altered assertions. Timing sensitivity under contention remains an operational test caveat.
- Earlier image-download TLS failures and the older stale-core-chart candidate remain historical evidence; their results were not rewritten or relabelled as this candidate's passes.

## Integrity And Remaining Gates

The completion validator confirmed the owned source matched the frozen candidate,
the archive hash matched, the real Git index was untouched, and all nine protected
files retained their hashes. No commit, push or shared-cluster modification occurred.

This report and the backlog closure entry are post-validation status documents;
they do not claim the newly edited working/staged tree has the tested hash.
Any subsequent source or build-input change requires affected reruns and an updated
candidate attestation. SG-13 subsequently verified staged-tree equality for local
commit `261c5038f1c578bdc493b5bd4cb5fb7160fc0b88`; its tree is
`bc818b4178b2b3cdf1cb90c9c2eeb99b28eddfbf`. Executable inputs are unchanged
from this SG-11 candidate; four status documents were separately reviewed and
revalidated, with refreshed scans and renewed exact owner approval.

SG-13 preflight subsequently found that this snapshot inherited a protected
`admin_server_test.go` cleanup from its temporary baseline. Excluding that cleanup
from publication reproduces two lint failures. After initially declining, the
user explicitly authorized only that already-tested cleanup for SG-13. All other
protected paths remain excluded. The historical test results attest this exact
snapshot; the subsequent SG-13 checks validated the authorized local commit. See
[SG-13 local closure](security-gate-backlog.md#sg-13-review-exact-staging-and-publish-only-after-clearance).

SG-12 subsequently refreshed scans against this exact candidate and closed with
[explicit owner-approved dispositions](sg12-finding-dispositions.md). That separate
decision retains raw scanner failures, residual risks and a 24-hour freshness
limit. SG-11 closure itself grants no scanner exception. On 2026-10-02 the user
explicitly closed SG-13 without push or remote-SHA verification. Production
activation and remote publication remain unapproved; the push freeze is unchanged.