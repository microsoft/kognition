# ADR-0002 — Default `KnowledgeBaseClass` via annotation

- **Status:** Accepted
- **Date:** 2026-07-03
- **Deciders:** Kognition core team
- **Related:** [ADR-0001 — KnowledgeBaseClaim + built-in in-memory KB provisioner](0001-knowledge-base-claims.md)

## Context

ADR-0001 introduces a built-in `KnowledgeBaseClass` named `kognition-inmemory` (provisioner `kognition.dev/inmemory-bm25`) so that a `KnowledgeBase` can be created end-to-end without any external dependency. Every `KnowledgeBase` still has to name a class explicitly:

```yaml
apiVersion: kognition.dev/v1alpha1
kind: KnowledgeBase
metadata: { name: onboarding-docs, namespace: acme }
spec:
  knowledgeBaseClassName: kognition-inmemory   # required today
  sources:
    - type: inline
      data: "hello world"
```

Two annoyances with the current shape:

1. **Getting-started friction.** Every quickstart, every sample, every generated manifest carries a `knowledgeBaseClassName` field even when the user has not yet formed a preference. The value ends up being cargo-culted rather than chosen.
2. **Cluster/tenant policy has no expression.** Operators frequently want *"in this cluster, `KnowledgeBase`s default to `pgvector-prod` unless the author says otherwise"* — a preference that lives on the class, not the KB. Today the only way to express it is convention plus code review.

Kubernetes solves the same problem for `StorageClass`, `IngressClass`, and `RuntimeClass` with a **default-class annotation** on the class:

- `storageclass.kubernetes.io/is-default-class: "true"`
- `ingressclass.kubernetes.io/is-default-class: "true"`

An admission plugin fills in the class name on the consumer resource when it is omitted, and rejects a second class carrying the annotation. Nothing about the class type itself changes — the annotation is the whole contract.

## Decision

Adopt the same pattern for `KnowledgeBaseClass`, expressed as a well-known annotation and enforced by the existing admission chain.

### 1. Annotation

```yaml
apiVersion: kognition.dev/v1alpha1
kind: KnowledgeBaseClass
metadata:
  name: pgvector-prod
  annotations:
    knowledgebaseclass.kognition.dev/is-default-class: "true"
spec:
  provisioner: kognition.dev/pgvector
  # ...
```

Key: `knowledgebaseclass.kognition.dev/is-default-class`. Value: the string `"true"` (any other value, including `"false"` or absence, means *not default*). This mirrors the `storageclass.kubernetes.io/is-default-class` shape exactly, including the string-typed boolean.

### 2. Defaulter behaviour

The mutating admission plugin for `KnowledgeBase` (already present in `internal/admission/`) gains a step:

- If `spec.knowledgeBaseClassName == ""`, list `KnowledgeBaseClass` objects, pick the single one whose annotation equals `"true"`, and set `spec.knowledgeBaseClassName` to its name.
- If zero classes carry the annotation, leave `spec.knowledgeBaseClassName` empty and let the validating step reject the object with a clear message: *"knowledgeBaseClassName is required and no default KnowledgeBaseClass is configured (set annotation `knowledgebaseclass.kognition.dev/is-default-class: \"true\"` on one class)"*.
- If two or more classes carry the annotation, refuse the write on the *class* side (see #3) so that we never silently pick one at defaulter time.

Defaulting runs against both apiserver-mode (standalone store) and k8s-mode (real cluster) because the admission chain is shared.

### 3. Uniqueness enforcement on the class

The `KnowledgeBaseClass` validator refuses a create/update if the write would leave more than one class annotated `"true"` cluster-wide. The error message names both the incoming class and the existing default. This matches how `storageclass_admission_controller` guards against multiple defaults.

The check is intentionally *at write time only* — a pre-existing multi-default state (e.g. from a botched Helm upgrade) surfaces on the next class edit but does not brick reads. The defaulter, if it ever encounters a multi-default state, logs a warning and returns an error to the KB write so the caller can pick explicitly.

### 4. Scope: `KnowledgeBase` only

`KnowledgeBaseClaim` references a `KnowledgeBase` by name in the same namespace (ADR-0001), not a class. It has no `knowledgeBaseClassName` field, so this ADR does not touch claims. The default-class contract is strictly between `KnowledgeBase` and `KnowledgeBaseClass`.

Similarly, this ADR does not introduce a *default `MemoryClass`* or a *default `ToolClass`*. Each of those can adopt the same annotation shape in a later ADR if the pain surfaces — the pattern is deliberately keyed by resource kind (`knowledgebaseclass.kognition.dev/is-default-class`, `memoryclass.kognition.dev/is-default-class`, …) rather than a shared `kognition.dev/is-default-class` so that a cluster can carry independent defaults per resource family, matching how k8s scopes its own annotations per class kind.

## Consequences

**Positive**

- Quickstarts and samples can drop `knowledgeBaseClassName` when they want to say *"pick whatever the cluster considers default"*. Especially useful for `kognitionctl` scaffolding, which currently has to guess.
- Operators express cluster-wide policy in one place (the class object), not in every KB manifest.
- Existing manifests that name a class explicitly keep working unchanged — defaulting only fires when the field is empty.
- Zero new fields on any spec; the entire contract lives in a namespaced-ish annotation, which is what k8s users already recognise.

**Negative / risks**

- Default-class annotations are famously easy to lose track of. Mitigated by (a) `kognitionctl get knowledgebaseclass -o wide` calling out the default (planned) and (b) refusing to create a second default.
- Users who omit the class field on purpose in one cluster may be surprised when the same manifest fails in a cluster with no default configured. Mitigated by the specific validation error message pointing at the annotation to set.
- Splitting the enforcement between the KB defaulter (fill in the name) and the class validator (guard uniqueness) means two code paths must stay in sync about which annotation is authoritative. Constant is centralised in the admission package to avoid drift.

## Alternatives Considered

1. **Ship `kognition-inmemory` as the hard-coded default in code.** Rejected — it would make the built-in class privileged in a way the pluggable-provisioner model rejects, and operators could not swap the default without shipping a patch. The annotation lives on the object, so any class can be the default without code changes.
2. **New field on `KnowledgeBaseClass.spec`, e.g. `default: true`.** Rejected — the k8s ecosystem has settled on the annotation form for exactly this case, and using a spec field would prevent adding the same pattern to `MemoryClass` / `ToolClass` later without introducing an inconsistent CRD field shape family. Annotations are also easier to toggle with `kubectl annotate` and don't require CRD schema changes.
3. **Namespace-scoped default via a `KnowledgeBaseDefaults` object.** Rejected as premature — cluster-wide default matches every user request seen so far, and per-namespace defaults can be layered on later (either as a second annotation on a `Namespace` object, or as a `KnowledgeBaseClaimTemplate`) without breaking the cluster-wide behaviour introduced here.
4. **Do nothing; keep `knowledgeBaseClassName` required.** Rejected — the pain is real enough (see #Context, item 1) that samples in the repo already misrepresent user intent, and the fix is small.
