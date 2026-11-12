# Standalone Compose

Compose provides a local development deployment with PostgreSQL persistence and
four gateways. Contract tests and service startup do not establish deployed
feature parity or recording readiness.

## Credentials

Use PowerShell 7 in the repository root. For a new deployment, generate separate
credentials without printing them:

```powershell
$credentialNames = @(
    'KOGNITION_BOOTSTRAP_TOKEN',
    'KOGNITION_AGENT_SERVICE_TOKEN',
    'KOGNITION_RUNTIME_AUTH_TOKEN',
    'MEMORY_SERVICE_INTERNAL_TOKEN',
    'MEMORY_CAPABILITY_HMAC_KEY',
    'TOOL_CAPABILITY_HMAC_KEY',
    'MODEL_CAPABILITY_HMAC_KEY',
    'AGENT_DELEGATION_CAPABILITY_HMAC_KEY'
)
foreach ($credentialName in $credentialNames) {
    $material = [Security.Cryptography.RandomNumberGenerator]::GetBytes(32)
    [Environment]::SetEnvironmentVariable(
        $credentialName, [Convert]::ToBase64String($material), 'Process')
}
$bindingKey = @{
    version = 1
    epoch = 1
    algorithm = 'HMAC-SHA-256'
    domain = 'kognition.dev/runtime-approval/action/v1'
    status = 'Active'
    material = [Convert]::ToBase64String(
        [Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
}
$env:KOGNITION_RUNTIME_APPROVAL_BINDING_KEYS_JSON =
    ConvertTo-Json -InputObject @($bindingKey) -Compress
```

Keep these values in an access-controlled secret store and reload the same values
for subsequent starts. Do not rerun generation against an existing deployment:
changing keys can invalidate capabilities and approval bindings. Process
environment variables disappear when the shell exits. Do not commit secrets or
print resolved Compose configuration.

The bootstrap token grants operator access. The AgentService token only permits
AgentService invocation; it must differ from both the bootstrap and runtime
control tokens. Never distribute it to agent workloads.

## Start or Upgrade

Back up an existing database and verify that the backup can be restored before
upgrading. Keep the existing Compose project name so it selects the same volume.
Do not run `docker compose down -v` against a database you need to retain.

The migrator runs before database consumers. It serializes migration attempts,
checks migration checksums and contiguous history, and applies pending SQL in one
transaction. SQL errors roll back both schema changes and ledger updates.

For a fresh database, leave `KOGNITION_MIGRATION_BASELINE` unset. For a legacy
database without a migration ledger, startup fails until you set it to the exact
SQL filename of the last verified applied migration in
[`internal/store/postgres/migrations`](../internal/store/postgres/migrations).
Verify the complete prefix against the database and the previously deployed
release. The baseline is an operator attestation: the migrator does not inspect
the schema to prove those migrations ran. Never guess a baseline or select the
latest filename merely to bypass the check. Once the ledger exists, unset the
baseline; subsequent starts validate the ledger normally.

```powershell
docker compose config --quiet
docker compose up -d --build --wait --wait-timeout 180
docker compose ps -a
```

Confirm the `migrate` service exited with code zero. Compose startup alone does
not prove that each gateway can authorize and complete a request.

## Opt-In HTTP Runtime

The base CPI dispatcher accepts only omitted, empty, `default`, or `native`
runtime classes. It rejects `autogen` and custom classes before contacting a
model provider. Configure a compatible external runtime dispatcher for those
classes; changing their class to bypass the rejection does not preserve their
runtime behavior.

The base deployment keeps synchronous CPI execution. The HTTP overlay enables
controller-issued Memory, Tool, Model, and delegation capabilities for dedicated
native Python runtimes. It does not provision Agents, Models, managed identities,
grants, or approvals; create those resources through the normal control plane.

For one Agent, prepare an access-controlled runtime environment file based on
[`external-runtime.env.example`](../runtime/agent/external-runtime.env.example).
Set its namespace, Agent name, and Model ID to the corresponding resources.
The example assumes no inline instructions, explicit runtime parameters, tools,
memory claims, or knowledge base claims. Additional Agent configuration must
match the controller-generated runtime environment exactly, including serialized
JSON values. Omitted values use runtime defaults; undeclared behavior overrides
are rejected. The overlay supplies the gateway URLs and runtime authentication.
Do not put capability signing keys or provider credentials in the runtime file.

```powershell
$env:KOGNITION_HTTP_RUNTIME_ENV_FILE = 'C:/private/external-agent.env'
$env:KOGNITION_RUNTIME_ENDPOINTS =
    '{"default/external-agent":"http://agent-runtime:8080/invoke"}'
docker compose -f docker-compose.yml -f docker-compose.http-runtime.yml config --quiet
docker compose -f docker-compose.yml -f docker-compose.http-runtime.yml up -d --build --wait --wait-timeout 180
```

- Each `namespace/Agent` must map to its own dedicated runtime. Additional Agents
  require additional services and environment files. Do not map to the public
  AgentService invocation route, which would re-enter the control plane.
- Only native/default runtime classes and unpinned Agent revisions are supported.
  Knowledge-base claims are rejected before configuration resolution because the
  existing retrieval path can expose database and embedding-provider credentials.
  The controller checks startup configuration before minting capabilities, and
  the runtime checks it again on invocation. Update/restart the dedicated runtime
  whenever the bound Agent configuration changes.
- Invocations wait for the persisted Running phase and exact session ID before
  delivery. Duplicate dispatches share one pending delivery owner, so cancellation
  stops the original activation wait or in-flight request. Empty input performs
  a warm readiness check without making a model call.
- HTTP redirects are rejected for authenticated runtime control calls. The
  overlay publishes no runtime port. Its internal HTTP assumes a trusted local
  Docker network; use authenticated HTTPS endpoints across untrusted networks.
- Runtime results are held in an in-memory cache, not a durable HTTP receipt
  store. A controller restart can recover a result still held by the runtime.
  Repeating a retained completed or failed session does not execute it again,
  even after another session takes over. The cache holds at most 64 sessions;
  replay protection does not survive eviction or a runtime process restart.
  A missing, foreign, invalid, or unavailable session result fails closed with
  `UsageAccountingUncertain`, disables automatic retry, and retains the usage
  reservation. Investigate provider usage and any surviving execution before
  operator-led recovery; do not delete reservations to force another paid call.
- CPI remains the canary execution path. HTTP unit and configuration checks do
  not establish deployed four-plane parity or human acceptance.

## Health Checks

The default published ports bind only to `127.0.0.1`:

| Service | Health URL | Port override |
|---------|------------|---------------|
| API server | `http://127.0.0.1:8080/healthz` | `KOGNITION_API_PORT` |
| Memory policy gateway | `http://127.0.0.1:8091/v1/memory/health` | `KOGNITION_MEMORY_GATEWAY_PORT` |
| Tool gateway | `http://127.0.0.1:8092/healthz` | `KOGNITION_TOOL_GATEWAY_PORT` |
| Model gateway | `http://127.0.0.1:8093/healthz` | `KOGNITION_MODEL_GATEWAY_PORT` |
| Communication gateway | `http://127.0.0.1:8094/healthz` | `KOGNITION_COMMUNICATION_GATEWAY_PORT` |

PostgreSQL uses port 5432 by default; override it with
`KOGNITION_POSTGRES_PORT` when another instance already occupies that port.

## Security and Recovery Limits

- This development configuration uses a static PostgreSQL password, internal
  HTTP, and a mounted Docker socket. The socket grants substantial host access.
  Production deployment requires separate infrastructure hardening.
- Tool concurrency holders cover the configured timeout across all retry
  attempts, or an earlier request deadline, plus a recovery grace period.
  Executors must honor cancellation. A remote side effect can outlive a local
  timeout; automatic retries still require an idempotent operation.
- Calls without a bounded execution lifetime retain their concurrency holder
  until explicit release. After a process crash, they fail closed and require
  operator investigation before removing abandoned coordination state. Do not
  clear state while an operation may still be running.
- Standalone CPI saves completed provider responses in the private PostgreSQL
  `model_dispatches` table after usage and completion evidence are persisted.
  Recreating the backend and dispatcher recovers the original session, output,
  and usage without another provider call. An atomic claim permits one execution
  per instance operation. Recovery tests use a controlled provider and a reopened
  database pool, not a killed OS process or an external paid provider.
- A claim without a saved completion remains uncertain and blocks re-execution,
  including a crash after the provider responds but before persistence finishes.
  Do not delete claims to force retries: the original call may have been charged.
- Private completion records currently have no automatic retention cleanup.
  Include them in database access controls, backup protection, and data lifecycle
  planning. Output still appears in `AgentInstance.status` through the existing
  controller path; private storage does not hide that API-visible result.
- Local tests cover controller-issued capability delivery and PostgreSQL Memory
  mutation replay. Deployed controller-to-runtime-to-gateway execution and
  four-plane parity evidence remain outstanding.
- Run `./e2e/hackathon-trustworthiness-parity-test.ps1` for contract conformance.
  Its report deliberately leaves `deployedDemoParityVerified` and
  `recordingReady` false until their separate acceptance gates are completed.