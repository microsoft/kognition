# memory-all-types

End-to-end sample that exercises **every Kognition memory type** —
`working`, `episodic`, `semantic`, `procedural`, and `shared` — through a
single agent (`memory-explorer`) plus a peer (`memory-explorer-peer`) that
makes the multi-agent shape of `shared` visible.

This is the all-types companion to [`samples/memory-in-action/`](../memory-in-action/),
which covers only `working`.

## What this proves

1. **API surface.** Each of the five `MemoryType` values is admitted as a
   distinct `MemoryClaim`, each gets a `MemoryVolume`, each reaches
   `Bound`, and all five appear on the Agent's `spec.memory.claims`.
2. **Generic Tool surface.** The agent uses `sandbox_memory_store` with a
   per-type `tag` (`working` / `episodic` / `semantic` / `procedural` /
   `shared`) so the conceptual type is preserved as record metadata, then
   `sandbox_memory_recall` reads them back — with an optional `tag`
   filter to constrain results to one type.
3. **Filterable sandbox storage.** `sandbox_memory_recall(query, tag=<type>)`
   returns exactly the records of that type, even though every type
   shares the same in-process list in the sandbox.

## Memory types

| Type | Conceptual meaning | Example fact in this sample |
|---|---|---|
| `working` | Short-lived scratch for the current task. | `intermediate sum = 42` |
| `episodic` | A timestamped event the user just shared. | `On 2024-03-12 the user reported issue #871` |
| `semantic` | A durable fact about the world or the user. | `The user's dog is named Pepper` |
| `procedural` | A how-to / recipe / repeatable procedure. | `To deploy: run make build then kubectl apply` |
| `shared` | Information meant to be visible to other agents. | `Team policy: every release needs an SBOM` |

## What's in here

| Resource | Kind | Purpose |
|---|---|---|
| `explorer-episodic` | `MemoryClaim` | Reserves an `episodic` volume off the cluster `default` `MemoryClass`. |
| `explorer-semantic` | `MemoryClaim` | Reserves a `semantic` volume. |
| `explorer-working` | `MemoryClaim` | Reserves a `working` volume. |
| `explorer-procedural` | `MemoryClaim` | Reserves a `procedural` volume. |
| `explorer-shared` | `MemoryClaim` | Reserves a `shared` volume. |
| `sandbox-memory-store` | `Tool` | POSTs `{content, tag}` to `tool-sandbox`. |
| `sandbox-memory-recall` | `Tool` | POSTs `{query, top_k, tag}` to `tool-sandbox`. Supports tag-filtered recall. |
| `memory-explorer` | `Agent` | References all five claims; instructions force a `tag` on every store call. |
| `memory-explorer-peer` | `Agent` | References only `explorer-shared` to make the multi-agent shape of `shared` visible. |

## Prerequisites

- Kognition installed in the cluster.
- The tool sandbox running:
  Use the same commands in Bash or PowerShell:

  ```sh
  kubectl apply -f samples/workflows/tool-sandbox.yaml
  kubectl -n default wait --for=condition=Available --timeout=60s deployment/tool-sandbox
  ```
- A working `Model` named `gpt-6-luna` in the `default` namespace
  ([`samples/foundry-agent/`](../foundry-agent/) shows how to set one up
  against Azure AI Foundry).
- `kognitionctl` built from this repo.

## Apply

```powershell
kubectl apply -f samples/memory-all-types/manifests.yaml

# Every claim should reach Bound.
kubectl -n default wait --for=jsonpath='{.status.phase}'=Bound `
  memoryclaim/explorer-episodic memoryclaim/explorer-semantic `
  memoryclaim/explorer-working memoryclaim/explorer-procedural `
  memoryclaim/explorer-shared --timeout=60s

# Both agents should reach Ready.
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready `
  agent/memory-explorer agent/memory-explorer-peer --timeout=120s

# Sanity check — five claims, five volumes, all Bound:
kubectl get memoryclaims -n default
kubectl get memoryvolumes -n default
```

**Bash (Linux/macOS):**

```bash
kubectl apply -f samples/memory-all-types/manifests.yaml
kubectl -n default wait '--for=jsonpath={.status.phase}=Bound' \
  memoryclaim/explorer-episodic memoryclaim/explorer-semantic \
  memoryclaim/explorer-working memoryclaim/explorer-procedural \
  memoryclaim/explorer-shared --timeout=60s
kubectl -n default wait '--for=jsonpath={.status.phase}=Ready' \
  agent/memory-explorer agent/memory-explorer-peer --timeout=120s
kubectl get memoryclaims -n default
kubectl get memoryvolumes -n default
```

## Exercise every memory type

Each turn below stores one fact under a specific tag. The agent's
instructions force a `tag` on every store call. The "fresh session"
property of `kognitionctl chat` means turn 6 starts with no in-process
state — every fact has to come back through `sandbox_memory_recall`.

```powershell
# 1. working — short-lived scratch
.\bin\kognitionctl.exe chat memory-explorer `
  "Remember as a working note: intermediate sum = 42." --mode kubernetes

# 2. episodic — timestamped event
.\bin\kognitionctl.exe chat memory-explorer `
  "Record this episodic event: on 2024-03-12 the user reported issue #871." --mode kubernetes

# 3. semantic — durable fact
.\bin\kognitionctl.exe chat memory-explorer `
  "Remember this semantic fact: the user's dog is named Pepper." --mode kubernetes

# 4. procedural — recipe
.\bin\kognitionctl.exe chat memory-explorer `
  "Remember this procedure: to deploy, run 'make build' then 'kubectl apply -f manifests.yaml'." --mode kubernetes

# 5. shared — team-wide note
.\bin\kognitionctl.exe chat memory-explorer `
  "Remember this shared team policy: every release must include an SBOM." --mode kubernetes

# 6. Recall everything from a fresh session. The agent has to call
# sandbox_memory_recall — there is no in-process state across invocations.
.\bin\kognitionctl.exe chat memory-explorer `
  "What do you know? List the procedure, the dog name, issue #871, the working note, and the shared policy." --mode kubernetes
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl chat memory-explorer \
  "Remember as a working note: intermediate sum = 42." --mode kubernetes
./bin/kognitionctl chat memory-explorer \
  "Record this episodic event: on 2024-03-12 the user reported issue #871." --mode kubernetes
./bin/kognitionctl chat memory-explorer \
  "Remember this semantic fact: the user's dog is named Pepper." --mode kubernetes
./bin/kognitionctl chat memory-explorer \
  "Remember this procedure: to deploy, run 'make build' then 'kubectl apply -f manifests.yaml'." --mode kubernetes
./bin/kognitionctl chat memory-explorer \
  "Remember this shared team policy: every release must include an SBOM." --mode kubernetes
./bin/kognitionctl chat memory-explorer \
  "What do you know? List the procedure, the dog name, issue #871, the working note, and the shared policy." --mode kubernetes
```

## Verify each type independently

Filter by tag against the sandbox to prove every type is stored distinctly.
The records share one backing list but the `tag` field pins each one to
its conceptual type.

In one terminal (Bash or PowerShell):

```sh
kubectl -n default port-forward service/tool-sandbox 18080:8080
```

In a second terminal, choose one of the following blocks.

**PowerShell (Windows):**

```powershell
foreach ($t in "working","episodic","semantic","procedural","shared") {
    Write-Host "`n=== $t ===" -ForegroundColor Cyan
    Invoke-RestMethod -Uri http://127.0.0.1:18080/memory-recall `
      -Method POST -ContentType "application/json" `
      -Body (@{ query = ""; tag = $t; top_k = 25 } | ConvertTo-Json) `
      | ConvertTo-Json -Depth 5
}
```

**Bash (Linux/macOS):**

```bash
for t in working episodic semantic procedural shared; do
  printf '\n=== %s ===\n' "$t"
  curl --fail --silent --show-error http://127.0.0.1:18080/memory-recall \
    --header 'Content-Type: application/json' \
    --data "{\"query\":\"\",\"tag\":\"$t\",\"top_k\":25}" |
    python3 -m json.tool
done
```

Each block should return only the records tagged with that type.

The runtime also logs every tool call:

```powershell
kubectl -n default logs deployment/agent-memory-explorer --tail=40 | Select-String "sandbox_memory"
```

**Bash (Linux/macOS):**

```bash
kubectl -n default logs deployment/agent-memory-explorer --tail=40 | grep -F 'sandbox_memory'
```

## How the wiring works

- The dispatcher resolves both `sandbox-memory-store` and
  `sandbox-memory-recall` Tool CRs and injects the full specs into
  `KOGNITION_TOOLS_RESOLVED` for both agents.
- `build_tools()` in [`runtime/agent/tools.py`](../../runtime/agent/tools.py)
  turns each into a `StructuredTool` whose executor POSTs to the tool's
  `endpoint`.
- The agent's instructions are the contract that maps the single
  `sandbox_memory_store` tool onto five conceptual types via `tag`
  metadata — the LLM is told to pick the right tag for the content kind.
- `sandbox_memory_recall`'s optional `tag` parameter makes the
  conceptual type recoverable at query time.

## Honest limitations of the current build

- **This sample is intentionally not the first-party memory data path.** The
  five claims prove API and binding behavior, while explicit sandbox Tool CRs
  make five independent tags easy to inspect. For built-in `memory_store` and
  `memory_recall`, use [`memory-in-action`](../memory-in-action/README.md).
- **In-process storage.** The sandbox holds records in a Python list.
  Bouncing the `tool-sandbox` pod wipes everything. Bind a real store
  behind an HTTP endpoint and swap the Tool CRs to point at it when you
  need durability.
- **Cross-agent visibility in this sample works through the sandbox.**
  Because both agents call the same sandbox in the same namespace, a
  record written by `memory-explorer` with `tag="shared"` is readable by
  `memory-explorer-peer`. After a backend handle is provisioned, the built-in
  runtime selects the same volume when two agents reference the same bound
  shared claim.
- **The sandbox has no vector search.** Its recall endpoint is keyword-OR scoring; the
  sandbox does not embed. For semantic search, put a real store
  (mem0, pgvector, etc.) behind an HTTP endpoint of the same shape.
