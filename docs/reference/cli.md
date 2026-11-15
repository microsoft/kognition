---
title: CLI reference
description: Build kognitionctl, select a backend explicitly and use its supported commands.
---

# CLI reference

`kognitionctl` manages Kognition resources and invokes prepared workloads.
Its `auto` backend mode can select Kubernetes when a usable kubeconfig exists.
Prefer explicit backend selection for documented and repeatable operations.

## Build and inspect help

```powershell
go build -o .\bin\kognitionctl.exe .\cmd\kognitionctl
.\bin\kognitionctl.exe --help
.\bin\kognitionctl.exe chat --help
```

On Linux or macOS:

```bash
go build -o ./bin/kognitionctl ./cmd/kognitionctl
./bin/kognitionctl --help
./bin/kognitionctl chat --help
```

Use the Go version declared in [go.mod](../../go.mod). Build from the same source
revision as the target installation.

## Global options

| Option | Default | Purpose |
| --- | --- | --- |
| `--mode` | `auto` | Select `standalone`, `kubernetes` or automatic backend detection. |
| `--server` | `http://localhost:8080` | Standalone API endpoint. Explicitly setting it selects standalone behavior in auto mode. |
| `--namespace`, `-n` | `default` | Namespace for resource operations; check the namespace embedded in applied manifests too. |
| `--output`, `-o` | Command-specific | Structured output such as `yaml` or `json`; lists normally use a table. |
| `--token-file` | Unset | Read the standalone API bearer token from an access-controlled file. |
| `--kube-context` | Current context | Select a context from the chosen kubeconfig. |
| `--kube-namespace` | `kognition` | Namespace of the API Service used by proxy-backed operations. |
| `--kube-service` | `kognition-apiserver` | API Service name for proxy-backed operations. |
| `--kube-port` | `8080` | API Service port for proxy-backed operations. |

For standalone authentication, `KOGNITION_API_TOKEN` supplies the bearer token
unless `--token-file` overrides it. The CLI refuses to send that token over
plaintext HTTP to a non-loopback server.

For Kubernetes, set `KUBECONFIG` to the reviewed file and select the intended
context. Do not change the global context just to run an example.

## Resource operations

The following examples assume an authenticated local standalone installation.
Use the [first-agent target selection](../getting-started/first-agent.md#2-select-one-backend)
for a Kubernetes installation.

```powershell
$target = @('--mode', 'standalone', '--server', 'http://127.0.0.1:8080',
            '--namespace', 'team-a')
.\bin\kognitionctl.exe @target get agents
.\bin\kognitionctl.exe @target get agent hello-agent -o yaml
.\bin\kognitionctl.exe @target apply -f .\first-agent.yaml
.\bin\kognitionctl.exe @target events agent hello-agent
```

**Bash (Linux/macOS):**

```bash
target=(--mode standalone --server http://127.0.0.1:8080 --namespace team-a)
./bin/kognitionctl "${target[@]}" get agents
./bin/kognitionctl "${target[@]}" get agent hello-agent -o yaml
./bin/kognitionctl "${target[@]}" apply -f ./first-agent.yaml
./bin/kognitionctl "${target[@]}" events agent hello-agent
```

`apply` accepts a file or stdin with `-f -`. It accepts Kognition resources using
`kognition.dev/v1alpha1`; a foreign manifest needs the explicit compatibility
import workflow. The resource's embedded namespace matters.

## Command families

| Command | Purpose |
| --- | --- |
| `get`, `describe` | Inspect resources. |
| `apply`, `delete` | Create/update or remove resources; these are writes. |
| `events`, `logs`, `status`, `watch` | Inspect state and execution evidence. |
| `chat <agent> <message...>` | Invoke an Agent; a provider call can incur charges. |
| `workflow` | Inspect workflow executions and their logs. |
| `revisions`, `revision`, `revision-diff` | Inspect immutable revision history and compare configuration. |
| `approve` | Submit an explicit approval decision under the configured policy. |
| `goal` | Submit a goal to a configured IntentGateway. |
| `dashboard` | Open the local operations interface. |
| `studio` | Open resource authoring and inspection tools. |
| `version` | Inspect version information. |

Discover exact arguments and additional commands with
`kognitionctl <command> --help`. Do not infer that every operation is supported
identically in every deployment mode.

## Execution and revisions

```powershell
.\bin\kognitionctl.exe @target chat hello-agent "Reply with a short greeting."
.\bin\kognitionctl.exe @target revisions agent hello-agent
.\bin\kognitionctl.exe @target revision agent hello-agent 2
.\bin\kognitionctl.exe @target revision-diff agent hello-agent 1 2 -o json
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl "${target[@]}" chat hello-agent "Reply with a short greeting."
./bin/kognitionctl "${target[@]}" revisions agent hello-agent
./bin/kognitionctl "${target[@]}" revision agent hello-agent 2
./bin/kognitionctl "${target[@]}" revision-diff agent hello-agent 1 2 -o json
```

Use revision numbers that actually exist. Revision diffs describe immutable
`spec` snapshots, not metadata or current status. In Kubernetes mode, interactive
Agent chat uses a controller-owned AgentInstance and waits for its result.

## Local interfaces

```powershell
.\bin\kognitionctl.exe @target dashboard
.\bin\kognitionctl.exe @target studio
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl "${target[@]}" dashboard
./bin/kognitionctl "${target[@]}" studio
```

These interfaces use the selected backend and its credentials. Keep local
listeners on a trusted interface, and use [Studio's operational guidance](../studio.md)
before performing writes or interactive invocation.
