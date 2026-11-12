# Refactor Baseline: Tests And Ownership

- Plan task: RP01 (baseline inventory), second artifact. Complements [cloud SDK coupling](refactor-baseline-cloud-coupling.md).
- Date: 2026-11-10 to 2026-11-11.
- Source: `fix/refactor-baseline` at `cb5c92b` plus the test-fix commits that follow it, and uncommitted changes in `go.mod`, `go.sum`, CI, notices, the SPIRE fixture, `.trivyignore.yaml`, approval recovery and `tools/securitygate` that this work does not own.
- Host: Windows, the CI toolchain Go 1.26.10 (`GOTOOLCHAIN=go1.26.10`), Helm 3.19.0 (the version CI pins), Python 3.12, Node 22. Shared 12-CPU VM with other users' workloads; load 8 to 35.
- Not a release baseline. Counts below are from the runs described on this tree.

## Static inventory

[refactor-test-ownership.csv](refactor-test-ownership.csv) lists every tracked test file with its runner, build tag, static test count, skip sites and a proposed destination owner. Regenerate it with `tools/refactor/Get-TestInventory.ps1`. Counts are static: sub-tests and parameterized cases are not expanded, so they are lower than executed counts.

| Runner | Files | Tests | Skip sites |
| --- | ---: | ---: | ---: |
| Go | 523 | 3,927 | 59 |
| pytest | 14 | 220 | 11 |
| Playwright | 22 | 218 | 3 |
| PowerShell | 26 | n/a | 9 |

Go files by build tag: 451 default (3,715 tests, 21 skip sites), 70 `integration` (212 tests, 38 skip sites), and one each for `integration && windows` and `integration && !windows`.

### Proposed owners

Ownership is assigned by path only, using the layout decided in the [refactor plan](repository-refactor-plan.md): `integrations/<name>/`, `samples/` and core.

| Proposed owner | Files | Tests | Basis |
| --- | ---: | ---: | --- |
| core | 463 | 3,228 | default |
| core, flagged for review | 62 | 862 | file mentions a provider term |
| integrations/azure | 28 | 84 | path (`azuresearch`, `foundry`, `deploy/aks`, Azure scripts) |
| integrations/aws | 1 | 11 | path |
| integrations/anthropic | 1 | 11 | path |
| integrations/mem0 | 1 | 8 | path |
| samples | 29 | 161 | path (`samples/`, `tools/recording/`, `e2e/hackathon*`) |

The 62 flagged core files are the migration risk. Most only use a provider name as fixture data, but some assert provider behavior that would have to move with it. The largest are `internal/apiserver/handler_test.go` (108 tests, mentions Foundry), `internal/k8s/dispatcher/dispatcher_test.go` (88), `runtime/ingester/test_ingest.py` (57, Azure Blob and S3 fetchers), `runtime/agent/test_tools.py` (43) and `internal/controller/knowledge_base_reconciler_test.go` (41). RP04 must triage each flagged file; none can be moved or dropped by path alone.

Only 28 Azure files, and a single file each for AWS, Anthropic and Mem0, are provider-owned by path. That is far smaller than the provider surface in the source, which means provider assertions are mostly embedded in core tests today.

## Executed results

The runs mirror the jobs in `.github/workflows/ci.yml`. Environment that CI provides and a developer machine lacks was supplied explicitly: a disposable `pgvector/pgvector:pg16` container for `DATABASE_URL` and `KOGNITION_PGVECTOR_TEST_DSN`, envtest control-plane binaries (`setup-envtest use 1.30.x`) and `powershell-yaml` 0.4.12. A first run without the database showed 76.2% coverage and skipped DB-backed tests, so a run without CI's services understates coverage and is not comparable.

| Gate | Command (CI) | Result |
| --- | --- | --- |
| Format | `gofmt -l .` | clean |
| Lint | `golangci-lint run --new-from-rev=e759c24 ./...` | 0 issues (tagged build too) |
| Headers | `tools/copyright-headers.ps1 -Mode Check` and its tests | pass |
| Generated bindings | `buf lint`, `buf generate`, header apply, `git diff proto/gen` | clean; pinned-binding parity test passes |
| Traceability | seven `.github/scripts/*.sh` checks via Git Bash | all pass |
| Go unit | `go test -p=1 -race -coverpkg=./... ./...` | 7,771 passed, 0 failed, 95 packages, 14 skips |
| Go coverage | `check-go-coverage.sh --min 80` | 80.5% (floor 80%) |
| Go integration | `go test -tags integration ./...` | 8,503 passed, 0 failed, 84 packages, 44 skips |
| Python agent, ordinary profile | `pytest` in `runtime/agent` | 257 passed, 1 skipped (AutoGen not installed, by design) |
| Python real AutoGen 0.7.5 | CI's AutoGen step | 89 passed, 0 skipped; full agent suite 210 passed |
| Python ingester | `pytest` in `runtime/ingester`, clean environment | 76 passed |
| Python watchdog | `run_bounded.py ... pytest tools/tests` | 11 passed (needs a clean `TEMP`, see below) |
| Go vulnerabilities | `govulncheck ./...` | none reachable; one in a required module that is not called |
| Python dependencies | `pip-audit` on the three requirement files | none |
| Trivy fs v0.74.0 | HIGH,CRITICAL, unfixed ignored | pass with the pending SPIRE fixture, `.trivyignore.yaml` and `go.mod` bumps; fails on committed HEAD alone |
| SPIRE fixture dispositions | `go run ./tools/securitygate identity-fixture` | pass (pending files) |
| Helm and deploy suites | ten PowerShell suites with Helm 3.19 | all pass (core 508 assertions, Azure Search 28) |
| Browser suites | dashboard, Studio, tutorial | 30 passed 2 skipped; 77 passed 1 skipped; 6 passed |
| Recording tools | `node --test` on 18 files | 156 passed |

### Failures found and fixed

Each was reproduced first and verified afterwards. None of the first four were caused by the refactor work except the first.

| Finding | Cause | Fix |
| --- | --- | --- |
| `tools/securitygate` `TestSQLDisposition` | Migration 036 raised the rendered chart to 36 migrations; the gate pins the exact count and digest so SQL changes get human review. The committed pin expects 34 and the pending edit 35. | Reviewed 036, pinned 36 and the new digest. Recomputing the digest without 036 reproduces the prior pin exactly. Committed as `cb5c92b`. |
| `api/k8s/v1alpha1` `TestKnowledgeBackendBindingCRDContract` | CRD gained `embeddingModelBinding` and `reclaimPolicy` earlier; the test still asserted 12 properties. Pre-existing. | Expect 13 and assert the embedding binding contract. |
| `cmd/kognition-knowledge-pgvector` manager test | `run` now requires `KOGNITION_PGVECTOR_DATABASE_ADDRESS` first. Pre-existing. | Test supplies an approved address. |
| `deploy/aks` install and runtime-values suites | Stale against the current script (destination verified through `docker buildx` by tag, no `az`) and chart (Kubernetes-mode migrations are a content-addressed Job, not a hook). Pre-existing, reproduced on `e759c24`. | Test mocks and assertions updated. |
| `internal/controller` informer test | About 1.1 million dispatches inside a 2 second wait; failed twice on a loaded host, passes alone. | 30 second budget for that test only. Assertions unchanged. |
| `providers/pgvector` lifecycle test | `DROP DATABASE` of the test's own database raced with a closing backend. | `WITH (FORCE)`. |
| Dashboard Trust Decision test | Observed a transient loading state through a 100 ms mock delay. | Mock holds the response until the loading text is asserted. |

### Environment findings

- The watchdog test `test_pytest_configuration_terminates_blocked_test` failed on this host at every load. Pytest 9 lists sibling entries along the path to a test file outside the rootdir, and `%TEMP%` has over 22,000 entries here, so startup took about 16 seconds against the test's 30 second budget. With an empty `TEMP` the whole suite passes. A CI runner's temp directory is small, so this is not a product defect.
- A full-module race run under load 25 to 35 exceeded the 10 minute package timeout for `internal/store/postgres` and left an orphaned embedded PostgreSQL on port 15432, which made the next run fail at start with "process already listening". The package passes in 246 seconds in isolation. Check for and stop an orphaned embedded server (by its `kognition-store-postgres-*` path) before rerunning.
- `golangci-lint` built with an older Go than the local toolchain panics on current sources. Rebuild it with the CI toolchain.

### Remaining skips (not failures)

Go skips by reason: cluster-deployed and SPIRE acceptance (about 14, covered in the earlier deployed acceptance on a disposable cluster, which was not rerun against this source), isolated Compose, TLS and authority fixtures, live API server, paid cloud tests (Foundry, Azure Search, paid embeddings; not authorized), image-based AutoGen tests, Docker-engine runner tests, bash-script wrappers on Windows (the same scripts were run directly through Git Bash and pass), and shell-entrypoint acceptance. The opt-in memory-handle tests, including `TestPgvectorPerVolumeDimensions`, were run separately against a migrated `migration_check_*` database and pass.

## Not run

- Cluster-deployed acceptance against this source. The reviewed-target gate passed (614 seconds) on the disposable cluster before the controller and dispatcher fixes, which are not deployed. Provider-binding and AutoGen children are skipped.
- Cloud-dependent tests that incur cost or need credentials: Foundry live, Azure Search live, paid embeddings.
- Isolated fixtures: SPIRE Workload API, Compose deployed trust, TLS database, authority and live API server.
- Docker-engine runner tests: the runner dials a Unix socket, which this Windows host's Docker (named pipe) does not expose. Needs a Linux host.
- Shell-entrypoint acceptance: needs `make`, which is not installed here.
- The e2e PowerShell scripts, performance, soak and cold/warm local-model qualification.

## Known gaps carried into the plan

- Standalone governed Knowledge Gateway (RP18/RV22) is open.
- Model-driven tool use and agent memory through a real local model are unproven (see the stabilization follow-up in the plan).
- Two defects found during live activation are fixed in source but not deployed: Tool health probe interval and gateway-mode dispatch for profiled non-OpenAI providers.
- The Azure SDK is linked into twelve core binaries; see the coupling baseline.

## Completion check for RP01

Still needed before RP01 can close: baseline results for the "Not run" items on suitable infrastructure (a Linux host with `make` and a Docker socket, an isolated cluster, and approved cloud access), a reviewed operation-level matrix of registered resources and supported modes, and triage of the 62 flagged core test files. The authored coverage margin is thin (80.5% against an 80% floor), so refactor steps that move code must keep their tests with it. This document does not authorize extraction or claim parity.
