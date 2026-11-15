# Securing a Kognition deployment

This guide covers the security controls for a production Kognition install:
API authentication, TLS, network isolation, pod hardening, availability, and
provider credentials. API authentication, TLS, network policy, ingress, and
disruption budgets are opt-in. Baseline container and pod security contexts
are enabled by default and remain configurable through Helm values.

Design rationale for the larger decisions lives in the ADRs:

- [ADR-0003 — API authentication (OIDC) and TLS](adr/0003-apiserver-authentication.md)
- [ADR-0004 — Multi-replica watch fan-out via PostgreSQL NOTIFY](adr/0004-watch-fanout-pg-notify.md)
- [ADR-0005 — Secretless outbound provider auth](adr/0005-secretless-provider-auth.md)
- [ADR-0007 — Agent Gateway Fabric](adr/0007-agent-gateway-fabric.md)
- [ADR-0028: First installation lifecycle](adr/0028-first-installation-lifecycle.md)

## Deployment modes and the security baseline

| Mode | Resource API | AuthN / AuthZ / TLS / audit |
| --- | --- | --- |
| **kubernetes** | CRDs on the real Kubernetes API server | Provided by Kubernetes |
| **standalone** | First-party REST API server + PostgreSQL | Provided by the controls below |

In **kubernetes mode** the API surface is the Kubernetes API server, so its
authentication, RBAC, TLS, and audit apply. The controls in the "API
authentication" and "TLS" sections below are specific to **standalone mode**.
Network, pod-hardening, PDB, and provider-auth controls apply to both.

### Guarded Kubernetes Installation

For a new release, the [fresh installer](getting-started/kubernetes.md) verifies
admission, migrations and service readiness before granting permissions and
starting the controller. It issues a distinct first-install record and does not
fabricate release-qualification reports.

Existing-installation upgrades, enrollment changes and recovery follow the
[installation and recovery contract](features/knowledge-provider-execution/installation-recovery-contract.md)
and [separate activation contract](features/knowledge-provider-execution/activation-transaction-contract.md).
The installation security owner approves dedicated workload credential domains;
an authorized administrator controls enrollment, admission, guard configuration
and persistent isolation baselines. Secret GET bypasses admission: controller
compromise can expose all credentials in enrolled namespaces and the entire
contents of named installation Secrets it may read.

The maintenance examples below assume an already authorized, uncompromised
installation. They do not authorize activation, restore grants after failed
installation, or recover a compromised controller. For suspected compromise,
follow the installation contract's containment and issuer-revocation procedure;
routine previous-key overlap retains the compromised key's authority. Source
Secret deletion or rotation alone does not invalidate existing copies, processes
or sessions. Production qualification remains separate from installation readiness.
See the [SG-09 review](development-plans/features/knowledge-provider-execution/sg09-operator-contract-review.md)
for current historical-finding dispositions and remaining approvals.

### Runtime authorization coverage

Kognition currently enforces these invocation boundaries:

| Path | Enforced controls |
| --- | --- |
| Standalone Agent chat and AgentService invoke | ModelGrant, applicable enforced AgentPolicy, token quota reservation and settlement |
| Standalone model-requested tools | ToolGrant, applicable enforced AgentPolicy, AgentIdentity and AgentRole for Agent principals, ToolGrant rate limits, workspace tool-call quota |
| Controller-side Workflow tool steps | ToolGrant, applicable enforced AgentPolicy, AgentIdentity and AgentRole for Agent principals, ToolGrant rate limits, workspace tool-call quota |
| Kubernetes model-requested tools | Tool Gateway session and exact invocation capabilities, current AgentInstance identity and session, ToolGrant, AgentRole, enforced AgentPolicy, Tool resource version, workspace tool-call quota, durable invocation events |
| AgentInstance dispatch in both modes | Session and token ceilings before dispatch, exact terminal settlement, optional SPIFFE URI propagation |

Token reservations are durable and replica-safe in both modes. Standalone mode
updates PostgreSQL quota rows and reservations in one transaction. Kubernetes
mode uses a resource-version-protected ConfigMap ledger in the controller
namespace as the accounting authority and projects totals to
`CognitiveQuota.status`. The controller refuses to start without
`KOGNITION_CONTROLLER_NAMESPACE`, which prevents mutable ledgers from being
stored in tenant namespaces. Both backends reclaim reservations older than 24
hours without refunding charges across quota reset or resource generations.

Each Agent automatically receives a deterministic SPIFFE URI through an
`AgentIdentity` resource. The controller propagates it through ephemeral
dispatch context and Kubernetes runtime status. This URI and legacy
`status.phase: Ready` are attribution metadata, not cryptographic proof.
The opt-in SPIRE source verifies SVID chains, key correspondence and the exact
Agent-incarnation URI; configured gateway transport verifies peers independently.
SPIRE installation and enrollment policy belong to the platform.

Optional `status.workloadIdentity` exposes certificate metadata only. The opt-in
reconciler requires current controller-owned AgentIdentity/Agent UID binding and
a trusted controller-side reader, validates the incarnation URI and validity,
and refreshes every five seconds. Expiry, source outage and invalid ownership
clear certificate readiness. Without a reader, reconciliation removes this
status object. No private key, certificate body or arbitrary issuer diagnostics
are copied into status. Resource status is observational and is never accepted
as gateway authentication or authorization evidence.

The opt-in status transport accepts one exact controller SPIFFE peer and verifies
its SVID chain on every request. Its client uses a fixed HTTPS origin, pins the
current Agent-incarnation URI, and accepts only seven metadata fields within
8 KiB. Duplicate, unknown, missing or null fields are rejected. Reported validity
must match the actual TLS leaf certificate. Redirects, stale UID, invalid validity
and issuer outage return no ready metadata; successful results are not cached.
Requests have a five-second bound. The server factory opens no listener and
exports neither certificate bodies nor private keys. The proxy executable can
enable a separate mTLS status listener with
`KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR` and
`KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID`. Both settings are required; the
listener accepts a literal IP/port and one distinct same-domain controller peer.

Both production controllers optionally load
`KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE` once at startup. This operator-owned
JSON file contains at most 128 registrations within 64 KiB. Each registration
contains exactly `namespace`, `agentName`, canonical `agentUID`, and a fixed HTTPS
`endpoint`. Ambiguous fields, duplicate Agent bindings and unsafe origins fail
before issuer contact. Missing or recreated registrations never reuse another
Agent's metadata. Updating the file requires a controller restart. Protect the
referenced ConfigMap in the control namespace with operator-only write access;
read-only mounts do not prevent an authorized ConfigMap writer from replacing
its contents. An immutable ConfigMap with an explicit replacement/restart
procedure is recommended.

### Platform-Managed SPIRE Delivery

The selected delivery mechanism is the platform-installed SPIFFE CSI driver,
`csi.spiffe.io`. Kognition neither installs SPIRE/the driver nor creates
enrollment entries. Enable Helm `workloadIdentityStatus.enabled` with the trust
domain, controller SPIFFE ID, existing registry ConfigMap and runtime namespace.
Both controller modes receive a read-only Workload API mount at
`/spiffe-workload-api` and a read-only registry mount. The socket address is
`unix:///spiffe-workload-api/spire-agent.sock`.

Generated Kubernetes Agent pods additionally require
`workloadIdentityStatus.managedWorkloads.enabled`, a digest-pinned `proxyImage`,
four `gateways` bindings (`model`, `tool`, `memory`, `communication`), and enforced
NetworkPolicies. Bindings use fixed HTTPS origins and distinct same-domain
`peerID` values. `runtimeNamespace` must equal `k8sController.agentNamespace`.
The proxy alone receives the CSI mount; agent code uses loopback port 8096.
Every proxy is non-root, read-only, non-escalating and drops all capabilities.
Its readiness command checks the existing verified-source `/readyz` endpoint
with a one-second deadline, rather than trusting an open TCP listener.

The workload admission exception allows only the named `workload-api` volume,
exact driver, read-only setting, no custom attributes/secret references, and the
proxy's read-only mount. Agent, init and ephemeral container socket mounts,
hostPath, other CSI drivers, elevated ServiceAccounts and API tokens remain
forbidden. The managed Service exposes mTLS status on port 8097. Runtime ingress
permits this port only from the controller namespace/component; controller
egress selects managed Agent pods in the configured runtime namespace. Enabling
SPIRE for an existing dedicated revision requires a new revision because its
Service selector and ports are immutable.

The platform must attest the dedicated workload independently of tenant labels,
bind enrollment to the current Agent incarnation and protect socket permissions.
The registry must explicitly bind the same UID and chosen workload endpoint.
For multiple replicas, certificate validity describes the authenticated endpoint
used for that observation, not every replica. Gateway origins must match the
approved gateway namespaces/ports permitted by runtime networking, and those
gateways must actually serve SPIRE-authenticated HTTPS. Chart rendering does not
establish enrollment, driver permissions, gateway authentication or installed
readiness.

Owner-approved local SPIRE/CSI acceptance closes T-0085/T-0086 for the implementation and reviewed fixture
profile. Real Kubernetes/PostgreSQL metadata lifecycle and recreated-UID denial,
plus production-handler Tool/Memory mTLS, replay, wrong-peer and revoked-grant
checks passed. The Memory gateway now checks the authenticated peer against the
live Agent incarnation before authorization. Capability-only compatibility is
unchanged. See the [acceptance scope and evidence](development-plans/reviews/backlog-review-2026-10-02.md#approved-installed-identity-acceptance).
This does not certify arbitrary platform enrollment, a full Helm controller
rollout, cross-pod NetworkPolicy enforcement or multi-replica deployment behavior.

Kubernetes runtime memory calls traverse the memory policy gateway. The gateway
verifies a short-lived capability bound to the current AgentInstance, rechecks
the current `MemoryGrant` and `AgentRole`, and proxies only authorized requests
to the raw memory service.

Kubernetes runtime tool calls traverse the Tool Gateway. Dispatch sends the
runtime a non-executable capability bound to its current `AgentInstance` and
session. After model selection, the runtime exchanges that capability for one
exact Tool capability. The gateway validates the live AgentIdentity,
AgentInstance, Tool resource version, ToolGrant, AgentRole, and enforced
AgentPolicy at exchange time and again before execution. Quota reservation and
a durable authorization event occur immediately before execution. The Tool
Gateway owns target endpoint egress; runtime NetworkPolicy allows runtime pods
to reach the gateway rather than arbitrary Tool endpoints.

Before authorization or target execution, the Tool Gateway durably claims the
capability's invocation ID with a binding over the Agent, subject, Tool
revision, and input. An identical retry returns the recorded terminal response
without another quota reservation, audit lifecycle, or target request. A retry
while the original request is pending returns a conflict, and reusing an
invocation ID with different input is rejected.

### Capability HMAC Key Rotation

Memory and Tool capabilities include a JWT `kid`. Each gateway signs with its
active HMAC key and verifies the active key plus one previous key during a
bounded overlap. The Secret stores the key material and its stable identifier:

| Secret data key | Purpose |
|---|---|
| `capability-hmac-key` | Active signing key |
| `capability-hmac-key-id` | Active signing key identifier |
| `capability-hmac-previous-key` | Previous verification-only key during overlap |
| `capability-hmac-previous-key-id` | Identifier for the previous key |

Use a unique active key ID for every rotation. Do not reuse `active` after the
initial installation because a capability's `kid` must continue to identify
the key that signed it.

1. Deploy the key-ring-aware controller, memory policy gateway, and Tool
  Gateway before changing Secret data. Existing installs receive active ID
  `active` automatically.
2. Record the current active key and ID, then update the Secret so the new key
  and unique ID are active while the former active key and ID move to the
  previous entries. Key data remains base64 encoded in the Kubernetes Secret.
3. Restart and wait for the memory policy and Tool Gateway Deployments first.
  They now verify both generations while the controller still signs with its
  old in-memory active key.
4. Restart and wait for the Kubernetes controller. It begins issuing
  capabilities with the new active key ID.
5. Retain the previous key for at least the longest capability TTL plus rollout
  time. The current runtime session capability TTL is five minutes.
6. Remove both previous Secret entries and restart the gateways after the
  overlap expires.

For example, patch a Tool Gateway Secret with printable random key material.
The Secret API base64-encodes values, then injects their decoded value into the
gateway environment, so the key itself must not contain a NUL byte. Generate
at least 32 printable random characters with an approved KMS or password
generator; the example uses 48 characters.

```powershell
$oldKey = kubectl get secret kognition-tool-gateway -n kognition-system -o jsonpath='{.data.capability-hmac-key}'
$oldKeyID = kubectl get secret kognition-tool-gateway -n kognition-system -o jsonpath='{.data.capability-hmac-key-id}'
$alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
$newKey = -join (1..48 | ForEach-Object { $alphabet[[System.Security.Cryptography.RandomNumberGenerator]::GetInt32($alphabet.Length)] })
$newKeyBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($newKey))
$newKeyID = "2026-08-19-rotation-1"
$newKeyIDBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($newKeyID))
$patch = @{ data = @{ "capability-hmac-key" = $newKeyBase64; "capability-hmac-key-id" = $newKeyIDBase64; "capability-hmac-previous-key" = $oldKey; "capability-hmac-previous-key-id" = $oldKeyID } } | ConvertTo-Json -Compress
kubectl patch secret kognition-tool-gateway -n kognition-system --type merge -p $patch
kubectl rollout restart deployment/kognition-tool-gateway -n kognition-system
kubectl rollout status deployment/kognition-tool-gateway -n kognition-system --timeout=180s
```

**Bash (Linux/macOS), with Python 3 available:**

Run in a protected shell with tracing disabled. This builds the Secret patch
in memory and pipes it to kubectl rather than printing key material:

```bash
set -o pipefail
kubectl get secret kognition-tool-gateway -n kognition-system -o json |
  python3 -c '
import base64, json, secrets, string, sys
data = json.load(sys.stdin)["data"]
encode = lambda value: base64.b64encode(value.encode()).decode()
key = "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(48))
print(json.dumps({"data": {
    "capability-hmac-key": encode(key),
    "capability-hmac-key-id": encode("2026-08-19-rotation-1"),
    "capability-hmac-previous-key": data["capability-hmac-key"],
    "capability-hmac-previous-key-id": data["capability-hmac-key-id"]
}}))
' | kubectl patch secret kognition-tool-gateway -n kognition-system \
      --type merge --patch-file /dev/stdin || exit 1
kubectl rollout restart deployment/kognition-tool-gateway -n kognition-system
kubectl rollout status deployment/kognition-tool-gateway -n kognition-system --timeout=180s
```

Use a new reviewed key ID for each rotation in either shell. `/dev/stdin` is
available on Linux and macOS; no GNU-specific `base64` decoding flags are needed.

Apply the same procedure to `kognition-memory-policy-gateway`, then restart
`kognition-k8s-controller` after both gateways are ready.

### RuntimeApproval Writer Authority

Human approval/rejection and reconciliation use the authenticated dedicated
resource-webhook endpoints and their exact SubjectAccessReview checks. Configure
`KOGNITION_RUNTIME_APPROVAL_DECISION_SERVICE_ACCOUNT` explicitly; the Kubernetes
chart selects the dedicated resource-webhook ServiceAccount. Missing authority
configuration denies human state transitions. Lifecycle-writer membership alone
does not permit human actor/audit evidence or reconciliation dispositions.

Execution-plane accounts are explicit in
`KOGNITION_RUNTIME_APPROVAL_TOOL_SERVICE_ACCOUNT`,
`KOGNITION_RUNTIME_APPROVAL_MEMORY_SERVICE_ACCOUNT` and
`KOGNITION_RUNTIME_APPROVAL_COMMUNICATION_SERVICE_ACCOUNT`. A gateway cannot
create or mutate another plane's approvals. Claim/finalization writes require
authenticated token-bound Pod name/UID and live Pod/ServiceAccount/container
verification; unconfigured writers, missing extras and stale containers fail
closed. The dedicated webhook receives only control-namespace Pod GET for this
verification.

Preparation, delivered decision evidence, expiry and fencing prerequisites are
checked at admission. Gateway writes cannot replace a retained claimant/fence,
erase termination proof, finalize Uncertain claims or rewrite terminal results.
Human authority may perform only evidence-bound Uncertain reconciliation, not
ordinary gateway finalization. A process nonce is retained, but does not identify
separate processes sharing one live container credential independently.

The workspace owner explicitly selected the container/workload credential as the
trust boundary for this profile. Same-container processes therefore remain one
trusted principal; per-process credentials or a transition broker would be a
separate architecture/enrollment change. This is not production approval.

Controller recovery of a retained claim may only mark it Uncertain after an
independent uncached read proves the exact Pod/container incarnation absent.
Admission verifies the same canonical proof and forbids ordinary completion,
claim reassignment or retained-fact rewrites. Cached absence, Lease age, an API
read failure or a caller-supplied proof string is insufficient. The dedicated
webhook uses only control-namespace Pod GET/LIST for these checks.

Controller pre-claim writes permit only canonical initialization and expired
preparation reaping, not gateway claim creation. When inline human authority is
explicitly configured, it may perform only the dedicated evidence-bound human
reconciliation transition; it cannot finalize an ordinary gateway execution.

Gateways retain automatic NotRequired initialization, expiry, evidence
acknowledgement and execution-state responsibilities. Declared controller
namespaces also bound gateway Lease writes. The binding-key lock and rate/usage
locks belong in the control namespace; the approval-record lock remains in each
declared workspace. Gateway writes cannot adopt an unowned or Helm-owned Lease,
write arbitrary coordination names, or delete Leases.

Bootstrap now requires nine native policy/binding pairs, including the mandatory
gateway Lease pair. Keep resource admission enabled and fail-closed. The
qualified isolated deployed profile closes SEC-F1-002/SEC-F1-003 with actual
two-replica Tool, Memory store/delete and Communication workflows, verified
webhook/decision TLS, retained owner/fence receipts and duplicate-effect checks.
It uses trusted synthetic identity/provider fixtures and real PostgreSQL
mutation receipts. It does not certify disabled/Ignore webhooks, full controller
activation, arbitrary workspace enrollment, same-container process isolation,
paid/provider-specific acceptance or production rollout. See the
[qualified gateway authority review](development-plans/features/gateway-authority-hardening/security-review-code.md).

Communication may bind an AgentService target in another workspace while the
RuntimeApproval remains owned by the source workspace. All namespaced source
and authorization references stay source-owned. The MAC includes the exact
target namespace, UID and resource version; immutable-spec admission rejects
retargeting. Both exchange and invocation recheck the current cross-workspace
grant and communication policy. Tool and Memory target namespace restrictions
are unchanged.

### Resource Webhook Audit Creation

When resource webhooks are enabled, their dedicated identity can create ConfigMaps
only through the mandatory native state policy. It permits new immutable `kognition-audit-<32 hex>`
batches in the installation namespace, with canonical audit/workspace/agent
labels and only a nonempty `binaryData["entries.json"]` payload of at most
900 KiB. Other names, namespaces, data keys, metadata authority, updates, patches
and deletion are denied independently of the webhook process and its RBAC.
Controller/platform state behavior and the disabled-webhook profile are unchanged.

This boundary does not authenticate audit content or limit the number of
otherwise permitted batches. A compromised writer can still forge audit-shaped
records, consume storage, read installation ConfigMaps and exercise its separate
decision authority. The raw KSV-0049 create-permission finding remains visible;
local mitigation is not release/target risk approval. See the
[verified audit-boundary review](development-plans/features/knowledge-provider-execution/security-review-code.md#webhook-audit-creation-boundary-2026-10-19).

### Gateway State And Legacy Receipt Isolation

The mandatory native state policy also covers every enabled gateway ConfigMap
writer. Tool, Communication and Model may write only their exact enrolled ledger
names and data/ownership metadata; audit-only writers may only append bounded
immutable audit batches. Required cost/usage records remain append-only.
Old/new ownership checks deny migration, Helm-owned, unowned, unrelated and
other-plane state writes independently of RBAC. Model coordination is limited
to installation usage-accounting locks.

Tool and Communication use separate receipt ledgers. Gateways cannot mutate
the old shared ledger. Matching terminal legacy outcomes replay saved results;
unresolved, malformed or unavailable legacy evidence fails closed pending
reviewed recovery. Upgrade under the existing writer freeze and retain every
legacy fence. Do not refund, shorten retention or delete records to bypass it.

The [exact local closure](development-plans/features/gateway-authority-hardening/finding-closure-2026-10-20.md)
records the repaired escalation and current owner-approved per-occurrence
scanner dispositions. Raw required-permission findings remain visible, with
their stated own-state, credential and storage residuals. No production
activation or future-candidate approval follows from that local decision.

### RuntimeApproval Binding Key Lease Recovery

The Kubernetes controller serializes RuntimeApproval creation against binding
key rotation with the `kognition-runtime-approval-binding-keys` Lease in the
controller namespace. A controller process that terminates during creation can
leave a nonempty `spec.holderIdentity`. The controller fails closed while that
holder remains and does not take over based on Lease age because a paused
writer could resume.

Recover only after stopping every Kubernetes controller replica that can write
RuntimeApprovals. Preserve the Lease annotation because it contains the
authoritative binding key epoch and material digests.

Gateway and resource-webhook processes may also use this Lease. The controller
scale-down example below is insufficient by itself when those writers are
enabled: establish verified quiescence of every writer before changing a holder.
No timeout or Lease age proves an uncertain action was not executed.

Each step must succeed before the next one runs. If scale-down or rollout
fails, stop: do not clear the holder while a controller replica might still be
writing.

```powershell
$ErrorActionPreference = "Stop"
function Invoke-Kubectl {
    & kubectl @args
    if ($LASTEXITCODE -ne 0) { throw "kubectl $($args -join ' ') failed with exit code $LASTEXITCODE" }
}

$namespace = "kognition-system"
$deployment = "kognition-k8s-controller"
$replicas = (& kubectl get deployment $deployment -n $namespace -o jsonpath='{.spec.replicas}')
if ($LASTEXITCODE -ne 0 -or $replicas -notmatch '^[1-9][0-9]*$') { throw "Could not read a nonzero replica count for $deployment" }
Invoke-Kubectl scale deployment $deployment -n $namespace --replicas=0
Invoke-Kubectl rollout status deployment $deployment -n $namespace --timeout=180s
Invoke-Kubectl patch lease kognition-runtime-approval-binding-keys -n $namespace --type merge -p '{"spec":{"holderIdentity":""}}'
Invoke-Kubectl scale deployment $deployment -n $namespace --replicas=$replicas
Invoke-Kubectl rollout status deployment $deployment -n $namespace --timeout=180s
```

**Bash (Linux/macOS):**

```bash
set -euo pipefail
namespace=kognition-system
deployment=kognition-k8s-controller
replicas="$(kubectl get deployment "$deployment" -n "$namespace" -o jsonpath='{.spec.replicas}')"
[[ "$replicas" =~ ^[1-9][0-9]*$ ]] || { echo "Could not read a nonzero replica count for $deployment" >&2; exit 1; }
kubectl scale deployment "$deployment" -n "$namespace" --replicas=0
kubectl rollout status deployment "$deployment" -n "$namespace" --timeout=180s
kubectl patch lease kognition-runtime-approval-binding-keys -n "$namespace" --type merge -p '{"spec":{"holderIdentity":""}}'
kubectl scale deployment "$deployment" -n "$namespace" --replicas="$replicas"
kubectl rollout status deployment "$deployment" -n "$namespace" --timeout=180s
```

Run the Bash block in a script or subshell so `set -e` does not close your
interactive terminal on failure.

Do not delete the Lease or edit
`kognition.dev/runtime-approval-binding-key-state`. Restore controller replicas
only after all old Pods have terminated. Binding key activation uses a single
resource-version compare-and-swap, so a delayed activation writer cannot
overwrite a newer epoch.

Controller-side OpenAPI, MCP, and remote Browser tools allow public HTTPS
destinations by default. Remote Browser tools call a reviewed browser service;
the controller and Tool Gateway do not start a local browser process. Plain
HTTP and endpoints resolving to private, loopback, link-local, or
metadata-service addresses are denied at execution time, including after
redirects. An intentional in-cluster tool must set
`spec.metadata.allowPrivateNetwork: "true"`; grant this exception only to
reviewed endpoints and keep NetworkPolicy egress narrow. When
`networkPolicy.enabled=true`, a private Tool must also match a
`toolGateway.privateEgress` entry containing its namespace labels, Pod labels,
and TCP port. This separates the application-level Tool approval from the
network-level egress approval; either missing approval prevents successful
production use in a NetworkPolicy-enforcing cluster.

Browser `input.url` is separately validated as a public HTTPS destination
before it reaches the remote Browser service. That service must enforce the
same policy for redirects and all subsequent browser-driven navigation.

Shell and direct stdio Tools remain unavailable. They require a separate
class-approved sandbox target before Kognition can execute process input
without exposing the controller or Tool Gateway process.

When `networkPolicy.enabled=true`, set
`networkPolicy.kubeletProbeIngressCIDRs` to the node CIDRs used by kubelet
readiness and liveness probes. Native Kubernetes NetworkPolicy has no generic
selector for node hosts, and the chart fails fast if this list is empty rather
than deploying policies that cause HTTP-probed workloads to crash-loop on an
enforcing CNI. Keep these entries to node CIDRs; do not use a broad Pod CIDR or
`0.0.0.0/0`.

For Cilium clusters, set `networkPolicy.ciliumHostProbeIngress: true`. The chart
then emits a cluster-wide Cilium policy for dynamic runtime Pods and
component-scoped policies for release workloads. Kubelet probes can be
classified as Cilium `world` traffic, so these policies allow only the probed
TCP port from `world`, `host`, `remote-node`, and `health`.

The Kubernetes controller invokes and polls managed runtime Pods on TCP 8080.
Its NetworkPolicy permits that port only to Pods labelled
`kognition.dev/agent` across namespaces. Private Tool and
Model endpoint validation uses the same explicit `toolGateway.privateEgress`
and `modelGateway.privateEgress` selectors as the corresponding gateways.

Exec Tool health probes run only in a class-approved one-shot OCI sandbox. Set
`ToolClass.spec.executionTier: pod` and `ToolClass.spec.sandboxImage` to enable
them. In standalone Docker Compose deployments, set `DOCKER_GID` to the group
ID of the mounted Docker socket. The controller sends direct argv through the
Docker Engine API; it does not execute a shell or run a command in the
controller container. Kubernetes creates an equivalent short-lived Job with no
service-account token, no egress, dropped capabilities, a read-only root
filesystem, and the configured probe deadline.

### Tool Gateway Resilience

`Tool.spec.executionPolicy` controls gateway behavior for one Tool revision:
timeout, maximum concurrent calls, idempotency-gated retry attempts, and an
optional circuit breaker. Only Tools declared `idempotent: true` may configure
more than one retry attempt. Circuit breakers open only after transient target
errors and fail fast until their cooldown expires.

The Tool Gateway exposes `/metrics` on its existing HTTP Service port. Enable
`toolGateway.monitoring.serviceMonitor` and
`toolGateway.monitoring.prometheusRule` only on clusters with Prometheus
Operator CRDs installed. The alert rules cover invocation failures, open
circuits, concurrency rejections, and audit completion failures. Enable
`toolGateway.monitoring.grafanaDashboard` to render a Grafana dashboard
ConfigMap with invocation outcome, latency, and resilience-decision panels.

## 1. API authentication (OIDC) — standalone mode

The standalone API server does not authenticate by default and logs a warning
at startup when auth is disabled. Enable OIDC so every request (except
`/healthz` and `/readyz`) must carry a valid bearer token from your identity
provider — Microsoft Entra ID, Google, Auth0, Okta, Keycloak/Dex, or any
compliant issuer.

```yaml
# values.yaml
apiserver:
  auth:
    enabled: true
    oidc:
      # Microsoft Entra ID example (v2.0 issuer):
      issuer: "https://login.microsoftonline.com/<tenant-id>/v2.0"
      audience: "<app-or-client-id>"
      usernameClaim: "preferred_username"   # default: email
      groupsClaim: "groups"                  # default: groups
      requiredGroups: ["kognition-operators"]  # optional coarse authorization
    # Optional bootstrap token for service-to-service / admin use:
    bootstrapToken:
      secretName: "kognition-bootstrap"
      secretKey: token
```

Behavior:

- Missing / invalid token → `401` with `WWW-Authenticate: Bearer`.
- `requiredGroups` set and the caller is in none → `403`.
- The verified identity is stored in the request context and used by
  identity-sensitive handlers (e.g. approval quorum counts the authenticated
  principal, not a caller-asserted field).

Under the hood the deployment sets `KOGNITION_AUTH_ENABLED`,
`KOGNITION_OIDC_ISSUER`, `KOGNITION_OIDC_AUDIENCE`, and friends; you can set
these directly if not using the chart.

**Enterprise SSO overlay.** Native OIDC composes with a gateway. You may still
front the server with oauth2-proxy / Envoy for centralized SSO and keep OIDC on
the server for defense in depth.

## 2. TLS — standalone mode

Terminate TLS at the API server, or at the Ingress.

**At the server** — provide a `kubernetes.io/tls` Secret (e.g. from cert-manager):

```yaml
apiserver:
  tls:
    enabled: true
    secretName: kognition-apiserver-tls   # keys: tls.crt, tls.key
```

The container then serves HTTPS and the probes switch to the `HTTPS` scheme.

**At the Ingress** — leave `apiserver.tls` off and terminate at the edge:

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: kognition.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: kognition-apiserver-tls
      hosts: ["kognition.example.com"]
```

> Because the API server does not authenticate by default, if you expose it via
> Ingress make sure authentication is enforced — either enable `apiserver.auth`
> or require auth at the gateway.

## 3. Network policies

Enabled with `networkPolicy.enabled: true` (requires a CNI that enforces
NetworkPolicy, e.g. Calico or Cilium). The chart ships **per-component** policies
that:

- lock down the databases (`postgres`, `kb-pgvector`) and the control plane so
  only Kognition workloads may reach them;
- constrain component egress to the destinations they need. The Tool Gateway
  retains target egress because it executes dynamically declared, endpoint-
  validated Tools on behalf of runtime pods.

Policies select only named components, so dynamically created **agent** and
**ingester** pods (labeled `app.kubernetes.io/managed-by: kognition`) are matched
as allowed peers across namespaces. Agent runtime Pods also receive a
controller-managed NetworkPolicy that permits DNS, configured data-plane
gateways, kubelet probes, and controller runtime operations; direct private
Tool and Model endpoint egress remains denied.

Allow an ingress controller to reach the API server:

```yaml
networkPolicy:
  enabled: true
  apiServerExtraIngress:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ingress-nginx
```

## 4. Pod hardening (securityContext)

Applied automatically to all control-plane workloads via `podSecurityContext`
and `containerSecurityContext` (the images are distroless-static/nonroot):

- `runAsNonRoot: true`, `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`
- `seccompProfile: RuntimeDefault`

The bundled PostgreSQL StatefulSets use a gentler profile (`dbPodSecurityContext`
/ `dbContainerSecurityContext`: `runAsUser: 999`, writable root filesystem) since
Postgres writes its data volume. All values are overridable.

## 5. Availability (HA)

- **Watch fan-out** is delivered through PostgreSQL `LISTEN/NOTIFY`, so running
  more than one API-server replica is correct (see ADR-0004). The listener
  reconnects with backoff on connection loss.
- **PodDisruptionBudgets** are opt-in and render only for workloads with
  `replicaCount > 1`, so single-replica node drains are never blocked:

  ```yaml
  podDisruptionBudget:
    enabled: true
    minAvailable: 1
  apiserver:
    replicaCount: 3
  ```

- Controllers use **leader election** (`k8sController.leaderElection.enabled`),
  so extra replicas are warm standbys.
- Liveness and readiness probes are defined on every workload, including
  `pg_isready` on the databases.

## 6. Secrets

- The bundled Postgres passwords default to demo values — **override them** in
  production (`postgres.auth.password`, `kbPgvector.auth.password`) or point at
  an external/managed database and disable the bundled one.
- Prefer Kubernetes Secrets or an external secrets operator over the `internal`
  SecretRef backend (which stores data in an annotation) for anything beyond
  dev.
- `.env` files are for local development only and are gitignored.

## 7. Outbound provider credentials

Kognition authenticates to AI providers with, in order of preference for Azure:

| Mode (`SecretRef` `credentialType`) | When to use | Secret stored? |
| --- | --- | --- |
| `managed-identity` | Kognition runs on an Azure host / AKS node identity | No |
| `workload-identity` | AKS with the Azure Workload Identity webhook | No |
| `client-secret` | Any cluster; explicit service principal | Yes |
| `default` | Auto-detect via `DefaultAzureCredential` | Only if present |

Secretless modes (`managed-identity`, `workload-identity`, and `default` without
a secret) require **no stored client secret**; see
[ADR-0005](adr/0005-secretless-provider-auth.md) and the SecretRef section of
[TUTORIAL.md](../TUTORIAL.md). Workload identity also needs the workload pod's
ServiceAccount annotated for the Azure Workload Identity webhook.

For API-key providers (OpenAI, Anthropic) and AWS Bedrock, use the `api-key`
SecretRef type and the provider SDK's ambient credentials (e.g. IRSA for
Bedrock) respectively.

## Production checklist

- [ ] `apiserver.auth.enabled: true` with your OIDC issuer (standalone mode)
- [ ] TLS at the server (`apiserver.tls`) or Ingress (`ingress.tls`)
- [ ] `networkPolicy.enabled: true` on a policy-enforcing CNI
- [ ] `networkPolicy.kubeletProbeIngressCIDRs` set to kubelet node CIDRs, or
  `networkPolicy.ciliumHostProbeIngress: true` on Cilium
- [ ] Non-default database passwords, or an external managed database
- [ ] Disable `memoryService` and use a durable external memory backend before
  running multiple gateway or controller replicas
- [ ] Use `values-production.yaml` as a base, then set private image registry,
  production certificate issuer, and external service endpoints
- [ ] Secretless provider auth where possible (managed / workload identity)
- [ ] `podDisruptionBudget.enabled: true` and `replicaCount > 1` for HA
- [ ] Rotate any credential that was ever exposed outside a Secret
