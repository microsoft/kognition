# Kognition Quickstart

Prepare a guarded Kubernetes installation, then create an agent only after
separately approved activation. Missing security gates block activation.

## Prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| Docker Desktop | Latest | Enable Kubernetes in Settings |
| Kubernetes / kubectl | Kubernetes 1.30+; compatible kubectl | Explicit reviewed kubeconfig; local evidence covers 1.30.3 and 1.31.1 |
| Helm | 3.x | [Install guide](https://helm.sh/docs/intro/install/) |
| AI Provider credentials | — | Azure AI Foundry or OpenAI API key |

## 1. Install the Kognition Platform

Follow the [installation and recovery contract](docs/features/knowledge-provider-execution/installation-recovery-contract.md)
to prepare the target, namespaces, images, CRDs, certificates, exact artifacts and
resource inventory. Maintain the required external writer freeze. The supported
path builds images separately and bootstraps reviewed inactive admission:

```bash
make docker-k8s
make deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
```

On Windows, use `./make.ps1` with the same target and arguments. Deployment does
not load images, credentials or samples, create namespaces, or activate the
controller. Required optional components belong in the reviewed values.

Resource admission has an independent webhook Deployment and must be available
while the controller is inactive. Guarded installs skip Helm hooks; complete
[platform migrations and KnowledgeBase database prerequisites](docs/features/knowledge-provider-execution/installation-recovery-contract.md#database-prerequisites)
before testing those workloads. For explicit local targeting and sample upload
checks, follow [the bootstrap guide](docs/bootstrapping.md).

Inspect the reviewed target; the controller remains at zero replicas:

```bash
kubectl --kubeconfig ./reviewed/cluster.kubeconfig -n kognition-system get pods
```

## 2. Create Your First Agent

The remaining examples require a separately approved
[activation transaction](docs/features/knowledge-provider-execution/activation-transaction-contract.md)
and an enrolled dedicated workload namespace. Use the reviewed kubeconfig and
workload namespace explicitly for every resource command. Do not run producers or
credential helpers during an installation or recovery freeze.

Choose your provider — **Azure AI Foundry** or **OpenAI-compatible**.

### Option A: Azure AI Foundry

```yaml
# Save as my-agent.yaml
---
kind: ModelClass
apiVersion: kognition.dev/v1alpha1
metadata:
  name: foundry
spec:
  provisioner: foundry
  latencyClass: low
  costTier: standard
  contextWindow: 128000
  modalities: [text]
---
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
---
kind: Model
apiVersion: kognition.dev/v1alpha1
metadata:
  name: gpt-4-1
spec:
  className: foundry
  modelID: gpt-4.1
  provider: foundry
  endpoint: https://YOUR_RESOURCE.services.ai.azure.com
  capabilities: [chat, tool-calling]
  credentialRef:
    name: foundry-creds
  parameters:
    apiVersion: "2024-10-21"
---
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: hello-agent
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: "You are a helpful assistant. Answer concisely."
  runtime:
    class: default
    timeout: 30s
    maxTokens: 4096
```

### Option B: OpenAI-Compatible (Azure Cognitive Services)

```yaml
# Save as my-agent.yaml
---
kind: ModelClass
apiVersion: kognition.dev/v1alpha1
metadata:
  name: openai
spec:
  provisioner: openai
  latencyClass: low
  costTier: standard
  contextWindow: 128000
  modalities: [text]
---
kind: SecretRef
apiVersion: kognition.dev/v1alpha1
metadata:
  name: openai-creds
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
  backendRef: secret/openai-creds
  key: token
---
kind: Model
apiVersion: kognition.dev/v1alpha1
metadata:
  name: gpt-4-1
spec:
  className: openai
  modelID: gpt-4.1
  provider: openai
  endpoint: https://YOUR_RESOURCE.cognitiveservices.azure.com/openai/deployments/YOUR_DEPLOYMENT_NAME
  capabilities: [chat, tool-calling]
  credentialRef:
    name: openai-creds
  parameters:
    apiVersion: "2024-10-21"
---
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: hello-agent
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: "You are a helpful assistant. Answer concisely."
  runtime:
    class: default
    timeout: 30s
    maxTokens: 4096
```

### Apply and Test

```bash
kubectl apply -f my-agent.yaml

# Verify the agent is ready
kubectl get agents.kognition.dev

# Trigger the agent with a prompt
cat <<EOF | kubectl apply -f -
apiVersion: kognition.dev/v1alpha1
kind: AgentInstance
metadata:
  name: test-run
spec:
  agentRef:
    name: hello-agent
  revision: 1
  sessionContext:
    input: "What is 2 + 2?"
EOF

# Check result
kubectl get agentinstances.kognition.dev test-run -o yaml
```

## 3. Create Your First Workflow

Chain multiple agents together:

```yaml
# Save as my-workflow.yaml
---
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: researcher
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: "You are a research specialist. Provide brief factual summaries."
  runtime:
    class: default
    timeout: 60s
    maxTokens: 4096
---
kind: Agent
apiVersion: kognition.dev/v1alpha1
metadata:
  name: writer
spec:
  modelRef:
    name: gpt-4-1
  instructions:
    inline: "You are a writer. Given research notes, produce a short article."
  runtime:
    class: default
    timeout: 60s
    maxTokens: 4096
---
kind: Workflow
apiVersion: kognition.dev/v1alpha1
metadata:
  name: research-pipeline
spec:
  entryPoint: research
  steps:
    - name: research
      agentRef:
        name: researcher
      input: "Research: {{.goal}}"
      timeout: 60s
    - name: write
      agentRef:
        name: writer
      input: "Write an article from this research: {{.steps.research.output}}"
      timeout: 60s
  edges:
    - from: research
      to: write
  budgets:
    maxDuration: 5m
    maxTokens: 20000
```

```bash
kubectl apply -f my-workflow.yaml
kubectl get workflows.kognition.dev
```

## Next Steps

| Topic | Link |
|-------|------|
| Full tutorial with all features | [TUTORIAL.md](TUTORIAL.md) |
| Deployment strategies (Helm) | [TUTORIAL.md — Deployment](TUTORIAL.md#deployment-strategies) |
| Sample manifests | [samples/](samples/) |
| Architecture overview | [docs/kognition-manifest.md](docs/kognition-manifest.md) |

## Samples Reference

| Sample | Description | Path |
|--------|-------------|------|
| Simple Agent | Minimal agent (ModelClass + Model + Agent) | `samples/simple-agent/` |
| Foundry Agent | Azure AI Foundry with client-secret auth | `samples/foundry-agent/` |
| OpenAI Agent | Azure OpenAI-compatible provider | `samples/openai-agent/` |
| Full-Stack Agent | Tools, memory, autonomy, policies, workflow | `samples/full-stack-agent/` |
| Multi-Agent Workflow | 3-agent pipeline with loops and approval gates | `samples/multi-agent-workflow/` |
| MVP-1 Five-Agent Workflow | 5-agent linear workflow deployed and started by one multi-document `kognitionctl apply` | `samples/five-agent-workflow/` |
| Evaluation | Quality gates and dashboard results for `hello-agent` EvaluationRuns | `samples/evaluation/` |
| Governance Demo | Workspace, identity, RBAC, admission policies | `samples/governance-demo.yaml` |
| K8s Mode Demo | Broad end-to-end cross-section of native CRDs | `samples/k8s-mode-demo.yaml` |
| Helm Multi-Agent | Helm-packaged workflow deployment | `samples/helm-multi-agent/` |

Automatic `./bootstrap.ps1` and `make quickstart` Kubernetes installation are
blocked. See [docs/bootstrapping.md](docs/bootstrapping.md) for separate preparation,
reviewed bootstrap and post-activation sample requirements.
