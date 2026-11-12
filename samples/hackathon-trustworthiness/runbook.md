# Trustworthiness Demo Runbook

**Status:** Technical rehearsal available; production recording gates open
**Version:** 1.3 (2026-10-02)

## Entry Point

Run the selector from the repository root. It performs read-only Kubernetes
preflight checks and chooses the primary or local fixture fallback path. The
fixture is not the production standalone API server or workflow runtime.

```powershell
pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-demo.ps1 `
  -DemoRunId <unique-lowercase-run-id>
```

Set an explicit `KUBECONFIG` and pass `-PreflightContext` to select the primary
cluster; that context is forwarded to the Kubernetes runner when preflight passes.
The selector does not switch the global context. A failed preflight starts the
local fixture and dashboard. For an already-enrolled namespace or retained scope,
use the direct Kubernetes runner below; the selector does not expose those flags.

## Platform Prerequisites

- Complete reviewed installation and separately approved activation under the
  [installation contract](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md).
  Do not enroll a namespace, grant controller privileges or restart activation
  just to run this demo.
- Pre-enroll the exact `kognition-hack-<run-id>` namespace. Keep the independent
  sandbox guard and resource-admission server ready. Use the actual release and
  controller namespace, which may differ from defaults.
- Complete [database initialization](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md#database-prerequisites).
  Missing `memory_mutation_receipts` causes Memory replay failures even with ready
  gateway Pods. Use the reviewed migration binary/Job, not an approval bypass.
- Require the Model, Tool, Memory Policy and Agent Communication gateways, the
  agent runtime image, and storage for the publication PVC. The runner uses local
  deterministic model and publication fixtures through real gateway paths; it
  does not demonstrate a live Foundry model or external publication destination.
- Run from the repository root in a dedicated shell. Preserve reports and audit
  evidence, and never print capability tokens, kubeconfig material or Secret data.

## Primary Rehearsal

For a panel inspection, retain one reviewed, initially empty scope. The run ID
must match its pre-enrolled namespace; this example does not create enrollment:

```powershell
$env:KUBECONFIG = (Resolve-Path ./reviewed/target.kubeconfig).Path
$context = 'reviewed-context'
$release = 'reviewed-release'
$systemNamespace = 'reviewed-system'
$runId = 'reviewed-run-id'
$scope = "kognition-hack-$runId"
$report = "test-results/hackathon-trustworthiness-$runId.json"
.\e2e\hackathon-trustworthiness-e2e.ps1 -DemoRunId $runId `
  -KubernetesContext $context -ControllerNamespace $systemNamespace `
  -ReleaseName $release -ReportPath $report `
  -PreprovisionedScope -KeepScopeNamespace -ApprovalMode automated
```

Use `-ApprovalMode presenter` for actual presenter confirmation. Automated mode
populates inspectable demo evidence but does not prove human participation.
Inspect the report's `passed`, `failure`, `assertions` and `decisionTimeline`;
retaining the scope is intentional and does not prove cleanup.

Recording repeatability still requires three sequential runs with distinct
reviewed IDs and successful assertions and cleanup, followed by a fourth retained
run. Omit `-KeepScopeNamespace` only when teardown is intended and authorized.
Restart that validation sequence after a failure or manual repair. Namespace
deletion changes installation inventory; do not silently recreate or re-enroll a
same-name namespace under an earlier approval.

Cleanup is retention-aware: remove all workloads and run-owned external
resources. A terminating namespace may contain validated RuntimeApproval
tombstones and matching quota tickets with future retention deadlines and expected
security finalizers. Review `cleanup.retainedSecurityRecords` in each report.
The current runner reports this as `retained`, not a successful full teardown;
do not count it as a passing cleanup assertion or a clean three-run sequence.
Approval tombstones require at least 90 days of retention. Never shorten that
retention or strip finalizers to make a rehearsal pass.

The primary report must prove all four trust planes against the same run ID:

| Assertion | Required measurement |
|---|---|
| `tool-phase` | Signed capability reaches the Tool target and revocation prevents later execution |
| `memory-phase` | Authorized Memory access reaches the target and revoked access is denied |
| `model-phase` | Signed capability completes a real Model Gateway provider call |
| `communication-phase` | Source session exchanges an exact capability and invokes a ready AgentService |
| `decision-audit-correlation` | Tool, Memory, Model, and Communication decisions match the exact run ID |
| `decision-timeline` | Durable correlated evidence is available for inspection |

Start the dashboard from this checkout with the retained report bound explicitly:

```powershell
go build -o ./bin/kognitionctl.exe ./cmd/kognitionctl
./bin/kognitionctl.exe dashboard --mode kubernetes --kube-context $context `
  --namespace $scope --kube-namespace $systemNamespace `
  --audit-namespace $systemNamespace --run-proof $report `
  --bind 127.0.0.1 --port 18080
```

Open `http://127.0.0.1:18080/?view=trust&runId=<run-id>&focus=1`.
The audit namespace is the controller namespace, not
the retained workload namespace. Use the same controller namespace as the runner.
Verify that Tool, Memory, Model, and Communication each contain run-scoped
evidence. Read assertion totals from this report's `assertions`, and evidence
totals from `decisionTimeline`; never substitute historical totals. Three-run
repeatability requires the three separate successful cleanup reports, not the
retained run's assertion badge. Archive all reports and the final video before
approved teardown. Leave the dashboard and workload running during inspection.

The primary runner defaults to `-ApprovalMode presenter`: the presenter must
type the exact confirmation `publish` before it submits ApprovalDecision.
Cancellation submits no approval. Automated validation can explicitly use
`-ApprovalMode automated`; its report is not human-approval evidence.
RuntimeApproval phase helpers use administrative test privileges to submit demo
decisions, including gateway-identity impersonation for Communication. This does
not exercise a human approver's authenticated decision-endpoint experience.
The workflow invokes a real deterministic SQLite-backed publication target.
The runner verifies zero publications before approval, lifecycle timestamps,
exact content and run ID, and one durable publication after target restart.
Replay and conflicting-content behavior also have focused target tests.
The retained `pub-970e27f9` Kubernetes run verified automated approval and target
restart persistence; a presenter-recorded take and the three clean runs remain
required. Do not describe that automated run as human approval.

## Retained Recovery

Preserve the failed report before retrying. A missing initial `status` or `phase`
is a normal resource-observation race; the runner waits for it. A terminal
`WorkflowRun` failure must remain failed: use a new reviewed run scope rather than
patching status or replacing history.

- For a source-session failure before workflow creation, use
  `hackathon-trust-profile-phases.ps1 -CommunicationOnly -AttemptId <new-id>`
  with the same explicit context, release, controller namespace, run ID and
  `-ScopeNamespace`. Keep the failed instance. The helper creates a distinct
  controller-owned instance and waits for both instance Running and Agent Ready.
  It must still reject inactive or mismatched sessions.
- The Memory helper accepts `-AttemptId <new-id>` for distinct retry resources and
  invocation IDs. Keep the successful report at `memory-boundary-<run-id>.json`
  beside the final runner report; archive prior attempts separately.
- Once all four planes have genuine run-correlated evidence, resume the main
  runner with `-ResumeRetainedScope -KeepScopeNamespace`, the same target/run ID,
  and a new `-ReportPath`. Supply the intended approval mode if the workflow gate
  has not yet been approved. Resume verifies retained Memory evidence and must
  not invent completed phases. Its `retained-decision` report label does not
  establish that a human approved the run; inspect the actual approval provenance.

### Verified Local Inspection

The 2026-10-02 retained run `trust-rehearsal-c` in
`kognition-hack-trust-rehearsal-c` passed 13 assertions with 39 durable timeline
entries. Chromium observed 29 executable decisions across Tool, Memory, Model and
Communication, `Live`, Run Proof `13/13 passed`, no empty panels and no page errors.
Approval was automated. The local dashboard was started at
`http://127.0.0.1:52713/?view=trust&runId=trust-rehearsal-c&focus=1`; that URL works
only while its process and cluster remain available.

Ignored local artifacts are
`test-results/hackathon-trustworthiness-trust-rehearsal-c-ready-20261002.json`,
`test-results/memory-boundary-trust-rehearsal-c.json`, and
`test-results/hackathon-trust-focus-20261002.png`. Earlier failed attempts were
preserved. This repaired inspection run does not close three-clean-run,
presenter/narration, production, or deployed standalone-parity gates.

## Mode Parity Gate

```powershell
pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-parity-test.ps1
```

This gate requires every named production contract test to execute and pass;
any skip, failure, or missing test fails the gate. It covers Tool/Model transport
contracts, PostgreSQL and Kubernetes approval disposition contracts, workflow
gating, Memory and Communication approval effects, and four-plane evidence.
The Kubernetes store contract uses a fake client; gateway suites use controlled
dependencies. These are not two deployed-mode end-to-end runs.

Both Memory Policy and Agent Communication gateway commands support explicit
`KOGNITION_MODE=standalone` with `DATABASE_URL`; Kubernetes remains the default.
The gate starts both real executables against an isolated PostgreSQL database
with unavailable kubeconfig and verifies their health endpoints. It also checks
resource and audit persistence after reopening the database backend. These
startup checks use controlled upstreams and do not prove agent execution parity.
Standalone Agent Communication additionally requires `KOGNITION_API_SERVER_URL`
and `KOGNITION_API_SERVER_TOKEN`; it ignores resource-provided endpoint URLs and
rejects redirects. Operators must provide the existing capability and approval
binding keys as well. Runtime identity/capability provisioning and the shared
deployed scenario remain required before production parity can be certified.
The joined gateway/API contract verifies authenticated invocation, exact session
delivery, replay after gateway recreation, and revocation against the actual API
handler with a controlled provider and in-memory store. It does not establish
PostgreSQL restart durability or controller-owned runtime provisioning.

Full recording parity additionally requires the same real scenario through the
production standalone API/controller/PostgreSQL and Kubernetes controllers:
four-plane allow/deny/revoke evidence, durable approval, no effect before
approval, exactly one publication after approval, replay protection, evidence
after restart, and cleanup. The fixture cannot satisfy this gate. Until both
deployed runs pass, `deployedDemoParityVerified` and `recordingReady` remain false.

## Automatic Fallback

Kubernetes preflight uses only `cluster-info` and `get` operations. The first
failed check starts deterministic local Tool and Memory fixtures automatically.
The fallback builds temporary binaries, starts them on free loopback ports,
queries Trust Decisions through the dashboard proxy, writes the report, stops
both processes, verifies both endpoints are unreachable, and removes temporary
files.

For a fixture-only UI rehearsal, force an unavailable context, use fixed ports,
and retain the processes while inspecting the dashboard:

```powershell
pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-demo.ps1 `
  -DemoRunId trust-fallback-recording `
  -PreflightContext kognition-intentionally-unavailable `
  -FixturePort 18081 `
  -DashboardPort 18080 `
  -KeepFallbackServers
```

Stop the two process IDs printed by the runner immediately after inspection.
Run without `-KeepFallbackServers` for normal rehearsals and acceptance tests.

## Fallback Evidence Checks

The report must contain these successful fallback assertions:

| Assertion | Required measurement |
|---|---|
| `toolAllow` | Allowed Tool decision with `targetReached=true` |
| `memoryAllow` | Allowed Memory decision with `targetReached=true` |
| `denyBeforeExecution` | Denied Tool decision with `targetReached=false` |
| `standaloneApprovalEvidence` | Static fixture response, not a durable approval gate |
| `liveRevocationEvidence` | `grant_revoked` with `targetReached=false` |
| `decisionExplorer` | Four run-scoped decisions returned through the dashboard proxy |

Fallback reports must mark Kubernetes namespace isolation, admission, and
NetworkPolicy claims as `unavailable`. Approval is
`standalone-evidence-only`; it is not a production standalone or Kubernetes
approval. Omit approval, publication, Model, Communication, durability, and
production mode-parity claims from a fixture walkthrough.

Run the degraded-environment acceptance test with:

```powershell
pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-fallback-e2e.ps1
```

## Browser Rehearsal Contract

The presenter storyboard and recording-production notes are local-only
preparation documents. This executable fallback rehearsal has no dependency on
them:

```powershell
pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-storyboard-e2e.ps1
```

The acceptance check starts a live fallback dashboard and drives Chromium at
1440 x 900. It inspects the four executable Tool and Memory decisions, rejects
browser errors and unexplained empty Trust Focus regions, captures the final
screen, records observed wall-clock duration, and requires Trust Focus as the
first and final screen. The bounded acceptance requirements are:

| Check | Requirement |
| --- | --- |
| Browser viewport | 1440 x 900 |
| Elapsed time | At most 115 seconds |
| Initial and final screen | Trust Focus |
| Observed decisions | Four run-scoped Tool/Memory decisions |
| Empty states and page errors | Zero |
| Final screenshot | Present and associated with the selected run |

Use synthetic sample inputs and inspect reports/screenshots for credentials or
sensitive data before sharing them. Reject failed assertions, manual repair or
unsupported claims. The elapsed time measures automated traversal, not a narrated
take. TC-0316 remains open until the presenter attests the recording checklist
and verifies the encoded video's duration and final playback; a passing browser
test does not close that human acceptance requirement.