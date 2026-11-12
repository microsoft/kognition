# ADR-0001 — KnowledgeBaseClaim + built-in in-memory KB provisioner

- **Status:** Accepted
- **Date:** 2026-07-03
- **Deciders:** Kognition core team

## Context

Kognition already models knowledge bases as two cluster-aware resources:

- `KnowledgeBaseClass` (cluster-scoped) — a provisioner + defaults, e.g. `inline`, `pgvector`, `qdrant`.
- `KnowledgeBase` (namespace-scoped) — a concrete corpus that picks a class, lists sources, and controls refresh.

Agents wire in a KB directly via `Agent.spec.knowledgeBases[] → KnowledgeBaseRef{Ref, Required}`. Two problems with this shape:

1. **No grant/binding layer.** The direct reference conflates *"this KB exists"* with *"this agent may query it, with these limits, in this mode"*. Access mode (read-only vs read-write), retrieval overrides (topK, filter, hybrid weights), and per-agent usage quotas have nowhere to live except on the KB itself, which forces the corpus curator and the agent author to be the same team.
2. **No zero-dependency provisioner.** The two useful provisioners today (`pgvector`, `qdrant`) both require an external system. The `inline` provisioner is not actually a searchable index — it just carries sources on the object. Trying the KB path end-to-end therefore requires bringing up a database, which cuts against Kognition's "runs locally, no external deps" starting story.

The `MemoryClaim ↔ MemoryClass ↔ MemoryVolume` triple already establishes a claim-style pattern in the platform, but it is a **provisioning** relationship (like PVC ↔ PV) — the claim asks the class to allocate a *private* backing store. Knowledge bases are almost never private per-agent; a KB is a **shared corpus** that many agents read.

## Decision

Introduce `KnowledgeBaseClaim` as a **grant/binding** between an agent and an existing `KnowledgeBase` (closer to `ServiceAccount + RoleBinding` than to PVC ↔ PV), and ship a built-in `provisioner: "memory"` KB class that runs entirely in-process with no external dependencies.

### 1. New type: `KnowledgeBaseClaim` (namespace-scoped)

```go
type KnowledgeBaseClaim struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              KnowledgeBaseClaimSpec   `json:"spec,omitempty"`
    Status            KnowledgeBaseClaimStatus `json:"status,omitempty"`
}

type KnowledgeBaseClaimSpec struct {
    // KnowledgeBaseName is the KnowledgeBase in the same namespace this claim binds to.
    KnowledgeBaseName string `json:"knowledgeBaseName"`

    // AccessMode is "ReadOnly" (default) or "ReadWrite".
    // ReadWrite lets tools that this claim grants to an agent add or delete documents.
    // +kubebuilder:validation:Enum=ReadOnly;ReadWrite
    // +kubebuilder:default=ReadOnly
    AccessMode string `json:"accessMode,omitempty"`

    // Retrieval overrides class/KB defaults for queries made through this claim.
    // Overrides never mutate the corpus.
    Retrieval *KBRetrievalOverride `json:"retrieval,omitempty"`

    // Usage caps enforced by governance/quota admission.
    Usage *KBUsageQuota `json:"usage,omitempty"`
}

type KBRetrievalOverride struct {
    TopK          *int32           `json:"topK,omitempty"`
    MinScore      *string          `json:"minScore,omitempty"` // decimal string, e.g. "0.35"
    Filter        map[string]string `json:"filter,omitempty"`
    HybridWeights *KBHybridWeights  `json:"hybridWeights,omitempty"`
}

type KBHybridWeights struct {
    Keyword string `json:"keyword,omitempty"` // decimal string, e.g. "0.4"
    Vector  string `json:"vector,omitempty"`  // decimal string, e.g. "0.6"
}

type KBUsageQuota struct {
    MaxQueriesPerMinute *int32 `json:"maxQueriesPerMinute,omitempty"`
    MaxTokensReturned   *int32 `json:"maxTokensReturned,omitempty"`
}

type KnowledgeBaseClaimStatus struct {
    // Phase: Pending | Bound | Lost.
    Phase string `json:"phase,omitempty"`

    // BoundKnowledgeBase mirrors spec.knowledgeBaseName once binding succeeds.
    BoundKnowledgeBase string `json:"boundKnowledgeBase,omitempty"`

    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
```

### 2. Agent wiring — hard swap

Delete `Agent.spec.knowledgeBases []KnowledgeBaseRef` and the `KnowledgeBaseRef` type. Replace with:

```go
type AgentSpec struct {
    // ...
    KnowledgeBaseClaims []KnowledgeBaseClaimRef `json:"knowledgeBaseClaims,omitempty"`
}

type KnowledgeBaseClaimRef struct {
    // Name of a KnowledgeBaseClaim in the same namespace as the Agent.
    Name string `json:"name"`

    // Required, when true, causes the Agent to depend on the claim being Bound
    // before it becomes Ready.
    Required bool `json:"required,omitempty"`
}
```

Downstream call sites updated in the same change (see task plan): `internal/admission/agent_validator.go`, `internal/controller/knowledge_base_reconciler.go`, `internal/convert/agent.go`, `internal/k8s/dispatcher/dispatcher.go`, tests, CRDs, and the sample under `samples/knowledge-base/manifests.yaml`. Studio's `kbPicker` becomes a `kbClaimPicker`.

### 3. Built-in `provisioner: "memory"` KB class

Zero external dependencies. BM25 keyword index only, kept fully in-process.

- Sources are read once at bind-time (and again on each `refreshPolicy` trigger) and tokenized into an in-memory BM25 index.
- No embedding model required; `indexMode: sparse` (existing enum value) is enforced.
- Data is lost on process restart — acceptable for the demo/starter tier; the KB just re-ingests from sources on the next reconcile.

### 4. Runtime location — works in both apiserver-mode and k8s-mode

Mirror the `kognition-memory-service` pattern:

- New `cmd/kognition-kb-service` binary exposes a small HTTP/JSON API (`/index`, `/query`, `/upsert`, `/delete`) backed by a `kb.Backend` interface with an `inmemory` (BM25) implementation.
- **apiserver-mode:** the apiserver process starts an in-process `inmemory` backend and mounts the same handler on its own listener (or via a shared mux); the built-in memory KB class is served without any extra pod.
- **k8s-mode:** the Helm chart ships an optional `kognition-kb-service` Deployment (single replica, no persistence) that the KB reconciler points KB claims at when the class uses the `memory` provisioner.
- Both modes therefore expose the same `kb.Backend` semantics; only the transport (in-process vs HTTP) differs.

## Consequences

**Positive.**

- Corpus curator and app team can be different teams: curator publishes a KB, app team creates a claim with its own retrieval knobs and access mode.
- Governance/quota hooks land on a small, namespaced object (the claim) rather than on the shared corpus.
- The demo path (`docker compose up`, `samples/simple-agent`, then talk to a KB) no longer requires provisioning a database.
- Conformance tests can run KB flows without any external service.

**Negative / to accept.**

- Hard swap breaks any manifest that used `agent.spec.knowledgeBases`. We update all in-tree samples and CRDs in the same PR; external users on `v1alpha1` will need to migrate. Migration is mechanical: `knowledgeBases[i].ref.name` → create a `KnowledgeBaseClaim` and reference it from `knowledgeBaseClaims[i].name`.
- BM25 quality is lower than a real vector index. This is fine for the built-in "just works" tier; users needing semantic search continue to pick `pgvector` / `qdrant`.
- The KB service adds one more binary and one more optional Helm deployment. Kept opt-in via KB class selection.

## Alternatives Considered

1. **Direct-ref stays, add access-mode fields to `KnowledgeBaseRef`.** Rejected: still no separation between curator and app team, and admission hooks for quota still have to reach into the Agent object to see who is using what.
2. **`KnowledgeBaseClaim` as strict `MemoryClaim` mirror (provisioning-style).** Rejected: it forces one KB per claim and duplicates corpora, which does not match how knowledge bases are actually used.
3. **Hash-embedding + cosine as the built-in "vector" provisioner.** Rejected for the first iteration: adds embedding surface area with no real semantic gain over BM25. Can be added later as a second in-memory mode if we want to teach the vector-search UX without a model service.
4. **Embed the in-memory backend only in apiserver-mode and require external systems for k8s-mode.** Rejected because the user's requirement is that the built-in tier works in both modes.
