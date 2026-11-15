# Memory In Action

This runnable sample demonstrates a bound `MemoryClaim` alongside generic
OpenAPI Tool dispatch. The Agent uses `sandbox_memory_store` and
`sandbox_memory_recall`, backed by the shared in-cluster tool sandbox.

## Current Memory Boundary

The runtime can auto-attach built-in `memory_store` and `memory_recall` when a
claim is bound and the memory-service URL is configured. The Kubernetes claim
reconciler currently creates `MemoryVolume` metadata but does not call the
memory service's `/v1/memory/provision` endpoint. Until that provisioning step
is wired end to end, the built-in tools return a volume-not-provisioned error
unless an operator provisions the bound handle separately.

This sample therefore keeps its data path in explicit Tool CRs so the
walkthrough works without manual backend provisioning. The `MemoryClaim` still
proves admission, binding, and lifecycle visibility.

## Resources

| Resource | Kind | Purpose |
| --- | --- | --- |
| `notes-claim` | `MemoryClaim` | Binds a working-memory control-plane volume from the `default` class. |
| `sandbox-memory-store` | `Tool` | Writes tagged content to the sandbox's in-process store. |
| `sandbox-memory-recall` | `Tool` | Searches the sandbox's records. |
| `notes-agent` | `Agent` | References the claim and both executable Tool CRs. |

## Prerequisites

- Kognition installed in Kubernetes mode.
- The shared tool sandbox:

  These commands work in Bash and PowerShell:

  ```sh
  kubectl apply -f samples/workflows/tool-sandbox.yaml
  kubectl -n default wait --for=condition=Available --timeout=60s deployment/tool-sandbox
  ```

- A working `Model` named `gpt-6-luna` in `default`.
- `kognitionctl` built from this repository.

## Apply And Run

```powershell
kubectl apply -f samples/memory-in-action/manifests.yaml
kubectl -n default wait --for=jsonpath='{.status.phase}'=Bound memoryclaim/notes-claim --timeout=60s
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready agent/notes-agent --timeout=120s

./bin/kognitionctl.exe chat notes-agent `
  "Remember that my favorite color is blue and my dog is named Pepper. Use sandbox_memory_store for each fact." `
  --mode kubernetes

./bin/kognitionctl.exe chat notes-agent `
  "Use sandbox_memory_recall first: what is my favorite color and my dog's name?" `
  --mode kubernetes
```

**Bash (Linux/macOS):**

```bash
kubectl apply -f samples/memory-in-action/manifests.yaml
kubectl -n default wait '--for=jsonpath={.status.phase}=Bound' memoryclaim/notes-claim --timeout=60s
kubectl -n default wait '--for=jsonpath={.status.phase}=Ready' agent/notes-agent --timeout=120s
./bin/kognitionctl chat notes-agent \
  "Remember that my favorite color is blue and my dog is named Pepper. Use sandbox_memory_store for each fact." \
  --mode kubernetes
./bin/kognitionctl chat notes-agent \
  "Use sandbox_memory_recall first: what is my favorite color and my dog's name?" \
  --mode kubernetes
```

## Verify Sandbox Storage

In one terminal (Bash or PowerShell):

```sh
kubectl -n default port-forward service/tool-sandbox 18080:8080
```

In a second terminal, query the forwarded endpoint.

**PowerShell (Windows):**

```powershell
Invoke-RestMethod -Uri http://127.0.0.1:18080/memory-recall `
  -Method POST -ContentType "application/json" `
  -Body '{"query":"color","top_k":10}' |
  ConvertTo-Json -Depth 5
```

**Bash (Linux/macOS):**

```bash
curl --fail --silent --show-error http://127.0.0.1:18080/memory-recall \
  --header 'Content-Type: application/json' \
  --data '{"query":"color","top_k":10}' | python3 -m json.tool
```

The sandbox stores records in process and resets when its pod restarts. It is
a Tool-plane demonstration, not a durable memory backend.
