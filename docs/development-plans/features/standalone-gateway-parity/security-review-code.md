# Standalone Gateway Pre-Commit Review

**Mode:** Code
**Date:** 2026-09-15
**Reviewer:** GitHub Copilot
**Scope:** Standalone gateways, migrations, Tool coordination,
scoped AgentService authentication, durable CPI and Memory recovery, and trustworthiness
rehearsal changes.
**Verdict:** Local pre-commit findings resolved and verification passed.
Deployment and recording acceptance remain outstanding.

## Findings

### SEC-C001: Existing Go Toolchain Has Reachable Advisories

**Severity:** Medium, provisional pending deployment-specific exploitability assessment.
**Category:** Vulnerable components.
**Location:** [Dockerfile](../../../../Dockerfile#L4) and
[go.mod](../../../../go.mod#L3).

The affected-package `govulncheck` scan exited with code 1 and reported seven
reachable standard-library advisories in Go 1.26.5. All identify Go 1.26.6 as the
fixed version. The Docker builder and module version already used 1.26.5 in
HEAD, so this is a baseline issue rather than a regression introduced here.
Reachability does not establish exploitation through the deployed configuration.

| Advisory | Component |
| --- | --- |
| GO-2026-6218 | net/url path resolution complexity |
| GO-2026-6091 | html/template JavaScript regexp context |
| GO-2026-6090 | crypto/tls post-handshake message limits |
| GO-2026-6089 | net/http unencrypted HTTP/2 header timeout |
| GO-2026-6088 | encoding/xml recursion guard |
| GO-2026-5972 | encoding/asn1 recursion depth |
| GO-2026-5026 | net/http IDNA label validation |

Update the build toolchain to a patched release, rebuild the affected binaries,
and rerun the dependency scan and focused tests before claiming clean security
verification. The scan also reported one imported-package and 18 required-module
advisories without detected vulnerable calls; these are not additional proven
runtime exposures.

### REV-C002: Migration Test Cleanup Can Leave Caller State Changed

**Severity:** Low.
**Category:** Test reliability.
**Location:** [tests/standalone-migrations.ps1](../../../../tests/standalone-migrations.ps1#L93).

The cleanup block drops its temporary database before restoring `DATABASE_URL`,
`KOGNITION_MIGRATION_BASELINE`, and the location stack. If the drop throws, all
three restoration steps are skipped and the cleanup exception can mask the
original failure. An isolated execution of the actual cleanup statements with
an injected SQL failure reproduced the problem without accessing a database.
The probe restored its own caller state afterward.

Place environment and location restoration in an inner `finally` that runs even
when database cleanup fails, and add a failure-injection regression test.

## Initial Review Verification

| Check | Result |
| --- | --- |
| Fourteen affected Go package and command targets | Passed, uncached; includes PostgreSQL and cancellation tests |
| Named production contract gate | All 31 required tests passed |
| Five PowerShell regression gates | Parity, cleanup, session, correlation, publication passed |
| Publication target unit tests | All 3 passed: persistence, concurrent replay, invalid input |
| Focused editor diagnostics | No reported errors in inspected implementation files |
| Candidate-file credential signature scan | No high-confidence signatures found; heuristic only |
| Whitespace validation before report creation | Passed |
| Dependency audit | Failed as described in SEC-C001 |

Reviewed controls include exact-route service authentication, separation of
invocation and runtime credentials, trusted-origin routing and redirect denial,
Gate3 wiring, parameterized PostgreSQL coordination, migration checksum and
baseline checks, bounded cancellation-independent holder cleanup, publication
idempotency, and retention-aware rehearsal cleanup. No additional confirmed
production regression was identified in these reviewed paths.

Environment and known local credential files are ignored. Compose retains
documented development-only credentials, plaintext local transport, and Docker
socket access; this review does not approve that configuration for production.

## Remediation Verification

The initial findings above describe the pre-remediation state. The following
checks close them for this local commit candidate on 2026-09-14.

| Check | Result |
| --- | --- |
| SEC-C001 toolchain update | Module and Docker builder now pin Go 1.26.6 |
| Affected-package dependency audit | Passed; zero reachable or imported-package vulnerabilities, 19 required-module advisories without detected vulnerable calls |
| REV-C002 caller-state restoration | Nested `finally` restores environment and location even when database cleanup throws |
| Cleanup failure regression | Failed before the fix and passed afterward for configured and absent environment values |
| Full Go suite | `go test ./... -count=1 -timeout=20m` passed |
| Prometheus restart evidence | TC-0243 scrapes retained terminal counts and owned step durations before and after collector recreation |
| Rehearsal traceability | Primary and fallback contract assertions match actual correlation, inventory, retention, and unavailable-claim behavior |
| Docker build | Go 1.26.6 builder compiled all listed binaries and produced the migrator image |
| Real pgvector migration chain | All 30 migration files passed fresh install, explicit legacy baseline, repeat startup, and resource and Memory row preservation |
| Focused editor diagnostics | No errors in the modified migration scripts, observability test, traceability tests, or module file |

The cleanup regression executes the public migration script with injected Docker
failures. A cleanup exception still propagates and may mask the original failure;
the fix guarantees caller-state restoration, not preservation of both exceptions.
The migration build was interrupted once by terminal reuse, then rerun successfully.

## Durable CPI Recovery Review

Completed synchronous CPI responses are persisted after usage and completion
evidence. An atomic namespace/instance-operation claim prevents competing
dispatchers from repeating the provider call. Stored responses are outside the
generic resource, event, and watch APIs. Cache and durable lookups enforce the
instance operation, namespace, and session binding. Recovery returns an existing
result without reauthorizing a new provider operation; it does not execute one.
The controller still exposes the final output in the existing instance status.

| Check | Result |
| --- | --- |
| Restart lifecycle | Reopened PostgreSQL backend and recreated controller recover output, session, and five-token usage; one provider call |
| Fail-closed recovery | Pending claims, failed completion persistence, corrupted records, and mismatched instance/session lookups rejected |
| Storage coordination | Twelve competing claims produce one winner; wrong-session updates and overwrites rejected |
| Response validation | Malformed JSON and non-object values rejected; stored content recovered intact |
| Full Go suite | `go test -p 1 ./... -count=1 -timeout=20m` passed |
| Gateway process test | Passed isolated and in serialized full suite; initial parallel suite hit its existing 60-second build deadline |
| Real migration chain | All 31 migration files passed fresh install, explicit legacy baseline, repeat startup, and retained resource/Memory checks |

A provider result lost before durable completion remains uncertain and cannot be
automatically retried. This is not exactly-once provider execution or recovery of
an unsaved response. The lifecycle test reconstructs pools and controllers with a
controlled provider, not an OS-process kill or paid-provider fault injection.
Private completion records have no automatic cleanup; database access, backups,
and data retention must account for their sensitive output. Deleting claims to
force retries is unsafe. These limits do not grant general production approval.

## Durable Memory Replay Review

Terminal Memory receipts are read after current capability, identity, role,
grant, and Gate3 authorization. Replay verifies namespace, invocation, operation,
binding, completion, response format, and approval execution state. Recovery of
a claimed approval uses its existing resource version and fencing token. Invalid
or unavailable evidence fails closed without preparing or repeating a mutation.
The preparation contract remains unchanged.

| Check | Result |
| --- | --- |
| Real pgvector restart | Reopened PostgreSQL backend and recreated gateways return identical response bytes and content type; one stored row and one target mutation |
| Current authorization | Changed payload and wrong volume rejected; revoked grant blocks both new writes and replay of a successful invocation |
| Receipt failures | Mismatched identity fields, missing completion, corrupt payload, unsupported format, unsafe content type, contradictory state, and lookup/finalization failures rejected |
| Response fidelity | Store and delete replay preserve formatted JSON, JSON strings, plain text, and empty bodies |
| Replay audit | Records `replayed`, `replay_detected`, and `targetReached=false`; no additional target completion claimed |
| Full Go suite | `go test -p 1 ./... -count=1 -timeout=20m` passed |
| Real migration harness | `tests/standalone-migrations.ps1 -VerifyMemory` passed all 31 migrations and the Memory integration test |
| Cleanup failure regression | Restores configured and absent Memory database settings, other migration settings, and caller location |

New versioned receipts retain response bytes and content type. Legacy receipts
remain readable, but their original whitespace and content type were not retained
and cannot be reconstructed exactly. The read interface is currently implemented
by PostgreSQL; this does not establish Kubernetes replay parity. A target mutation
whose result was never durably saved remains uncertain and cannot be retried
automatically. Private receipts contain response data and require database access
controls and retention governance.

The integration test uses real controller-created identity and Running instance
UID/session, a test-issued capability, and a controlled CPI driver. It reconstructs
backends and HTTP handlers; it does not kill an OS process or verify controller
delivery of capabilities to an actual runtime.

## Standalone Identity Prerequisite

The production controller manager now requires an automatic managed identity for
every dispatch, including unannotated instances without Tool or Memory references.
Previously only the integration fixtures enabled this requirement globally.
Manager wiring and public reconciliation tests pass for missing, unmanaged,
pending, and ready identities. The first three cases remain Pending without
dispatch or retry consumption; a ready managed identity permits dispatch.
Existing standalone instances without an eligible identity must wait for identity
reconciliation before starting a new session. Already-running sessions are not
changed by this startup gate.

`go test ./cmd/kognition-controller-manager ./internal/controller -count=1`
passed after this change. This is a prerequisite, not capability delivery: the
standalone CPI dispatcher currently invokes providers directly and has no runtime
session endpoint to receive gateway capabilities.

## Kubernetes Capability Activation

The HTTP dispatcher previously waited for a persisted Running session only when
the invocation carried a Memory capability. Tool-only, Model-only, and
delegation-only invocations could reach the runtime before gateway session
validation could accept them. All four capability types now use the same
activation gate and require the stored session ID to match.

The public Dispatch regression uses real capability issuers and a controlled
store to reject delivery while Pending or Running with a different session ID.
After activation, it checks the runtime authentication header, session ID, and
signed capability binding. Restoring the old condition failed the Tool, Model,
and delegation cases; restoring the fix passed all four cases and the full
dispatcher package. The fixture uses an OpenAI-compatible Model to exercise the
existing Model Gateway provider guard.

This is local HTTP-dispatch evidence. It does not establish gateway execution,
standalone runtime delivery, or deployed parity. No capability is added to model
prompts, resource status, or logs by this change.

## Verification Limits

- No Git remotes are configured. GitHub secret-scanning, CodeQL, Dependabot, and
  repository advisory checks could not be associated with a repository.
- The migration image and its shared binary build passed, but the gateway runtime
  images were not redeployed. Previously healthy deployments do not certify this
  commit candidate.
- Cancellation regression coverage uses a Kubernetes fake client. A real
  API-server cancellation test remains absent; PostgreSQL shared-state tests
  provide separate backend coverage.
- Deployed four-plane standalone/Kubernetes parity, three consecutive clean
  rehearsals, and presenter recording acceptance remain outstanding.
- Controller-issued active capability delivery remains a gap. Durable CPI output
  recovery and PostgreSQL Memory replay are covered by the local evidence above,
  not yet by a deployed parity rehearsal.

`deployedDemoParityVerified` and `recordingReady` remain false. Local verification
does not establish production readiness. No security retention was bypassed.