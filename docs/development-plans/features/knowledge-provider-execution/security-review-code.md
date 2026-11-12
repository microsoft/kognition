# Knowledge Provider Execution Security Review

- Mode: code, scoped to CPI query/index/cleanup transport, session-authorized gateway, Kubernetes bindings, provider lifecycle, Azure AI Search and pgvector retrieval.
- Status: local checks cover the controls below; live Azure acceptance and deployment security assessment remain incomplete.
- Related contract: [Knowledge providers](../../../knowledge-providers.md).
- Remaining implementation and acceptance work: [Security gate backlog](security-gate-backlog.md). The [October 20 gateway-state closure](../gateway-authority-hardening/finding-closure-2026-10-20.md) repairs the remaining gateway migration-write path and closes exact local scanner dispositions with fresh owner approval. Raw findings, stated residual trust and separate production/remote-CI gates remain explicit.

## Current Local Gate Decision

### Gateway State Closure, 2026-10-20

The [exact closure record](../gateway-authority-hardening/finding-closure-2026-10-20.md)
supersedes the open local finding disposition below. Every enabled gateway
ConfigMap writer now has independently enforced state boundaries. Tool and
Communication receipt ledgers are isolated, and legacy unknowns fail closed
without replay or refund. Minimum-version API tests and the actual deployed
assembly pass.

Fresh all-writer profiles retain 92 raw finding occurrences across four scans.
The strict SQL verifier confirms the exact schema false positive; the user
separately approved proposal
`b8de8eedcf80f16fa85da29f151bc462e77b434cc6aff572285573c473f17f08`
for executable proposal tree `9212a9b2e479f083194727f137d17d512e9c549a`.
The per-occurrence validator and thirteen tamper/staleness/secret negative
controls pass. This is exact local closure with stated residuals, not scanner
suppression, production approval or current remote-CI success.

### Webhook Audit Creation Boundary, 2026-10-19

Verdict: **local mitigation verified; release clearance not granted**.
The owner selected independent native enforcement rather than a residual-risk
waiver. The preceding gateway-authority work is preserved in local checkpoint
`f9e17d060d4fa3baf1c566347e9b7671b893f722`; nothing was pushed.

When resource webhooks are enabled, the existing mandatory `-state` policy
matches their dedicated identity in every namespace. Its additional validation permits only CREATE in
the installation namespace, with all of these requirements:

- Exact `kognition-audit-` plus 32 lowercase hexadecimal name.
- Immutable data with the existing audit/workspace labels and optional hashed
  agent labels; no unrelated labels, annotations, owners or finalizers.
- Only `binaryData["entries.json"]`, with a nonempty payload of at most 900 KiB.
  Native admission measures its base64 text; the encoded bound is 1228800 characters.
- No plaintext data, alternate payload keys, update, patch, deletion, index-marker
  creation or cross-namespace creation.

The webhook's RBAC grants remain unchanged. The policy does not depend on the
webhook process to enforce its own restrictions. Activation rejects an omitted
or renamed state policy/binding; the inventory remains nine pairs. The core
chart archive matches source, and the governance archive bytes are unchanged.

| Verification | Actual Result |
| --- | --- |
| Red regressions | Reproduced arbitrary ConfigMap creation under the webhook identity, mutation under permissive fixture RBAC, and acceptance of a substituted state policy pair. |
| Kubernetes 1.30.3 API/race matrix | 254 test/subtest results pass with zero failures or skips. Production audit append/read/deduplication works; exactly 900 KiB passes and one extra byte fails. Twenty-five create variants plus update/patch/delete require native-policy denial even when fixture RBAC permits them. |
| Activation CLI/API | 28 results pass with zero failures or skips, including built-command prepare/dry-run. Audit and activation package race suites and affected correctness lint pass. |
| Kubernetes 1.34.11 deployment | The same independent audit matrix passes against the actual assembly. All nine policies typecheck without warnings. Tool, Memory store/delete and Communication workflows still pass with real gateway replicas and exactly one synthetic effect per operation. |
| Evidence cleanup | Fifteen indexed artifacts remain byte-exact after removal of the new owned target. Existing clusters and the reviewed older Communication claim remain untouched. |
| Dependency and secret checks | Reachable Go audit exits zero. The proposed source tree and all matched render profiles have zero detected secrets. No dependencies were changed or scanner findings suppressed. |
| Disabled-profile compatibility | A red/green render regression preserves the original namespace-bound platform contract when webhooks are disabled, including a custom platform account with the otherwise reserved webhook name. All eighteen final enabled native policy/binding objects exactly equal the passed deployed objects; that enabled-profile evidence is reused without relabeling its source hashes. |

Deployed run `f0c9d121-b8c5-465a-b554-0aeeeb29ab57`, cluster UID
`ec07e3ef-080c-40c6-97f6-97221e969345`, was removed at
2026-10-19T20:37:47Z after successful acceptance. Its evidence index SHA256 is
`cd0c7d2adf912fca215d6658e9834beb5a4912d34c435d72b6958989aa475a9b`;
the deployed log SHA256 is
`da1c3567bd02ac1a8f61b1417343ee168541f95d80bec3d74b69856513735de8`.

Fresh Trivy 0.74.0 scans compare checkpoint `f9e17d0` with executable proposal
tree `bf221c919a8e6cc9162e872994b260bfaa1b0212`. The checks bundle digest is
`sha256:b591e9190cff4aa4e385b1a25bccfd9ecf178d3c77fd194e1410b7ce7e1df058`.
Bootstrap, activated-values, configured-TLS-values and packaged-bootstrap each
retain 21 findings, including three HIGH and zero CRITICAL; raw exits remain 1.
Rule/status/message and complete named-resource source fingerprints match all
84 occurrences, with no additions or removals. These fresh profiles are compared
with each other, not equated to historical scanner counts. TLS-value rendering
does not certify certificate provisioning or activation.

The required webhook ConfigMap-create grant remains a medium **KSV-0049**
occurrence in each profile. It is not a false positive. Native enforcement
mitigates arbitrary ConfigMap writes, but a compromised webhook can still forge
audit-shaped records, consume storage within permitted bounds, read installation
ConfigMaps and exercise its separately documented decision authority. Names and
labels do not authenticate audit content. Exact target/residual-risk acceptance
and broader scanner dispositions remain open; this control grants no waiver.

Private scan-index SHA256:
`12add094372b5bae8c277f745987a5d7dff9b5868a41d9c15610817091432fa9`.
Finding/source delta SHA256:
`fb28bb5b5775b66f7229d5aa824cbbadbdacb3310dd4bcea47fad72b5715e869`.
Later compatibility and review-document updates preserve the scanned RBAC/source
resources. The disabled-webhook branch has its own regression; enabled native
objects are compared against the sealed deployment before reusing its evidence.

Remote CI has no receipt for the unpushed candidate. The accessible mirror's
latest run is from October 2; its old executable-mode, runbook, webhook-fixture
and Trivy-setup failures are not current-candidate failures. Locally, the trace
CLI is already mode 100755, the fallback contract passes, the built-command
activation tests pass, and CI pins Trivy 0.74.0. The configured origin could not
be resolved through GitHub. No remote was changed and no workflow was triggered.

### Source Publication Delta Review, 2026-10-02

Verdict: **PASSED WITH FINDINGS for source publication**, under the approved
[change-based policy](../../../development-gates.md). This review does not close a
new SG-12 disposition, certify a release, grant production activation, or establish
remote CI success. The user separately authorized pushing source to both remotes.

Reviewed executable candidate: `1e043c594adafe7937d61d27f77a6bbb418cd9cf`, tree
`5180208a8d6bfc89441b058ed169c4250c15e9bf`. Subsequent review-document updates
do not alter its executable or chart inputs. Baseline: local commit `261c5038`.

#### Webhook Authority

| Surface | Current permission and justification | Remaining trust condition |
| --- | --- | --- |
| Identity and startup | Separate webhook ServiceAccount and two-replica process; no reconcilers, dispatchers, retention controller or controller leader election | Webhook availability is required for fail-closed resource admission |
| Kognition resources | Namespaced get/list in installation/enrolled namespaces; cluster class, binding and admission-policy get/list | Resource content is visible to this trusted admission component; cluster policy reads include other namespaces |
| Approval endpoints | TokenReview and exact-target SubjectAccessReview; configured controller/webhook/gateway subjects rejected before authorization | Direct approval-status updates and mounted binding keys make webhook compromise an approval-authority compromise |
| Approval persistence | RuntimeApproval status get/update; named Lease get/update and unrestricted Lease create | Kubernetes RBAC cannot restrict create by resourceNames; compromised identity can create other Leases in enrolled namespaces |
| Audit persistence | Installation-only ConfigMap get/list/create for immutable audit batches; no update/patch/delete | Compromise can create arbitrary installation ConfigMaps or misleading audit records; append-only application behavior is not an RBAC name restriction |
| Native and privilege APIs | No Secret API, native workload, Service, NetworkPolicy, RBAC mutation, impersonation, bind or escalation grants | Mounted TLS/signing Secrets remain sensitive even without Secret API reads; keep authority namespace outside enrollment |
| Activation binding | Candidate snapshot includes resource-admission identity, roles, Service, configuration and image; serving readiness required | Existing exact-target approval, writer freeze and recovery rules remain mandatory |

The new [role/deployment](../../../../deploy/helm/kognition/templates/resource-webhooks.yaml)
uses the production admission chain. The
[decision handler](../../../../internal/k8s/webhook/runtime_approval_decision.go)
authenticates and authorizes before calling the status writer. The
[audit store](../../../../internal/k8s/audit/configmap_store.go) creates immutable,
bounded batches and rejects conflicting duplicate payloads. These application
checks do not independently constrain a compromised process with the same identity.

#### New Finding: KSV-0049

- Severity: medium. Rule: **Manage configmaps**.
- Target: `kognition/templates/resource-webhooks.yaml`, installation Role
  `security-test-kognition-webhook`.
- Four added occurrences: bootstrap, activated, configured-generated-tls and
  packaged-bootstrap. The create verb needed for audit appends causes the finding.
- Status: **OPEN residual finding**, reviewed for source publication; not a false
  positive, not suppressed, and not covered by the historical owner's approval.
- Later mitigation: the October 19 native audit-only creation boundary above
  independently constrains writes. The raw finding and exact release/target
  residual-risk disposition remain explicit.
- Impact: a compromised webhook can create arbitrary ConfigMaps in the
  installation namespace. It cannot update/delete existing ConfigMaps or write
  administrator approval objects in a distinct, unenrolled authority namespace.
- Release/deployment follow-up: obtain the applicable exact owner disposition or
  implement and test an independently enforced audit-creation boundary before
  claiming this new authority is accepted for that target.

The raw finding's CauseMetadata points to nearby Lease lines in the combined YAML
document. The actual ConfigMap create grant is present, so that line attribution
does not justify dismissing the rule.

#### Focused Validation

| Evidence | Result and scope |
| --- | --- |
| Startup, chart, approval authentication/authorization, persisted decisions, binding-key activation, audit, activation and core archive parity | Race-enabled checks in seven affected Go packages; 217 passing test events, zero failures and zero skips |
| Bootstrap render scan | 36 exact baseline finding/source occurrences retained; one added KSV-0049; zero detected secrets; raw exit 1 |
| Activated render scan | 36 exact baseline finding/source occurrences retained; one added KSV-0049; zero detected secrets; raw exit 1 |
| Configured-generated-TLS render scan | 38 exact baseline finding/source occurrences retained; one added KSV-0049; zero detected secrets; raw exit 1 |
| Packaged-bootstrap render scan | 36 exact baseline finding/source occurrences retained; one added KSV-0049; zero detected secrets; raw exit 1 |
| Exact source secret scan | Zero detected secrets for tree `5180208a`; report retained from the planner implementation |

No added high/critical finding was detected in these four rendered profiles.
The 146 retained occurrences have identical finding and source hashes, but their
old candidate-specific approvals were not imported or expanded. Filesystem,
standalone, dependency, external-provider and production certification were not
rerun for this permission delta. Dependency manifests are unchanged from baseline;
existing full CI still runs after source publication.

Raw evidence: `%LOCALAPPDATA%/Temp/kognition-webhook-delta-20261002`, including
the exact candidate archive, four report/provenance pairs, multiset finding/source
deltas and `focused-tests.jsonl`. Archive SHA256:
`bbaad3d8519bfeeda8671a37645ae66d835b0f4d1e5ecd64a1a35752a076ae76`.
Focused-test log SHA256:
`ef0797a8f1ae614c665edf11dc0b8b15cfb180d2b5bcfcf8792d4fafb3d0c67f`.
The comparison records no inherited dispositions and grants no clearance.

### Recorded Baseline Decisions

On 2026-10-01, SG-11 and SG-12 closed for candidate tree
`1a2fd85548628e3828f6703d24d9024d8d3751fa` with
[tested local acceptance](sg11-local-acceptance.md) and
[exact owner-approved finding dispositions](sg12-finding-dispositions.md).
SG-12 is a residual-risk decision, not a clean scan: all raw scanner exits remain
1, the strict SQL exception is unchanged, and 162 exact occurrences are covered
by the recorded approval. Gateway ConfigMap authority, enrolled credential
exposure and local image-source risks remain present under explicit conditions.

This decision supersedes historical statements below that final local findings
had no owner disposition, only for that candidate and the approved proposal hash.
It does not clear SG-13, production activation or the deployment blockers below.
The scan freshness limit is 2026-10-02 22:55:53 UTC; staged-tree changes or expired
evidence require the applicable reruns and review.

Subsequent decision, 2026-10-02: SG-13 verified local commit
`261c5038f1c578bdc493b5bd4cb5fb7160fc0b88`, whose tree is
`bc818b4178b2b3cdf1cb90c9c2eeb99b28eddfbf`. The executable inputs match the
SG-11 candidate; four documentation updates received focused validation and fresh
scans with renewed exact owner approval. That evidence expires at
2026-10-03 01:50:13 UTC. The user then explicitly permitted closing SG-13 without
pushing, so SG-13 is CLOSED under local-only scope. Neither push nor remote-SHA
verification was performed. The push freeze and deployment blockers remain;
local task closure does not authorize production or remote publication.

## Verified Controls

| Boundary | Evidence |
| --- | --- |
| Service authentication | Real TLS tests reject missing client certificates, wrong URI identities, wrong DNS identities and untrusted roots |
| Scope authorization | Required per-query authorizer; stale namespace, KB UID, generation and configuration identity/digest rejected before adapter execution |
| Response isolation | Exact scope/request binding, result count and citation validation on both sides; faulty-provider tests bypass server SDK validation |
| Resource bounds | Message, query/vector/filter, result and deadline limits; cancellation propagated to adapter |
| Error disclosure | Backend and transport error messages/details sanitized while preserving supported status codes |
| Compatibility | Explicit provider name, protocol, configuration kind and required operations; no fallback backend |
| Configuration binding | Allowlisted configuration GVK; lookup in the KB namespace; committed class/configuration UID and digest checks; no Secret lookup |
| Binding freshness | Each authorization re-reads objects; tests reject old generation and missing configuration after state changes |
| Runtime capability | Signed session and KB allowlist; live Kubernetes checks reject replaced, inactive and deleting AgentInstances before provider dispatch |
| Index publication | Ordered bounded spool batches, exact provider acknowledgement and visibility confirmation precede conflict-checked publication of the generation and binding |
| Index deadline | Real TLS/gRPC tests with virtual time verify idle streams expire at the server deadline and abort active transactions |
| Durable cleanup | Immutable operation/configuration-bound locator, exact cleanup confirmation and retained ownership records prevent cleanup from using changed configuration |
| Azure Search isolation | Local HTTP fixtures verify scoped query/index/delete operations, acknowledgement handling, visibility checks and replay rejection; no live Azure acceptance was run |
| Cleanup ownership | Reconciliation retains provider finalizers for Delete, Retain and Archive without lifecycle confirmation, including missing classes with committed bindings |
| Pgvector configuration | Independent namespaced CRD and strict decoder require local Secret references, bounded identifiers/dimensions and verify-full TLS; OpenAPI/CEL tests reject invalid configurations |
| Pgvector SQL isolation | Real PostgreSQL tests exclude higher-ranked rows from other tenants, KBs, generations and configuration identities/digests; all scope/filter values are parameters |
| Pgvector response safety | Bounded SQL text projection, citation URL restrictions, float32/norm bounds, finite score and response-size validation; real table-lock cancellation test |
| Pgvector connection material | Namespace-local Secret loading, bounded credentials/CA data and sanitized errors; real TLS handshakes reject wrong host, untrusted/expired certificates and obsolete TLS |

Pgvector schema tests add Kubernetes apiserver/component-base 0.36.0 and
blang/semver/v4 4.0.0 indirect dependencies to run the existing Kubernetes version's
CEL validator. Existing runtime backend-reference guards remain in place. The SDK
and configuration decoder contain no credential lookup; the query contract carries
no backend URL or credentials.

## Deployment Blockers

- Registration remains a fixed installation-owned allowlist, not an administrator
  registration resource or dynamically watched endpoint directory. Production
  gateway/provider wiring uses direct API readers; live RBAC enforcement still
  requires deployment validation. SDK configuration must not come from tenants.
- Binding publication is implemented, but separate Kubernetes authorization reads
  do not provide transactional revocation. Validate controller-only status access,
  concurrent refresh/deletion and failure recovery on a deployed cluster.
  Configuration hashes exclude Secret contents.
- The gateway verifies signed session capabilities, current runtime state and
  committed bindings. Query-specific quota accounting and durable success auditing
  remain pending. The SDK alone is not an agent-facing authorization boundary.
- The dense query component pins scope and enforces SQL isolation but does not
  verify current binding state. A deployed adapter must connect it to per-query
  authorization and replace it on binding changes. Its borrowed pgx pool must be
  securely constructed for the authorized configuration; accepting a spec does
  not verify the pool's actual endpoint, database, role or TLS settings.
- Certificate issuance/rotation/revocation, network policy, concurrency/rate limits
  and deployed failure behavior require validation before exposure.
- Pgvector configuration permits DNS hosts but does not authorize them. Before
  credential resolution is wired, enforce trusted endpoint/network policy and
  namespace-scoped Secret access. Database TLS provisioning and hostname verification
  are required; the existing plaintext demo database cannot satisfy verify-full.

These are deployment gates, not evidence of a production-ready service. Charts
and binaries are provided; this local review did not deploy or enable an endpoint.

## Historical Namespace Hardening Assessment

Historical verdict: blocked. Local authorization and independent admission tests verify
the controls below. Activation, upgrade, network enforcement, producer acceptance,
and final candidate validation remain security gates.

### Independent Admission Evidence

- Six API-server ValidatingAdmissionPolicies constrain controller writes to
  ConfigMaps, Secrets, Leases, native workloads, Services, and NetworkPolicies.
  They inspect both old and new objects and fail closed. Services and
  NetworkPolicies use separate policies so CEL checks each resource schema.
  All six policies passed current-generation full-control-plane typechecking on
  local Kubernetes 1.30.3 and 1.31.1 with no expression warnings and confirmed
  deletion. Exact-identity behavioral allow/deny probes passed on 1.30.3.
- A separate admission-guard process, ServiceAccount, and TLS identity enforce
  controller Pod attachment and reserved stdio Pod restrictions. Guard permissions
  allow only uncached GETs of Pods, Jobs, and NetworkPolicies in enrolled workload
  namespaces. Render tests check these exact grants and reject TLS Secret names
  exposed through the controller's configured installation Secret reads.
- Attachment requires a restricted Pod, its current Job UID and invocation
  identity, and a Job-owned NetworkPolicy with the exact invocation selector and
  deny-egress rules. New stdio policies use the owning Job name so the guard can
  retrieve one exact object without list permission. Existing invocations with
  randomly named policies must drain before activating this version.
- The missing-policy regression failed before the fix and passed afterward.
  Race-enabled tests cover invalid policy ownership, stale UID, deletion, Helm
  ownership, selectors, and permissive rules. Pod, Job, and NetworkPolicy read
  failures for forbidden access, timeout, and cancellation deny attachment without
  disclosing backend error details. Runner tests verify deterministic policy naming.
- Real Kubernetes API-server tests deny attachment without the policy and permit
  the admission request after its valid Job-owned policy exists. They also cover
  guard outage, legacy unsafe targets, reserved Pod changes, identity separation,
  and other installations. The most recent focused race run passed for the guard,
  stdio runner, rendered chart, and attachment admission tests after Go formatting.

- [SG-03 local acceptance](sg03-local-acceptance.md) additionally verifies both
  deployed Service/TLS guards, CA/DNS/availability faults, guard API permission
  denial and a real API timeout, followed by a restored kubelet/SPDY stdin/stdout
  exchange. Owned fixtures were removed with UID checks and confirmed absence;
  controller replicas stayed zero and original grants were unchanged.

These checks do not prove CNI enforcement or continuous isolation.
Kubernetes NetworkPolicies are additive; checking one policy does not
exclude another allowing traffic, nor prevent deletion after the guard's read.
The following requirements still block publication:

- Activate independent admission before granting controller mutation privileges
  or starting work, including upgrades. Controller startup checks and Deployment
  readiness alone do not establish this boundary.
- Drain or migrate unsafe legacy Pods and state without deleting unrelated active
  objects. Validate allowed production producer lifecycles and API defaulting.
- Complete full stdio operational acceptance and enforced sandbox isolation
  under a NetworkPolicy-capable CNI in a local cluster. The SG-03 stream fixture
  does not close additive-policy or concurrent-change coverage.
- Establish the dedicated workload namespace credential trust boundary; admission
  does not intercept Secret GETs.
- Refresh the core chart archive, exact candidate tests and coverage, security
  scans with provenance, and explicit staging review. Previous archive parity and
  scan results below do not attest the current working tree. The protected
  governance archive and unrelated user files must remain excluded.

### Earlier Namespace And Scan Evidence

The following records describe incremental checks before independent admission
was added. Statements about unrestricted state mutation describe that earlier
snapshot, and the scan dispositions remain open pending current-candidate review.

- Namespaced Kognition resources use Roles and RoleBindings only in the release
  namespace and explicitly enrolled namespaces. Native workload mutation and
  Pod attachment are denied in the release namespace. Native workload lifecycle
  permissions remain in separate enrolled workload namespaces.
  The controller cache uses the same namespace set. Cluster-scoped classes and
  immutable binding records retain their separate ClusterRole permissions.
- Installation Secret access permits only `get` on the configured runtime-auth
  and enabled Knowledge Gateway source names. Effective-authorizer tests enable
  the Knowledge Gateway with custom Secret names and reject the old default
  name, unrelated names, writes, list, and watch. Reads expose the entire named
  Secret; source objects must contain only required material. Installation
  ConfigMap, Lease, and custom-resource permissions remain and do not constitute
  complete control-plane compromise isolation.
- Lease permissions are reduced to `get`, `create`, and `update` in enrolled
  namespaces. ConfigMap `patch` is removed; ledger writers use conflict-checked
  `Update`. Real-authorizer regressions failed on the excess grants before each
  removal and passed afterward. Controller, cost, audit, dispatcher, tool-gateway,
  and controller-command package suites passed after both changes.
  Installation ConfigMap `update` and `delete` still permit tampering with
  unrelated configuration, including migration SQL. Dynamic creation cannot be
  restricted by name prefix or ownership labels using ordinary RBAC. A dedicated
  state namespace or admission-enforced ownership boundary requires a separate
  design and migration decision; no acceptance of this risk is implied.
- Agent runtime and ingestion entry points reject installation placement before
  accessing provisioning dependencies. Helm rejects an installation-namespace
  Agent override. Tool execution in that namespace is denied by RBAC. Existing
  installation workloads require operator-managed migration and finalization
  before upgrading; these checks do not prove live upgrade acceptance.
- Secrets use uncached reads; Secret list/watch privileges are removed. A local
  Kubernetes authorizer test applies all rendered chart bindings and verifies
  allowed enrolled-namespace operations, denied unrelated/cluster-wide access,
  denied RBAC escalation, Secret rotation/deletion, and scoped Pod informer sync.
  This is not a complete deployed reconciliation test.
- Chart tests reject invalid enrollment, check duplicate removal and exact
  RoleBinding subjects, and compare core resource grants to declared CRD scopes.
- The controller uses a dedicated ServiceAccount, including the quota-ledger
  binding and trusted approval-writer identity. Render tests cover default,
  custom, and externally managed accounts and reject shared/default names.
  A real Kubernetes authorizer regression failed before this separation and now
  denies the shared account Secret and workload access in installation, enrolled,
  unrelated, and cluster-wide scopes. The memory service keeps only ConfigMap
  create/get/list in the installation namespace. Real audit-store append,
  idempotent retry, and query succeed under that account; update, patch, delete,
  watch, and cross-namespace audit access are denied. These checks do not validate
  an installed release's identity migration or external identity bindings.
- Migration pods require a non-root UID and RuntimeDefault seccomp. Their init
  and main containers use read-only filesystems, drop all capabilities, and
  prohibit privilege escalation. Both chart modes pass security-context tests.
  Both rendered Pod specs also passed restricted Pod Security admission in a
  local Kubernetes 1.30 API server; privilege-escalation variants were denied.
  The exact rendered migration command applied all 30 migrations to a disposable
  local PostgreSQL database as UID 999 with a read-only root filesystem, all
  capabilities dropped, and privilege escalation disabled. A second pass skipped
  all 30 versions. The database was removed afterward. The restricted busybox
  network probe passed previously. These checks do not validate an installed
  release's upgrade, finalizers, network policy, or database credentials.
- The full sample installer fails before mutation when its fixed namespace list
  is not enrolled. Offline tests cover missing/empty enrollment, lookup errors,
  legacy controllers, and standalone installations. Individual sample installers
  still require operators to enroll their target namespaces.

On 2026-09-23, a fresh Trivy scan examined 25 explicitly rendered Kubernetes-mode
config files at HIGH/CRITICAL severity with no suppression. The render used
release `security-test`, installation namespace `control`, enrolled workload
namespace `runtime`, and kubelet probe CIDR `172.16.0.0/12`. The scan returned
exit 1 after the Lease and ConfigMap permission reductions. Input SHA256 hashes
were unchanged across the read-only scan:

| Finding | Current disposition |
| --- | --- |
| KSV-0056, HIGH, Services management | Still open. Mutation is restricted to enrolled namespaces, but compromise can affect unrelated Services within those namespaces. |
| KSV-0056, HIGH, NetworkPolicies management | Still open. Mutation is restricted to enrolled namespaces, but compromise can change isolation within those namespaces. |
| KSV-0109, HIGH, migration ConfigMap | Content-verified static-SQL false positive; raw finding retained. The local verifier recognizes only the exact reviewed payload and metadata. |

The migration security-context findings from the earlier render are absent.
KSV-0041 is absent from this rendered scan, but Secret get/create/update/patch/delete
remain permitted in enrolled workload namespaces. A compromised controller can
access their credentials and alter their Services and NetworkPolicies. Dedicated
workload namespaces reduce that blast radius but do not provide object-level
ownership enforcement. No residual risk has been accepted.

Default Helm rendering omits Kubernetes-controller resources and is insufficient
security coverage. This rendered-config scan is distinct from the historical
filesystem scan below and does not attest the final staged filesystem snapshot.

Closure still requires mitigation or explicit security-owner acceptance of
required workload-namespace privileges and installation-state tampering risk,
current index validation, and installed-release migration acceptance. The core
chart archive was refreshed and passed source parity checks; the protected
governance archive remained byte-for-byte unchanged. Removing required mutation
verbs without a replacement lifecycle mechanism would break reconciliation.

### Content-Verified SQL Disposition

`go run ./tools/securitygate --report <raw-report> --rendered-root <render-root> --provenance <provenance-json> --candidate-sha256 <expected-candidate-sha256>`
verifies the exact KSV-0109 finding against all 30 rendered migration entries,
ConfigMap identity, allowed fields, and exact labels. The canonical SQL payload
SHA256 is `01fadf1322da74e76e78787f0e2113580682b1352e6366605339564531da42e6`.
The verifier normalizes CRLF, sorts keys, and separates each key/value with NUL.

All 18 regression cases passed, including injected SQL credentials, added keys,
annotation/label credentials, changed labels, duplicate YAML keys/documents,
changed finding metadata, and additional RBAC findings. The fresh raw report was
verified against the same rendered tree: the SQL finding was recognized and the
two RBAC findings still produced exit 1. Reports are never rewritten.

Retain the raw report, provenance, and exact scanned tree together. The verifier
checks input/report hashes, a 24-hour report age limit, and equality with the
expected candidate SHA256. Supply that digest from the independently frozen
candidate manifest, never by copying it from the scan's provenance. These checks
do not authenticate the provenance author or run Trivy. Regression tests reject
candidate mismatches and missing, malformed, or uppercase expected digests;
the command-line interface requires the candidate digest explicitly.
A changed migration, metadata,
or scanner message requires review. This tool handles only explicitly rendered
Kubernetes configuration reports, not filesystem archive findings. CI's strict
vulnerability, secret, and misconfiguration scans remain unchanged. No path-wide
or rule-wide scanner suppression has been added.

## Historical Filesystem Scan Disposition

Review date: 2026-09-23. The local Trivy 0.74 filesystem scans of committed
`HEAD` and the implementation snapshot returned the same four findings with
`vuln,secret,misconfig`, HIGH/CRITICAL severity and `ignore-unfixed` enabled.
Neither report contained vulnerability or secret-detector findings at that scope.
This comparison establishes no new scanner findings, not security clearance.
The raw scan exits with code 1; no ignore rules or scanner exclusions were added.

All four targets are expanded members of
`samples/helm-umbrella/charts/kognition-0.1.0.tgz`. Their source templates are
`deploy/helm/kognition/templates/clusterrole.yaml` and
`deploy/helm/kognition/templates/migrations-configmap.yaml`.

| Finding | Disposition | Evidence and residual risk |
| --- | --- | --- |
| KSV-0041, CRITICAL, Secrets management | Open baseline least-privilege risk; not accepted or suppressed | The controller copies runtime-auth Secrets and manages ingestion-input Secrets. `ensureRuntimeAuthSecret` checks source ownership, but Kubernetes RBAC grants cluster-wide Secret access without resource-name restrictions. Controller compromise can expose or modify unrelated credentials. |
| KSV-0056, HIGH, Services management | Open baseline least-privilege risk; not accepted or suppressed | Runtime dispatch and service reconciliation create/update Services. The ClusterRole also permits mutation outside controller-owned objects; application ownership checks are not an RBAC boundary. |
| KSV-0056, HIGH, NetworkPolicies management | Open baseline least-privilege risk; not accepted or suppressed | Runtime dispatch and tool execution construct namespace-local NetworkPolicies. Cluster-wide mutation permissions can change isolation outside the intended workload namespaces if the controller is compromised. |
| KSV-0109, HIGH, migration ConfigMap | Reviewed false positive for the reported SQL identifiers and expressions | Migrations 006 and 007 define token-budget accounting and quota-period backfill. Migrations 025 and 027 contain `status <> 'Prepared' AND preparation_token = ''` constraints. These are schema statements and empty-string checks, not embedded token values. The template packages static migration files, not database rows. |

At that snapshot, the three RBAC findings remained release/security blockers under the all-green
condition. Baseline presence and functional necessity do not authorize risk
acceptance. Removing these permissions without redesign would break supported
runtime and ingestion behavior. The subsequent namespace hardening is recorded
above; it does not constitute risk acceptance.

The original closure criteria required the controller/chart maintainers and installation security owner
to define managed namespaces, separate installation Secret reads from namespaced
workload writes, and restrict Service/NetworkPolicy mutation to approved namespaces.
Validate both allowed reconciliation and denied cross-namespace access through the
Kubernetes authorizer, preserve rotation/cleanup behavior, and rerun Trivy. Any
remaining exception needs explicit owner approval and narrowly bounded scope.
The ConfigMap disposition must be revisited if migration content or templating
starts carrying data values; its scanner finding remains visible today.

## Assessment Limits

The following paragraphs record earlier incremental checks. They are historical
evidence, not a final full-suite result for the current source. Current review
checks also cover gateway authorization, provider lifecycle and idle-stream
deadlines; repository-wide release checks must pass before publication.

The pre-commit review reproduced and fixed premature provider finalization in
six regression cases. All 11 affected Go package suites passed after the fix.
The five offline AKS script suites and 451 Helm assertions also passed; the
foundation suite compiles Bicep and checks that the base deploys no inference.
These checks do not provision resources or validate an installed provider.
The full Go suite subsequently passed, including umbrella-chart parity and the new
pgvector schema package. The provider chart passed strict Helm lint. Final decoder
inspection found a duplicate package declaration; it was removed and the provider
suite passed again before commit.

Transport tests use an in-memory adapter and loopback TLS. Additional provider
tests ran against a disposable local `pgvector/pgvector:pg16` container, proving
dense query SQL isolation and citation/filter behavior on PostgreSQL. The provider,
SDK and Kubernetes binding suites passed together after response-bound hardening.
The query slice adds no dependencies and does not modify runtime dispatch guards.
These tests do not establish deployed authorization, database TLS, credential
handling, lifecycle correctness, legacy migration or Azure AI Search isolation.
The complete Go suite passed with the live pgvector fixture enabled for both the
dense retrieval and connection-material increments. Connection-material tests cover
fake-client Secret isolation/rotation and loopback TCP TLS handshakes, not PostgreSQL's TLS upgrade
protocol or a securely configured pgx pool. The loader requires an authorized
configuration and uncached reader; endpoint approval and pool rotation are pending.
Regression tests verify malformed PEM blocks cannot be skipped before a valid CA
and TLS rejection cannot pass solely because the test handshake timed out.
Binding tests use a fake Kubernetes client and static CRD schema inspection, not
an API server, authorization cache or live RBAC enforcement. The independent
pgvector chart packages a CRD only and was not installed on a cluster. Its tests run
OpenAPI/CEL validation and unknown-field pruning locally. They do not establish live
admission, Secret authorization, database TLS or credential-bearing egress safety.
Repository-wide secret/dependency scans and GitHub security-alert review were not
performed in this slice. No claim of repository-wide security clearance is made.
