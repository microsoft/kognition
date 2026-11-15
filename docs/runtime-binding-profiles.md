# Runtime Binding Profiles

Status: local implementation and isolated acceptance, 2026-10-06. Production
enrollment, rollout and release certification remain separate approvals.

## Runtime Approval Instance Configuration

[ADR-0027](adr/0027-runtime-approval-instance-configuration.md) proposes
`RuntimeApproval.spec.agentInstanceRef.configurationDigest`. Repository
implementation binds the instance spec, generation, execution metadata and
runtime session in the keyed action envelope. Status-only resourceVersion
changes can continue; the original observed version remains in the stored
reference.

Configuration, ownership, deletion, identity, session, target, arguments,
authorization and other resource-version changes still reject reuse. Digests
are accepted only on the execution-instance reference. Legacy references
without a digest remain version-bound and are not automatically migrated.

Kubernetes schemas and all participating gateways must be qualified before
enabling new writers. Existing legacy approvals need explicit maintenance
handling. Separately approved isolated tutorial baselines qualify genuine
supervised Tool and Memory continuation. The original Rancher installation and
historical failed approvals remain unchanged; production rollout requires its
own qualification and authorization.

## Tool Authorization Denial Evidence

The Tool Gateway records authenticated capability-exchange authorization
denials through the same redacted decision builder used by invocation denial.
It validates the workload and session before attributing an event. Denial
evidence includes the actual identity, session, invocation, target revision and
applicable grant or policy references, without input values or credentials.
It issues no capability and reserves no invocation receipt or Tool quota.
Failure to persist the denial returns an explicit unavailable response.

Kubernetes resource and audit stores are separate. The controller's in-process
Tool Gateway must receive `NewStoreLedgerWithAudit`; detecting an AuditStore only
on its resource adapter omits normalized decisions. A transport `403` or a
Kubernetes Event alone does not establish a trust-scoring observation.

Periodic TrustProfile scoring consumes normalized policy and argument-constraint
denials as critical violations. The configured fallback class can apply even
when the resulting scalar score remains above a promotion threshold. Trust
class changes retain their incarnation binding and do not replace grants or
exact-action approval.

## Model Candidate Binding

The user accepted a signed Model revision plus canonical configuration digest
on October 5. The digest covers the complete serialized Model spec, including
the signature envelope, using canonical JSON and SHA-256. It does not hash
remote provider weights. An immutable provider version belongs in the signed
Model configuration where the provider supports one.

`ModelClass.spec.versionCandidates` maps version labels to a namespace, Model
name, UID, resource version and `specDigest`. `status.activeBinding` records
the promoted binding. Keep candidate Models separate from logical Models used
by Agent model references, and grant access to both logical and candidate Models.
The candidate must reference the same ModelClass and provisioner.

Promotion verifies the exact candidate binding and its operator-pinned signature,
then executes a tool-free inference request for `KOGNITION_MODEL_CANARY_OK` with
a ten-second lifecycle deadline and at most sixteen output tokens. Provider
usage must be reported and internally consistent. Failure keeps the previous
active version and binding. The candidate is resolved again before promotion.

Model Gateway tokens include the active candidate binding. After promotion or
binding changes, old tokens are rejected rather than redirected. Controllers
must issue fresh capabilities for the new serving binding. Credential references
resolve in the candidate's namespace; a candidate cannot borrow credentials from
another workspace. Canonical CPI execution uses the same active candidate and
requires its live ModelGrant through the shared authorization core.

Legacy classes without `versionCandidates` retain their compatibility behavior;
their metadata-only pins are not candidate inference acceptance. Concurrent-edit
fencing and capability routing pass local regressions. Approved disposable
PostgreSQL and Kubernetes acceptance now verifies candidate promotion, retained
serving rollback and recovery without another inference. This profile uses a
deterministic local HTTP provider; paid providers and production rollout remain
separate acceptance boundaries.

### Durable Canary Accounting

Both controller assemblies require a durable Model dispatch ledger, usage writer
and audit store before contacting the candidate. A stable operation digest binds
the class UID, version, full class configuration and exact candidate revision.
The controller checks cost/model-tier quotas, requires exact input token counting
with a 4096-token ceiling, writes authorization evidence and atomically claims
the operation. Before inference it reserves the exact input and sixteen output
tokens in the protected quota ledger. Generic expiry and generic settlement
cannot release that reservation. Insufficient token quota persists a terminal
denial; restoring the quota does not revive that same claim.

Version-2 terminal outcomes persist reported usage before settlement, usage
projection and completion audit. Exact immutable settlement dispositions and
idempotent usage/audit writes let a fresh reconciler finish a known outcome
without another inference. Failed response checks still settle actual usage.
Version-1 terminal records retain their existing no-replay behavior.

Missing, invalid or uncertain accounting evidence fails closed. A timeout or
post-dispatch outcome persistence failure leaves the operation claimed and its
reservation protected; reconciliation does not repeat it or guess a refund.
Recovery of genuinely unknown outcomes requires explicit investigation and a
supported procedure. Known retained outcomes can finish accounting automatically.
Oversized, invalid or mismatched retained result metadata remains fenced.

Drivers without exact input counting fail before dispatch. The accepted local
OpenAI-compatible fixture uses `gpt-4-0613` and the reviewed offline tokenizer
vocabulary; an unsupported alias such as `gpt-4o` is not silently estimated.

PostgreSQL installations require migration `033_model_canary_purpose.sql`, which
adds the `model-version-canary` usage purpose. Kubernetes stores immutable claim
and result ConfigMaps in the control namespace with exact workspace/operation
binding. Native admission denies other writers and deletion of those receipts.
Activation requires the complete current policy/binding inventory, including
this receipt protection.
Real PostgreSQL purpose/idempotence and API-server receipt protection tests pass.
The October 13 approved disposable fixture also applied the normal full migration
runner and exercised both real stores. The existing Kubernetes CAS token ledger
now retains protected reservations and permanent exact dispositions. No cloud,
production installation or paid model inference was performed. Evidence and
qualified completion are recorded in the
[backlog review](development-plans/reviews/backlog-review-2026-10-02.md#model-candidate-local-acceptance-2026-10-13).

## AutoGen HTTP Worker

### Offline kagent Compatibility Planning

`kognitionctl apply` rejects foreign or malformed explicit API versions before
HTTP or Kubernetes resource lookup. Omitted API versions retain the existing
Kognition input compatibility path. Foreign Agents require a separately reviewed
compatibility import; matching `kind: Agent` does not authorize overwriting a
Kognition Agent.

The offline planner accepts one bounded AutoGen-era `kagent.dev/v1alpha1` Agent
with an exact source SHA-256 and explicit target workspace/Model references:

```powershell
kognitionctl import kagent plan --file ./reviewed-agent.yaml `
  --sha256 '<exact-source-sha256>' --target-namespace reviewed-workspace `
  --model reviewed-model `
  --tool-binding upstream.Provider=reviewed-tool
```

**Bash (Linux/macOS):**

```bash
kognitionctl import kagent plan --file ./reviewed-agent.yaml \
  --sha256 '<exact-source-sha256>' --target-namespace reviewed-workspace \
  --model reviewed-model \
  --tool-binding upstream.Provider=reviewed-tool
```

Planning bypasses CLI client initialization in every mode. It preserves the
system message and tool order, fingerprints opaque provider configuration without
printing it, and reports missing operator Tool bindings. It never imports Python
provider modules, loads executable components, creates grants, contacts endpoints,
or emits an apply-ready resource. Provider semantics and capabilities require
separate review; explicit bindings alone do not prove equivalence. A source prompt
remains untrusted agent input even when preserved byte-for-byte after YAML decoding.

Pinned upstream acceptance input: release `v0.1.0`, commit
`b133e628c9534da4a68bf1e3d035426b9a768d9b`, Agent `kube-expert`, manifest
SHA-256 `e6e1a81d4b791498779ccbb2b042624f2acf401ef492a1574f0a6daa3793cb5b`.
Its 21 declared tools exceed that release's schema limit of 20. The real CLI
reports all 21 tools, the limit violation and missing bindings without truncation,
including when forced Kubernetes mode has an unavailable kubeconfig. This is a
blocked compatibility plan, not unchanged-workload acceptance. T-0150 remains open.

A schema-valid input from the same pinned release is now retained:
`observability-agent`, rendered manifest SHA-256
`185978317a9ee40cec7023e729f03cf468192967cdc4f260555820fd772016f7`.
The render uses a byte-identical upstream template and materializes only its
`Chart-template.yaml` metadata in an external copy; no upstream script or module
is executed. Five tools fit the release's twenty-tool limit, but require reviewed
bindings for `GetResources`, `GetAvailableAPIResources`, `QueryTool`,
`GeneratePromQLTool` and `DashboardManagementTool`. The offline plan has six
explicit blockers and stays non-executable. Kubernetes/Prometheus scopes and
Grafana dashboard mutations must be reviewed before porting; synthetic endpoints
or stubs cannot establish unchanged tool semantics.

TC-0119 is accepted for the approved local native-only-to-AutoGen registration
profile. Actual native execution passes with registration absent; an AutoGen
request then fails before another model call or an SDK Pod template. A separately
reviewed inactive reset adds only the registered worker image and operator TLS
configuration, leaving the frozen controller digest unchanged. The registered
controller creates the real SDK workload and completes the same tool-free input
with eighteen reported tokens and one model call. The public Runtime adapter
checks the actual deployed worker over operator-CA/Service-name verified TLS:
health/capabilities, session creation, foreign-principal denial, empty checkpoint
and fresh restore, and acknowledged cancel without inference. The exact selected
image separately passes the shared streaming/cancellation/checkpoint/backpressure
suite; that container receipt is not relabeled as deployed coverage.

Both native and registered targets use independent fresh authorities and original
authority recovery. Owned workloads finalize normally and the node is stopped
with evidence retained. This is a controlled registration/reset, not a hot upgrade
or production/remote-provider certification. The registered matrix is compiled
from frozen controller source `03dacb4`; unrelated parallel capability edits in
the working tree currently do not compile and were not modified or certified.
See the [registration evidence review](development-plans/reviews/backlog-review-2026-10-02.md#native-only-provider-registration-acceptance).

The optional image profile uses AutoGen 0.7.5. Configure
`KOGNITION_RUNTIME_CLASS=autogen` and the protected runtime/gateway environment.
The existing native profile remains the default and does not import the SDK.

The authenticated `/sessions` API supports create, step, status, checkpoint,
restore and cancel. It pins the startup Agent/namespace/configuration. Turns
require fresh scoped gateway capabilities; checkpoints retain SDK state but no
runtime or model bearer tokens. Running, failed, cancelled and approval-blocked
turns cannot produce usable checkpoints. Session capacity is sixty-four and
execution remains singleton per worker. A newly created, quiescent session can
checkpoint the real SDK's typed empty history.

The Go HTTP AgentClient pins the worker endpoint and configuration, refuses
redirects and requires HTTPS except literal loopback. A trusted capability
callback supplies turn credentials. The adapter preserves SDK tool-call IDs,
fences same-session concurrency, honors event credits and propagates stream
close to worker cancellation. Checkpoint reads fence new turns; restore rejects
empty or reused handles. Checkpoints are opaque sensitive state. A forced cancel
result requires the caller's hard-kill path; it is not proof of remote termination.
Cooperative cancellation honors the caller's explicit `TimeoutMS` deadline.

Isolated Linux acceptance exercises the real SDK worker and Go adapter with a
deterministic model endpoint. The actual container passes shared v0.1 runtime
conformance for its explicit Agent/namespace binding, empty-state recovery and
completed-history recovery. Separate tests reject a foreign namespace and enforce
event credits. The generic multi-namespace suite remains unchanged. Standalone
HTTP worker selection is supported; CPI dispatch rejects non-native classes
before provider contact instead of silently executing them through native CPI.
Live-provider and both-mode deployed rollout acceptance remain separate.

### Production Adapter Lifecycle

The configured production dispatcher selects the public AutoGen Runtime adapter
for `runtimeClass: autogen`. Native/default retain their legacy path. Contract
sessions bind the controller's stable session ID, Agent namespace/name, exact
configuration and immutable AgentInstance UID. Idempotent create and one-turn
replay fences prevent a delivery retry from repeating a paid call. Provider
execution waits for the persisted Running session; capabilities are issued at
step time. Polling preserves validated live usage and structured tool details.
Missing ownership after controller restart remains accounting-uncertain rather
than falling back to native delivery or automatically redispatching.

The actual SDK container passed the production dispatcher create/step/status,
usage, retry and acknowledged-cancel path. Automatic durable checkpoint recovery,
bounded completed-session reclamation and full deployed Agent/Tools/Memory parity
remain open; standalone loopback evidence does not certify Kubernetes rollout.

[ADR-0024](adr/0024-runtime-checkpoint-recovery.md) now accepts controller-owned
encrypted recovery: per-workspace operator AES-256-GCM key rings, quiescent-only
restore, seven-day terminal retention configurable up to thirty days and retained
unknown-outcome claims. It is now implemented locally with strict full-binding
and expiry AAD, hot key-file reads, PostgreSQL migration 034 and protected
control-namespace Kubernetes records. Prepared/claimed/terminal/retired states
use CAS. Activation now requires eight admission policy/binding pairs.

Persist an encrypted prepared checkpoint and claim before bound restore/Step.
Completion remains withheld until encrypted terminal status/checkpoint are
acknowledged. A fresh controller reads matching terminal results after worker
loss without another paid inference. Unknown outcomes, wrong UID/configuration,
expired ciphertext and removed keys fail closed. Worker history is released only
after durable terminal persistence. Periodic leader-scoped cleanup processes at
most 100 terminal records per pass; it retains replay-denial metadata and never
age-deletes unresolved claims.

Configure `runtimeRecovery.existingSecret`, `secretKey` and `retention` in Helm.
The existing Secret contains a namespace map of `{current, keys}` key rings;
each key value is strict base64 for 32 operator-provisioned bytes. Keys mount
read-only only into the controller, separately from capability/signing/TLS keys.
No operator key values are generated or recorded by this source configuration.

Encryption, real PostgreSQL CAS/expiry, Kubernetes CAS and actual API-server
writer/transition/deletion checks pass locally. Actual SDK production dispatch,
quiescent restore, safe release and controller/worker-loss terminal readback pass
disposable-container acceptance. Full installed Agent/Tools/Memory and
runtimeClass-only parity remains open pending guarded fixture activation.

### Kubernetes Worker Control TLS

[ADR-0023](adr/0023-runtime-control-transport.md) records the approved operator
HTTPS choice. Configure an explicit AutoGen image plus
`k8sController.runtimeControlTLS.workerSecretName` and `existingCASecret`.
Operators provision the worker `tls.crt`/`tls.key` in each runtime namespace and
the controller `ca.crt` in its installation namespace. Worker certificates must
cover the actual Service DNS. The implementation mounts existing material
read-only, verifies the explicit CA with TLS 1.3 and hostname checks, and uses
HTTPS probes. Missing image, Secret settings or trust fails closed. No keys or
Secrets are generated. Native defaults and transport remain unchanged.

Current generated AutoGen workers require one pinned replica; multi-replica
stateful routing needs a separate ownership contract. Configured operator paths
and chart/API checks are local wiring evidence, not deployed acceptance.

## SPIRE Gateway Profile

The four Model, Tool, Memory Policy and Communication gateway commands support
an opt-in SPIRE TLS server profile. Its configured identity is the gateway's
identity; request peers must match the live Agent incarnation.

| Environment | Value |
| --- | --- |
| `KOGNITION_WORKLOAD_IDENTITY_MODE` | `spire` or default `capability` |
| `KOGNITION_SPIFFE_ENDPOINT_SOCKET` | Protected local Workload API socket |
| `KOGNITION_SPIFFE_ID` | Exact enrolled gateway SPIFFE identity |
| `KOGNITION_SPIFFE_TRUST_DOMAIN` | Operator-approved trust domain |

The profile requires TLS 1.3 and SPIRE trust bundles. Issuer watch failures,
expired material, key mismatch or peer mismatch fail closed. Application checks
still require current Agent, identity, session, grants and policies. Informational
legacy URIs remain distinct from cryptographic peer identity.

The Agent workload URI uses its immutable UID:

```text
spiffe://<trust-domain>/workspace/<namespace>/agent/<name>/incarnation/<agent-uid>
```

### Python Workload Transport

[ADR-0022](adr/0022-python-workload-transport.md) records the approved Go loopback
transport. AutoGen 0.7.5 and the inspected Python SPIFFE SDK require incompatible
protobuf versions; SPIRE keys stay in the official Go source rather than Python.

Run `kognition-workload-identity-proxy` colocated with an operator-managed worker.
Its listener defaults to `127.0.0.1:8096` and rejects non-loopback addresses.
Configure its protected local socket, trust domain, Agent namespace/name/UID and
`KOGNITION_SPIFFE_GATEWAY_ENDPOINTS`, a JSON map with exactly `model`, `tool`,
`memory` and `communication` entries. Each entry requires an HTTPS origin in
`url` and an exact gateway SPIFFE identity in `peerID`.

Set the worker's `KOGNITION_WORKLOAD_IDENTITY_MODE=spire` and
`KOGNITION_WORKLOAD_PROXY_URL=http://127.0.0.1:8096`. Keep all explicit startup
configuration keys bound to the controller's runtime configuration. The worker
derives its four plane URLs from that loopback origin and checks `/readyz` before
building its graph. The transport forwards only allowlisted POST operations,
keeps existing capabilities, refuses redirects and bounds payloads to four MiB.

The disposable Docker fixture used a verified bootstrap bundle and passed real
thirty-second SVID issuance/rotation, wrong-incarnation denial and an actual
Python AutoGen turn through the proxy to an exact-peer SPIRE mTLS gateway fixture.
Fixture resources were removed afterward. No production registration, socket
mount or cluster resource changed. Protected pod/service-account enrollment,
generated Kubernetes Agent sidecar/socket wiring, resource-status propagation
and cross-mode rollout acceptance remain open before FR-0401 completion.

## Build Evidence

The pgvector production target built locally. Standard Python image installation
still encountered a Docker-to-PyPI TLS handshake failure. Local worker acceptance
used securely downloaded Linux wheels, a pinned base digest and an external
offline build recipe with package consistency checks. No TLS verification was
disabled. All 165 worker tests passed inside the current non-root Linux artifact.
The optional workload proxy production image built locally. Its release target
is part of the tested fifteen-image Go inventory; publication was not performed.
