# Model Invocation Profiles

Status: local implementation. No live installation or cloud change is authorized
by this guide. See [ADR-0025](adr/0025-model-invocation-contracts.md).
Bounded live Foundry conformance passed on 2026-10-23 as described below.

`ModelInvocationProfile` expresses portable intent. A concrete `Model` references
that namespace-local profile and selects an adapter-owned, versioned
`invocationContract`. Deployment aliases never select behavior in profiled
execution.

## Declare Intent And Bind A Deployment

```yaml
apiVersion: kognition.dev/v1alpha1
kind: ModelInvocationProfile
metadata:
  name: nonreasoning-tools-v1
  namespace: default
spec:
  operation: chat
  defaults:
    maxCompletionTokens: 4096
    reasoning: disabled
  limits:
    maxCompletionTokens: 16384
  requirements:
    toolCalling: required
    reasoning: disabled
  parameterHandling:
    unknown: Reject
    unsupported: Reject
---
apiVersion: kognition.dev/v1alpha1
kind: Model
metadata:
  name: assistant-model
  namespace: default
spec:
  className: foundry
  provider: foundry
  modelID: production-chat
  endpoint: https://example.cognitiveservices.azure.com
  credentialRef:
    name: foundry-creds
  invocationProfileRef:
    name: nonreasoning-tools-v1
    specDigest: sha256:ed0794bd4453a2a53936c73910ff373fff20afcb03b98097e3db595ca4370cae
  invocationContract: openai-tool-chat-v1
  capabilities: [chat, tool-calling]
  parameters:
    apiVersion: "2024-12-01-preview"
```

The profile contains no Azure fields. A different deployment can reference the
same intent when its adapter contract supports it. For example, the
`portable-chat-v1` [sample](../samples/model-invocation-profiles/manifests.yaml)
requires tool capability without assuming a vendor-specific reasoning control.

The reference pins the reviewed profile spec, so deleting and recreating its
name cannot change new invocations without a Model edit. Compute the digest
offline before signing or applying the Model:

```powershell
go run .\cmd\kognitionctl invocation-profile-digest `
  -f .\samples\model-invocation-profiles\manifests.yaml `
  --name nonreasoning-tools-v1
```

The command validates the selected profile, excludes metadata and empty top-level
spec objects, canonicalizes JSON using the repository's canonical JSON encoder,
then hashes the UTF-8 domain prefix `kognition.model-invocation/v1` plus a newline
and the canonical bytes. The result is lowercase `sha256:<hex>`. Generation
helpers use the same representation; model loading verifies the digest.

## Supported Adapter Contracts

| Contract | Adapters | Operation and behavior |
| --- | --- | --- |
| `openai-chat-v1` | OpenAI-compatible, Foundry | Chat Completions, `max_tokens`, optional temperature, tools and structured-output envelope |
| `openai-reasoning-chat-v1` | OpenAI-compatible, Foundry | Chat Completions, `max_completion_tokens`, provider-default sampling; disabled reasoning encodes `none`, required reasoning encodes `medium` |
| `openai-tool-chat-v1` | OpenAI-compatible, Foundry | Same reasoning dialect; function tools additionally require explicitly disabled reasoning |
| `anthropic-messages-v1` | Anthropic | Messages, `max_tokens`, optional temperature, native tool history; disabled reasoning means no thinking configuration |
| `bedrock-converse-v1` | AWS Bedrock | Converse, nested inference settings and native tool history; no reasoning-control translation |
| `ollama-chat-v1` | Ollama | Native chat, `num_predict` and temperature options, native tool history |
| `openai-embedding-v1` | OpenAI-compatible, Foundry | Embeddings, optional explicit dimensions |
| `ollama-embedding-v1` | Ollama CPI | Native embeddings without a dimensions override |

These are explicit supported dialects, not assertions about every model hosted
by a provider. Select one only after verifying the concrete deployment/version.
New wire dialects belong in the CPI adapter, not in core vendor unions or runtime
model-name tables. Responses, streaming, Vertex and vendor-specific advanced
thinking controls are not implemented by these contracts and are not substituted.
The gateway accepts an administrator-supplied existing CPI registry through
`WithCPIRegistry`; a registered adapter supplies its own versioned contracts
without another discovery framework or a core provider-name allowlist.

The Model Gateway uses the existing CPI adapters for Anthropic, Bedrock and
Ollama and preserves its OpenAI-compatible runtime interface. Those adapters
support text messages and function tools. Unencodable explicit fields, including
non-auto tool choice, parallel-tool controls, structured response formats and
user metadata, are rejected before dispatch. OpenAI/Foundry keep their supported
full request envelope. No runtime provider credential is introduced.

In Model Gateway mode the controller admits OpenAI-compatible and Foundry Models
directly. Any other provider, such as Ollama, is admitted only when its Model
references an explicit invocation profile; the gateway re-verifies the registered
adapter, profile binding and reviewed digest on every request. An unprofiled
non-OpenAI Model is rejected before an Agent runtime is created.

## Resolution And Validation

| Input | Behavior |
| --- | --- |
| Omitted temperature | Use provider-default sampling; do not inject an SDK temperature |
| Explicit unsupported temperature | Reject; do not remove it |
| Omitted completion limit | Use the Agent's explicitly configured limit, otherwise the profile default |
| Explicit limit above an Agent or profile limit | Reject; do not clamp it |
| Conflicting token-limit aliases | Reject |
| Unsupported reasoning mode or tool/reasoning combination | Reject; do not disable reasoning automatically |
| Unknown generation parameter or contract | Reject |
| Chat settings on an embedding profile | Reject |
| Profile plus legacy Model generation parameters | Reject; only transport API-version parameters remain in that map |

`requirements.toolCalling` and `requirements.structuredOutput` require model
capability; they do not force every individual turn to contain tools or a JSON
schema. `forbidden` also rejects actual use. Reasoning requirements constrain
every invocation. Reasoning modes are `providerDefault`, `disabled`, or `required`;
the contract specifies their exact supported meaning. A universal effort scale
is deliberately absent.

Completion-token limits cover a single provider completion, including reasoning
tokens counted by that dialect. They do not replace aggregate Agent invocation
budgets, workflow budgets, quotas, or usage settlement. All existing authorization
and accounting boundaries remain applicable.
The portable completion limit is a positive signed 32-bit integer; provider and
profile limits can constrain it further.

Model reconciliation validates profile/contract compatibility separately from
provider liveness. A Ready Model or authentication probe does not establish paid
provider feature conformance.

## Identity And Lifecycle

- Profile specs are immutable. Change behavior by creating a new named revision
  and updating the Model reference and its reviewed spec digest.
- Profile resolution is namespace-local. No cross-namespace credential access or
  arbitrary adapter URL is added.
- Gateway/native sessions pin profile UID, spec digest and adapter contract.
  Deletion, replacement or out-of-band spec mutation invalidates the binding.
- Profile session capabilities use a distinct versioned audience. An older
  profile-unaware gateway cannot accept them and silently apply legacy behavior.
- Native checkpoints retain that binding. Restore verifies the current profile
  before creating a session; a legacy unbound checkpoint cannot adopt a profile.
- Model-version candidates require `invocationProfileUID` and
  `invocationProfileDigest` alongside their existing exact Model binding.
  The digest uses the exported `modelinvocation.Digest` algorithm. A model spec
  digest alone does not pin an external profile.
- Canary preparation binds the profile resource identity and uses the same
  request preparation, without injecting a temperature into a profiled canary.
- Embedding ingestion resolves the profile before constructing a job payload.
  Requested dimensions must match the KnowledgeBase schema.

Python runtimes use the Model Gateway for profiled chat, including AutoGen.
They send portable requests to its explicitly selected Chat Completions interface,
retain omitted settings and use an opaque logical model ID. Direct profiled
Python-to-provider chat is rejected. The Go native runtime requires its configured
Store to resolve a referenced profile.

## Migration And Installation

Unprofiled Models retain the legacy compatibility path, including the temporary
Foundry/Python family shims. New profile-backed execution bypasses those shims.
This is an opt-in migration, not a silent rewrite of stored Models.

Azure sample installers now require `MS_FOUNDRY_INVOCATION_CONTRACT`. Set it to
the verified dialect, independently of `MS_FOUNDRY_MODEL`. Provisioning accepts
the corresponding explicit `-ChatInvocationContract` argument. Regenerate
nonsecret model/profile manifests and Helm values without any cloud operation:

```powershell
.\deploy\azure-samples\Export-Models.ps1
```

Generated profiles precede their Models. The multi-agent Helm sample owns its
own profile and Model, avoiding ownership conflicts with the model snapshot.
The API, CLI, conversion layer, admission, CRD, controller reader and gateway
reader all include the new resource.

Installing the new CRD, webhook rules, reader permissions and images remains a
reviewed platform change under the
[installation and recovery contract](features/knowledge-provider-execution/installation-recovery-contract.md).
Freeze writers, install reviewed inactive artifacts and obtain separate activation
approval. Generating manifests does not activate or upgrade a cluster.

## Bounded Live Foundry Conformance

The opt-in [live suite](../internal/modelgateway/invocation_live_integration_test.go)
uses the real profile resolver, Foundry CPI driver, Model Gateway, Go native
runtime and Python provider factory against an existing deployment. It uses
synthetic local resource and model-access fixtures; it creates no Azure or
Kubernetes resources and does not certify production grants, durable accounting,
tool-gateway effects, installed images or cluster activation.

After approving paid inference, supply an existing Python interpreter with the
agent's LangChain dependencies and run the
[verification wrapper](../deploy/azure-samples/Test-InvocationProfiles.ps1):

```powershell
.\deploy\azure-samples\Test-InvocationProfiles.ps1 `
  -PythonExecutable '<qualified-python-executable>' `
  -ResultsPath '<new-local-results.ndjson>'
```

The wrapper reads the existing ignored environment, scopes credentials to the
test process and restores its previous environment. It requires every scenario
to pass without skips. Inference is limited to eight requests, with at most 512
completion tokens per chat request and no automatic compatibility retries.
Prompts and tool results contain only synthetic verification data. The Python
child receives a session capability through standard input and no Azure credential.

The 2026-10-23 run used `kognition` in East US 2, `gpt-6-luna` version
`2026-09-22`, `openai-tool-chat-v1`, and `text-embedding-3-small` version `1`,
authenticated with the existing service principal. All eight scenarios passed:

| Scenario | Verified outcome |
| --- | --- |
| Foundry CPI chat | Exact `READY` output and reported completion usage |
| Model Gateway chat | Pinned deployed model and exact `READY` output |
| Gateway function-tool round trip | Structured call arguments, preserved call identity, supplied local result consumed in the next turn |
| Gateway structured output | Strict JSON schema returned `{"status":"ready"}` |
| Native runtime | Real provider completion and profiled checkpoint restore |
| Python SDK through gateway | Real LangChain request completed without provider credentials in the Python child |
| Profile-backed embeddings | One finite 1,536-dimensional vector with provider token totals |
| Local fail-closed boundaries | Five invalid settings, revoked model access and a replacement profile rejected with zero extra inference requests |

Measured usage was eight requests: 444 prompt tokens and 57 completion tokens,
501 tokens total. The model scenarios took 28.85 seconds, including Python SDK
startup. This is live protocol/runtime conformance for the exercised working
tree, not qualification of the separate frozen release or a deployed installation.
