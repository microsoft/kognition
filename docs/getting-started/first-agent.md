---
title: First agent
description: Define a model-bound Agent, grant model access, and verify an invocation on a prepared installation.
---

# First agent

This walkthrough creates an Agent on an already prepared installation. It does
not install the platform or provision a provider. A model invocation can incur
charges and send the message to the configured provider.

## Prerequisites

- Complete [local setup](local.md) or [Kubernetes setup](kubernetes.md).
- Obtain an approved workload namespace and a model named `approved-chat-model`
  in that namespace. Substitute the actual model name in the manifest.
- Confirm that the model's provider, credentials and invocation profile are
  configured. A Model resource name does not identify a provider protocol.
- Confirm that your identity may create an Agent and ModelGrant and invoke work.
- Use the CLI built from the same source revision as the installation.

The example namespace is `team-a`. Use a namespace that the operator has actually
prepared and, in Kubernetes mode, enrolled.

## 1. Create an Agent and grant

Save this as `first-agent.yaml`, substituting the approved namespace and model:

```yaml
apiVersion: kognition.dev/v1alpha1
kind: Agent
metadata:
  name: hello-agent
  namespace: team-a
spec:
  modelRef:
    name: approved-chat-model
  instructions:
    inline: Answer briefly and state when you are uncertain.
  runtime:
    class: default
    timeout: 30s
    maxTokens: 512
---
apiVersion: kognition.dev/v1alpha1
kind: ModelGrant
metadata:
  name: hello-agent-model-access
  namespace: team-a
spec:
  agentRef:
    name: hello-agent
  modelRef:
    name: approved-chat-model
```

An Agent model reference is configuration; the grant authorizes model use.
Do not embed provider credentials in this file. Additional policies in the target
installation can require further approvals.

## 2. Select one backend

For an authenticated standalone API, load the approved operator token into
`KOGNITION_API_TOKEN` or use an access-controlled `--token-file`:

```powershell
$target = @(
    '--mode', 'standalone',
    '--server', 'http://127.0.0.1:8080',
    '--namespace', 'team-a'
)
```

**Bash (Linux/macOS):**

```bash
target=(--mode standalone --server http://127.0.0.1:8080 --namespace team-a)
```

For Kubernetes, use the reviewed kubeconfig and its approved context:

```powershell
$env:KUBECONFIG = (Resolve-Path .\reviewed\cluster.kubeconfig).Path
$target = @(
    '--mode', 'kubernetes',
    '--kube-context', 'reviewed-context',
    '--namespace', 'team-a'
)
```

**Bash (Linux/macOS):**

```bash
export KUBECONFIG="$PWD/reviewed/cluster.kubeconfig"
target=(--mode kubernetes --kube-context reviewed-context --namespace team-a)
```

Replace `reviewed-context` with the context in that kubeconfig. Select only the
appropriate block; avoid automatic backend detection in repeatable operations.

## 3. Apply and inspect

```powershell
.\bin\kognitionctl.exe @target apply -f .\first-agent.yaml
.\bin\kognitionctl.exe @target get agent hello-agent -o yaml
.\bin\kognitionctl.exe @target events agent hello-agent
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl "${target[@]}" apply -f ./first-agent.yaml
./bin/kognitionctl "${target[@]}" get agent hello-agent -o yaml
./bin/kognitionctl "${target[@]}" events agent hello-agent
```

The namespace in the manifest must match the intended target. Successful `apply`
means the resource write was accepted, not that the provider is reachable or a
model request has completed.

## 4. Invoke one turn

After the operator confirms the provider binding and applicable policies:

```powershell
.\bin\kognitionctl.exe @target chat hello-agent "Reply with a short greeting."
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl "${target[@]}" chat hello-agent "Reply with a short greeting."
```

The command should return an agent response or an explicit failure.
Do not assert an exact model-generated sentence: model output can vary.
In Kubernetes mode, chat creates and waits for a controller-owned AgentInstance.
Inspect that execution separately:

```powershell
.\bin\kognitionctl.exe @target get agentinstances
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl "${target[@]}" get agentinstances
```

In standalone mode, the synchronous chat path returns the response and session
information directly. These are different runtime paths, not a claim of complete
deployment parity.

## 5. Investigate failures before retrying

| Observation | Check |
| --- | --- |
| Authentication or authorization failure | Operator credentials, target namespace and the caller's permissions. |
| Model access denied | ModelGrant, model namespace and applicable policies. |
| Model or runtime not ready | Model configuration, invocation profile, Agent status and events. |
| Provider error or timeout | Provider endpoint, credential validity, network path and configured limits. |
| Approval or quota blocks execution | Required approval and budget; do not bypass the policy. |
| Usage accounting is uncertain | Preserve the failed execution and investigate before another paid call. |

Stop here if the outcome is uncertain. Repeated invocation is not a recovery
procedure. Continue with [guides and samples](../guides/README.md) only after the
basic provider path is understood.
