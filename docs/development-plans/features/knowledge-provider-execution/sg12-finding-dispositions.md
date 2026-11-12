# SG-12 Exact Finding Dispositions

- Status: SG-12 CLOSED WITH EXACT OWNER-APPROVED DISPOSITIONS for the recorded local candidate. SG-13 subsequently closed locally by user scope revision on 2026-10-02; remote publication remains frozen.
- Review date: 2026-10-01.
- Scope: exact local candidate reviewed under [SG-11](sg11-local-acceptance.md), not production activation or publication approval.
- Candidate tree: `1a2fd85548628e3828f6703d24d9024d8d3751fa`.
- Candidate archive SHA256: `307b56d259ccc8b54b3ca7d1d52d974ac36f380da7e139cd9207b2a2e40d3054`.
- Proposal SHA256: `54a439a523eba9afed3690d03b9aacd465c73b5a2ca5df3ccb0e3b388a3d22bb`.
- Evidence root: `%LOCALAPPDATA%/Temp/kognition-sg11-20261001-producers`.
- Exact occurrence ledger: `sg12/disposition-proposal.json`; immutable original inventory: `sg12/finding-inventory.json`.

The ledger binds every occurrence to its raw report hash, source hash, full finding
hash, result/finding index, profile and candidate hash. It contains 162 occurrences
across six scan inputs, not 162 unique defects. Raw reports are retained in
`sg12/reports-sg12-closure/`; zero secrets were detected. All raw scans and strict
SQL disposition commands retain exit 1 because findings remain visible.

## Approved Decisions

| Findings | Occurrences | Approved disposition and exact scope |
| --- | --- | --- |
| KSV-0056 HIGH | 24 | Accept required controller Service/NetworkPolicy mutations in the rendered namespace Roles, with independent admission and tested Cilium controls. Controller compromise still has authority inside those dedicated namespaces. |
| KSV-0048 MEDIUM | 36 | Accept required controller Pod/Deployment/Job lifecycle mutations, constrained by independent workload/attachment admission and no-skip producer acceptance. Managed-workload disruption remains possible. |
| KSV-0049 MEDIUM | 48 | Accept controller/platform state and quota writes plus the specific gateway audit/ledger Roles. Dedicated tool and memory-policy gateway identities are not covered by controller/platform-only ConfigMap admission: tool gateway compromise can create/update installation ConfigMaps, and memory-policy gateway compromise can create them. |
| KSV-0113 MEDIUM | 16 | Accept named installation Secret GET and enrolled-namespace runtime credential lifecycle under ADR-0016. Admission cannot protect Secret reads; compromise can expose the enrolled credential domain and entire named installation Secrets. |
| KSV-0109 HIGH | 5 | Existing verified false positive for the exact static SQL payload and metadata. The automated exception is unchanged. |
| KSV-01010 MEDIUM | 5 | Separately approve the exact migration-021 false positive: schema/epoch/version metadata and JSON updates contain no cryptographic key material. This is not a new automated rule exemption. |
| KSV-0125 MEDIUM | 24 | Accept the exact upstream/local image references for local review. BusyBox/pgvector chart images are digest-pinned; mutable sample tags and local migration tags still require separate immutable-image and registry approval before production. |
| KSV-0020/KSV-0021 LOW | 2 | Accept UID/GID 1000 only for the disposable S3 emulator with nonroot, read-only root, dropped capabilities, seccomp and temporary storage. Host-mounted/shared-identity or production use is not covered. |
| DS-0026 LOW | 1 | Accept absent Docker HEALTHCHECK only for the short-lived provider test fixture, whose mTLS indexing, completion and cleanup are exercised by SG-11. It is not a production workload. |
| GO-2026-5932 UNKNOWN | 1 | Approve not-affected determination for x/crypto v0.56.0: OpenPGP is absent from this candidate's imports and reachable calls. New imports, build targets or advisories require reassessment. |

## Supporting Evidence

- SG-11 closure, full integration/race results and no-skip producer acceptance are hash-bound in the ledger. Policy typechecking passes on actual Kubernetes 1.30.3 and 1.31.1 control planes; producer isolation acceptance uses Kubernetes 1.31.1/Cilium 1.17.8.
- The SQL verifier confirms canonical payload `01fadf1322da74e76e78787f0e2113580682b1352e6366605339564531da42e6` for every rendered migration ConfigMap. Migration 021 defines durable key-state metadata, not embedded keys.
- The exact dependency graph and govulncheck output show no OpenPGP import or reachable vulnerability. This is not approval of all x/crypto advisories.
- Required mutation permissions are not removed to make scanning green. The proposal acknowledges residual credential, state-integrity, availability and supply-chain risks instead of claiming the controls eliminate them.

## Original Approval Conditions

1. Approve only the exact proposal digest and 162 hashed occurrences, not a path-wide, rule-wide or future-candidate exception.
2. Scope is local candidate review only. Production deployment, controller activation, cloud acceptance and SG-13 publication remain unapproved.
3. Dedicated Kognition namespaces must contain no unrelated workloads or credentials. The complete enrolled Secret domain is inside the accepted trust boundary.
4. Active stdio requires the tested administrator-owned Cilium profile. Generic NetworkPolicy and inactive/default scan renders are not equivalent runtime acceptance.
5. Gateway ConfigMap writer risks above are explicitly retained. Do not claim controller-only admission protects separate gateway identities.
6. Mutable sample tags and local image placeholders are not production provenance. Release-image identities, registry trust and public sample credentials need separate deployment review.
7. Preserve raw reports and the strict SQL verifier. Changed findings, sources, reports, evidence or candidates require new review; scans expire after 24 hours.
8. Closure documentation is a post-validation record. SG-13 must still reconcile the final staged tree with tested and scanned inputs.

## Owner Decision

The workspace user explicitly self-attested authorization as installation security
owner and selected: "I am authorized; approve these exact local dispositions."
This decision approves only the proposal digest above and its stated conditions.
The approval is a recorded human decision, not cryptographic identity verification.

The unchanged proposal remains in `sg12/disposition-proposal.json`; its
`proposed-*` labels describe the pre-approval document. The separate
`sg12/owner-approval.json` receipt records the decision. No raw findings were edited.

## Closure Validation

- `sg12/verify-dispositions.ps1` verified six reports and all 162 occurrences against candidate, report, input-tree, source-file, full-finding and control-evidence hashes, plus scan freshness and the exact owner approval.
- Ten negative controls passed: missing approval, wrong candidate, wrong proposal hash, expired scan, changed/extra/missing finding, detected secret, wrong report hash and source drift. Results are in `sg12/disposition-validator-tests.json`.
- `sg12/closure.json` records validation exit 0. Raw scanner exits remain 1 and the unchanged strict SQL verifier still exits 1 for other findings. Closure is approved disposition, not a clean scan or broader automated SQL exception.
- The earliest report freshness limit is **2026-10-02 22:55:53 UTC**. Re-run scans and review any changed occurrences before relying on this evidence after that time. Candidate or configuration changes require affected revalidation even before expiry.
- This status record was written after the original validation and was not part of that frozen candidate. The subsequent local-commit validation below reconciled the staged tree with refreshed evidence. Production activation and deployment remain unapproved.

## Subsequent Local Closure

- SG-13 validated local commit `261c5038f1c578bdc493b5bd4cb5fb7160fc0b88`, tree `bc818b4178b2b3cdf1cb90c9c2eeb99b28eddfbf`, archive SHA256 `5eee79832a65e72b3e54873393f5c6af9da6dc1008cd6615b002d75a9ecb821e`. Executable content is identical to the original candidate; only four reviewed security/status documents differ.
- Fresh six-profile scans retain the same 162 exact findings and source hashes. The owner explicitly approved renewed local-only proposal `81e075cbbc57b5251d743789632d63f1bbb0d57dac3da11d3c20a17aab34785d`. Evidence is under `%LOCALAPPDATA%/Temp/kognition-sg13-local-20261001/sg12`; the renewed freshness limit is **2026-10-03 01:50:13 UTC**. Raw scanner failures and all residual-risk restrictions remain.
- On 2026-10-02 the user instructed **"you can close it, even without the push."** SG-13 is therefore closed on its verified local commit. Push and remote-SHA verification are outside that revised closure and were not performed. The push freeze remains in force; no production or remote publication approval is granted.
- This administrative scope/status update is separate from the attested commit and does not recertify later working-tree changes. Historical scan and approval receipts remain unchanged.