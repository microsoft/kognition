# ADR-0005 — Secretless outbound provider auth (Azure managed / workload identity)

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Kognition core team

## Context

Agents and the knowledge-base ingester call external AI providers (Azure OpenAI /
Microsoft Foundry, OpenAI-compatible endpoints, Anthropic, Bedrock). For Azure,
Kognition already modeled several credential types on `SecretRef`
(`type: "azure-identity"` with `credentialType` ∈ `client-secret`,
`workload-identity`, `managed-identity`, `default`), and the Go Foundry CPI
driver (`internal/cpi/drivers/foundry/token_provider.go`) honored all of them.

However, the **runtime** paths did not. The Python agent runtime
(`runtime/agent`) and knowledge-base ingester (`runtime/ingester`) hardcoded
`ClientSecretCredential`, and the k8s-native credential-injection paths — the
dispatcher (agent pods) and the KB ingester launcher — **rejected** any
`credentialType` other than `client-secret`. In practice this forced a client
secret to be present for every Azure-backed agent or KB, defeating the point of
running with a managed identity or workload-identity federation.

A goal for wide adoption is **secretless operation on Azure**: when Kognition
runs on AKS with workload identity or on an Azure VM/managed-identity host,
provider auth should require no stored secret at all.

## Decision

Make the runtime credential selection mirror the Go driver and default to a
**secretless** path when no client secret is present.

1. **Runtime credential selection.** A shared selector in the agent runtime
   (`runtime/agent/azure_creds.py`) and an equivalent in the ingester
   (`runtime/ingester/ingest.py`) choose the azure-identity credential from
   `AZURE_CREDENTIAL_TYPE`:
   - `client-secret` → `ClientSecretCredential`
   - `workload-identity` → `WorkloadIdentityCredential` (federated token file)
   - `managed-identity` → `ManagedIdentityCredential` (optional user-assigned id)
   - `default` / unset → `ClientSecretCredential` when `AZURE_CLIENT_SECRET` is
     set, otherwise `DefaultAzureCredential`, which itself covers managed
     identity, workload identity, environment, and the Azure CLI.

2. **Credential-type propagation.** The k8s dispatcher and the KB ingester
   launcher now propagate all four modes to the workload pods as
   `AZURE_CREDENTIAL_TYPE` plus only the fields each mode needs (no client secret
   for the secretless modes), instead of rejecting anything but `client-secret`.

3. **Backward compatibility.** An empty `credentialType` still defaults to
   `client-secret` when a secret is supplied, so existing service-principal
   SecretRefs keep working unchanged.

Workload identity additionally requires the workload pod's ServiceAccount to be
annotated for the Azure Workload Identity webhook so it projects
`AZURE_FEDERATED_TOKEN_FILE`; that is deployment configuration owned by the
operator.

## Consequences

**Positive.**

- Fully secretless Azure provider auth on AKS (workload identity) and Azure hosts
  (managed identity) — no client secret stored anywhere.
- Consistent credential semantics across the Go control plane and both Python
  runtimes, driven by a single `SecretRef` field.
- No breaking change: existing client-secret configurations are unaffected.

**Negative / to accept.**

- Workload identity depends on the Azure Workload Identity webhook and a
  correctly annotated ServiceAccount — an operator responsibility documented but
  not enforced by the chart.
- `DefaultAzureCredential` walks a credential chain, which is slightly less
  deterministic than an explicit type; operators wanting determinism set
  `AZURE_CREDENTIAL_TYPE` explicitly.
- This ADR covers Azure. Equivalent federation for other clouds (e.g. AWS IRSA
  for Bedrock) is out of scope here and handled by those SDKs' own ambient
  credential chains.

## Alternatives Considered

1. **Require a client secret everywhere.** Rejected: contradicts the secretless
   goal and stores long-lived secrets unnecessarily.
2. **Always use `DefaultAzureCredential`.** Rejected as the sole path: explicit
   modes give operators determinism and faster failure on misconfiguration; the
   default chain is used only as the no-secret fallback.
3. **Handle credential selection only in the Go control plane and pass a minted
   token to the runtime.** Rejected: the runtimes make provider calls directly
   and token lifetimes/refresh are cleaner when the runtime owns the credential.

## Related

- Implementation: `runtime/agent/azure_creds.py`, `runtime/ingester/ingest.py`,
  `internal/k8s/dispatcher/dispatcher.go`,
  `internal/k8s/controller/kb_ingester.go`,
  `internal/cpi/drivers/foundry/token_provider.go`.
- Tutorial: [TUTORIAL.md](../../TUTORIAL.md) — SecretRef → Azure Identity
  Credential Types.
