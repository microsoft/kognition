# Kognition Tutorial

A resource-oriented walkthrough with sample configurations. Start with the
[Quickstart](QUICKSTART.md) and [first-agent walkthrough](docs/getting-started/first-agent.md):
you need a running installation, a configured provider and the applicable grants.
Adapt sample namespaces, model names and endpoints to that installation.

Use the [reference index](docs/reference/README.md) for the API definitions shipped
with your revision. The installation and operations guides take precedence over
older sample configuration.

---

## Table of Contents

1. [Resource Model Overview](#resource-model-overview)
2. [ModelClass — Classifying Providers](#modelclass)
3. [SecretRef — Credential Management](#secretref)
4. [Model — Connecting to AI Models](#model)
5. [Agent — Declaring Cognitive Workloads](#agent)
6. [AgentInstance — Invoking Agents](#agentinstance)
7. [Prompt — Reusable Instructions](#prompt)
8. [ToolClass and Tool — Extending Agent Capabilities](#toolclass-and-tool)
9. [MemoryClass and MemoryClaim — Persistent Agent Memory](#memoryclass-and-memoryclaim)
10. [KnowledgeBaseClass and KnowledgeBase — Grounded Knowledge](#knowledgebaseclass-and-knowledgebase)
11. [AutonomyClass and TrustProfile — Agent Autonomy](#autonomyclass-and-trustprofile)
12. [Workflow — Multi-Agent Orchestration](#workflow)
13. [AgentService and IntentGateway — Routing and Exposure](#agentservice-and-intentgateway)
14. [Evaluation, EvaluationDataset, and EvaluationRun — Agent Quality](#evaluation-evaluationdataset-and-evaluationrun)
15. [CognitiveQuota — Resource Budgets](#cognitivequota)
16. [AgentPolicy — Governance and Safety](#agentpolicy)
17. [Workspace — Multi-Tenant Isolation](#workspace)
18. [AgentIdentity — Workload Identity (SPIFFE)](#agentidentity)
19. [AgentRole and AgentRoleBinding — Agent RBAC](#agentrole-and-agentrolebinding)
20. [CognitiveAdmissionPolicy — Admission Control](#cognitiveadmissionpolicy)
21. [CognitiveLimitRange — Namespace Defaults](#cognitivelimitrange)
22. [CrossNamespaceGrant — Sharing Resources](#crossnamespacegrant)
23. [Solution — Intent-Level Goals](#solution)
24. [Probes — Health, Readiness, and Reasoning](#probes)
25. [Deployment Strategies](#deployment-strategies)
26. [Securing the Platform](#securing-the-platform)

---

## Resource Model Overview

Kognition follows the Kubernetes resource model. Every resource has:

```yaml
apiVersion: kognition.dev/v1alpha1
kind: <ResourceKind>
metadata:
  name: <resource-name>
  namespace: <namespace>   # omit for cluster-scoped resources
spec:
  # desired state
status:
  # observed state (managed by controllers)
```

### Scope Reference

| Scope | Resources |
|-------|-----------|
| **Cluster-scoped** | ModelClass, MemoryClass, ToolClass, KnowledgeBaseClass, ProviderRegistry |
| **Namespace-scoped** | All other resources |

---

## ModelClass

A **ModelClass** groups model deployments by provisioner, latency tier, and cost. It is cluster-scoped — one ModelClass can be shared across all namespaces.

### Spec Fields

| Field | Description |
|-------|-------------|
| `provisioner` | Provider type: `foundry`, `openai`, `anthropic`, `ollama` |
| `latencyClass` | `low`, `medium`, `high` — affects scheduling priority |
| `costTier` | `free`, `standard`, `premium` — for quota enforcement |
| `contextWindow` | Maximum context length (tokens) |
| `modalities` | Supported input types: `text`, `vision`, `audio` |

### Sample

📁 [`samples/simple-agent/manifests.yaml`](samples/simple-agent/manifests.yaml) — Lines 4–17

```yaml
kind: ModelClass
apiVersion: kognition.dev/v1alpha1
metadata:
  name: foundry
spec:
  provisioner: foundry
  latencyClass: low
  costTier: standard
  contextWindow: 128000
  modalities:
    - text
```

---

## SecretRef

A **SecretRef** provides credential access for Models. It decouples secrets from model definitions and supports multiple authentication backends.

### Backend Types

| Type | Description | Use Case |
|------|-------------|----------|
| `internal` | Kognition-managed (stored in annotation) | Dev/local clusters |
| `env` | Environment variable | Simple API key |
| `kubernetes` | Native K8s Secret reference | Production |

### Azure Identity Credential Types

When using `type: "azure-identity"` in the annotation:

| Credential Type | Description |
|-----------------|-------------|
| `client-secret` | App registration with secret |
| `managed-identity` | Azure Managed Identity (user- or system-assigned) |
| `workload-identity` | Kubernetes workload identity federation |
| `default` | Auto-detect via DefaultAzureCredential |

> **Secretless auth.** `managed-identity`, `workload-identity`, and `default`
> (with no `clientSecret`) require **no stored secret** — the control plane and
> both runtimes (agent and KB ingester) select the credential end-to-end. See
> [ADR-0005](docs/adr/0005-secretless-provider-auth.md) and
> [docs/security.md](docs/security.md).

### Sample

📁 [`samples/foundry-agent/manifests.yaml`](samples/foundry-agent/manifests.yaml)

```yaml
kind: SecretRef
apiVersion: kognition.dev/v1alpha1
metadata:
  name: foundry-creds
  annotations:
    kognition.dev/internal-secret-data: |
      {
        "type": "azure-identity",
        "credentialType": "client-secret",
        "tenantId": "YOUR_TENANT_ID",
        "clientId": "YOUR_CLIENT_ID",
        "clientSecret": "YOUR_CLIENT_SECRET"
      }
spec:
  backendType: internal
  backendRef: secret/foundry-creds
  key: token
```

---

## Model

A **Model** binds a specific AI model deployment to a ModelClass. It references credentials and sets provider-specific parameters.

### Spec Fields

| Field | Description |
|-------|-------------|
| `className` | References a ModelClass by name |
| `modelID` | The model identifier at the provider (e.g., `gpt-4.1`) |
| `provider` | Provider driver: `foundry`, `openai`, `openai-compatible` |
| `endpoint` | Provider API endpoint URL |
| `capabilities` | What the model supports: `chat`, `tool-calling`, `vision` |
| `credentialRef` | References a SecretRef for authentication |
| `parameters` | Provider-specific key-value pairs (e.g., `apiVersion`) |

### Provider Endpoint Formats

| Provider | Endpoint Format |
|----------|----------------|
| `foundry` | `https://<resource>.services.ai.azure.com` |
| `openai` | `https://<resource>.cognitiveservices.azure.com/openai/deployments/<deployment-name>` |
| `openai-compatible` | `https://api.openai.com/v1` |

### Sample

📁 [`samples/foundry-agent/manifests.yaml`](samples/foundry-agent/manifests.yaml)

```yaml
kind: Model
apiVersion: kognition.dev/v1alpha1
metadata:
  name: gpt-4-1
spec:
  className: foundry
  modelID: gpt-4.1
  provider: foundry
  endpoint: https://your-resource.services.ai.azure.com
  capabilities:
    - chat
    - tool-calling
  credentialRef:
    name: foundry-creds
  parameters:
    apiVersion: "2024-10-21"
```

---

## Agent

An **Agent** is the core workload resource — a declarative specification for an AI agent that can reason, use tools, and remember context.

### Spec Fields

| Field | Description |
|-------|-------------|
| `modelRef` | References the Model to use |
| `instructions` | Inline text or reference to a Prompt resource |
| `tools` | List of Tool references |
| `memory.claims` | List of MemoryClaim references |
| `runtime.class` | Runtime class (e.g., `default`, `gpu`) |
| `runtime.timeout` | Max duration per invocation |
| `runtime.maxTokens` | Token limit per invocation |
| `autonomyClassRef` | References an AutonomyClass |
| `replicas` | Desired number of instances |
| `probes` | Health, readiness, and reasoning probe config |
| `companions` | References to collaborating agents |
| `knowledgeBaseClaims` | References to `KnowledgeBaseClaim` bindings |

### Instructions — Inline vs Reference

```yaml
# Inline (simple agents)
instructions:
  inline: "You are a helpful assistant."

# Reference (reusable prompts with parameters)
instructions:
  ref:
    name: my-prompt-resource
```

### Sample — Minimal Agent

📁 [`samples/simple-agent/manifests.yaml`](samples/simple-agent/manifests.yaml)

```yaml
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: hello-agent
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: |
      You are a helpful assistant. Answer questions concisely.
  runtime:
    class: default
    timeout: 30s
    maxTokens: 4096
```

### Sample — Agent with Tools and Memory

📁 [`samples/full-stack-agent/manifests.yaml`](samples/full-stack-agent/manifests.yaml)

```yaml
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: research-agent
spec:
  modelRef:
    name: gpt-4o
  instructions:
    ref:
      name: research-assistant-prompt
  tools:
    - ref:
        name: web-search
      required: false
    - ref:
        name: calculator
      required: true
  memory:
    claims:
      - name: research-agent-episodic
      - name: research-agent-semantic
  runtime:
    class: default
    timeout: 60s
    maxTokens: 8192
  autonomyClassRef:
    name: supervised
  replicas: 2
```

---

## AgentInstance

An **AgentInstance** represents a single invocation of an Agent. Creating one triggers the agent to process the input and produce output.

### Spec Fields

| Field | Description |
|-------|-------------|
| `agentRef` | The Agent to invoke |
| `input` | The user prompt/question |
| `revision` | Optional: pin to a specific Agent revision |

### Status Fields

| Field | Description |
|-------|-------------|
| `phase` | `Pending` → `Running` → `Ready` or `Failed` |
| `output` | The agent's response |
| `tokenUsage` | Tokens consumed (input + output) |
| `startedAt` / `completedAt` | Timing information |

### Sample

📁 [`samples/agentinstance/manifests.yaml`](samples/agentinstance/manifests.yaml)

```yaml
kind: AgentInstance
apiVersion: kognition.dev/v1alpha1
metadata:
  name: hello-run-1
spec:
  agentRef:
    name: hello-agent
  revision: 1
  sessionContext:
    input: "What is the capital of France?"
```

```bash
# Apply and watch
kubectl apply -f samples/agentinstance/manifests.yaml
kubectl get agentinstances.kognition.dev hello-run-1 -w

# Read the output
kubectl get agentinstances.kognition.dev hello-run-1 -o jsonpath='{.status.output}'
```

---

## Prompt

A **Prompt** resource stores reusable instructions with template parameters. Multiple agents can share the same prompt with different parameter values.

### Spec Fields

| Field | Description |
|-------|-------------|
| `content` | The prompt template body (supports `{{paramName}}` placeholders) |
| `parameters` | Named parameters with descriptions, defaults, and required flags |
| `metadata` | User-defined key-value metadata |

### Sample

📁 [`samples/prompt/manifests.yaml`](samples/prompt/manifests.yaml)

```yaml
kind: Prompt
apiVersion: kognition.dev/v1alpha1
metadata:
  name: research-assistant-prompt
spec:
  content: |
    You are a research assistant with access to web search and calculation tools.

    ## Guidelines
    - Always cite sources when using web search results
    - Break complex calculations into steps
    - Use episodic memory to recall previous research sessions

    ## Persona
    - Name: {{name}}
    - Specialization: {{domain}}
  parameters:
    - name: name
      description: The name to use for the assistant persona
      required: false
      default: "Research Assistant"
    - name: domain
      description: The research domain specialization
      required: true
```

---

## ToolClass and Tool

**ToolClass** defines a class of tool provisioner (cluster-scoped). **Tool** declares a specific tool available to agents (namespace-scoped).

### ToolClass Spec

| Field | Description |
|-------|-------------|
| `provisioner` | Type: `mcp`, `openapi`, `browser`, `shell` |
| `executionTier` | Where the tool runs: `process`, `sidecar`, `pod`, `vm` |
| `transport` | Communication: `stdio`, `sse`, `http` |
| `resourceLimits` | CPU, memory, and timeout caps |

### Tool Spec

| Field | Description |
|-------|-------------|
| `className` | References a ToolClass |
| `protocol` | Interface: `MCP`, `OpenAPI`, `browser`, `shell` |
| `description` | Human-readable description (passed to the model) |
| `endpoint` | Tool endpoint or command |
| `schema` | Tool contract or JSON Schema; legacy `inputSchema` remains accepted |

### Protocols

| Protocol | API status | Built-in execution |
|----------|------------|--------------------|
| `MCP` | Admitted | MCP JSON-RPC over HTTP |
| `OpenAPI` | Admitted | HTTP request execution |
| `browser` | Admitted | Requires an external executor/plugin |
| `shell` | Admitted | Requires an external executor/plugin |

The API also models `stdio` and `sse` transports on ToolClass, but the current
controller-side executor runs only OpenAPI over HTTP and MCP over HTTP. It
returns a structured unsupported-protocol result for browser and shell tools.

### Sample

📁 [`samples/tools/manifests.yaml`](samples/tools/manifests.yaml)

```yaml
---
kind: Tool
apiVersion: kognition.dev/v1alpha1
metadata:
  name: web-search
spec:
  className: sandbox-openapi
  protocol: OpenAPI
  description: Search the demo product documentation corpus
  endpoint: http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/knowledge-search
  inputSchema:
    type: object
    properties:
      query:
        type: string
    required: [query]
---
# The `sandbox-openapi` ToolClass is shipped with the shared tool-sandbox
# manifest (samples/workflows/tool-sandbox.yaml). Apply that first, then the
# calculator Tool below points at its HTTP endpoint.
kind: Tool
apiVersion: kognition.dev/v1alpha1
metadata:
  name: calculator
spec:
  className: sandbox-openapi
  protocol: OpenAPI
  description: Perform mathematical calculations
  endpoint: http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/calculator
  inputSchema:
    type: object
    properties:
      expression:
        type: string
        description: Mathematical expression to evaluate
    required:
      - expression
```

---

## MemoryClass and MemoryClaim

**MemoryClass** defines a memory backend provisioner (cluster-scoped). **MemoryClaim** requests memory capacity from a MemoryClass (namespace-scoped) — analogous to PersistentVolumeClaim in Kubernetes.

### MemoryClass Spec

| Field | Description |
|-------|-------------|
| `backendType` | Storage: `pgvector`, `in-memory`, `redis`, `mem0` |
| `parameters` | Backend-specific config (dimensions, index type, etc.) |
| `reclaimPolicy` | `Delete`, `Retain`, or `Archive` |
| `summarizationConfig` | Automatic summarization settings |
| `consistencyMode` | `lww` (last-write-wins), `crdt`, `lock` |

### MemoryClaim Spec

| Field | Description |
|-------|-------------|
| `memoryClassName` | References the MemoryClass |
| `type` | Memory type: `episodic`, `semantic`, `working`, `procedural`, `shared` |
| `capacity` | Storage capacity (e.g., `1Gi`, `512Mi`) |
| `accessModes` | `ReadWrite`, `ReadOnly` |

### Memory Types

| Type | Description | Use Case |
|------|-------------|----------|
| `episodic` | Timestamped interaction history | Conversation recall |
| `semantic` | Structured knowledge indexed for retrieval | Domain knowledge |
| `working` | Short-term scratchpad (cleared per session) | Multi-step reasoning |
| `procedural` | Learned procedures and patterns | Skill acquisition |
| `shared` | Cross-agent shared memory | Multi-agent collaboration |

### Runtime wiring

When an Agent declares `spec.memory.claims`, the dispatcher resolves the bound
`MemoryVolume`, injects its backend handle and the memory-service URL, and the
runtime automatically attaches two tools:

| Built-in tool | Purpose |
|---------------|---------|
| `memory_store(content, tag)` | Persist content to the bound volume with optional metadata. |
| `memory_recall(query, k, tag)` | Search the bound volume using semantic and lexical retrieval. |

If several claims are listed, the current runtime chooses one volume handle:
the first bound `shared` claim, otherwise the first bound claim. Two agents
that reference the same shared claim therefore select the same handle.

The selected handle must also be provisioned in the memory service. The
Kubernetes claim reconciler currently creates `MemoryVolume` metadata but does
not call `/v1/memory/provision`, so built-in tools need an operator-provisioned
handle today. The runnable samples use explicit sandbox Tool CRs to avoid that
manual step while still exercising claim admission and binding.

### Samples

| Sample | Coverage |
|--------|----------|
| 📁 [`samples/memory/manifests.yaml`](samples/memory/manifests.yaml) | Declarative — three `MemoryClass`es (pgvector / in-memory / redis) and four `MemoryClaim`s. No agent wired. |
| [`samples/memory-in-action/`](samples/memory-in-action/) | One agent, one `working` claim, and runnable sandbox store/recall Tool CRs. |
| [`samples/memory-all-types/`](samples/memory-all-types/) | All five claim types plus a peer agent sharing one claim. The current manifest uses tagged sandbox tools so all types can be compared independently. |

### Exercising every memory type

The walkthrough below uses [`samples/memory-all-types/`](samples/memory-all-types/) and assumes the cluster-default `MemoryClass` (installed by the Helm chart) and a `Model` named `gpt-4-1`.

```powershell
kubectl apply -f samples/memory-all-types/manifests.yaml

kubectl -n default wait --for=jsonpath='{.status.phase}'=Bound `
  memoryclaim/explorer-episodic memoryclaim/explorer-semantic `
  memoryclaim/explorer-working memoryclaim/explorer-procedural `
  memoryclaim/explorer-shared --timeout=60s

kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready `
  agent/memory-explorer --timeout=120s

# One turn per memory type — the agent's instructions force a `tag` on
# every memory_store call.
.\bin\kognitionctl.exe chat memory-explorer "Remember as a working note: intermediate sum = 42." --mode kubernetes
.\bin\kognitionctl.exe chat memory-explorer "Record this episodic event: on 2024-03-12 the user reported issue #871." --mode kubernetes
.\bin\kognitionctl.exe chat memory-explorer "Remember this semantic fact: the user's dog is named Pepper." --mode kubernetes
.\bin\kognitionctl.exe chat memory-explorer "Remember this procedure: to deploy, run 'make build' then 'kubectl apply -f manifests.yaml'." --mode kubernetes
.\bin\kognitionctl.exe chat memory-explorer "Remember this shared team policy: every release must include an SBOM." --mode kubernetes

# Fresh session — has to come back through memory_recall, not in-process state.
.\bin\kognitionctl.exe chat memory-explorer "What do you know? List the procedure, the dog name, issue #871, the working note, and the shared policy." --mode kubernetes
```

This sample deliberately uses the generic sandbox tools, so verify its tagged
records through the sandbox endpoint:

```powershell
kubectl -n default port-forward service/tool-sandbox 18080:8080

foreach ($t in "working","episodic","semantic","procedural","shared") {
    Write-Host "`n=== $t ===" -ForegroundColor Cyan
    Invoke-RestMethod -Uri http://127.0.0.1:18080/memory-recall `
      -Method POST -ContentType "application/json" `
      -Body (@{ query = ""; tag = $t; top_k = 10 } | ConvertTo-Json) `
      | ConvertTo-Json -Depth 5
}
```

See [`samples/memory-all-types/README.md`](samples/memory-all-types/README.md)
for the full tagged-sandbox walkthrough. For first-party memory, the important
current limitation is one selected volume per agent runtime; shared-volume
selection and vector-backed semantic retrieval are available after the chosen
handle is provisioned in the memory service.

---

## KnowledgeBaseClass and KnowledgeBase

**KnowledgeBaseClass** declares a knowledge-base provisioner (cluster-scoped).
**KnowledgeBase** holds sources in a namespace, and a
**KnowledgeBaseClaim** binds an Agent to that corpus. When a claim resolves,
the runtime automatically exposes one `knowledge_search` tool that searches
inline content in memory or a configured pgvector store.

### KnowledgeBaseClass Spec

| Field | Description |
|-------|-------------|
| `provisioner` | Driver identifier, such as `inline` or `pgvector`. |
| `indexMode` | Index strategy: `inline`, `dense`, `sparse`, or `hybrid`. |
| `embeddingModelRef` | `Model` used for ingestion and query embeddings. |
| `chunkStrategy` | Default `{size, overlap, mode}` for splitting ingested documents. |
| `reclaimPolicy` | `Retain`, `Delete`, or `Archive` for the backing index when a `KnowledgeBase` is removed. |
| `vectorStore` | Backing store configuration. The in-tree external ingester currently supports `type: pgvector`. |
| `parameters` | Provisioner-specific key/value pairs. |

### KnowledgeBase Spec

| Field | Description |
|-------|-------------|
| `knowledgeBaseClassName` | References the cluster-scoped `KnowledgeBaseClass`. |
| `sources` | List of data sources (see Source Types below). |
| `indexConfig` | Per-KB overrides for `embeddingModelRef`, `chunkSize`, `chunkOverlap`. |
| `refreshPolicy` | `{mode: periodic|onDemand|webhook, interval, webhookPath}` controlling rebuilds. |

### Source Types

| Type | Status | Description |
|------|--------|-------------|
| `inline` | Searchable directly | Document body in `data`; no external ingester is required. |
| `url` | Ingested with pgvector | Remote document in `uri`. |
| `git` | Ingested with pgvector | `git.url`, optional `git.branch`, and optional `git.path`. |
| `configMap` | Ingested with pgvector in Kubernetes mode | `configMap.name`; each data value becomes a document. |
| `blob` | Ingested with pgvector | `blob.provider` (`azure` or `s3`), `bucket`, optional prefix/filter fields, and optional `credentialSecretRef`. |
| `file`, `directory` | Declared but unsupported by the external ingester | Payload construction rejects these source types until a safe filesystem contract is defined. |

### Wiring into an Agent

Create a claim and reference the claim from the Agent:

```yaml
apiVersion: kognition.dev/v1alpha1
kind: KnowledgeBaseClaim
metadata:
  name: orbital-notes-claim
spec:
  knowledgeBaseName: orbital-notes-kb
  accessMode: ReadOnly
---
apiVersion: kognition.dev/v1alpha1
kind: Agent
metadata:
  name: orbital-docs-agent
spec:
  knowledgeBaseClaims:
    - name: orbital-notes-claim
      required: true   # block readiness until the KB is Ready
```

The dispatcher follows each claim to its bound KnowledgeBase and injects the
resolved inline or pgvector configuration into the runtime. No Tool CR is
required for `knowledge_search`. The basic sample also declares a sandbox
search Tool to demonstrate generic OpenAPI dispatch over the same corpus; the
real pgvector samples use only the built-in tool.

### Sample

📁 [`samples/knowledge-base/manifests.yaml`](samples/knowledge-base/manifests.yaml) — full end-to-end demo with grounded chat. See [`samples/knowledge-base/README.md`](samples/knowledge-base/README.md) for the walkthrough.

```yaml
---
# Cluster-scoped: in-runtime inline KB provisioner (no embedder, no external index).
kind: KnowledgeBaseClass
apiVersion: kognition.dev/v1alpha1
metadata:
  name: inline-kb
spec:
  provisioner: inline
  indexMode: inline
  reclaimPolicy: Retain
---
# Namespace-scoped: three inline chunks about a fictional product.
kind: KnowledgeBase
apiVersion: kognition.dev/v1alpha1
metadata:
  name: orbital-notes-kb
  namespace: default
spec:
  knowledgeBaseClassName: inline-kb
  sources:
    - type: inline
      data: |
        Orbital Mark 7 release schedule: General availability ships on 2026-09-15.
        Public preview opened on 2026-06-01 for partners under the Mark 7 EAP.
    - type: inline
      data: |
        Mark 7 supported regions at launch: westus3, eastus2, northeurope, and
        australiaeast. Additional regions follow in the 2026-Q4 expansion wave.
    - type: inline
      data: |
        Mark 7 known limitations on day one: no fine-tuning endpoint
        (planned for Mark 7.2), no JSON-mode for streaming responses, and a
        32k-token context window.
---
kind: KnowledgeBaseClaim
apiVersion: kognition.dev/v1alpha1
metadata:
  name: orbital-notes-claim
  namespace: default
spec:
  knowledgeBaseName: orbital-notes-kb
  accessMode: ReadOnly
---
# Agent that grounds against the KB. The instructions tell the model to
# call knowledge_search before answering any factual question.
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: orbital-docs-agent
  namespace: default
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: |
      You answer questions about the Orbital Mark 7 product. Always call
      knowledge_search first and quote the relevant chunk verbatim before
      summarizing. If knowledge_search returns nothing useful, say so.
  knowledgeBaseClaims:
    - name: orbital-notes-claim
      required: true
```

Run the grounded chat:

```powershell
kognitionctl chat orbital-docs-agent `
  "When does Orbital Mark 7 ship to general availability and what regions are supported at launch?" `
  --mode kubernetes
```

### Caveats

- **Inline and pgvector use different retrieval paths.** Inline content uses a small keyword fallback. A pgvector class uses the configured embedding Model and nearest-neighbor search.
- **External ingestion is opt-in.** Enable both `kbPgvector` and `kbIngester` in the chart for URL, Git, ConfigMap, and blob sources.
- **One generated tool spans all claims.** `knowledge_search` multiplexes across every resolved claim and applies claim-level `topK` and `minScore` overrides.
- **Controller RBAC must include all KB resources.** The Helm chart grants access to classes, bases, claims, index runs, and index schedules.

---

## AutonomyClass and TrustProfile

**AutonomyClass** defines a tier of agent autonomy — what actions an agent can take without human approval. **TrustProfile** tracks an agent's trust score over time, enabling progressive autonomy.

### AutonomyClass Spec

| Field | Description |
|-------|-------------|
| `level` | Numeric tier (0=none, 10=supervised, 50=assisted, 90=autonomous) |
| `requiresApproval` | Whether human approval is needed before actions |
| `maxCostPerAction` | Token/cost budget threshold before escalation |
| `allowedTools` | Which tool classes can be used at this level |
| `approvalPolicy` | Per-action-type approval requirements |
| `constraints` | Limits on concurrent actions and token budgets |

### TrustProfile Spec

| Field | Description |
|-------|-------------|
| `agentRef` | The agent being evaluated |
| `initialScore` | Starting trust score (0–100) |
| `initialClass` / `maxClass` | Autonomy class bounds |
| `scoringWeights` | How events affect the score |
| `evaluationInterval` | How often to re-evaluate |

### Sample

Apply [`samples/autonomy/manifests.yaml`](samples/autonomy/manifests.yaml) first, then use the exact PowerShell commands in [`samples/autonomy/trustprofile.template.yaml`](samples/autonomy/trustprofile.template.yaml) to bind the TrustProfile to the server-generated Agent UID.

---

## Workflow

A **Workflow** orchestrates multiple steps (agents or tools) as a directed graph. The topology is **edge-primary**: `steps` are pure execution nodes and `edges` define every transition, including conditional and parallel ones. Workflow execution supports data flow between steps, conditional branching (CEL), parallel dispatch with concurrency caps, retries with backoff, human-in-the-loop approval gates with quorum, shared per-run memory, and OpenTelemetry tracing.

A Workflow is a **template** — it does not run on its own. To execute it, create a `WorkflowRun` that references the Workflow and supplies the input and parameters for that single execution.

### Workflow vs WorkflowRun

| Resource | Role |
|----------|------|
| `Workflow` | The reusable template: steps, edges, approval gates, budgets, defaults |
| `WorkflowRun` | A single execution: input string, parameters, status, approval audit log |

On first reconcile, the WorkflowRun snapshots the resolved `WorkflowSpec` into `status.snapshotSpec` so execution survives deletion of the parent Workflow.

### WorkflowSpec Fields

| Field | Description |
|-------|-------------|
| `steps` | List of execution nodes (agent- or tool-backed). |
| `edges` | Directed transitions between steps. Steps with no inbound edges are entry points (DAG). |
| `entryPoint` | Starting step. Inferred for DAGs; **required for cyclic workflows**. |
| `approvalGates` | Manual approval checkpoints attached to specific edges. |
| `budgets` | Workflow-level limits (`maxDuration`, `maxTokens`, `maxIterations`, `maxTokensPerCycle`). |
| `parameters` | Static `map<string,string>` exposed to step templates via `{{ .Params.key }}` and CEL via `params.key`. |
| `maxParallelSteps` | Cap on concurrently-running steps. `0` = unlimited parallelism. |
| `failFast` | Cancel all pending steps the moment any step fails. |
| `continueOnError` | Allow execution to continue when a step fails, at workflow scope. |
| `maxConcurrentRuns` | Cap on simultaneous WorkflowRuns of this Workflow. `0` = unlimited. |
| `historyLimit` | Max completed WorkflowRuns retained; oldest are garbage-collected. `0` = unlimited. |
| `memoryClaim` | Per-run shared `MemoryClaim` materialized for all steps in the run. |
| `memoryRetentionPolicy` | `Delete` (default) or `Retain` the per-run claim on terminal phase. |
| `emergent` | Planner-driven DAG mutation with `maxSteps` / `maxTokens` / `maxCost` budgets. |

### WorkflowStep Fields

| Field | Description |
|-------|-------------|
| `name` | Unique identifier within the workflow (used in edges, templates, and CEL). |
| `type` | `agent` (default) dispatches via an AgentJob; `tool` invokes a Tool directly with no LLM round-trip. |
| `agentRef` | Required when `type=agent`. Acts as the identity principal for ToolGrant evaluation when set on a tool step. |
| `toolRef` | Required when `type=tool`. Mutually exclusive with the agent path. |
| `input` | Go-template string for the step's input. See **Data Flow & Templating** below. |
| `timeout` | Per-step timeout for acyclic and cyclic workflows. |
| `condition` | CEL expression that gates dispatch. If false, the step is marked `Skipped`. |
| `retry` | Per-step retry: `maxAttempts`, `backoff`, `backoffMultiplier`, `maxBackoff`. |
| `continueOnError` | Let this step fail without failing the workflow. |
| `memoryClaim` | Override the workflow-level shared claim with a per-step isolated claim. |

### WorkflowEdge Fields

| Field | Description |
|-------|-------------|
| `from` / `to` | Source and destination step names. |
| `condition` | CEL expression. When multiple edges target the same step, **any** edge whose condition evaluates true allows the step to run (OR semantics). |

### Triggering a Workflow

Create a `WorkflowRun` resource. Its `spec.input` populates `{{ .Input }}` in step templates and the `input` variable in CEL.

```yaml
apiVersion: kognition.dev/v1alpha1
kind: WorkflowRun
metadata:
  name: pipeline-run-001
spec:
  workflowRef:
    name: content-pipeline
  input: "the rise of agentic AI in enterprise software"
  parameters:
    audience: "engineering managers"
    tone: "analytical"
```

### Sample — Minimal Sequential Workflow

📁 [`samples/multi-agent-workflow/manifests.yaml`](samples/multi-agent-workflow/manifests.yaml)

A three-step pipeline: researcher → writer → reviewer.

```yaml
kind: Workflow
apiVersion: kognition.dev/v1alpha1
metadata:
  name: content-pipeline
spec:
  steps:
    - name: researcher
      agentRef:
        name: researcher
      input: "Research the following topic thoroughly: {{ .Input }}"
      timeout: 90s
    - name: writer
      agentRef:
        name: writer
      input: "Write an article based on this research:\n{{ .Steps.researcher.Output }}"
      timeout: 90s
    - name: reviewer
      agentRef:
        name: reviewer
      input: "Review this article:\n{{ .Steps.writer.Output }}"
      timeout: 60s
  edges:
    - { from: researcher, to: writer }
    - { from: writer, to: reviewer }
  budgets:
    maxDuration: 10m
    maxTokens: 50000
```

### Data Flow & Templating

Step `input` is a **Go template** with the following context:

| Reference | Source |
|-----------|--------|
| `{{ .Input }}` | `WorkflowRun.spec.input` |
| `{{ .Params.<key> }}` | `Workflow.spec.parameters` merged with `WorkflowRun.spec.parameters` |
| `{{ .Steps.<name>.Output }}` | Output of a completed predecessor step |
| `{{ .Steps.<name>.Phase }}` | Lifecycle phase of a predecessor (`Succeeded`, `Failed`, `Skipped`, …) |

Example combining all three:

```yaml
- name: summarizer
  agentRef:
    name: summarizer
  input: |
    Audience: {{ .Params.audience }}
    Tone: {{ .Params.tone }}
    Original question: {{ .Input }}
    Research: {{ .Steps.researcher.Output }}
    Draft: {{ .Steps.writer.Output }}
```

### Conditional Branching

Both **edges** and **steps** accept a `condition` expressed in [CEL](https://github.com/google/cel-spec). The same activation is available to both:

| Variable | Type | Description |
|----------|------|-------------|
| `steps` | `map<string, map<string, string>>` | `steps.<name>.output` and `steps.<name>.phase` for completed predecessors. |
| `input` | `string` | The WorkflowRun input. |
| `params` | `map<string, string>` | Merged parameters. |

The expression must return a `bool`. Evaluation is bounded to 100 ms.

```yaml
edges:
  # Always run drafting after research.
  - { from: researcher, to: writer }

  # Branch on reviewer verdict.
  - from: reviewer
    to: publisher
    condition: 'steps.reviewer.output.contains("approved")'

  - from: reviewer
    to: rewriter
    condition: 'steps.reviewer.output.contains("revision-needed")'
```

Step-level conditions skip the step entirely when false:

```yaml
- name: translate
  agentRef:
    name: translator
  condition: 'params.target_language != "en"'
  input: "Translate to {{ .Params.target_language }}:\n{{ .Steps.writer.Output }}"
```

**Cyclic workflows** (e.g. a reviewer → writer revision loop) are supported but **must set `entryPoint`** and cannot use per-step `timeout`, `retry`, or `continueOnError` — use the workflow-level equivalents.

### Parallel Dispatch

Any step with multiple outbound edges fans out. All downstream steps whose inbound conditions are satisfied are dispatched in parallel. Use `maxParallelSteps` to cap concurrency.

```yaml
spec:
  maxParallelSteps: 3
  steps:
    - { name: fetch,      agentRef: { name: fetcher } }
    - { name: summarize,  agentRef: { name: summarizer } }
    - { name: translate,  agentRef: { name: translator } }
    - { name: sentiment,  agentRef: { name: sentiment-analyzer } }
    - { name: aggregate,  agentRef: { name: aggregator },
        input: |
          Summary: {{ .Steps.summarize.Output }}
          Translation: {{ .Steps.translate.Output }}
          Sentiment: {{ .Steps.sentiment.Output }} }
  edges:
    - { from: fetch, to: summarize }
    - { from: fetch, to: translate }
    - { from: fetch, to: sentiment }
    - { from: summarize, to: aggregate }
    - { from: translate, to: aggregate }
    - { from: sentiment, to: aggregate }
```

`aggregate` runs after **all** of its inbound predecessors complete (AND semantics on join; OR semantics applies only when edge conditions disagree on whether the same step should run).

### Tool Steps

Set `type: tool` to invoke a Tool directly, with no agent, prompt, or LLM round-trip. Useful for deterministic transforms, fetches, validators, and side-effects between cognitive steps.

```yaml
steps:
  - name: fetch-doc
    type: tool
    toolRef:
      name: http-fetch
    input: '{ "url": "{{ .Input }}" }'

  - name: summarize
    agentRef:
      name: summarizer
    input: "Summarize:\n{{ .Steps.fetch-doc.Output }}"

  - name: write-to-store
    type: tool
    toolRef:
      name: blob-writer
    # agentRef acts as the identity principal for ToolGrant evaluation.
    agentRef:
      name: summarizer
    input: '{ "key": "summary.txt", "value": {{ .Steps.summarize.Output | toJson }} }'
```

### Human-in-the-Loop — Approval Gates

`approvalGates` pause execution **on a specific edge** until a human submits a decision. The gate's `requiredApprovers` field implements quorum (multi-party sign-off), and `timeout` auto-rejects pending gates.

```yaml
spec:
  steps:
    - { name: draft,    agentRef: { name: writer } }
    - { name: publish,  agentRef: { name: publisher } }
  edges:
    - { from: draft, to: publish }
  approvalGates:
    - from: draft
      to: publish
      requiredApprovers: 2     # quorum: two distinct approvers required
      timeout: 2h              # auto-reject if not approved within 2 hours
```

**Submitting a decision (CLI):**

```bash
# Single-approver gate (legacy/anonymous path).
kognitionctl approve <approval-id> --decision approve --reason "ship it"

# Direct REST call to the run-scoped subresource (supports quorum).
curl -X POST \
  https://APISERVER/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/pipeline-run-001/approval \
  -H 'Content-Type: application/json' \
  -d '{
        "step": "publish",
        "decision": "approve",
        "approver": "alice@example.com",
        "comment": "LGTM"
      }'
```

When `requiredApprovers > 1`, the `approver` field is **required** and must be unique per submission — only distinct identities count toward the quorum. To reject, send `"decision": "reject"`; rejection is immediate and does not require quorum.

Every decision is appended to `WorkflowRun.status.approvalRecords` as an audit log entry with timestamp, approver, decision, and optional comment.

> ⚠️ The apiserver does not currently authenticate the `approver` claim. Production deployments needing trustworthy quorum **must** front the approval endpoint with an authenticating proxy that overwrites or validates the `approver` field.

### Retries, Timeouts, and Error Handling

```yaml
steps:
  - name: flaky-fetch
    agentRef:
      name: fetcher
    timeout: 30s
    retry:
      maxAttempts: 4         # 1 initial + 3 retries
      backoff: 2s
      backoffMultiplier: 2.0 # 2s → 4s → 8s
      maxBackoff: 30s
    continueOnError: true    # workflow proceeds even if all attempts fail
```

| Knob | Scope | Effect |
|------|-------|--------|
| Step `retry` | All | Re-dispatch on failure with exponential backoff. |
| Step `timeout` | All | Single-attempt deadline. |
| Step `continueOnError` | All | Failed step does not fail the workflow. |
| Workflow `failFast` | All | Cancel pending parallel steps on first failure. |
| Workflow `continueOnError` | All | Do not fail the workflow when any step fails. |
| Workflow `budgets.maxDuration` | All | Hard wall-clock cap for the entire run. |
| Workflow `budgets.maxTokens` | All | Cap on total tokens consumed across all steps. |

`failFast: true` and `continueOnError: true` are mutually exclusive in semantics — pick one error policy per workflow.

### Shared Memory Across Steps

Setting `spec.memoryClaim` materializes a single `MemoryClaim` for the entire run and binds it to every step, so agents can collaborate through shared memory volume (`ReadWriteMany`).

```yaml
spec:
  memoryClaim:
    memoryClassName: ephemeral
    accessMode: ReadWriteMany
  memoryRetentionPolicy: Delete  # or Retain to keep the claim after the run completes
  steps:
    - { name: researcher, agentRef: { name: researcher } }
    - { name: writer,     agentRef: { name: writer } }
    - { name: reviewer,   agentRef: { name: reviewer } }
```

A per-step `step.memoryClaim` overrides the run-level claim for that step in isolation (useful when one step needs its own scratch space).

### Concurrency and Retention at the Workflow Level

```yaml
spec:
  maxConcurrentRuns: 5   # at most 5 WorkflowRuns of this Workflow active at once
  historyLimit: 50       # keep the 50 most recent completed runs; GC older ones
```

### Observability

Workflow observability includes:

- **OpenTelemetry spans** — one root span per run, child spans per step, with attributes for step name, phase, attempts, agent/tool refs, token usage.
- **Prometheus retained projections** — `kognition_workflow_active_runs`, `kognition_workflow_retained_runs`, `kognition_workflow_retained_step_duration_seconds_sum`, and `kognition_workflow_retained_step_duration_observations`. The controller reconstructs these gauges from retained WorkflowRuns and their owned AgentJobs after restart. Values can decrease when `historyLimit` removes records, and missing duration evidence emits no series.
- **CLI step timeline:**

  ```bash
  kognitionctl workflow logs pipeline-run-001
  ```

  Prints a chronological table of every step with start time, duration, attempts, and final phase.

- **Approval audit:** `kubectl get workflowrun pipeline-run-001 -o yaml` shows `status.approvalRecords[]` with every approve/reject decision, the approver, comment, and timestamp.

### Putting It All Together

The canonical end-to-end sample exercising agents, edges, an approval gate, and budgets:

📁 [`samples/multi-agent-workflow/manifests.yaml`](samples/multi-agent-workflow/manifests.yaml)

Apply it and trigger a run:

```bash
kubectl apply -f samples/multi-agent-workflow/manifests.yaml

cat <<EOF | kubectl apply -f -
apiVersion: kognition.dev/v1alpha1
kind: WorkflowRun
metadata:
  name: pipeline-run-001
  namespace: default
spec:
  workflowRef:
    name: content-pipeline
  input: "the rise of agentic AI in enterprise software"
EOF

kognitionctl workflow logs pipeline-run-001
```

---

## AgentService and IntentGateway

**AgentService** exposes an agent for external or inter-agent communication (analogous to a Kubernetes Service). **IntentGateway** routes incoming requests to agents, AgentServices, or workflows based on patterns.

Endpoint behavior depends on deployment mode:

| Mode | AgentService endpoint | IntentGateway submission |
|-------|-----------------------|--------------------------|
| `standalone` | `POST /apis/kognition.dev/v1alpha1/namespaces/<namespace>/agentservices/<name>/invoke` delegates to the secured Agent chat path. | HTTP `POST /apis/kognition.dev/v1alpha1/namespaces/<namespace>/intentgateways/<name>/goals` and unary gRPC `kognition.v1alpha1.IntentGateway/SubmitGoal` create a routed Solution. |
| `kubernetes` | The controller creates a core `Service` selecting the current shared Agent runtime, or a dedicated immutable revision workload when `revisionPin` is set, and reports its full `/invoke` URL in status. | The dedicated ClusterIP `intent-gateway` Deployment serves the same HTTP and gRPC contracts on ports `8080` and `8081`. It reports its HTTP service-DNS endpoint only while Available. |

`revisionPin` is validated against immutable revision history and reported as
`status.activeRevision`. In Kubernetes mode, a pinned service creates a
service-owned Deployment rendered from the saved Agent revision; its core
Service selects only that workload. Updating or removing the pin replaces or
removes the dedicated workload without changing the shared current Agent
runtime. In standalone mode, the service invocation uses the same immutable
revision snapshot for its model call.

An IntentGateway route targeting a ready AgentService creates a Solution that
compiles to a one-shot AgentJob with the service-selected Agent configuration.
Pinned services use their immutable revision snapshot in both modes.

### AgentService Spec

| Field | Description |
|-------|-------------|
| `agentRef` | The Agent to expose |
| `revisionPin` | Optional: route traffic to a specific revision |
| `loadBalancingStrategy` | `round-robin` or `random` |
| `selector` | Match agents by label |
| `port` | Service port |

### IntentGateway Spec

| Field | Description |
|-------|-------------|
| `routes` | List of pattern→target mappings |
| `routes[].pattern` | Intent pattern (glob matching) |
| `routes[].targetRef` | Target Agent or Workflow name |
| `routes[].targetKind` | `Agent` or `Workflow` |

### Sample

📁 [`samples/routing/manifests.yaml`](samples/routing/manifests.yaml)

```yaml
---
kind: AgentService
apiVersion: kognition.dev/v1alpha1
metadata:
  name: research-service
spec:
  agentRef:
    name: research-agent
  loadBalancingStrategy: round-robin
  port: 8080
---
kind: IntentGateway
apiVersion: kognition.dev/v1alpha1
metadata:
  name: main-gateway
spec:
  routes:
    - pattern: "research *"
      targetRef:
        name: research-pipeline
      targetKind: Workflow
    - pattern: "summarize *"
      targetRef:
        name: summarizer-agent
      targetKind: Agent
    - pattern: "*"
      targetRef:
        name: hello-agent
      targetKind: Agent
```

---

## Evaluation, EvaluationDataset, and EvaluationRun

**EvaluationDataset** defines named test cases. **Evaluation** binds a dataset,
an executable target, graders, and thresholds. The Evaluation controller creates
an immutable **EvaluationRun** for each Evaluation generation; the run creates
one target AgentJob per dataset case and records per-case and aggregate results.

An `llm_judge` grader creates a second AgentJob after the target case completes.
The judge Agent receives the case input, expected answer, optional reference and
rubric, and target output. It must return JSON containing a score from `0` to
`1`, and may also return `passed`, `reason`, or `message`.

### Evaluation Spec

| Field | Description |
|-------|-------------|
| `targetRef` | Target resource. Evaluation currently executes `Agent` targets. |
| `datasetRef` | EvaluationDataset name, optional namespace, and optional version. |
| `graders` | Built-in graders and optional `llm_judge` graders. |
| `metricThresholds` | Required metrics and inclusive minimum or maximum bounds. |

Each dataset case requires `id` and `input`. `expected` drives deterministic
task-success checks. `reference` and `rubric` are supplied to an LLM judge.

### LLM Judge Requirements

| Parameter | Description |
|-------|-------------|
| `agentRef` | Required judge Agent in the same namespace as the EvaluationRun. |
| `metric` | Optional emitted metric name. Defaults to `judge_score`. |
| `passingScore` | Optional default score required to pass. Defaults to `0.7`. |
| `rubric` | Optional grader-level rubric. Overrides a case rubric. |
| `reference` | Optional grader-level reference. Overrides a case reference. |

The judge Agent needs a ModelGrant for its selected Model when ModelGrant policy
enforcement is enabled. A running EvaluationRun fails with `DatasetChanged` if
the referenced dataset changes after target jobs have been created. Apply a new
Evaluation generation or create a new EvaluationRun to evaluate updated data.

### Sample

The evaluation sample targets the `hello-agent` from `samples/simple-agent` and
uses `hello-agent-judge` as an LLM judge.

```powershell
./make.ps1 samples-install SAMPLE=simple-agent
./make.ps1 samples-install SAMPLE=evaluation

kubectl get evaluationruns.kognition.dev -n default
kubectl get evaluationrun hello-agent-quality-g2 -n default -o yaml
```

Inspect `status.caseResults` for each target output and metric, and
`status.metrics` for aggregate results. The sample and its prerequisites are in
[`samples/evaluation/`](samples/evaluation/).

---

## CognitiveQuota

**CognitiveQuota** sets resource budgets per namespace — analogous to Kubernetes ResourceQuota but for cognitive resources.

### Spec Fields

| Field | Description |
|-------|-------------|
| `hard` | Hard limits: `agents`, `tools`, `workflows`, count caps |
| `tokenBudget.maxInputTokens` | Max input tokens per reset interval |
| `tokenBudget.maxOutputTokens` | Max output tokens per reset interval |
| `resetInterval` | When counters reset (e.g., `24h`) |

### Sample

📁 [`samples/governance/quota.yaml`](samples/governance/quota.yaml)

```yaml
kind: CognitiveQuota
apiVersion: kognition.dev/v1alpha1
metadata:
  name: team-quota
spec:
  hard:
    agents: 10
    tools: 5
    workflows: 5
  tokenBudget:
    maxInputTokens: 1000000
    maxOutputTokens: 200000
  resetInterval: 24h
```

---

## AgentPolicy

**AgentPolicy** defines governance rules — OPA/Rego policies, tool grants, and safety rules that agents must comply with.

### Spec Fields

| Field | Description |
|-------|-------------|
| `rules` | Embedded OPA/Rego policy |
| `enforce` | `true` = block violations; `false` = warn only |
| `scope` | `workspace` (all agents) or `agent` (specific targets) |
| `targetAgents` | Agent names when scope=agent |
| `toolGrants` | Per-tool allow/deny with optional conditions |
| `safetyRules` | Named safety constraints with severity |
| `complianceRules` | Compliance constraints evaluated at admission |

### Sample

📁 [`samples/governance/policy.yaml`](samples/governance/policy.yaml)

```yaml
kind: AgentPolicy
apiVersion: kognition.dev/v1alpha1
metadata:
  name: safety-policy
spec:
  enforce: true
  scope: workspace
  toolGrants:
    - toolRef: web-search
      allow: true
      conditions: |
        package kognition.conditions
        allow { input.user_approved == true }
    - toolRef: shell-exec
      allow: false
  safetyRules:
    - name: no-pii-in-output
      severity: error
      rego: |
        package kognition.safety
        deny[msg] {
          contains(lower(input.output), "ssn")
          msg := "output appears to reference SSN"
        }
    - name: cost-limit
      severity: warning
      rego: |
        package kognition.safety
        deny[msg] {
          input.cost > 1.0
          msg := "action cost exceeds $1.00 limit"
        }
  complianceRules:
    - name: require-instructions
      severity: error
      rego: |
        package kognition.compliance
        deny[msg] {
          not input.spec.instructions.inline
          not input.spec.instructions.ref
          msg := "agents must have instructions defined"
        }
```

---

## Workspace

A **Workspace** provides multi-tenant isolation for a namespace — configuring retention policies and supply-chain requirements.

### Spec Fields

| Field | Description |
|-------|-------------|
| `displayName` | Human-friendly workspace name |
| `requireSignedArtifacts` | Enforce signature verification on Tools and Models |
| `retention.traceTTL` | How long to retain trace data |
| `retention.auditLogTTL` | Audit log retention |
| `retention.costRecordTTL` | Cost record retention |
| `retention.eventTTL` | Event retention |

### Sample

📁 [`samples/governance/workspace.yaml`](samples/governance/workspace.yaml)

```yaml
kind: Workspace
apiVersion: kognition.dev/v1alpha1
metadata:
  name: production
  namespace: production
spec:
  displayName: Production Workspace
  requireSignedArtifacts: true
  retention:
    traceTTL: 720h       # 30 days
    auditLogTTL: 2160h   # 90 days
    costRecordTTL: 2160h # 90 days
    eventTTL: 168h       # 7 days
```

---

## Cost Aggregation

Kognition records normalized model and tool operation costs in micro-USD. Use
the `cost` command to aggregate a Workspace by attribution dimension:

```powershell
kognitionctl --mode standalone --server http://localhost:8080 -n production cost --group-by agent,model
kognitionctl --mode kubernetes -n production --kube-namespace kognition-system cost --group-by solution,operation_type
```

Both modes accept `--start`, `--end`, `--agent-identity`, `--solution`,
`--solution-revision`, `--limit`, and `--offset`. Standalone queries use the
namespace-scoped API endpoint
`GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/costrecords`.

Kubernetes mode reads immutable cost-record ConfigMaps from
`--cost-namespace`, which defaults to `--kube-namespace`. Queries and retention
sweeps are deliberately bounded; a query that exceeds the supported ConfigMap
page limit returns an error and must be narrowed with filters or a time range.

---

## AgentIdentity

**AgentIdentity** records a deterministic SPIFFE URI for an agent. A URI alone is
attribution metadata, not cryptographic authentication. The selected issuer and
SVID-backed transport requirements are described in
[ADR-0018](docs/adr/0018-workload-identity-issuer.md); use the documented runtime
profile and acceptance scope when enabling that integration.

### Sample

The [Helm governance identity template](samples/helm-governance/templates/identities.yaml)
shows how the governance chart declares identities. The example below illustrates
the resource shape.

```yaml
---
kind: AgentIdentity
apiVersion: kognition.dev/v1alpha1
metadata:
  name: researcher-identity
spec:
  agentRef:
    name: researcher
---
kind: AgentIdentity
apiVersion: kognition.dev/v1alpha1
metadata:
  name: writer-identity
spec:
  agentRef:
    name: writer
```

---

## AgentRole and AgentRoleBinding

**AgentRole** defines permissions for agents (what resources they can access). **AgentRoleBinding** binds an agent to a role — analogous to Kubernetes RBAC.

### AgentRole

```yaml
rules:
  - resources: [Tool, Memory]
    verbs: [get, invoke, read, write]
```

### Sample

📁 [`samples/governance/rbac.yaml`](samples/governance/rbac.yaml)

```yaml
---
kind: AgentRole
apiVersion: kognition.dev/v1alpha1
metadata:
  name: tool-invoker
rules:
  - resources:
      - Tool
    verbs:
      - get
      - invoke
---
kind: AgentRole
apiVersion: kognition.dev/v1alpha1
metadata:
  name: memory-reader
rules:
  - resources:
      - Memory
    verbs:
      - get
      - read
---
kind: AgentRoleBinding
apiVersion: kognition.dev/v1alpha1
metadata:
  name: writer-tool-invoker
spec:
  roleRef:
    name: tool-invoker
  subjects:
    - kind: Agent
      name: writer
---
kind: AgentRoleBinding
apiVersion: kognition.dev/v1alpha1
metadata:
  name: researcher-memory-reader
spec:
  roleRef:
    name: memory-reader
  subjects:
    - kind: Agent
      name: researcher
```

---

## CognitiveAdmissionPolicy

**CognitiveAdmissionPolicy** provides OPA-based admission control — validates agent specs at creation/update time, similar to Kubernetes ValidatingWebhookConfiguration.

### Spec Fields

| Field | Description |
|-------|-------------|
| `failurePolicy` | `Fail` (block) or `Ignore` (warn only) |
| `matchKinds` | Which resource kinds to intercept |
| `matchOperations` | Which operations: `CREATE`, `UPDATE`, `DELETE` |
| `rego` | OPA policy to evaluate |

### Sample

📁 [`samples/governance/admission.yaml`](samples/governance/admission.yaml)

```yaml
kind: CognitiveAdmissionPolicy
apiVersion: kognition.dev/v1alpha1
metadata:
  name: no-empty-instructions
spec:
  failurePolicy: Deny
  matchKinds:
    - Agent
  matchOperations:
    - CREATE
    - UPDATE
  rego: |
    package kognition.admission

    deny[msg] {
      input.kind == "Agent"
      not input.spec.instructions.inline
      not input.spec.instructions.ref
      msg := "Agent must define instructions (inline or ref)"
    }
```

---

## CognitiveLimitRange

**CognitiveLimitRange** sets per-namespace defaults for agent sessions — applied automatically to new agents that don't specify their own limits.

### Spec Fields

| Field | Description |
|-------|-------------|
| `limits[].type` | Resource type (currently `Agent`) |
| `limits[].tokensPerSession` | Default max tokens |
| `limits[].timeoutSeconds` | Default timeout |
| `limits[].maxConcurrentSessions` | Max concurrent runs |

### Sample

📁 [`samples/governance/limitrange.yaml`](samples/governance/limitrange.yaml)

```yaml
kind: CognitiveLimitRange
apiVersion: kognition.dev/v1alpha1
metadata:
  name: default-limits
spec:
  limits:
    - type: Agent
      tokensPerSession: 16384
      timeoutSeconds: 120
      maxConcurrentSessions: 5
```

---

## CrossNamespaceGrant

**CrossNamespaceGrant** allows resources in one namespace to be referenced by resources in another. Without a grant, cross-namespace references are denied by admission control.

### Spec Fields

| Field | Description |
|-------|-------------|
| `from` | Namespace owning the resource |
| `to` | Namespace allowed to reference it (`*` for all) |
| `resources` | List of kind+name pairs to share |

### Sample

📁 [`samples/crossnamespace/manifests.yaml`](samples/crossnamespace/manifests.yaml)

```yaml
kind: CrossNamespaceGrant
apiVersion: kognition.dev/v1alpha1
metadata:
  name: share-model-to-team-b
  namespace: shared-models
spec:
  from: shared-models
  to: team-b
  resources:
    - kind: Model
      name: gpt-4-1
    - kind: Tool
      name: "*"
---
# Grant to all namespaces
kind: CrossNamespaceGrant
apiVersion: kognition.dev/v1alpha1
metadata:
  name: global-tools
  namespace: platform
spec:
  from: platform
  to: "*"
  resources:
    - kind: Tool
      name: calculator
    - kind: Tool
      name: web-search
```

---

## Solution

A **Solution** declares a high-level goal for the planner to decompose into executable steps. The controller decomposes it into a Workflow at runtime.

### Spec Fields

| Field | Description |
|-------|-------------|
| `goal` | Natural-language description of the desired outcome |
| `constraints` | Boundaries for the planner |
| `preferences` | Hints (key-value) |
| `seed` | Determinism seed for reproducible plans |
| `modelRef` | Planner model to use |
| `overrides` | Per-Solution quota/policy overrides |

### Sample

📁 [`samples/solution/manifests.yaml`](samples/solution/manifests.yaml)

```yaml
kind: Solution
apiVersion: kognition.dev/v1alpha1
metadata:
  name: write-blog-post
spec:
  goal: |
    Write a comprehensive blog post about the benefits of declarative
    AI orchestration for enterprise teams. Include research, examples,
    and a compelling narrative.
  constraints:
    - "Must be between 1000-2000 words"
    - "Must cite at least 3 sources"
    - "Must include code examples"
  preferences:
    tone: professional
    audience: platform-engineers
  modelRef:
    name: gpt-4-1
```

---

## Probes

Agents support three types of probes for observability and reliability:

| Probe | Purpose | What it Checks |
|-------|---------|----------------|
| **HealthProbe** | Liveness | Runtime is responsive |
| **ReadinessProbe** | Dependencies | Model, tools, memory reachable |
| **ReasoningProbe** | Cognitive | Agent produces correct answers to canary tasks |

### Sample

📁 [`samples/probes/manifests.yaml`](samples/probes/manifests.yaml)

```yaml
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: monitored-agent
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: "You are a reliable assistant."
  runtime:
    class: default
    timeout: 30s
    maxTokens: 4096
  probes:
    healthProbe:
      intervalSeconds: 30
      timeoutSeconds: 5
      failureThreshold: 3
      successThreshold: 1
    readinessProbe:
      intervalSeconds: 30
      timeoutSeconds: 10
      failureThreshold: 3
      successThreshold: 1
    reasoningProbe:
      intervalSeconds: 300
      timeoutSeconds: 30
      failureThreshold: 2
      canaryTask: "What is 2 + 2?"
      expectedOutcome: "4"
      maxTokens: 256
```

---

## Deployment Strategies

### Strategy 1: Direct kubectl Apply

Best for development and single-agent deployments.

```bash
kubectl apply -f samples/simple-agent/manifests.yaml
```

### Strategy 2: Kustomize Overlays

Use Kustomize for environment-specific configuration:

```bash
kubectl apply -k deploy/overlays/production/
```

### Strategy 3: Helm Charts for Complex Solutions

**Helm is the recommended approach for deploying multi-agent solutions in production.** It provides:

- Parameterized deployments (credentials, endpoints, model selection)
- Version management and rollback
- Reusable, shareable packages
- Value overrides per environment

#### Example: Deploying the Multi-Agent Pipeline

📁 [`samples/helm-multi-agent/`](samples/helm-multi-agent/)

```bash
# Install with custom values
helm install my-pipeline samples/helm-multi-agent \
  --set credentials.azure.tenantId=YOUR_TENANT_ID \
  --set credentials.azure.clientId=YOUR_CLIENT_ID \
  --set credentials.azure.clientSecret=YOUR_CLIENT_SECRET \
  --set model.endpoint=https://your-endpoint.services.ai.azure.com \
  --set model.apiVersion="2024-10-21"

# Check deployment
kubectl get agents,workflows

# Upgrade with new settings
helm upgrade my-pipeline samples/helm-multi-agent \
  --set agents.reviewer.runtime.timeout=90s

# Rollback if needed
helm rollback my-pipeline 1
```

#### Chart Structure

```
samples/helm-multi-agent/
├── Chart.yaml           # Chart metadata
├── values.yaml          # Default configuration
├── README.md            # Usage documentation
└── templates/
    ├── _helpers.tpl     # Template helpers
    ├── modelclass.yaml  # ModelClass (conditional)
    ├── secretref.yaml   # SecretRef with credentials
    ├── model.yaml       # Model resource
    ├── agents.yaml      # All agent definitions
    ├── workflow.yaml    # Workflow with edges
    └── NOTES.txt        # Post-install instructions
```

#### Writing Your Own Helm Chart

📁 [`samples/helm-governance/`](samples/helm-governance/) — A Helm chart for governance resources.

Key patterns:

```yaml
# templates/agent.yaml — conditional rendering
{{- range $name, $agent := .Values.agents }}
{{- if $agent.enabled }}
---
apiVersion: kognition.dev/v1alpha1
kind: Agent
metadata:
  name: {{ $name }}
  namespace: {{ $.Values.namespace }}
spec:
  modelRef:
    name: {{ $.Values.model.name }}
  instructions:
    inline: {{ $agent.instructions | quote }}
  runtime:
    class: {{ $agent.runtime.class | default "default" }}
    timeout: {{ $agent.runtime.timeout | default "30s" }}
    maxTokens: {{ $agent.runtime.maxTokens | default 4096 }}
{{- end }}
{{- end }}
```

### Strategy 4: GitOps with ArgoCD

For production environments, use ArgoCD to sync Kognition resources from Git:

📁 [`deploy/argocd-application.yaml`](deploy/argocd-application.yaml)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: kognition-agents
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/your-org/kognition-manifests
    targetRevision: main
    path: environments/production
  destination:
    server: https://kubernetes.default.svc
    namespace: production
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

### Strategy 5: Helm Umbrella Chart

For complex deployments with multiple pipelines, create an umbrella chart:

📁 [`samples/helm-umbrella/`](samples/helm-umbrella/)

```yaml
# Chart.yaml
apiVersion: v2
name: kognition-platform
description: Full Kognition deployment with multiple pipelines
type: application
version: 1.0.0
dependencies:
  - name: kognition
    version: ">=0.1.0"
    repository: "file://../../deploy/helm/kognition"
  - name: content-pipeline
    version: "0.1.0"
    repository: "file://../../samples/helm-multi-agent"
  - name: governance
    version: "0.1.0"
    repository: "file://../../samples/helm-governance"
```

This is a packaging illustration, not a supported Kubernetes installation path.
The [umbrella sample](samples/helm-umbrella/README.md) combines platform and producer
resources. Install a new core platform through
[Kubernetes setup](docs/getting-started/kubernetes.md) or
[AKS setup](docs/getting-started/aks.md), then configure governance and pipeline
workloads separately. Existing releases use the
[upgrade/recovery workflow](docs/features/knowledge-provider-execution/installation-recovery-contract.md).
Do not refresh bundled archives during recovery.

---

## Securing the Platform

Standalone API authentication and TLS are configurable. Baseline pod/container
hardening is enabled by default, and the fresh Kubernetes installer requires its
admission and Cilium enforcement profile before starting the controller. Review
the [security guide](docs/security.md) for production configuration and the
[ADRs](docs/adr/) for design rationale.

> **Deployment modes.** In **kubernetes mode** the resource API is the
> Kubernetes API server, so its authentication, RBAC, TLS, and audit apply. The
> API authentication and TLS steps below are specific to **standalone mode**
> (the first-party REST API server).

### API Authentication (OIDC)

By default the standalone API server does not authenticate requests and logs a
warning at startup. Enable OIDC so every request (except health checks) must
present a valid bearer token from your identity provider — Microsoft Entra ID,
Google, Auth0, Okta, Keycloak/Dex, or any compliant issuer.

```yaml
# Helm values
apiserver:
  auth:
    enabled: true
    oidc:
      issuer: "https://login.microsoftonline.com/<tenant-id>/v2.0"  # Entra ID
      audience: "<app-or-client-id>"
      usernameClaim: "preferred_username"
      requiredGroups: ["kognition-operators"]   # optional coarse authorization
```

Invalid or missing tokens get `401`; callers outside `requiredGroups` get `403`.
The verified identity is used by identity-sensitive handlers (e.g. approval
quorum), replacing the previously caller-asserted `approver` field.

### TLS

Terminate TLS at the API server with a `kubernetes.io/tls` Secret (e.g. from
cert-manager):

```yaml
apiserver:
  tls:
    enabled: true
    secretName: kognition-apiserver-tls
```

…or terminate at an Ingress via `ingress.enabled` + `ingress.tls`. If you expose
the API server publicly, make sure authentication is enforced at the server or
the gateway.

### Network Policies

On a policy-enforcing CNI (Calico, Cilium), lock down traffic to the databases
and control plane while keeping DNS, database, and provider (HTTPS) egress open:

```yaml
networkPolicy:
  enabled: true
```

### High Availability

Multiple API-server replicas are supported — watch events fan out through
PostgreSQL `LISTEN/NOTIFY` (see [ADR-0004](docs/adr/0004-watch-fanout-pg-notify.md)).
Combine with PodDisruptionBudgets (rendered only for multi-replica workloads):

```yaml
apiserver:
  replicaCount: 3
podDisruptionBudget:
  enabled: true
```

### Secretless Provider Auth (Azure)

On Azure, agents and the KB ingester can authenticate to Azure OpenAI / Foundry
with **no stored secret** using a managed identity or workload-identity
federation — set the `SecretRef` `credentialType` accordingly (see the
[SecretRef](#secretref) section). `default` / `managed-identity` /
`workload-identity` require no `clientSecret`.

---

## What's Next

- Browse the [samples/](samples/) directory for ready-to-use manifests
- Secure your install with [docs/security.md](docs/security.md)
- Review the [Kognition Manifest](docs/kognition-manifest.md) for the full architecture vision
- Check [deploy/QUICKSTART.md](deploy/QUICKSTART.md) for platform installation details
- Explore the [Helm chart](deploy/helm/kognition/) for production deployment options
