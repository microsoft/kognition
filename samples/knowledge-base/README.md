# Inline KnowledgeBase

This sample grounds an Agent against inline KnowledgeBase content using the
runtime's built-in `knowledge_search` tool. It requires no vector database,
external ingester, or Tool CR.

## Resources

| Resource | Kind | Purpose |
| --- | --- | --- |
| `inline-kb` | `KnowledgeBaseClass` | Declares the inline provisioner. |
| `orbital-notes-kb` | `KnowledgeBase` | Contains three inline documents about the fictional Orbital Mark 7 product. |
| `orbital-notes-claim` | `KnowledgeBaseClaim` | Binds workloads in `default` to the KnowledgeBase in `ReadOnly` mode. |
| `orbital-docs-agent` | `Agent` | References the claim and receives `knowledge_search` automatically. |
| `orbital-notes-reindex-now` | `KnowledgeBaseIndexRun` | Optional one-shot reconciliation check from `indexrun.yaml`. |
| `orbital-notes-hourly` | `KnowledgeBaseIndexSchedule` | Optional hourly schedule from `indexschedule.yaml`. |

## Prerequisites

- Kognition installed in Kubernetes mode.
- A working `Model` named `gpt-6-luna` in the `default` namespace.
- `kognitionctl` built from this repository.

## Apply And Run

```powershell
kubectl apply -f samples/knowledge-base/manifests.yaml
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready knowledgebase/orbital-notes-kb --timeout=120s
kubectl -n default wait --for=jsonpath='{.status.phase}'=Bound knowledgebaseclaim/orbital-notes-claim --timeout=60s
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready agent/orbital-docs-agent --timeout=120s

./bin/kognitionctl.exe chat orbital-docs-agent `
  "When does Orbital Mark 7 ship, and which regions support it at launch?" `
  --mode kubernetes
```

**Bash (Linux/macOS):**

```bash
kubectl apply -f samples/knowledge-base/manifests.yaml
kubectl -n default wait '--for=jsonpath={.status.phase}=Ready' knowledgebase/orbital-notes-kb --timeout=120s
kubectl -n default wait '--for=jsonpath={.status.phase}=Bound' knowledgebaseclaim/orbital-notes-claim --timeout=60s
kubectl -n default wait '--for=jsonpath={.status.phase}=Ready' agent/orbital-docs-agent --timeout=120s
./bin/kognitionctl chat orbital-docs-agent \
  "When does Orbital Mark 7 ship, and which regions support it at launch?" \
  --mode kubernetes
```

The Agent runtime logs the generated tool call:

```powershell
kubectl -n default logs deployment/agent-orbital-docs-agent --tail=80 |
  Select-String "knowledge_search"
```

**Bash (Linux/macOS):**

```bash
kubectl -n default logs deployment/agent-orbital-docs-agent --tail=80 |
  grep -F 'knowledge_search'
```

Inline retrieval splits each source on blank lines and applies a lightweight
keyword score. For embedding-backed semantic retrieval, use
[`knowledge-base-kognition-docs`](../knowledge-base-kognition-docs/README.md)
or one of the blob samples.

## Index Runs And Schedules

The inline class has no external ingester. Applying the optional run or
schedule still exercises admission and controller behavior, but an IndexRun
completes without launching a batch Job because the documents already live in
the KnowledgeBase spec.

The optional run/schedule commands work in Bash and PowerShell:

```sh
kubectl apply -f samples/knowledge-base/indexrun.yaml
kubectl -n default get kbrun -w

kubectl apply -f samples/knowledge-base/indexschedule.yaml
kubectl -n default get kbsched
```

Use the pgvector samples to exercise real Jobs, chunking, embedding, and
vector retrieval.
