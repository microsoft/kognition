# The Kognition Manifest

> *Cognition is an orchestration problem. Kubernetes already solved
> orchestration. So we built on it.*

Kognition's API shape, control loop, and operational model are deliberately
modeled on Kubernetes. This page is the **manifest** for that choice — why
we made it, what it commits us to, and how every Kognition resource maps
to a familiar Kubernetes concept.

See the [resource interaction map](resource-interactions.md) for the complete
43-kind reference, ownership, routing, policy, and data-plane graph.

---

## 1. Why Kubernetes-native?

### 1.1 Cognition is an orchestration problem

Running production agents looks a lot more like running production services
than like calling an LLM. You need:

- Long-lived, addressable workloads (the agent process).
- Per-request scheduling units (an invocation / job).
- Connection pooling to expensive backends (the model).
- Quotas, fair-sharing, and admission control across tenants.
- A way to declare what should run and let the platform make reality match.

Kubernetes already solved every one of those problems for containers. We get
the entire vocabulary, control-loop model, and tooling ecosystem for free by
shaping our API the same way.

### 1.2 Declarative is the right abstraction for agents

A `Workflow`, an `AgentPolicy`, and a `MemoryClaim` are exactly the kind of
artifact that should be GitOps-managed: reviewed, versioned, applied, rolled
back, diffed. Imperative agent SDKs ("call this function, then maybe that
one") fight every operational practice we already use for services.

Kognition resources are pure declarations of *desired state*. Reconcilers
make the world match. The same engineer who knows how to operate a
`Deployment` already knows how to operate an `Agent`.

### 1.3 The control-loop model maps perfectly to agents

The Kubernetes pattern — **spec describes intent, status describes
observed reality, a controller reconciles the gap** — is exactly what an
agent platform needs:

| Need | Kubernetes pattern | Kognition use |
|---|---|---|
| "Make sure N replicas exist" | Deployment + ReplicaSet | `Agent` → `AgentInstance` pool |
| "Run this once until completion" | Job | `AgentJob` |
| "Run this on a schedule" | CronJob | `AgentJob` (scheduled) |
| "Bind storage on demand" | PVC + StorageClass | `MemoryClaim` + `MemoryClass` |
| "Cap usage per tenant" | ResourceQuota | `CognitiveQuota` |
| "Apply default limits" | LimitRange | `CognitiveLimitRange` |
| "Gate admission" | ValidatingAdmissionPolicy | `CognitiveAdmissionPolicy` |
| "Authorize an actor" | Role / RoleBinding | `AgentRole` / `AgentRoleBinding` |
| "Reach another workload" | NetworkPolicy | `AgentCommunicationPolicy` |
| "Stable network identity" | Service | `AgentService` |
| "Catalog of providers" | StorageClass / IngressClass | `ModelClass`, `ToolClass`, `MemoryClass`, `KnowledgeBaseClass` |

Whenever someone on the team asks "how should X work?", we have a strong
prior already: *how does Kubernetes do it?*

### 1.4 Concrete operational benefits

Running on real Kubernetes (the optional `kognition-k8s-controller` mode)
unlocks the full Kubernetes operational surface for agent workloads:

- **`kubectl` and `kubectl-kognition`** — Inspect, edit, patch, watch
  agents and workflows with the same tooling and reflexes used for every
  other workload.
- **GitOps-ready** — Argo CD, Flux, Helm, Kustomize all work because the
  resources are CRDs. See [`deploy/argocd-application.yaml`](../deploy/argocd-application.yaml).
- **RBAC, Namespaces, ServiceAccounts** — Reuse the existing tenancy
  boundary. No parallel auth system to learn or maintain.
- **NetworkPolicy & Service Mesh** — Apply Cilium, Calico, Istio, Linkerd
  policies directly to agent pods. Pod-to-LLM-provider egress is governed
  the same way as any other workload.
- **Pod Security Standards, SecurityContextConstraints** — Agent pods
  inherit cluster-wide security posture out of the box.
- **HorizontalPodAutoscaler, KEDA, Cluster Autoscaler** — Scale agent
  Deployments on the same signals as everything else (CPU, custom
  metrics, queue depth).
- **Prometheus / OpenTelemetry / Grafana / Tempo** — Standard pod-level
  scrape and OTLP export work unchanged. We do not invent a parallel
  observability stack.
- **Cluster-grade scheduling** — Taints, tolerations, node affinity,
  topology spread, pod disruption budgets all apply to agent pods.
- **Disaster recovery** — Velero, etcd backups, namespace replay all
  cover agent state because it lives in the same store.
- **Cost attribution** — OpenCost, Kubecost, and provider billing already
  understand pod-level and namespace-level chargeback.

### 1.5 What this avoids

By not inventing a new substrate we avoid:

- A second scheduler with its own opinions about pods and nodes.
- A second tenancy model that has to be reconciled with cluster RBAC.
- A second secret store, identity model, network policy engine, audit log,
  observability stack, and disaster-recovery story.
- A second set of failure modes for operators to learn.

The unit of cognitive isolation is the **Namespace**. The unit of
addressability is the **Service**. The unit of execution is the **Pod**.
We did not need to invent any of these.

---

## 2. The landscape — where Kognition sits

Several categories of tool overlap with what Kognition does. None of them
occupy the same space, and the difference matters:

**Agent SDKs embedded in your application** (LangChain / LangGraph,
CrewAI, AutoGen, Semantic Kernel, Microsoft Agent Framework, the OpenAI
Agents SDK). These are *libraries*. They optimize for "how do I write an
agent loop in my own process?" They have no opinion about tenancy,
quota, admission, identity, network policy, or rollout — that is the
hosting application's job. Kognition treats those as first-class
concerns of the control plane and lets the SDK choice stay
implementation detail of the runtime container.

**Hosted agent services** (OpenAI Assistants, Azure AI Foundry agents,
AWS Bedrock Agents, Vertex AI Agent Builder). These optimize for "I do
not want to run anything." You hand over the agent definition and the
provider runs it. Kognition's `Model` and `Tool` abstractions let you
*use* these as backends, but the governance, memory, identity, and
workflow plane stay yours — on your cluster, your namespaces, your
audit log.

**General workflow engines** (Temporal, Argo Workflows, Prefect,
Airflow, Step Functions). These optimize for "run this DAG of tasks
durably." Kognition's `Workflow`/`WorkflowRun` shares this lineage, but
a workflow engine has no model of an `Agent` as a long-lived addressable
identity with its own tools, memory volumes, and autonomy class. We
borrow the durability and DAG semantics; we add the cognitive primitives
on top.

**MLOps / model-serving platforms** (KServe, Ray Serve, BentoML,
Kubeflow). These optimize for serving a *model* — typically a single
inference call. Kognition's `Model` resource can point at one of these,
but an agent is not a model: it has state, tool calls, multi-turn
memory, and policy. Serving infrastructure is one input to the system,
not the system itself.

**Kubernetes-native agent and MCP platforms** (kagent, KMCP / ToolHive,
Dapr Agents, LangChain-on-K8s operators). This is the category closest
to Kognition — projects that also took the bet that the cluster is the
right place to run agents and MCP tool servers. They are excellent at
what they cover: kagent exposes an `Agent` and `Tool` CRD aimed at
cloud-native ops/SRE use cases on top of AutoGen; KMCP / ToolHive
package MCP servers as first-class K8s workloads; Dapr Agents brings
the Dapr building blocks (state, pub/sub, workflows) to agent code.
What they intentionally do *not* try to be is a full cognitive control
plane: there is generally no separate `Memory`/`MemoryClaim`/
`MemoryClass` plane, no `Identity` and `Grant` model wired into K8s
RBAC, no admission-time `AutonomyClass`/`CognitiveQuota`/
`CognitiveLimitRange` governance, and no unified `Workflow` /
`WorkflowRun` plane sitting above the agents. Kognition's bet is that
**all of those primitives belong in the API**, not in glue code on top
of an `Agent` CRD — and that MCP servers, when you want them, slot in
as `Tool` resources the same way containers slot into a Pod.

The short version: **SDKs are how an agent thinks, hosted services are
where a model runs, workflow engines are how steps are sequenced, MLOps
is how models are served, and other K8s-native agent projects expose an
agent + tool surface.** Kognition is the **control plane** that ties
those layers — plus memory, identity, governance, and autonomy — into a
multi-tenant, declarative platform, the same role Kubernetes plays for
containers.

---

## 3. The Cognitive Factors

The [12-Factor App](https://12factor.net/) gave web services a shared
vocabulary for what "good" looked like on a cloud platform. The
following factors play the same role for cognitive workloads on
Kubernetes. They are **not** about how to write an individual agent
loop — that is the SDK layer's job. They are about how a *cognitive
control plane* should treat agents, tools, memory, identity, and
governance as first-class resources, and they double as a checklist any
agent platform can be measured against.

### I. Declarative over imperative

Every cognitive object — agents, workflows, tools, models, memory,
identity, grants, governance — is a CRD. There is no imperative API
where you "construct" an agent in code and then register it. You apply
a manifest, the control plane reconciles it.

### II. One agent, one identity, one workload

An `Agent` CR maps to a single Deployment with a single identity.
Replicas scale that identity horizontally, they do not multiply it. We
do not collapse multiple agents into one shared runtime process, and we
do not split one agent across multiple deployments. The identity, not
the pod, is the unit of cardinality.

### III. Memory is a pluggable resource

Memory follows the storage pattern: `MemoryClass` describes a kind of
memory backend, `MemoryClaim` requests one, `MemoryVolume` is the bound
instance, and an agent mounts it. Memory is never library state hidden
inside an agent's container — it is a declared, addressable,
audit-friendly resource the platform manages.

### IV. Tools are addressable and grantable

A `Tool` is a first-class object with its own version, owner, and
schema. Agents do not "import" tools; they are *granted* the ones they
may use. The grant — not the agent's source code — is the source of
truth for capability.

### V. Models are configuration, not code

The `Model` resource decouples *which model* an agent uses from the
agent's behavior. Swapping providers, regions, or model versions is a
declarative edit, not a redeploy of agent code.

### VI. Workflows are external to agents

An agent never knows it is part of a `Workflow`. The workflow
controller drives the agent through invocations; the agent reacts to
inputs. This keeps agents composable across different orchestrations
and prevents workflow logic from leaking into agent code.

### VII. Governance happens at admission

`CognitiveQuota`, `CognitiveLimitRange`, `AutonomyClass`, and
`CognitiveAdmissionPolicy` are enforced when the resource is applied,
not at runtime by a sidecar or a wrapper SDK. If a workload should not
exist, it never reaches a pod.

### VIII. Trust is explicit

The autonomy level of every agent is named (`AutonomyClass`), the
identity it acts as is named (`Identity`), and every privileged action
it can take is named (`Grant`). There are no implicit, ambient, or
"developer mode" capabilities.

### IX. Identity is a Kubernetes identity

An agent's identity is bound to RBAC the same way a Pod's
ServiceAccount is. Permissions to read a memory volume, call a tool, or
invoke another agent are Kubernetes authorization decisions, not
strings in a config map.

### X. Observability is non-negotiable

Every invocation produces a trace, every grant produces an audit
record, every reconcile produces a status condition. If something
happened in the platform and there is no record of it, that is a bug.

### XI. GitOps is the source of truth

The cluster's set of `kubectl get …` outputs *is* the system. There is
no separate registry, no console-only state, no "click here to deploy
an agent" path that bypasses the manifest. Roll-forward and rollback
are git operations.

### XII. Composable, not monolithic

Memory, knowledge, tools, models, identity, governance, and autonomy
are independent planes. You can adopt them incrementally and replace
any one of them without rewriting the others. There is no "Kognition
all-in-one object" that ties them together — the *cluster* ties them
together, the way it always has for containers.

These twelve are the credo, the contribution surface, and the
yardstick. When evaluating any agent platform — including ours —
these are the questions worth asking.

---

## 4. Two layers of "Kubernetes-native"

It matters to be precise about *where* the Kubernetes shape shows up:

### Layer A — API shape (always on)

The Kognition API server (`kognition-apiserver`) and the in-process
controller manager (`kognition-controller-manager`) speak a
Kubernetes-shaped REST API (`apiVersion` / `kind` / `metadata` /
`spec` / `status`, list/get/watch semantics, optimistic concurrency via
`resourceVersion`, finalizers, conditions, events). This is true whether
or not a Kubernetes cluster is present.

You get the declarative model, the resource catalog, the reconciler
loop, and the CLI ergonomics with nothing more than PostgreSQL.

### Layer B — Kubernetes-mode projection (optional)

When `kognition-k8s-controller` is deployed into a real cluster, Kognition
CRDs are projected onto that cluster:

- **Namespace colocation**: all child resources are created in the same
  namespace as their parent CR. An `Agent` in `team-a` gets its
  `Deployment` + `Service` in `team-a`; an `AgentJob` in `team-a` creates
  its `AgentInstance`s in `team-a`; a `WorkflowRun` in `team-a` spawns
  `AgentJob`s in `team-a`. This applies uniformly to every resource type.
  Set `KOGNITION_AGENT_NAMESPACE` to override runtime pod placement with
  a single dedicated namespace for all agent workloads.
  See [`internal/k8s/dispatcher/dispatcher.go`](../internal/k8s/dispatcher/dispatcher.go).
- Kognition CRDs (`Agent`, `Workflow`, `MemoryClaim`, ...) are installed
  via the Helm chart at [`deploy/helm/kognition`](../deploy/helm/kognition).
- Admission webhooks (`internal/k8s/webhook`) validate and default
  resources at apiserver admission time.

In Layer B, `kubectl get agents -A` is the source of truth. Colocated runtime
resources are named `agent-<name>`. Shared-namespace runtime resource names
include a hash of the source Agent identity; discover them with the
`kognition.dev/agent-name` and `kognition.dev/agent-namespace` labels.

---

## 5. Resource mapping reference

The table below maps every Kognition resource to its closest Kubernetes
equivalent and notes what is **materialized today** in K8s mode versus
what is **conceptually analogous**. Kubernetes mode installs a CRD for each of
the 47 canonical kinds. `ToolGrant` is first-class in both storage modes;
embedded `AgentPolicy.spec.toolGrants` entries remain a runtime authorization
compatibility path.

Legend:

- **🟢 materialized** — K8s-mode controller creates the K8s primitive.
- **🟡 CRD-only** — projected as a Kognition CRD on the cluster; consumed
  by Kognition reconcilers, no native K8s object emitted.
- **⚪ design analogue** — shares the design pattern but has no direct
  K8s counterpart.

### 3.1 Agent lifecycle

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `Agent` | `Deployment` | 🟢 | One `Deployment` + `Service` per `Agent`, colocated in the Agent's namespace by default. Shared placement through `KOGNITION_AGENT_NAMESPACE` uses a namespace-qualified hashed name to isolate same-named Agents. PodTemplate uses the configured runtime adapter image; env vars carry model binding and provider config. |
| `AgentInstance` | `Pod` (logical) / `Job` retry semantics | 🟡 | A single invocation against the shared agent Deployment. The pod is reused; the instance is the per-request control object that tracks session, phase, output. Created in the same namespace as its parent `Agent` or `AgentJob`. |
| `AgentJob` | `Job` | 🟡 | Run-once cognitive work with success/failure semantics. Owns one or more `AgentInstance`s, all created in the job's namespace. |
| `AgentService` | `Service` | 🟢 | Publishes a stable core `Service` that selects the referenced Agent runtime pods. A shared runtime namespace uses a tenant-qualified hashed name. When `revisionPin` is set, the controller renders a dedicated service-owned Deployment from the immutable Agent revision and selects only its pods. Standalone mode uses the same snapshot through its API `/invoke` subresource. |
| `AgentAutoscaler` | `HorizontalPodAutoscaler` | 🟡 | Declares scaling targets and signals (queue depth, latency, cost) for the underlying Deployment. |

### 3.2 Workflow & orchestration

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `Workflow` | (no direct analog) — closest is Argo `Workflow` CRD | ⚪ | DAG of agent invocations. Pure Kognition concept; Kubernetes itself has no first-class workflow primitive. |
| `WorkflowRun` | `Job` (for the workflow as a whole) | 🟡 | Single execution of a `Workflow`. Steps map to child `AgentJob`s created in the run's namespace. |
| `IntentGateway` | `Ingress` | 🟢 | Entry point that turns a natural-language goal into an Agent, AgentService, or Workflow execution. Both standalone and Kubernetes modes serve matching HTTP (`/goals`) and unary gRPC (`kognition.v1alpha1.IntentGateway/SubmitGoal`) contracts. Both apply optional API-key authentication through a `SecretRef` and persist the matched route in `Solution.spec.target`. AgentService routes require ready instances and compile an immutable snapshot of the service-selected Agent revision into an `AgentJob`; Workflow routes compile into a `WorkflowRun`. Kubernetes serves through the dedicated `intent-gateway` ClusterIP Service and publishes its HTTP service-DNS endpoint only while the gateway Deployment is Available. |
| `Solution` | `Application` (Argo CD) | 🟡 | A bundled, deployable composition of agents, workflows, tools, and policies. |

### 3.3 Memory plane

This is the area where the Kubernetes design pattern is most directly
borrowed. The shape mirrors `StorageClass` → `PersistentVolumeClaim` →
`PersistentVolume` exactly.

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `MemoryClass` | `StorageClass` | 🟡 | Declares a memory backend driver (`inmemory`, `pgvector`, `mem0`, ...) and its provisioning parameters. |
| `MemoryClaim` | `PersistentVolumeClaim` | 🟡 | Tenant-scoped request for memory of a given class, size, and access mode. |
| `MemoryVolume` | `PersistentVolume` | 🟡 | Dynamically or statically provisioned memory backend instance bound to a claim. |
| `Memory` | (mount of a Volume into a Pod) | 🟡 | Attachment of a `MemoryVolume` into an `Agent`/`AgentInstance`. MemoryClaim provisions the generated backend handle before binding, and `ReclaimDelete` deprovisions it before removing the volume. The runtime exposes memory tools for the selected binding. |
| `MemoryRecord` | (object inside the volume) | ⚪ | An individual durable item (turn, fact, embedding) within a `Memory`. No K8s analog — it lives at the data plane. |
| `MemoryGrant` | `Role` + `RoleBinding` scoped to a volume | 🟡 | Authorizes a principal to read/write a specific `Memory`. |

### 3.4 Knowledge plane

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `KnowledgeBaseClass` | `StorageClass` | 🟡 | Declares a retrieval backend (vector store, hybrid search, web). |
| `KnowledgeBase` | `PersistentVolumeClaim`+`PersistentVolume` | 🟡 | A bound, queryable corpus. Acts as both claim and provisioned instance because the backend is typically external (e.g., Azure AI Search). |
| `KnowledgeBaseClaim` | `RoleBinding` to a volume | 🟡 | Binds an Agent to a same-namespace KnowledgeBase with retrieval overrides and usage caps. |
| `KnowledgeBaseIndexRun` | `Job` | 🟢 | Launches one batch ingestion Job when the class uses the external ingester. |
| `KnowledgeBaseIndexSchedule` | `CronJob` | 🟡 | Creates IndexRun resources from a five-field UTC cron schedule. |

### 3.5 Tools

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `ToolClass` | `StorageClass` / `IngressClass` | 🟡 | A pluggable kind of tool (HTTP, MCP, plugin, function). |
| `Tool` | `Service` (external) + `ConfigMap` | 🟡 | A concrete addressable capability an agent can call, plus its schema/config. |
| `ToolGrant` | `Role` + `RoleBinding` | 🟡 | First-class authorization resource in standalone and Kubernetes modes. The Tool Gateway evaluates native grants before the embedded `AgentPolicy.spec.toolGrants` compatibility path. |

### 3.6 Models

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `ModelClass` | `StorageClass` | 🟡 | Declares a model provider (Azure OpenAI, Foundry, OpenAI, Anthropic, ...). `spec.parameters.cost.inputMicrousdPerMillionTokens` and `spec.parameters.cost.outputMicrousdPerMillionTokens` optionally configure normalized micro-USD rates for model-inference span tags. Configure both rates from the provider contract. |
| `Model` | `Service` (external endpoint) | 🟡 | A concrete model deployment (provider, model ID, endpoint, credentials). Agents reference one via `spec.modelRef`. |
| `ModelGrant` | `Role` + `RoleBinding` | 🟡 | Authorizes an `Agent` to call a `Model`. |
| `ModelInvocationProfile` | Immutable configuration | Local implementation | Declares portable defaults, requirements and limits. Each concrete Model selects an explicit adapter-owned invocation contract. See [Model Invocation Profiles](model-invocation-profiles.md). |
| `ModelAutoscaler` | `VerticalPodAutoscaler` | 🟡 | Promotes or demotes model tiers from declared SLO and budget signals. |

### 3.7 Identity & access

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `AgentIdentity` | `ServiceAccount` | 🟡 | The identity an agent acts as. Token issuance, federation, workload identity all key off this. |
| `AgentRole` | `Role` / `ClusterRole` | 🟡 | A set of permissions over Kognition verbs/resources. |
| `AgentRoleBinding` | `RoleBinding` / `ClusterRoleBinding` | 🟡 | Binds an `AgentIdentity` to an `AgentRole` in a scope. |
| `AgentCommunicationPolicy` | `NetworkPolicy` | 🟡 | Declarative allow/deny rules between agents, tools, and external endpoints. |
| `CrossNamespaceGrant` | `RoleBinding` across namespaces | 🟡 | Allows selected Kognition resource references to cross namespace boundaries. |
| `Workspace` | `Namespace` | 🟡 | Logical tenancy unit. In K8s mode it composes with a Kubernetes `Namespace`. |

### 3.8 Governance, quota, admission

This is the area with the most literal one-to-one mapping.

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `AgentPolicy` | `PodSecurityPolicy` (deprecated) / OPA Gatekeeper `Constraint` | 🟡 | Per-resource policy declaration enforced at admission and runtime. |
| `CognitiveAdmissionPolicy` | `ValidatingAdmissionPolicy` / `MutatingAdmissionPolicy` | 🟡 | CEL-like predicates over admission requests for Kognition kinds. |
| `CognitiveQuota` | `ResourceQuota` | 🟡 | Caps per-namespace cognitive usage: tokens, invocations, cost, concurrent jobs. Enforced by the `quota_enforcer` admission hook. |
| `CognitiveLimitRange` | `LimitRange` | 🟡 | Defaults and per-object min/max for cognitive resources (token budgets, timeouts). Applied by the `limit_range_defaulter`. |

### 3.9 Autonomy & trust (Kognition-specific)

These have no direct Kubernetes counterpart — they are the domain-specific
additions that justify the platform existing as more than a relabeling
of Kubernetes.

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `AutonomyClass` | (no direct analog) | ⚪ | Declares the autonomy tier (e.g., advisory, supervised, autonomous) and the approval policy that applies. |
| `TrustProfile` | (no direct analog) | ⚪ | Aggregated trust signals for an identity (behavioral history, risk score). |
| `AutonomyReview` | (no direct analog; analogous to a manual `Approval` in CD pipelines) | ⚪ | A pending human-in-the-loop decision gating an agent action. |
| `ApprovalDecision` | `CertificateSigningRequest` approval update | 🟡 | Records an authenticated approve or deny decision for a pending gate. |

### 3.10 Secrets, prompts, configuration

| Kognition | Kubernetes analogue | Status | Notes |
|---|---|---|---|
| `SecretRef` | `Secret` (reference) | 🟡 | Points at a backing secret (K8s `Secret`, Key Vault, etc.) without inlining the value. Resolved at admission/dispatch time. |
| `Prompt` | `ConfigMap` | 🟡 | Versioned, declarative prompt content with structured metadata. |

---

## 6. How "feels like Kubernetes" plays out in practice

A few concrete examples of the operational ergonomics this design buys:

**A new engineer onboarding**

```bash
kubectl get agents -A
kubectl describe agent writer -n default
kubectl logs -n default deploy/agent-writer
kubectl get workflowrun -A
kubectl get cognitivequota -A
```

No new mental model. They already know what each verb means.

**GitOps roll-forward of an agent change**

```bash
git apply prompt-tweak.yaml
git push                           # Argo CD picks it up
kubectl get agents writer -o yaml  # observe new generation
kubectl rollout status deploy/agent-writer -n default
```

The same review/approval/promotion pipeline used for any other workload.

**Multi-tenant isolation**

```yaml
apiVersion: kognition.dev/v1alpha1
kind: CognitiveQuota
metadata:
  name: team-alpha-cap
  namespace: team-alpha
spec:
  hard:
    tokens.daily: "1000000"
    workflowruns: "50"
    cost.monthly.usd: "500"
```

Quota lives next to `ResourceQuota`. Admission webhook denies submissions
that exceed it. Same model the cluster already uses for CPU and memory.

**Network governance**

```yaml
apiVersion: kognition.dev/v1alpha1
kind: AgentCommunicationPolicy
metadata:
  name: writer-egress
spec:
  podSelector:
    matchLabels:
      kognition.dev/agent: writer
  egress:
    - to:
        - models:
            matchLabels: { provider: foundry }
        - tools:
            matchNames: [web-search]
```

The shape is `NetworkPolicy`. The vocabulary is cognitive. The operator
intuition transfers one-to-one.

---

## 7. When NOT to use the K8s-mode controller

The Kognition API shape is always Kubernetes-shaped, but installing
`kognition-k8s-controller` into a real cluster is opt-in. Skip it when:

- You are doing local development against `docker compose` and just want
  the apiserver + PostgreSQL.
- You are building a hosted SaaS where the cluster is an implementation
  detail and `kognition-apiserver` is the only public surface.
- Your workload is short-lived enough that the overhead of a
  per-`Agent` `Deployment` is wasteful — in that case run agents inside
  the in-process runtime via `kognition-controller-manager`.

In all of those cases you still get Layer A (the Kubernetes-shaped API),
so the design discipline and the upgrade path to Layer B stay intact.

---

## 8. Further reading

- [`PROJECT.md`](../PROJECT.md) — full specification, including the
  resource catalog and reconciler contracts.
- [`deploy/QUICKSTART.md`](../deploy/QUICKSTART.md) — installing the
  K8s-mode controller into a cluster.
- [`api/v1alpha1/register.go`](../api/v1alpha1/register.go) — the
  canonical list of registered kinds.
- [`internal/k8s/dispatcher/dispatcher.go`](../internal/k8s/dispatcher/dispatcher.go)
  — how `Agent` → `Deployment` + `Service` projection works today,
  including the shared-Deployment invariant.
- [`internal/admission`](../internal/admission) — defaulters, validators,
  and quota/limit-range enforcement (the Kubernetes admission analogues).
