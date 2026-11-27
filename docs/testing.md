# Testing Strategy

## Purpose

Kognition uses layered tests because standalone and Kubernetes modes share resource contracts but differ at infrastructure boundaries. A green unit suite does not prove Kubernetes RBAC, Service routing, network isolation, mounted Secrets, or external provider compatibility.

The suite reduces regression risk. It cannot prove the absence of every defect. Each new behavior must add coverage at the lowest layer that exercises its public contract and at every deployment boundary it introduces.

CI requires at least 80% authored Go statement coverage. The gate reads the
module-wide profile using `-coverpkg=github.com/kognition-ai/kognition/...`,
de-duplicates repeated blocks, and excludes only generated Protobuf sources and
`zz_generated*.go` files. Run it locally after producing a profile:

```powershell
python tools/run_bounded.py --timeout 1800 -- go test ./... -count=1 -timeout=3m "-coverpkg=github.com/kognition-ai/kognition/..." "-coverprofile=coverage.out"
bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80
```

**Bash (Linux/macOS):**

```bash
python3 tools/run_bounded.py --timeout 1800 -- go test ./... -count=1 -timeout=3m \
    "-coverpkg=github.com/kognition-ai/kognition/..." "-coverprofile=coverage.out"
bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80
```

The PowerShell example also needs Bash, for example Git Bash on Windows, for
the coverage-check script.

## Test Tiers

For local selection and evidence reuse, follow the
[change-based gate policy](development-gates.md). Existing automated CI remains
required; the manual SG-01 through SG-13 campaign is not repeated for every source
change. Release certification and exact-target activation are separate boundaries.

| Tier | Runs | Required coverage |
|---|---|---|
| Go unit | Every pull request | Pure logic, conversions, validation branches, controller state transitions, error mapping |
| Tagged integration | Every pull request | Public HTTP API, production admission chain, reconciliation with in-memory collaborators, CPI and memory contracts |
| Python runtime | Every pull request | Agent session handling, runtime authentication, memory capability forwarding, tracing, graph behavior |
| Helm | Every pull request | Rendered workload configuration, RBAC, Secrets, network policies, image wiring |
| Envtest | Every pull request | CRD schema, real API-server admission webhooks, mutation and validation, status subresources |
| Kind | Protected branch or nightly | Controllers, Deployments, Services, Jobs, Secret mounts, RBAC, network policies, finalizers |
| Provider E2E | Nightly or release candidate | Foundry, Azure Blob, real embeddings, configured external MCP or OpenAPI services |

Reviewed-target producer qualification gives ingestion, Agent runtime and optional
AutoGen lifecycle phases independent bounded contexts. Later producer checks
must not inherit a deadline already consumed by ingestion. This separates fixture
budgets without changing application startup/invocation limits, denial probes,
cleanup assertions or the enclosing acceptance timeout. Preserve failed attempts
and recheck exact resource absence before any retry or activation review.

The complete reviewed-target run needs more wall-clock time than its phase
budgets suggest on a loaded shared host: a 2026-11-09 run on a VM carrying other
clusters exceeded a 15 minute `go test` deadline. The process-wide alarm then
killed the run during fixture cleanup and left temporary controller RoleBindings,
copied Roles and a disposable database Pod. The test now refuses to start with
less than 25 minutes remaining. Run it with `-timeout 30m`.

If a run is killed anyway, the installation stays inactive but bootstrap
verification rejects the residual controller grants. Identify them by the exact
`target-probe-<run>-<n>` names and UIDs recorded in the failed run, review each
object's subject and role, and delete only those objects with UID and
resource-version preconditions. The run can also leave a disposable database Pod
and fixture custom resources (KnowledgeBase, Model and SecretRef) in the workload
namespace; they carry the `kognition.dev/admission-fixture-owner` annotation, and
the next run refuses to adopt them ("existing fixture left untouched"). Inventory
every `kognition.dev` resource, not only Pods and RBAC, and leave Helm-owned
platform objects such as the default MemoryClass alone. Confirm absence and rerun
the inactive verification before another attempt. Do not delete platform grants or
reuse the failed report as acceptance evidence.

## Local Commands

Run plugin socket lifecycle regressions on Linux using the Go version declared
in `go.mod`:

```bash
go test -race -count=100 -timeout=5m ./internal/plugin
```

Socket replacement checks cover both file identity and modification time because
Linux filesystems can reuse an inode when a plugin recreates its socket at the
same path. Keep the preserved-identity regression deterministic rather than
relying on a filesystem to reproduce inode reuse.

The [SQL disposition verifier](../tools/securitygate/gate.go) pins the reviewed
migration payload, including filenames and license comments. For any migration
byte change, review the complete delta, verify the PostgreSQL and Helm copies
match, and refresh the reviewed digest without bypassing the content check.
Run `go test ./tools/securitygate`; its negative controls reject unreviewed SQL,
comment and filename changes even when the migration count is unchanged.

```powershell
# Fast Go unit suite
.\make.ps1 test

# In-process public API and reconciliation integration suite
.\make.ps1 integration

# Offline model export integration tests require this pinned YAML module.
# CI installs it before running the integration suite.
Install-Module powershell-yaml -RequiredVersion 0.4.12 -Scope CurrentUser

# Verify the exact identity fixture and its scoped scanner dispositions.
go run .\tools\securitygate identity-fixture

# Python agent runtime tests
.\.venv\Scripts\python.exe -m pip install -r runtime\agent\requirements-test.txt
.\.venv\Scripts\python.exe tools/run_bounded.py --timeout 600 -- .\.venv\Scripts\python.exe -m pytest -q runtime\agent

# Python KB ingester tests
.\.venv\Scripts\python.exe -m pip install -r runtime\ingester\requirements-test.txt
.\.venv\Scripts\python.exe tools/run_bounded.py --timeout 600 -- .\.venv\Scripts\python.exe -m pytest -q runtime\ingester

# Helm render and contract checks
helm lint deploy/helm/kognition
& .\deploy\helm\kognition\tests\helm-regression-tests.ps1

# Deployed Kubernetes memory gateway, raw-service authorization boundary,
# revoked-grant provenance filtering, lifecycle/cascade audit, and denied
# operation audit latency for get/list/search.
.\make.ps1 e2e-kubernetes-memory NAMESPACE=kognition-system RELEASE=kognition

# Deployed Tool Gateway session exchange, exact invocation, replay-safe receipt, durable ledger, and revocation.
# Requires the Tool Sandbox sample from .\make.ps1 samples-install.
.\make.ps1 e2e-kubernetes-tool NAMESPACE=kognition-system RELEASE=kognition

# Deterministic model-selected Tool call through the deployed runtime and Tool Gateway.
# Uses an in-cluster OpenAI-compatible mock and requires the Tool Sandbox sample.
.\make.ps1 e2e-kubernetes-runtime-tool NAMESPACE=kognition-system RELEASE=kognition

# Deployed Model Gateway authorization and accounting through an enforcing CNI.
# Uses an in-cluster OpenAI-compatible provider and verifies immutable token/pricing
# facts, normalized micro-USD and model-tier quota projection, exact-limit denial,
# live grant/policy revocation, provider credential isolation, and runtime redaction.
.\make.ps1 e2e-kubernetes-model NAMESPACE=kognition-system RELEASE=kognition

# Class-approved one-shot exec Tool health probes. The Kubernetes controller
# creates a non-root, deny-egress Job and verifies Ready, Degraded, recovery,
# and cleanup. Requires a controller image containing the exec probe runner.
.\make.ps1 e2e-kubernetes-tool-exec-probe NAMESPACE=kognition-system RELEASE=kognition

# Standalone Docker Engine sandbox probe integration. Run from a Linux
# container or host with a Unix Docker socket mounted at /var/run/docker.sock.
$env:KOGNITION_DOCKER_EXEC_PROBE_INTEGRATION = "1"
go test ./internal/controller -run TestDockerExecProbeRunnerFunctional -count=1

# Enforcing-CNI Tool Gateway test. Verifies capability-authorized Shell Tool
# invocation through a durable ToolInvocationRun, a controller-owned non-root
# deny-egress sandbox, returned MCP output, and sandbox cleanup.
.\e2e\kubernetes-tool-gateway-network-policy-e2e.ps1 `
	-KubeContext kind-kognition-cilium-e2e `
	-ControllerNamespace kognition-cilium-system `
	-ReleaseName kognition-cilium

# Opt-in live Tool Gateway capability-key rotation. This changes the deployed
# Tool Gateway Secret and retains the previous key during the overlap window.
.\make.ps1 e2e-kubernetes-capability-rotation NAMESPACE=kognition-system RELEASE=kognition

# Cross-workspace AgentService delegation through the deployed Agent Communication Gateway.
# Uses two gateway replicas and in-cluster mock Models to verify real source/target
# runtime sessions, cross-replica receipt-safe replay, conflicting replay rejection,
# immediate policy revocation, and replica restoration.
.\make.ps1 e2e-kubernetes-agent-communication NAMESPACE=kognition-system RELEASE=kognition

# Webhook envtest suite after installing envtest assets
$env:KUBEBUILDER_ASSETS = (setup-envtest use 1.30.x -p path)
go test -tags=integration -count=1 ./internal/k8s/webhook/...
```

**Bash (Linux/macOS):**

Install PowerShell 7 for the repository's `.ps1` test helpers. Create the virtual
environment first if needed with `python3 -m venv .venv`; on Windows use
`python -m venv .venv`. Install the pinned YAML module before integration tests
in either shell.

```bash
pwsh -NoProfile -Command 'Install-Module powershell-yaml -RequiredVersion 0.4.12 -Scope CurrentUser'
make test
make integration
go run ./tools/securitygate identity-fixture

./.venv/bin/python -m pip install -r runtime/agent/requirements-test.txt
./.venv/bin/python tools/run_bounded.py --timeout 600 -- ./.venv/bin/python -m pytest -q runtime/agent
./.venv/bin/python -m pip install -r runtime/ingester/requirements-test.txt
./.venv/bin/python tools/run_bounded.py --timeout 600 -- ./.venv/bin/python -m pytest -q runtime/ingester

helm lint deploy/helm/kognition
pwsh -NoProfile -File ./deploy/helm/kognition/tests/helm-regression-tests.ps1

# These are deployed tests; use only the separately approved test installation.
make e2e-kubernetes-memory NAMESPACE=kognition-system RELEASE=kognition
make e2e-kubernetes-tool NAMESPACE=kognition-system RELEASE=kognition
make e2e-kubernetes-runtime-tool NAMESPACE=kognition-system RELEASE=kognition
make e2e-kubernetes-model NAMESPACE=kognition-system RELEASE=kognition
make e2e-kubernetes-tool-exec-probe NAMESPACE=kognition-system RELEASE=kognition

# Linux host/container with the Unix Docker socket available.
KOGNITION_DOCKER_EXEC_PROBE_INTEGRATION=1 \
    go test ./internal/controller -run TestDockerExecProbeRunnerFunctional -count=1

pwsh -NoProfile -File ./e2e/kubernetes-tool-gateway-network-policy-e2e.ps1 \
    -KubeContext kind-kognition-cilium-e2e \
    -ControllerNamespace kognition-cilium-system -ReleaseName kognition-cilium

# Explicitly approved live key-rotation and cross-workspace tests.
make e2e-kubernetes-capability-rotation NAMESPACE=kognition-system RELEASE=kognition
make e2e-kubernetes-agent-communication NAMESPACE=kognition-system RELEASE=kognition

export KUBEBUILDER_ASSETS="$(setup-envtest use 1.30.x -p path)"
go test -tags=integration -count=1 ./internal/k8s/webhook/...
```

The SPIRE acceptance fixture keeps reviewed host-attestation, kubelet and CSI
privileges. Its scanner policy applies only to that exact fixture and finding
set; CI verifies both source and policy digests before scanning. Changed
fixtures, broader paths, or vulnerability suppressions fail the guard and
require new review. Dependency and secret scans remain unfiltered. These source
checks do not establish deployed identity acceptance or authorize activation.

## Native Checkpoint Compatibility

The native adapter's version-1 checkpoint is an opaque, bounded token containing
the session checkpoint and complete conversation, including tool-call/result
bindings. Checkpoint and concurrent Step requests are rejected while a turn is
active. Resume validates the whole token before allocating a session and does
not re-execute completed tools.

Legacy session-only tokens are rejected because their missing conversation
cannot be restored faithfully. Finish existing turns and create a new session
when migrating a runtime with legacy tokens. Invalid, oversized, unknown-version
or ambiguous tokens must not allocate a new session. The native token limit is
4 MiB; do not truncate tokens or bypass the quiescence check.

Tokens contain sensitive conversation and tool-result data. Keep them out of
logs and shared evidence; use protected storage and access controls. This
format is native-adapter state, not an interchangeable Python/AutoGen token.
The optional AutoGen worker uses its SDK state; its complete HTTP checkpoint
transport and both-mode conformance remain separate completion requirements.

## Reviewed Local Target Acceptance

`TestDeployedReviewedAdmissionTarget` in
[the deployed admission suite](../internal/traceability/admission_deployed_integration_test.go)
tests the actual reviewed installation instead of a separate chart fixture.
It is opt-in, requires an isolated loopback cluster (or, only for the reviewed AKS
profile below, a dedicated remote cluster) and an inactive, drained
controller, and must not be run against an active demo or production installation.
Keep external writers frozen throughout testing and subsequent activation review.

Required inputs use the `KOGNITION_ADMISSION_` prefix:

| Suffix | Required value |
| --- | --- |
| `KUBECONFIG`, `EXPECTED_CLUSTER_UID` | Explicit target file and exact kube-system UID |
| `TARGET_MANIFEST`, `TARGET_SHA256` | Reviewed inactive render and exact file hash |
| `WRITERS_FROZEN`, `ISOLATED` | Both explicitly `true` |
| `CNI`, `STREAM_IMAGE` | `cilium` and the preloaded reviewed BusyBox image |
| `INGESTER_IMAGE`, `DATABASE_IMAGE`, `AGENT_IMAGE` | Verified images available in that node |
| `TARGET_RUNTIME_SECRET` | Actual installation runtime source Secret name |

### Dedicated remote cluster (AKS)

A non-loopback API endpoint is accepted only with
`KOGNITION_ADMISSION_NETWORK_PROFILE=reviewed-aks-cilium-1.18.15`. That profile
pins the AKS-managed Cilium image and enforcement settings and verifies the exact
agent digest on every node. In addition to the inputs above, set:

| Suffix | Required value |
| --- | --- |
| `NETWORK_PROFILE` | `reviewed-aks-cilium-1.18.15` |
| `REMOTE_ENDPOINT` | Exactly the API endpoint pinned in the installation plan and kubeconfig |
| `DEDICATED_CLUSTER_UID` | Your attestation that the cluster with this kube-system UID is dedicated to this installation |
| `PERMITTED_NAMESPACES` | Optional comma-separated operator namespaces that may exist besides the reviewed ones |
| `NETWORK_FIXTURE_NAMESPACE`, `NETWORK_FIXTURE_NAMESPACE_UID`, `RECORDING_RUN` | Operator fixture namespace, its exact UID and recording run, as for any reviewed Cilium profile |

TLS must be verified, and the cluster may hold no namespace beyond `default`,
`kube-*`, the reviewed installation, workload, authority and fixture namespaces
and those you list. Install the chart with `global.aksAdmissionsEnforcerOptOut=true`
so AKS leaves the reviewed webhook selectors unchanged. See the
[AKS runbook](../deploy/aks/README.md) and the
[SG-07 record](development-plans/features/knowledge-provider-execution/sg07-local-acceptance.md)
for the exact settings and the recorded single-cluster result.

The test performs six native-policy probes per enrolled namespace, real Cilium
isolation and kubelet attachment checks, production stdio execution/cleanup,
restricted ingestion and authenticated agent lifecycle checks. It temporarily
grants test identities, exercises baseline replacement, rotates/deletes the runtime
source credential and restores it, and verifies final drain. These are mutations;
review their scope first and prepare activation evidence only after they finish.
Never copy generated tokens or Secret contents into reports.

For the reviewed legacy pgvector profile with external provider routing disabled,
the local 2026-10-02 run explicitly selected:

This recorded selection uses shell-neutral arguments (Bash or PowerShell):

```sh
go test -json -tags=integration ./internal/traceability -count=1 -timeout=15m -run '^TestDeployedReviewedAdmissionTarget$/^(native-policies|deployed-attach|production-stdio|production-ingestion)$/^(success|deadline|non-root-failure|agent-runtime)$'
```

That selection passed nine parent/subtest events with zero skips. It excludes the
external provider-binding case because that feature is disabled in this profile;
it cannot certify a provider-enabled installation. Preserve raw JSON events,
image/manifest hashes, failed attempts and cleanup results. Verify every required
case ran; package PASS, skipped opt-in tests or healthy Deployments alone are not
activation approval. Follow the [activation contract](features/knowledge-provider-execution/activation-transaction-contract.md).

The [Trustworthiness runbook](../samples/trustworthiness/runbook.md)
describes the separate post-activation workload and UI checks. Its PowerShell
publication/session regressions can run without changing the cluster:

```powershell
.\e2e\trustworthiness-publication-test.ps1
.\e2e\trustworthiness-session-test.ps1
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./e2e/trustworthiness-publication-test.ps1
pwsh -NoProfile -File ./e2e/trustworthiness-session-test.ps1
```

## Test Deadlines

- Local Go `test` and `integration` targets require Python 3 for the standard-library watchdog. They enforce a 30-minute total deadline, including compilation and discovery. Go retains its 3-minute unit and 15-minute integration per-package limits.
- Python tests require `pytest-timeout` through each runtime's test requirements. The root configuration enforces 120 seconds per test, including fixture setup and teardown, and prints thread stacks before aborting a stuck session. Missing timeout support fails collection.
- Use `tools/run_bounded.py --timeout SECONDS -- COMMAND` for whole-command protection. It inherits output, preserves command exit status, requests process-tree termination on timeout, and returns `124`. Cleanup has an additional bounded allowance. If the OS reports incomplete cleanup, the runner warns explicitly; check for surviving child processes. Timeouts are failures, never successful test results.
- CI caps Go test jobs at 30 minutes, the Python agent job at 15 minutes, and the ingester test step at 10 minutes.
- Run editor test-tool requests sequentially with actual test-file paths, not directory paths. If the editor still shows a run after its test processes have exited, cancel that stale run or reload the window. Use the bounded CLI result as evidence; an editor timer alone does not prove a test is executing.
- Verify timeout protection with `python -m pytest tools/tests -q`. Its intentional blocking probes run in subprocesses and must exit with the expected timeout failure.

## Live Azure AI Search Acceptance

The opt-in `TestLiveAzureSearchLifecycle` test uses the real provider client,
indexer, querier, and cleaner against an existing, approved disposable Search
index. It creates no Azure resources or index schema. Normal unit runs exclude
the test; tagged integration runs skip it unless explicitly enabled.

Prerequisites:

- An Azure AI Search service and provider-compatible index with filterable
	ownership fields (`namespace`, `knowledgeBaseUid`, `generation`, `operationId`,
	`configurationUid`, `configurationDigest`), retrievable citation fields,
	searchable `content`, and a configured `embedding` vector field.
- `KOGNITION_AZURESEARCH_DIMENSIONS` matching that vector field's dimensions.
- An identity available to `DefaultAzureCredential`, with document read/write
	access to the approved index. The deployed provider still uses workload
	identity; this harness also permits an existing developer credential.
- Approval to write temporary documents and incur Search requests. Never point
	this test at a production index.

Run after setting the service, index, and dimension variables to approved values:

```powershell
$env:KOGNITION_AZURESEARCH_LIVE_TEST = "1"
try {
		.\.venv\Scripts\python.exe tools/run_bounded.py --timeout 420 -- go test -tags=integration -count=1 -timeout=6m -run '^TestLiveAzureSearchLifecycle$' -v ./providers/azuresearch
} finally {
		Remove-Item Env:KOGNITION_AZURESEARCH_LIVE_TEST -ErrorAction SilentlyContinue
}
```

**Bash (Linux/macOS):**

```bash
KOGNITION_AZURESEARCH_LIVE_TEST=1 ./.venv/bin/python tools/run_bounded.py \
    --timeout 420 -- go test -tags=integration -count=1 -timeout=6m \
    -run '^TestLiveAzureSearchLifecycle$' -v ./providers/azuresearch
```

The Bash assignment applies only to this command and its children; the opt-in
does not remain enabled in the parent shell.

The required target variables are `KOGNITION_AZURESEARCH_SERVICE`,
`KOGNITION_AZURESEARCH_INDEX`, and `KOGNITION_AZURESEARCH_DIMENSIONS`.
The test checks exact visible index counts, keyword/dense/hybrid queries,
document content and citations, namespace isolation, generation isolation after
refresh, Retain preservation, and Delete isolation. It uses synthetic vectors,
so it does not evaluate embedding quality or relevance ranking.

Each run logs its unique ownership ID and registers bounded cleanup, including
when assertions fail. Process termination or lost service access can prevent
cleanup; inspect the logged ownership ID and operation IDs before removing any
leftover documents. A skipped run is not live-service acceptance evidence.
This provider-level test does not exercise Kubernetes reconciliation, binding
promotion, gateway authentication, runtime retrieval, or persisted Retain records.
Those remain separate end-to-end gates.

## Required Regression Slices

### Control Plane

| Workflow | Required assertions | Current test tier |
|---|---|---|
| Resource CRUD, revisions, events, and watches | API identity, conflict behavior, SSE ordering | Tagged integration |
| Admission and policy | Defaulting, deny reasons, namespace isolation, authenticated caller identity | Tagged integration and envtest |
| Authorization grants | Model, tool, memory, role, and cross-namespace grants deny by default and allow exact bindings | Tagged integration and Kind |
| Deletion | Finalizers retry safely and delete only owned resources | Tagged integration and Kind |
| WorkflowRun | Linear, parallel, approval, budget, retry, timeout, cancellation, and output propagation | Tagged integration and Kind |
| IntentGateway and AgentService | Goal to Solution to AgentJob to AgentInstance, target deletion, immutable revision pinning | Tagged integration and Kind |

### Runtime and Data Plane

| Workflow | Required assertions | Current test tier |
|---|---|---|
| Runtime invocation | Controller token, session isolation, warm-pool no-op behavior, completion polling | Python runtime and Kind |
| Model usage accounting | Provider-reported input/output tokens; immutable pricing, class, tier, reservation, and duration snapshots; replay safety; reset boundaries; normalized micro-USD and model-tier projection; exact-limit pre-dispatch denial | Unit, PostgreSQL integration, Helm, Kind with Cilium |
| Runtime memory policy | Capability binding, grant revocation, raw-service isolation, cross-namespace Secret mirror, NetworkPolicy enforcement | Unit, Python runtime, Kind |
| Tool Gateway | Gateway-only runtime metadata omits target endpoints; active and previous capability-key overlap; session capability exchange; exact tool/version/invocation binding; ToolGrant, AgentRole, and enforced AgentPolicy authorization; replay-safe durable invocation receipts; private target egress allowlist rendering; returned Tool result in bounded `AgentInstance.status.toolCallsDetail`; durable start/completion events; policy revocation | Unit, Python runtime, Helm, Kind |
| Runtime Tool Gateway | Model-selected Tool call traverses dispatcher session capability delivery, the runtime gateway client, exact capability exchange, gateway execution, final model response, bounded status detail, and durable events | Deterministic Kind |
| Agent Communication Gateway | Source-session exchange, exact target binding, cross-workspace grant and policy enforcement, cross-replica receipt replay safety, immediate policy revocation, and Prometheus request metrics | Unit, Helm, Kind |
| Tool Gateway resilience | Per-Tool timeout, idempotent transient retry, concurrency rejection, circuit opening, and Prometheus metric emission | Unit and Helm |
| Tool execution | MCP and OpenAPI invocation, timeouts, trace propagation, structured errors | Tagged integration and container or Kind |
| KnowledgeBase | Ingestion, retrieval binding, refresh, failure recovery, deletion cleanup | Tagged integration, Docker Compose, Kind |
| Memory storage | Provision, concurrent access, semantic ranking, metadata filters, right-to-be-forgotten cascade | Tagged integration and pgvector container |

### Platform Delivery

| Workflow | Required assertions | Current test tier |
|---|---|---|
| Helm delivery | Images, environment variables, RBAC, CRDs, Secrets, NetworkPolicies | Helm regression suite |
| Kubernetes projection | Conversion round-trip, controller status writes, webhook behavior, RBAC failures | Envtest and Kind |
| External providers | Credential paths, model calls, Blob source access, embedding dimensions | Provider E2E |

## Test Placement

- Add deterministic public API and reconciliation tests under `tests/integration` with the `integration` build tag.
- Add webhook and CRD API-server tests under `internal/k8s/webhook` with the `integration` build tag.
- Add Kind scripts under `e2e` when a real pod, Service, Job, RBAC rule, Secret mount, or NetworkPolicy is part of the contract.
- Add external-provider tests as opt-in scripts. They must fail fast when required credentials or network access are absent.
- Add a regression test for every production defect at the layer that would have caught it before release.

### NetworkPolicy Enforcement

The default Kindnet CNI accepts `NetworkPolicy` resources but does not enforce
their packet filters. Helm and Kind tests in this repository verify selector,
port, and policy-shape contracts. Run deny-path tests for runtime bypass and
unapproved Tool Gateway private egress in a separate Calico- or
Cilium-enforcing Kubernetes environment before production rollout.

## Priority Backlog

1. Add a Kind runtime-memory policy test that proves tenant runtime NetworkPolicy enforcement.
2. Add tagged integration tests for ModelGrant, ToolGrant, MemoryGrant, AgentRole, and CrossNamespaceGrant default-deny behavior.
3. Add Kind WorkflowRun coverage for parallel sessions, approval gates, and terminal failure propagation.
4. Add Kind AgentService revision tests covering dispatch, target deletion, and cleanup retry behavior.
5. Add pgvector container tests for ingestion, recall ranking, refresh, and deletion cleanup.
6. Add provider-nightly tests for Foundry, Azure Blob, and a real MCP service.

## Change Checklist

Before merging a feature that crosses a process or deployment boundary:

1. State the public behavior and its failure modes.
2. Add or update the deterministic unit and integration tests.
3. Add envtest or Kind coverage when Kubernetes enforces any part of the behavior.
4. Add a provider E2E scenario when a cloud service, credential, or remote protocol is material to correctness.
5. Run the relevant commands from this document and record failures as defects, not skipped assumptions.
