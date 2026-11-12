# Kognition Docs KB Sample

End-to-end sample that indexes this repo's top-level documentation
(`README.md`, `PROJECT.md`, `BACKLOG.md`, `docs/kognition-manifest.md`)
into the built-in pgvector store and exposes it to an agent through a
`KnowledgeBaseClaim` + `knowledge_search` tool.

## What you get

- A tiny `python -m http.server` Deployment (`kognition-docs`) that
  serves the four markdown files from an in-cluster ConfigMap so the
  ingester can fetch them via `type: url`.
- A `Model` (`text-embedding-3-small`) reusing the existing
  `foundry-creds` SecretRef (Azure service-principal client-secret,
  no OpenAI key required).
- A `KnowledgeBaseClass` (`kognition-docs-kb-class`) targeting the
  chart-managed `kognition-kb-pgvector` StatefulSet, dimension 1536.
- A `KnowledgeBase` (`kognition-docs-kb`) with four URL sources.
- A one-shot `KnowledgeBaseIndexRun` (`kognition-docs-reindex-now`).
- An `Agent` (`kognition-docs-agent`) that consumes the KB via a
  `KnowledgeBaseClaim`.

## Prerequisites (one-time)

1. Install current Kubernetes-mode images with pgvector and the ingester:

   ```powershell
   ./make.ps1 docker-k8s
  ./make.ps1 deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
   ```

  Follow the [guarded installer contract](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md).
  Include `kbPgvector`, `kbIngester` and exact image settings in reviewed values;
  load images separately into the explicit reviewed cluster. The deploy target
  bootstraps inactive admission and does not load images or credentials.
  All remaining sample commands require separately approved activation, an
  explicit reviewed kubeconfig, dedicated enrolled namespaces and no active
  bootstrap/recovery freeze. Missing security gates block activation.

2. The pgvector `vector` extension is enabled (bundled with the
   pgvector/pgvector image):

   ```powershell
   kubectl -n kognition-system exec kognition-kb-pgvector-0 -- `
     psql -U kognition_kb -d kognition_kb `
     -c "CREATE EXTENSION IF NOT EXISTS vector"
   ```

3. The `foundry-creds` SecretRef exists in the `default` namespace with
   an `azure-identity` / `client-secret` payload (already true in this
   workspace).

## Apply

`apply.ps1` handles two things the static YAMLs cannot:

- Builds the `kognition-docs` ConfigMap from the actual repo files, so
  the KB always mirrors your working tree.
- Copies the chart-managed `kognition-kb-pgvector` Secret from
  `kognition-system` into `default` so the ingester can read it in-namespace.

```powershell
./samples/knowledge-base-kognition-docs/apply.ps1
```

Then watch the reindex:

```powershell
kubectl -n default get kbrun kognition-docs-reindex-now -o wide -w
```

Expected terminal state:

```
NAME                          PHASE       DOCS   CHUNKS
kognition-docs-reindex-now    Succeeded   4      >0
```

If it fails, inspect the Job pod:

```powershell
kubectl -n default logs job/kb-indexrun-kognition-docs-reindex-now
```

## Query

Once the KB reports Ready:

```powershell
./bin/kognitionctl.exe chat kognition-docs-agent `
  "What CRDs does Kognition expose?" `
  --mode kubernetes
```

The agent should call `knowledge_search` and cite one or more of the
four indexed files in its answer.

## Cleanup

```powershell
kubectl -n default delete -f samples/knowledge-base-kognition-docs/indexrun.yaml --ignore-not-found
kubectl -n default delete -f samples/knowledge-base-kognition-docs/agent.yaml --ignore-not-found
kubectl -n default delete -f samples/knowledge-base-kognition-docs/kb.yaml --ignore-not-found
kubectl -n default delete -f samples/knowledge-base-kognition-docs/docs-server.yaml --ignore-not-found
kubectl delete -f samples/knowledge-base-kognition-docs/kb-class.yaml --ignore-not-found
kubectl -n default delete -f samples/knowledge-base-kognition-docs/model.yaml --ignore-not-found
kubectl -n default delete configmap kognition-docs --ignore-not-found
kubectl -n default delete secret kognition-kb-pgvector --ignore-not-found
```
