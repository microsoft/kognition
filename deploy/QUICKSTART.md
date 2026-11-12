# Kognition Quickstart Guide

Deploy Kognition on a local Kubernetes cluster. The Make targets are the
recommended path; the manual sections are useful for troubleshooting or
custom image registries.

## Prerequisites

- Docker Desktop or equivalent container runtime
- A local Kubernetes cluster: [kind](https://kind.sigs.k8s.io/), [minikube](https://minikube.sigs.k8s.io/), or Docker Desktop K8s
- [Helm 3](https://helm.sh/docs/intro/install/)
- `kubectl` configured for the cluster

## Quick Path (using Make targets)

Standalone installation and guarded Kubernetes bootstrap use different workflows:

```bash
# Standalone mode (API server + PostgreSQL + controller manager)
make docker-standalone
make deploy-standalone

# Kubernetes mode: prepare/review the plan first; ends with controller inactive
make docker-k8s
make deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
```

For Kubernetes mode, follow the [installation and recovery contract](../docs/features/knowledge-provider-execution/installation-recovery-contract.md).
It specifies explicit target selection, deterministic artifacts, namespace and
resource inventories, certificate/CRD preparation, writer freeze and recovery.
Both wrappers invoke the same installer. They do not load images or credentials,
install cert-manager, run samples or activate the controller. Activation is separate
and remains blocked until all required security gates pass.

The resource-admission server runs separately while reconciliation is inactive.
Guarded installation skips Helm hooks, including the usual migration hook. Run
reviewed platform migrations and initialize the separate KnowledgeBase `vector`
extension before affected workloads; see
[database prerequisites](../docs/features/knowledge-provider-execution/installation-recovery-contract.md#database-prerequisites).

Verify:

```bash
make status
```

After separately approved activation and namespace enrollment, install a sample agent:

```bash
make samples-install SAMPLE=simple-agent
```

After approval of every target namespace and credential writer, install and validate
the complete sample bundle. Do not run it during a bootstrap/recovery freeze:

```bash
make samples-full-install
```

The bundle creates its sample namespaces, applies Foundry credentials from the
repository `.env`, installs the local Azurite and S3-compatible knowledge-base samples,
and deploys the compatible governance and multi-agent Helm samples in isolated
namespaces. It fails if an included sample cannot apply or reaches a terminal
failure state. It requires `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and
`AZURE_CLIENT_SECRET` in `.env` for provider-backed Models.

Use the explicit target and interpreter checks in
[the bootstrap guide](../docs/bootstrapping.md). The Azurite uploader may report
`SKIP` with exit zero when its SDK is absent; require actual upload counts. The
standalone `autonomy` sample's conflicting Agent is excluded from the combined
bundle, while Tool demos receive their class and UID-bound trust profiles.

`samples/knowledge-base-blob-e2e` remains a real-Azure integration profile. Run
`samples-provision-azure` from a runner that can reach the Storage data plane,
then install it explicitly. `samples/helm-umbrella` is an
alternative full-platform installation profile; run it in a separate cluster
because it deploys cluster-scoped controllers and admission webhooks.

On Windows, replace `make` with `.\make.ps1` (or `.\make.cmd`).

### Development Observability

The observability helper changes release values and is not a supported Kubernetes
upgrade path. For Kubernetes mode, review observability changes through the guarded
installer. In standalone mode, the opt-in bundle deploys Jaeger, Prometheus Operator, Prometheus, and
Grafana in `kognition-observability`, enables OTLP trace export, and turns on
the controller and Tool Gateway ServiceMonitors, alert rules, the Tool Gateway
dashboard, and the Kognition Agents 360 and Workflow 360 dashboards.

```bash
make observability-install
make jaeger-ui   # http://localhost:16686
make grafana-ui  # http://localhost:3000
kubectl -n kognition-observability get secret kognition-observability-grafana -o jsonpath='{.data.admin-password}' | base64 --decode
```

`make jaeger-install` remains a backward-compatible alias for the full bundle.
Remove it with `make observability-uninstall` (or `make jaeger-uninstall`).

---

## Manual Steps (detailed)

### 1. Build container images

```bash
# From the repository root:
docker build --target apiserver       -t kognition-apiserver:0.1.0          .
docker build --target controller      -t kognition-controller-manager:0.1.0 .
docker build --target k8s-controller  -t kognition-k8s-controller:0.1.0     .
docker build --target memory-service  -t kognition-memory-service:0.1.0     .
docker build -f runtime/agent/Dockerfile -t kognition-agent-runtime:0.1.0 .
docker build -f runtime/ingester/Dockerfile -t kognition-kb-ingester:0.1.0 .
```

The Python release images install dependencies at build time and remove pip from
the final image. Rebuild an image to change its dependencies; package installation
inside a running release container is unsupported.

The default agent image uses the native runtime. Select the optional AutoGen
profile with `--build-arg KOGNITION_RUNTIME_PROFILE=autogen`. Its reviewed SDK
version is `0.7.5`, with the explicitly selected `pillow==12.3.0`. That Pillow pin
retains known security findings and does not establish production release approval.

An approved alternative package feed can be supplied through the build-only
`PIP_INDEX_URL` argument. Feeds must use verified HTTPS. The default remains public
PyPI; the image does not retain feed configuration.

> The release pipeline also publishes a `kognitionctl` CLI container for
> CI/scripting use. It is not deployed to the cluster and does not need to be
> built locally for a basic install.

### Load images into local cluster

**kind:**

```bash
for img in kognition-apiserver kognition-controller-manager kognition-k8s-controller kognition-memory-service kognition-agent-runtime kognition-kb-ingester; do
  kind load docker-image --name "$REVIEWED_KIND_CLUSTER" $img:0.1.0
done
```

**minikube:**

```bash
for img in kognition-apiserver kognition-controller-manager kognition-k8s-controller kognition-memory-service kognition-agent-runtime kognition-kb-ingester; do
  minikube --profile "$REVIEWED_MINIKUBE_PROFILE" image load $img:0.1.0
done
```

> **Note:** Pre-built images are also published to GHCR by the release pipeline
> (see `.github/workflows/release.yml`) at `ghcr.io/<OWNER>/<name>:<tag>`,
> where `<OWNER>` is the GitHub repository owner. To pull from GHCR instead of
> building locally, set `global.imageRegistry: ghcr.io/<OWNER>` in reviewed values.
> Kubernetes mode uses the guarded installer; the direct Helm example below is
> standalone only.

## 2. Install standalone mode with Helm

```bash
helm install kognition deploy/helm/kognition \
  --namespace kognition-system \
  --create-namespace \
  --set mode=standalone \
  --set apiserver.image.pullPolicy=IfNotPresent \
  --set controllerManager.image.pullPolicy=IfNotPresent \
  --set memoryService.image.pullPolicy=IfNotPresent
```

## 3. Verify installation

```bash
kubectl -n kognition-system get pods
```

Wait until all pods are Running/Ready:

```text
NAME                                            READY   STATUS    RESTARTS   AGE
kognition-apiserver-xxxxx                       1/1     Running   0          30s
kognition-controller-manager-xxxxx              1/1     Running   0          30s
kognition-postgres-0                            1/1     Running   0          30s
```

## 4. Access the API server

```bash
# Port-forward to access the API
kubectl -n kognition-system port-forward svc/kognition-apiserver 8080:8080
```

In another terminal:

```bash
# Apply a sample Agent resource
cat <<EOF | curl -s -X POST http://localhost:8080/apis/kognition.dev/v1alpha1/namespaces/default/agents -H "Content-Type: application/json" -d @-
{
  "apiVersion": "kognition.dev/v1alpha1",
  "kind": "Agent",
  "metadata": {
    "name": "hello-agent",
    "namespace": "default"
  },
  "spec": {
    "modelRef": {"name": "gpt-4o"},
    "instructions": "You are a helpful assistant."
  }
}
EOF

# Check it was created
curl -s http://localhost:8080/apis/kognition.dev/v1alpha1/namespaces/default/agents/hello-agent | jq .
```

## 5. Using kognitionctl (alternative)

```bash
# Build the CLI
go build -o kognitionctl ./cmd/kognitionctl

# Apply from a YAML file
./kognitionctl apply -f samples/simple-agent/manifests.yaml
./kognitionctl get agents -n default
```

## 6. Kubernetes Mode (CRD-based)

Kubernetes mode projects Kognition resources as native CRDs with full webhook
validation. This mode replaces the standalone API server and controller
manager with the `kognition-k8s-controller`; the Kubernetes API server becomes
the resource API.

The controller watches the Helm release namespace,
`k8sController.managedNamespaces`, and a nonempty
`k8sController.agentNamespace`. The release namespace is control-plane-only:
the controller cannot create, mutate, delete, or attach to native workloads there.
Place native Agent runtimes, KnowledgeBases with ingestion Jobs, and Tools with
execution Jobs in separate enrolled workload namespaces. A nonempty
`k8sController.agentNamespace` must differ from the release namespace.
Names must be explicit DNS-label namespace names;
wildcards and empty entries are rejected. Create and review all namespaces before
installing or upgrading the release. The guarded installer creates no namespaces.

For example, include dedicated existing namespaces in reviewed values:

```yaml
mode: kubernetes
k8sController:
  managedNamespaces:
    - team-a-agents
    - team-b-agents
```

Record their exact UIDs in the plan. Package and render the reviewed chart, then
use the shared installer through either wrapper or its direct CLI. Direct Helm
upgrade/install/rollback is not the supported Kubernetes installation path.

Before upgrading an existing installation, inventory all namespaces containing
Kognition resources and enroll those still requiring reconciliation. Removing a
namespace stops its reconciliation and cleanup; complete deletion/finalization
before removing it. Cluster-scoped Kognition classes and binding records retain
their separate cluster permissions. Namespace enrollment is a privilege boundary,
not an ownership boundary within a namespace.

Use dedicated workload namespaces without unrelated privileged workloads,
ServiceAccounts, or credentials. The controller retains Secret lifecycle,
Service, and NetworkPolicy permissions throughout each enrolled workload
namespace; ownership checks do not restrict a compromised controller's RBAC.
Installation Secret reads are limited to the configured runtime-auth and enabled
Knowledge Gateway source names, but Kubernetes Secret reads return the entire
Secret. Store only the required source material in those objects.

Before applying this stricter RBAC to an existing release, inventory native
workloads in the installation namespace. Stop new work and complete their
cleanup/finalization with the old controller permissions still available.
Recreate the required resources in dedicated enrolled namespaces using the
resource-specific migration procedure; namespace changes are not in-place moves.
Preserve persistent data and binding identities, and verify recovery before
resuming traffic. This upgrade does not relocate or delete old workloads
automatically. Tightening RBAC first can strand their cleanup.

The Kubernetes controller uses a dedicated ServiceAccount named from the chart
fullname (truncated to 52 characters) plus `-controller`. Configure it through
`k8sController.serviceAccount.name`, `.create`, and `.annotations`. With
`create=false`, provision that account in the release namespace before upgrading.
The controller account must differ from the shared platform account and `default`.
The Deployment, controller RBAC bindings, quota-ledger binding, and trusted
approval-writer identity all use the dedicated name.

When upgrading, explicitly transfer any controller-required identity annotations
and external identity bindings from the shared account to the dedicated account.
Existing controller Pods retain their old identity until replaced; the RBAC
change can interrupt reconciliation during rollout. Verify rollout completion,
reconciliation, and approval processing before resuming work. Local chart and
authorization tests do not establish live upgrade acceptance. The memory service
retains only installation-local ConfigMap create/get/list access for audit batches;
it no longer inherits controller workload or Secret permissions.

Both deployment wrappers take enrollment exclusively from the reviewed plan and
values. `AGENT_NAMESPACE` does not replace that inventory. Review the complete
desired namespace set on every upgrade; Helm uses reset values, never reuse-values.
Before running a sample bundle after approved activation, prepare and explicitly
enroll its required dedicated namespaces. The sample installer does not expand
controller privileges automatically.

When running the controller outside Helm, set `KOGNITION_CONTROLLER_NAMESPACE`
and use comma-separated `KOGNITION_MANAGED_NAMESPACES` for additional namespaces.
The controller namespace and nonempty `KOGNITION_AGENT_NAMESPACE` are included
automatically. An empty comma-separated entry is invalid.

Agents with `runtime.class: default` or `runtime.class: native` use
`k8sController.agentImage`. Configure an adapter image before using another
runtime class; an unconfigured class is rejected instead of silently running
under the native runtime:

```yaml
mode: kubernetes
k8sController:
  agentNamespace: agent-runtimes
  runtimeImages:
    autogen: registry.example/autogen-runtime:v1
```

```bash
# Build the required images
make docker-k8s

# Bootstrap reviewed inactive admission
make deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
```

Prepare images separately as required by the reviewed profile:

```bash
docker build --target k8s-controller -t kognition-k8s-controller:0.1.0 .
docker build --target memory-service -t kognition-memory-service:0.1.0 .
docker build -f runtime/agent/Dockerfile -t kognition-agent-runtime:0.1.0 .
docker build -f runtime/ingester/Dockerfile -t kognition-kb-ingester:0.1.0 .
```

Use the contract's direct installer CLI for manual bootstrap/recovery. Specify
required runtime and ingestion settings in reviewed values. The controller remains
at zero replicas after bootstrap; a running guard alone does not approve activation.

Inspect Pods using the reviewed target:

```bash
kubectl --kubeconfig ./reviewed/cluster.kubeconfig -n kognition-system get pods
```

Only after separately approved activation, apply resources in enrolled namespaces:

```bash
kubectl apply -f samples/simple-agent/manifests.yaml
kubectl get agents.kognition.dev
```

## 7. Semantic memory (embeddings)

By default, the memory service ranks text queries with BM25 over raw record
content. To add semantic recall, point the memory service at an embedding
deployment. It then embeds record content on write (`/v1/memory/store`), embeds
text queries on search (`/v1/memory/search`), and combines semantic and lexical
rankings. Precomputed vector queries remain supported.

Agents keep no embedder of their own: the `memory_recall` and `memory_store`
tools attach automatically whenever an agent binds a memory volume, and call
the service with a plain text query.

Enable it via Helm values (supported providers: `openai-compatible`,
`foundry`, `ollama`):

The direct Helm example below is for standalone mode only. In Kubernetes mode,
include these settings in the complete reviewed values and use guarded bootstrap;
prepare the credential Secret separately before the writer freeze.

```bash
# Store the embedding API key (skip for keyless providers such as a local Ollama)
kubectl -n kognition-system create secret generic memory-embedding \
  --from-literal=apiKey="$OPENAI_API_KEY"

helm upgrade --install kognition deploy/helm/kognition \
  --namespace kognition-system --create-namespace \
  --set memoryService.embedding.provider=openai-compatible \
  --set memoryService.embedding.endpoint=https://api.openai.com/v1 \
  --set memoryService.embedding.model=text-embedding-3-small \
  --set memoryService.embedding.apiKeySecret.name=memory-embedding \
  --set memoryService.embedding.apiKeySecret.key=apiKey
```

- **Azure AI Foundry:** `provider=foundry`,
  `endpoint=https://<resource>.openai.azure.com`, and
  `model=<your embeddings deployment name>`. The service constructs the
  deployment-specific embeddings route. Set `apiVersion` only when the
  deployment requires a version other than the default `2024-02-01`. Omit the
  API key Secret to use the ambient default Azure credential chain. The chart
  labels the memory-service Pod for AKS Workload Identity; annotate its Service
  Account with the identity client ID. Remote API-key endpoints must use HTTPS.
- **Local Ollama sidecar:** `provider=ollama`,
  `endpoint=http://localhost:11434`, `model=nomic-embed-text`; omit the API
  key secret.

Set `provider`, `endpoint`, and `model` together. Helm rejects partial
configuration. When all three are empty, stores retain raw content without an
embedding and text recall uses BM25. Unrelated queries return
`"No relevant memories found."` instead of arbitrary recent records. The agent
runtime requests a minimum relevance score of `0.5` for memory recall.

## 8. Monitoring & Diagnostics

```bash
make status                         # Helm releases, pods, agents, workflows
make logs                           # Tail all platform pod logs
make logs COMPONENT=k8s-controller  # Tail specific component
make port-forward                   # Forward apiserver to localhost:8080
```

## Uninstall

For Kubernetes mode, finalize or explicitly migrate work before removing controller
privileges. Review destructive cleanup separately; uninstall is not recovery and
must not bypass an installation claim. Do not remove shared namespaces or CRDs.

```bash
# Via Make
make undeploy-standalone    # or undeploy-kubernetes
make samples-uninstall      # remove all samples

# Manual
helm uninstall kognition -n kognition-system
kubectl delete namespace kognition-system
```

## Alternative: Docker Compose (no K8s required)

For quick local testing without Kubernetes:

```bash
docker compose up --build
# API available at http://localhost:8080
```

## GitOps with Argo CD

The [Application example](argocd-application.yaml) uses manual synchronization
with automated pruning and self-healing disabled. It is an inactive installation
template; its placeholders require operator review before use.

1. Provision reviewed namespaces, CRDs, certificates and Secrets separately. Create
   the restricted `kognition-installation` AppProject with only the reviewed
   repository, destinations and required resource kinds. Exclude the separate
   activation authority namespace.
2. Pin the source to an immutable commit and every enabled image to a reviewed
   digest. Supply non-secret values with `k8sController.replicas=0`,
   `k8sController.activated=false`, enforcing network policies and fail-closed
   admission. When cert-manager is disabled, supply the reviewed public
   `webhook.caBundle` and preprovision its serving TLS Secret.
3. Compare Argo's rendered manifests with the reviewed inactive target before
   manual synchronization. Account for ordinary Kubernetes migration Jobs and
   verify the database schema before Memory operations.
4. Physically stop the Argo application-controller and settle outstanding requests.
   Maintain the external writer freeze while verifying the inactive, drained target
   and collecting genuine exact-target SG-03/07/08/09 evidence.
5. Obtain fresh owner approval and use the existing
   [activation transaction](../docs/features/knowledge-provider-execution/activation-transaction-contract.md).
   Only that transaction may scale the controller and grant workload authority.
   The Application ignores only the controller Deployment's replica field.

Keep source changes and recovery inside the
[installation and recovery contract](../docs/features/knowledge-provider-execution/installation-recovery-contract.md).
Do not resume automatic writers during approval, activation or compensation.
Successful rendering, synchronization and Pod readiness do not close acceptance
or unresolved image-security findings.
