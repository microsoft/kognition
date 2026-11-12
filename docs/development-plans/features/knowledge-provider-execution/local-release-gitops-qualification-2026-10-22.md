# Local Release And Inactive GitOps Qualification

- Date: 2026-10-22.
- Kubernetes AutoGen TLS follow-up: 2026-10-23.
- Scope: reviewed local artifacts and isolated inactive acceptance.
- Release security, external publication and activated workflow acceptance remain open.

## Verified Milestones

| Milestone | Evidence |
| --- | --- |
| Release matrix | 18 OCI indexes and 36 Linux AMD64/ARM64 platforms; actual ELF/build settings, SBOM and provenance subjects verified |
| Python runtime compatibility | 834 image-owned tests passed with no failures or skips; all six non-root/no-provider startup checks passed |
| Runtime dependencies | AutoGen AgentChat/Ext 0.7.5 and explicitly selected Pillow 12.3.0; installed build-only pip removed |
| Inactive GitOps | Real read-only local Git and pinned Argo Core; 101 declared objects reached Synced/Healthy without hosted CI |
| Guarded preparation | Production safety executable verified 49 exact live objects and three image references with both writers stopped |
| Native admission | All nine current-generation policies were warning-free; genuine native policy probes and ten authority-boundary denials passed |
| Migration retention | Same completed Job UID survived 630.98 seconds beyond the previous 600-second cleanup threshold |
| Native producers | Real attachment/concurrent streams, additive-allow and policy-replacement isolation, stalled-guard denial and production stdio success/deadline cleanup passed |
| Native ingestion | Three restart/reset attempts across generations 1/2 persisted a document/chunk in isolated pgvector and verified sandbox/finalizer cleanup |
| Isolated Agent/provider follow-up | A separate disposable canonical fixture passed real authenticated native dispatch, Service revisions, credential rotation/deletion and SDK mTLS provider binding/finalization under race detection |
| Kubernetes AutoGen TLS | Generated Linux/AMD64 workers passed real Service-DNS TLS 1.3, trust/name/authentication denials, one signed model turn, same-generation retry and durable terminal recovery under race detection |

Runtime implementation modules executed from the images, not a mounted replacement.
The native producer exercise used synthetic local data, deterministic embeddings
and UID-preconditioned cleanup. It did not execute AutoGen or an external provider.

## Exact Historical Identities

| Identity | Value |
| --- | --- |
| Runtime image source tree | `b3ffac5e4fb9fe835d06537122da72b55fb60dd2` |
| Qualified operator/chart source tree | `8cbdc73936b6d6b015764ae3342e6d943118832e` |
| Local Git source revision | `4753ed843189fc051b8ebc1f8991f92a5cb3a6f9` |
| Cluster UID | `52331b50-1a65-4f7f-be5c-57c92a1bf2f1` |
| Argo-rendered manifest SHA256 | `b78a045df2358a15be758c1bd55120c3c4b492a05b7ce8427ab4494fbc771aa8` |
| Chart archive SHA256 | `055d0e11c139b11143547209c237bb5884e13e6b49ac4e27b063cdaf0e9ac2c8` |
| Qualified safety executable SHA256 | `c717c9daf2a2cd8d2dbff9cdfbba0f02778479ecb36ebcb355de929f2ee55d7f` |

These identities describe the measured snapshots, not every later working-tree
edit or commit. The native adversarial drill replaced the baseline NetworkPolicy
UID and changed the guard generation while restoring intended contents. The
pre-commit review subsequently added malformed-inventory rejection and regression
coverage. Rebuild and prepare the exact final executable and target before any
approval; do not reuse historical target identities or acceptance freshness.

Retained session evidence has these SHA256 identities:

| Receipt | SHA256 |
| --- | --- |
| Complete image artifact verification | `f8271f86cedae4e1fbfeaba70f68199b9ed0c54fe43eca440cc9c8b8baf671a2` |
| Release qualification | `ff6d43de77f27cb60100e72ad33a54f5997655cd0c5cb9e86b716e36f890e468` |
| Inactive GitOps qualification | `ca0deed39ed78666440c6cb1b56b9f209ccc45f8390466c64be960a193d69de5` |
| Native producer qualification | `d1568c3e1383857cc129f7a814d37fae86b2e60ecc3de8d7b3592c9e7746cec0` |
| Isolated Agent/provider qualification | `d56054efccc0b5a046025389f799819227010b876d9d967cbd1d90822c3e9380` |
| Kubernetes AutoGen TLS qualification | `9ba1311803472a96a7070543e5fbeba03eb5d6ffa24398c034939245610f9707` |

Raw reports, private kubeconfigs, executable/archive exports, serving keys and
one-shot environment provisioning state are not repository content. Protect and
retain the original evidence separately. Hash references do not supply signatures
or production acceptance.

## Deliberate Remaining Gates

- Pillow stays exactly 12.3.0 by owner constraint. Its known findings and other
  distribution-image findings remain open; no waiver or production clearance was
  granted. Image scans describe their recorded database/date, not future CVEs.
- Full fresh SG-03/07/08/09 evidence and explicit exact-target owner approval are
  still required before activation, active GitOps updates or transaction recovery.
- The isolated Agent/provider and Kubernetes AutoGen TLS follow-ups remain local
  fixture evidence. Native control used authenticated HTTP; AutoGen used verified
  Service-DNS TLS 1.3. Activated-controller restart, active workflow/recovery and
  exact-target activation acceptance remain open. The installation's immutable
  shared source Secret was not rotated or deleted.
- Argo's out-of-band writer RBAC and append-only admission audit ConfigMaps were
  retained, not pruned or hidden to clear an orphan warning.
- Concurrent Azure/Foundry and Memory-provider edits were excluded from the frozen
  qualification. No shared cluster or older retained Communication claim was
  modified or replayed.
- External publication and GitHub execution remain deferred.

Use the [pipeline-free procedure](../../../../deploy/GITOPS.md) and the
[activation contract](../../../features/knowledge-provider-execution/activation-transaction-contract.md). Successful source
review, local commits, rendering, Pod readiness or partial acceptance never
replace the remaining gates.

## Pre-Commit Review

The scoped review found and fixed a malformed-inventory panic in the Cilium API
validator. A public-interface regression reproduces the original panic and now
requires explicit rejection before any cluster access. Additional release/profile
confusion cases bring the hostile-policy matrix to 20 variants.

All 93 Go packages were accounted for under race-enabled validation: 79 test
packages passed and 14 contain no tests. Cold Windows instrumentation exceeded
the aggregate command deadlines, so the remaining packages ran in bounded batches.
The timeouts are retained as failed attempts, not counted as passes. Combined
authored statement coverage is 80.2%, exceeding the unchanged 80% repository gate.
The new API policy helpers have 100% and 94.1% function coverage.

The reviewed candidate also passed all-package build/type-check, affected-package
correctness lint, module checksum verification, current Go vulnerability audit,
zero-finding secret scans, six exact-image checks plus three rejection controls,
497 core chart checks, 28 adapter chart checks and all three chart lint/renders.
This review does not remove the explicit remaining gates above or establish an
absolute guarantee that every possible bug is absent.

## Isolated Agent And Provider Follow-Up

The canonical combined lifecycle test passed in 193.22 Go-reported seconds
(207.90 seconds wall time), with zero failures, skips or race reports. It ran on
separate cluster UID `fcac94f9-754d-4a53-a1fe-ce658dbb5f24`, Kubernetes 1.31.1 and
the same pinned Cilium 1.17.8 profile. Its dedicated mutable source credential
existed only in its disposable installation namespace.

- Real dispatch/status completed through the native runtime and local model.
- Service revisions and finalizer cleanup settled with exact ownership.
- Source rotation preserved copied Secret identity; source deletion blocked new
  revision creation while retaining already materialized credentials.
- The real SDK provider used mTLS, exact scope/configuration/binding identities
  and a confirmed Delete cleanup for one document/chunk.
- Both temporary namespaces, grants, admission objects and producer CRDs were
  confirmed absent before deleting the temporary cluster and network.
- The original immutable source's UID, resource version and data fingerprint,
  eight core CRD identities/versions, and stopped primary writers were unchanged.
  A fresh audited prepare passed afterward.

An initial attempt on the existing qualified cluster safely refused global CRD
collisions and then correctly hit its unenrolled-namespace webhook permissions.
No webhook scope, read grant or admission setting was weakened. The temporary
shared-cluster workaround was removed; the owner approved full cluster isolation.

The provider fixture now uses the reviewed Go 1.26.10 base digest. Its previously
ignored Dockerfile-specific build context is tracked so clean source exports
include the provider. The real missing-context build failure and pinned-toolchain
red/green regression were retained. Final traceability race tests passed, integration
lint reported zero issues, and source-secret and provider Go vulnerability audits
passed. No cloud provider, hosted CI, Pillow upgrade or activation was involved.

## Kubernetes AutoGen TLS Follow-Up

The full canonical producer bundle passed on 2026-10-23 in 153.26 Go-reported
seconds (173.13 seconds wall time), with zero failures, skips or race reports.
The measured harness tree was `918445e392a3013e510cdb5bf160cb9d893f9b5f`; its actual
Linux/AMD64 ELF and Go 1.26.10 race/CGO build settings were verified. It used
separate cluster UID `307fd017-0971-4c03-a546-15376b517b0c`, Kubernetes 1.31.1 and
the same pinned Cilium 1.17.8 profile.

- Production Kubernetes Dispatch reached generated workers through their actual
  Service DNS names, without port-forward or Go-transport overrides.
- Correct trust negotiated verified TLS 1.3. Wrong CA and Service identity,
  missing/invalid bearer credentials and plaintext HTTP failed closed before
  model effects.
- The image-owned AutoGen AgentChat/Ext 0.7.5 runtime retained Pillow 12.3.0,
  non-root identity, read-only TLS delivery and no API-token automount.
- The actual SDK request carried the exact signed model-session binding and made
  one synthetic local model call with 11 prompt and seven completion tokens.
  Retrying the original Pending generation preserved its session and effect count.
- A fresh dispatcher recovered the terminal result from the real encrypted
  Kubernetes ConfigMap backend after worker removal. Native admission denied
  administrator deletion of the ledger; its UID/terminal phase remained intact
  until normal teardown of its owning disposable namespace.
- Existing ingestion, non-root-failure, mTLS provider and native Agent lifecycle
  children also passed. Final affected race regressions passed in 79.169 seconds,
  integration lint reported zero issues, and offline source-secret scanning found
  zero secrets. These checks do not grant dependency/license or release clearance.

The private runner joined only the disposable node's network namespace and used
a container-private LF-only cluster resolver. Host/node resolvers and admission
permissions were unchanged. Required fixture objects were independently confirmed
absent before deleting the exact owned cluster, node and network. Original source
credentials, eight core CRDs and stopped writers were identical afterward; audited
read-only preparation passed. Failed attempts and prerequisite timeouts remain
retained as failures, separately from the successful qualification.
