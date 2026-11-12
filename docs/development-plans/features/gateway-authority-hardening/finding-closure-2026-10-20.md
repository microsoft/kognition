# Gateway State Finding Closure

- Status: closed for the exact isolated local candidate, with verified repairs and exact owner-approved residual dispositions.
- Date: 2026-10-20.
- Baseline: `2ce5c52b718cb446376c8df7275e3c5710d9baf8`.
- Verified executable proposal tree: `9212a9b2e479f083194727f137d17d512e9c549a`.
- Exact disposition proposal SHA256: `b8de8eedcf80f16fa85da29f151bc462e77b434cc6aff572285573c473f17f08`.
- Scope: deterministic local gateway-state validation. Production activation, publication, cloud/paid-provider acceptance and remote CI are not approved.

## Repaired Vulnerability

A gateway credential with installation ConfigMap update permission could modify
the mutable migration ConfigMap because the native state policy did not match
that identity. The migration Job would later execute the altered SQL. The
isolated API regression reproduced this path before repair.

The repair addresses the authority boundary:

- The mandatory native state policy covers every enabled Tool, Memory Policy,
  Communication and Model Gateway writer. Disabled identities remain outside
  the match, and enabled identities cannot alias controller/platform authority.
- Gateway writes are limited to their exact enrolled ledger names, ownership,
  namespace annotations and payload keys, or bounded immutable audit/cost
  records. Both old and new objects are checked. Migration, unrelated,
  Helm-owned, unowned and other-plane state cannot be adopted or deleted.
- Tool and Communication now use separate receipt ledgers, selected by their
  fixed process component. A stolen credential cannot mutate the other
  gateway's receipts. Model accounting locks are restricted to installation
  usage-lock names.
- Legacy shared receipts remain read-only to gateways. A matching terminal
  receipt returns its saved outcome without redispatch. Pending, prepared,
  ambiguous, malformed or unavailable legacy evidence fails closed. No
  reservation is refunded or execution owner reassigned during migration.

The existing nine policy/binding pairs remain mandatory. No required runtime
operation was disabled, no broad privilege was added, and the governance chart
archive remains byte-identical. The core archive matches its updated source.

## Verification

| Gate | Actual Result |
| --- | --- |
| Reproducing regression | Gateway updates/patches/deletion of the actual rendered migration ConfigMap and unrelated creation succeeded before repair under permissive fixture RBAC. Native admission now denies them. |
| Kubernetes 1.30.3 native API | 333 test/subtest results pass with zero failures or skips, including every gateway identity, foreign-state/adoption negatives, real audit/cost/receipt helper positives, read-only legacy protection, activation guards and chart parity. |
| Runtime regression | Eight complete affected gateway/store/audit/cost/activation race suites pass. Legacy success/failure replay, unresolved-state fencing, missing ownership/payload and unavailable authoritative reads are covered. |
| Actual Kubernetes 1.34.11 assembly | All nine policies reach their observed generation without warnings. The gateway-state and webhook boundaries pass independently of fixture RBAC. Tool, Memory store/delete and Communication races/replay each produce exactly one synthetic provider effect. |
| Quality/security | Matching Go 1.26.6 correctness lint, reachable Go vulnerability audit, strict scan-verifier regressions and source secret scanning pass. |
| Evidence lifecycle | All fifteen indexed deployment artifacts remain byte-exact after cleanup through a disposable kubeconfig copy. The failed pre-workflow fixture was separately reviewed and removed with explicit owner approval. |

Successful run: `7f430deb-63de-4bb9-bb8f-c66668aaa408`; cluster UID:
`9fe9f5ac-3545-49e1-90db-33d12b14eebd`. Cleanup completed at
2026-10-20T21:11:01Z.

| Retained Evidence | SHA256 |
| --- | --- |
| Deployment evidence index | `579bbecbc7ffa40dcefd94f44dce37d74e980eca8bc30aaedce033c3306b7fa8` |
| Deployed workflow log | `35f31bec2dd87a28f63e9c4c42da86fc29a5647da57e42375f465e10f9eb34d8` |
| Deployed source inventory | `e4ec727c6c3d0c93d9e96814279e826d75be7826c90b8bd7ade17fd8d9af8b93` |
| Finding/source comparison | `bd23b7448fc16b876f8874473baa12bc3389dd55d9cd497cd9c54e585e886964` |

The first deployed attempt failed before workflows because the test helper
required Helm inside the node. It now consumes the exact already-rendered
migration artifact. That attempt remains a failure in its retained record and
was never replayed. The earlier Docker-unavailable attempt created no node.
Existing clusters received no acceptance fixtures; the older reviewed
Communication claim remains unchanged.

## Exact Scanner Dispositions

Fresh Trivy 0.74.0 scans cover bootstrap, activated-values,
configured-TLS-values and packaged-bootstrap, with all gateway writer
identities enabled. Each has 23 unchanged raw occurrences: three HIGH, no
CRITICAL and no detected source secrets. All 92 exact finding/source
occurrences match the fresh baseline. The checks bundle digest is
`sha256:b591e9190cff4aa4e385b1a25bccfd9ecf178d3c77fd194e1410b7ce7e1df058`.
The larger identity profile is not equated to the earlier 21-finding renders.

| Rule | Four-Profile Occurrences | Verified Disposition |
| --- | --- | --- |
| KSV-0049 | 44 MEDIUM | Arbitrary gateway installation/migration writes are repaired. Required controller/platform state and gateway audit/ledger permissions remain, constrained by tested native enforcement. Permitted-state forgery, installation ConfigMap reads and storage consumption remain explicit writer trust. |
| KSV-0056 | 8 HIGH | Required controller routing/isolation authority is constrained by namespace Roles and independent Service/NetworkPolicy admission. Managed-namespace availability and administrator-controlled network changes remain local-profile residuals. |
| KSV-0048 | 12 MEDIUM | Required managed-workload lifecycle is constrained by workload/attachment admission. Compromised controller disruption remains possible; active stdio/Cilium qualification is outside this local approval. |
| KSV-0109 | 4 HIGH | Exact static SQL false positive. The unchanged strict verifier confirms the 34-key payload and metadata, SHA256 `fdfc909439a0319018e2faa7a11b43548940068fd833aeee053e1d29c0baed0d`; no embedded credentials are present. |
| KSV-01010 | 4 MEDIUM | Exact migration-021 key epoch/version/material-digest metadata in that same verified payload. It contains no cryptographic key material. No automated rule exemption was added. |
| KSV-0113 | 8 MEDIUM | Required named installation Secret reads and enrolled credential lifecycle. Admission cannot protect Secret GET; the allowed Secret values remain inside the trusted credential domain. |
| KSV-0125 | 12 MEDIUM | Only the exact local/pinned image references are accepted for isolated validation. Production registry trust, immutable publication and provenance remain separate requirements. |

The owner self-attested authorization and selected **"I am authorized; approve
these exact local dispositions"** for the proposal hash above. This is a
recorded human decision, not cryptographic identity verification or inheritance
of an older approval.

The local validator verifies every report, source, input, control receipt,
candidate and occurrence against that exact approval. All 92 occurrences pass;
thirteen negative controls reject wrong approvals/candidates, missing or extra
findings, source/control tampering, stale scans, secret findings and broader
clearance. Raw reports and strict SQL gate exits remain 1. No finding was
suppressed or rewritten.

## Rollout And Closure Limits

Use the existing writer freeze and guarded activation procedure for an upgrade.
Old gateways must not continue writing shared receipt ledgers while new
credentials/policies are installed. Preserve legacy records and unresolved
claims for reviewed recovery; never delete them to make new execution succeed.

This closes the code defect and the exact local finding dispositions. A new
candidate, changed source/finding, expired scan or production target requires
affected revalidation and its own decision. Documentation-only checkpoint
changes are separately reconciled with the verified executable proposal.
Neither a clean scanner exit nor production/remote-CI approval is claimed.
