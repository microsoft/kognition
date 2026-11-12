# Installation And Recovery Contract

- Status: implemented local SG-05 workflow; production activation remains blocked.
- Updated: 2026-10-21.
- Implementation: [installer](../../../tools/kognition-install/main.go), [plan](../../../internal/k8s/installation/plan.go), [workflow](../../../internal/k8s/installation/workflow.go).

The installer ends at verified inactive admission. Keep writers frozen until the
separate [activation transaction](activation-transaction-contract.md) succeeds
with genuine SG-03/07/08/09 evidence. Neither Helm success nor this document
authorizes production activation.

## Independent Resource Admission

Kubernetes resource mutation, validation, audit and authenticated RuntimeApproval
endpoints run in the dedicated `<release>-webhook` Deployment using the
`resource-webhooks` command. This process starts production admission handlers
without reconcilers, dispatchers, retention jobs or controller leader election.
Its Service and certificate remain available while the reconciliation controller
has zero replicas and no workload grants. The independent sandbox admission guard
remains a separate workload and identity.

The resource admission identity reads Kognition objects in enrolled namespaces
and cluster-level classes and admission policies. It appends installation audit
ConfigMaps, manages RuntimeApproval coordination Leases and updates authenticated
approval status. It has no Kubernetes Secret API access, workload mutation,
RBAC administration or authority-namespace permissions. Its mounted binding keys
and TLS material still place it inside the trusted control plane. Cluster-wide
admission-policy reads are required for class admission. Activation snapshots bind
its Deployment, Service, identity, RBAC, webhook configurations and image digest.

For deterministic full-feature installation, provision gateway Secrets under their
standard release names and set `secret.create=false` for `memoryPolicyGateway`,
`toolGateway`, `modelGateway`, `knowledgeGateway` and `agentCommunicationGateway`. Preserve the keys
expected by each gateway. The default remains chart-generated credentials for
existing development usage. Keep Secret contents out of command output and
protect local values and manifest artifacts with platform access controls.

Known installer limitations observed during local reinstall:

- An unchanged Helm-owned `MemoryClass/default` may remain only when its exact
  UID/content hash is in `retainedObjects`, it has no deletion/finalization state,
  and its spec appears identically in both reviewed release renders. The drain
  rejects all unreviewed classes, changed specs, replacements and active resources.
  Do not remove finalizers or bypass the installer.
- Initial zero-replica observation can race bootstrap verification. Verify the
  observed inactive state and use exact-claim recovery after executor/request
  settlement, retaining the original failure and artifacts.
- The legacy `k8sController.replicas` override must be explicitly zero in reviewed
  inactive values. The installer rejects a nonzero rendered controller.

## Target And Preparation

1. Select an explicitly authorized cluster using a separate static kubeconfig.
   Do not change the global context. Exec/auth-provider credential plugins,
   insecure TLS and impersonation are unsupported. Protect snapshots and plan
   artifacts as credentials; POSIX mode bits alone do not establish Windows ACLs.
2. Provision and review the installation namespace, dedicated workload namespaces
   and a separate administrator-owned authority namespace. Record each UID and
   the kube-system UID. The controller must have no authority-state or namespace
   mutation rights. Protect namespace administration and inherited RBAC too.
3. Prepare required CRDs, image availability and certificates explicitly. This
   command does not create namespaces, install cert-manager, load images, read
   cloud credentials or install samples. CRDs and certificate infrastructure
   require their own reviewed change; Helm hooks and CRD application are skipped.
    Complete the database prerequisites below before running affected workloads.
4. Freeze every release, admission and workload writer: operators, GitOps,
   autoscalers, schedulers and custom producers. Keep the old controller and its
   deliberately retained finalization permissions available until existing work
   has completed cleanup or an operator-approved migration.
5. Follow the [drain contract](drain-verification-contract.md). Inventory native
   producers, all Kognition resources, terminating objects, finalizers, old
   controller identities/grants and randomly named invocation policies. Back up
   persistent data and binding identities. Namespace migration is recreation,
   not an in-place move. The installer does not migrate data or remove finalizers.

Use dedicated workload namespaces without unrelated credentials. Admission cannot
restrict Secret GET, and a controller compromise can expose credentials throughout
its enrolled trust domain. See [ADR-0016](../../adr/0016-installation-trust-boundary.md).

## Database Prerequisites

In Kubernetes mode, the chart renders a regular, content-addressed
[migration Job](../../../deploy/helm/kognition/templates/migrations-job.yaml)
for its PostgreSQL-backed components. `--no-hooks` does not skip that Job.
Review its image, database Secret reference and SQL ConfigMap as part of the
inactive manifest, including an Argo synchronization. Require successful Job
completion and verify the resulting schema before Memory operations.

Standalone mode uses post-install/post-upgrade Helm hooks, which `--no-hooks`
skips. If the reviewed installation does not run the chart Job, run the repository's
[migration binary](../../../cmd/kognition-migrate/main.go) as an explicitly
reviewed one-shot Job against the platform PostgreSQL database. Use the verified
image and the same `DATABASE_URL` Secret reference as the Memory Policy Gateway;
do not expose the connection string in arguments, logs or committed manifests.
Require a successful Job exit and verify the resulting schema before proceeding.
Ready gateway Pods do not prove schema readiness. On an existing database, review
migration compatibility and backups first.

Preserve the installed migration ledger format. The chart Job records versions
in `schema_migrations(version, applied_at)`; the standalone migration binary
records SQL filenames and checksums in `schema_migrations(name, checksum,
applied_at)`. These ledgers are not interchangeable. Use the existing migration
path, or explicitly review a ledger conversion after a verified backup and
restore. Do not run the standalone binary against a chart-managed ledger.

Memory backend handles are opaque, case-sensitive strings. Migration
`035_memory_opaque_volume_handles.sql` changes `memory_vectors.volume_id` from
UUID to `TEXT COLLATE "C"`, preserving canonical UUID-backed records and the
composite primary key. Apply the matching standalone or chart migration before
using controller-generated `pgvector-...` handles. The conversion takes an
exclusive table lock and can rebuild indexes. Quiesce Memory writers, review
lock and statement timeouts, restore a backup into an isolated verification
database, and verify data, indexes and the installed ledger before resuming.
Preserve failed actions and use new invocation IDs after repair. Do not strip
handle prefixes, edit bound volume handles, reset receipts or alter historical
migrations.

Migration `036_memory_volume_dimensions.sql` adds durable, case-sensitive
per-volume embedding dimensions. Existing volumes retain their records and
1,536-dimensional contract. New volumes support 1 through 16,000 dimensions;
omitting the dimension selects 1,536. Provision local `all-minilm` volumes with
384 dimensions before storing records. Set `MemoryClass.spec.parameters.embeddingDimension`
to `"384"` for controller provisioning, or send `embedding_dimension: 384` to
the authenticated Memory service provision endpoint. Configure the service's
`MEMORY_EMBEDDING_PROVIDER`, `MEMORY_EMBEDDING_ENDPOINT` and
`MEMORY_EMBEDDING_MODEL` consistently with those volumes.

The migration takes an exclusive table lock and rebuilds the vector index.
Quiesce writers, back up data and the installed ledger, and verify an isolated
restore before applying it through the installation's existing migration path.
HNSW indexes cover 384 and 1,536 dimensions; other dimensions use exact search.
Provisioning is idempotent only for the same dimension. Incompatible
reprovisioning, writes and searches fail explicitly, including searches of empty
volumes. Deprovisioning deletes the volume's records and dimension metadata.
Changing embedding models requires a separately reviewed re-embedding or new
volume; matching dimensions alone does not establish model compatibility.

The opt-in PostgreSQL regressions require an explicitly configured disposable
database whose name starts with `migration_check_`. After applying the matching
production migrations, set `KOGNITION_TEST_MEMORY_DATABASE_URL` through protected
configuration and run:

```powershell
go test .\internal\store\postgres -run '^TestPgvector(PerVolumeDimensions|OpaqueVolumeHandleIsolation)$' -count=1 -timeout 3m
```

These regressions verify legacy/default and local dimensions, case-sensitive
isolation, unchanged records after rejected mutations, concurrent provisioning,
and deprovisioning.

For an in-cluster embedding server, configure `memoryService.privateEgress`
with its exact namespace labels, Pod labels and numeric TCP ports.
`modelGateway.privateEgress` and `toolGateway.privateEgress` are separate
allowlists; configuring either one does not authorize raw memory service egress.
The memory allowlist defaults to empty and never selects Agent runtimes or the
Memory Policy Gateway. Invalid, empty-peer or out-of-range-port entries fail
chart validation. Keep backend servers outside controller-enrolled credential
namespaces when they are independently operator-managed, and restrict their
ingress to the approved platform consumers.

The Python Agent supports Ollama through the existing OpenAI-compatible Model
Gateway endpoint. The runtime still requires a session capability; it does not
send provider endpoints or provider credentials to that gateway request.
Profile-backed and legacy local Tool requests use the same governed route.
Qualify the actual runtime image and declared network peers before activation;
an image-only local profile does not establish backend reachability.

The Memory Gateway requires `memory_mutation_receipts` even in Kubernetes mode.
`memory mutation replay unavailable` with SQLSTATE `42P01` for that table means
the platform migrations have not completed. Do not bypass replay checks or reset
approval state to conceal this failure.

The separate KnowledgeBase pgvector database also needs the `vector` extension.
Create it in the configured KnowledgeBase database using its authorized database
role, and confirm `pg_extension` contains `vector`. The platform database and the
KnowledgeBase database are separate targets; initializing one does not initialize
the other. `vector type not found in the database` identifies this missing
prerequisite. Preserve failed index runs and submit new named runs after repair.

## Enrollment And Administrative Authority

The installation security owner approves the credential exposure boundary and
namespace enrollment. An authorized cluster administrator applies the reviewed
namespace, RBAC, admission, certificate and Cilium configuration. Application
authors and the controller cannot enroll a namespace by adding workload labels.
Only separately authorized administrators may change the authority namespace,
guard identity, admission policies/bindings or persistent isolation baselines.
Protect those rights against inherited group grants and automated writers too.

Treat enrollment changes as an installation change under the same writer freeze,
drain, inactive bootstrap and separate activation protocol. Review namespace UIDs,
all credentials and workloads, effective grants, and Cilium enforcement before
adding a namespace. A same-name recreated namespace is a new identity requiring
new inventory and evidence. Before removal, finalize or explicitly migrate owned
work and data while the old required cleanup authority is available; merely
removing a values entry does not certify drain, revoke copied credentials or
remove live workloads. Reject enrollment containing unrelated tenant credentials.

## Credential Compromise Response

This is an operator procedure, not an automated incident-response command or a
certified production exercise. The installation security owner directs response;
the cluster administrator contains Kubernetes authority, and each credential
issuer revokes its own material. Record the incident, target cluster/namespace
UIDs, affected identities, actions and verification results without recording
Secret values in logs, reports or chat.

1. Freeze release, admission and workload writers and stop new dispatch. Preserve
  audit evidence and protected authority journals/receipts. Retain independent
  admission and Cilium enforcement. If the controller or its credentials are
  compromised, do not rely on that controller to perform trustworthy cleanup.
2. From a trusted administrator context, contain the compromised identity and
  affected runtime identities: stop affected controller/workload processes,
  revoke effective API grants and issuer credentials, and verify containment.
  Revocation and process termination do not prove outstanding API requests have
  settled or close already established streams. Keep independent network
  restrictions and confirm termination and request settlement before recovery.
  Emergency containment may strand finalizers; record that state instead of
  deleting finalizers, claims or data to make a drain check pass.
3. Treat every Secret readable by the compromised identity as potentially
  exposed: named installation sources, their entire contents, enrolled namespace
  credentials, materialized copies, mounted values and externally issued tokens.
  Include runtime-auth and provider/database material where accessible. If
  administrator, guard or authority-state access is suspected, expand the
  incident boundary; controller-only isolation evidence no longer suffices.
4. Revoke exposed material at its issuer and replace sources and copies through
  reviewed procedures. Source deletion blocks new provisioning but preserves
  existing copies; it is not revocation. Restart or recreate consumers that
  retain old material, revoke affected sessions, and verify old credentials fail
  and replacements succeed. Routine HMAC previous-key overlap is unsuitable for
  a compromised key: retaining it continues to authorize forged capabilities.
  Coordinate issuer/verifier replacement and accept an outage if necessary.
5. Inspect persisted workloads, Services, policies, bindings and data for changes
  under the compromised identity. Resolve cleanup/migration using trusted,
  narrowly authorized operators. Verify exact UIDs and absence, not DELETE
  acceptance alone. Do not automatically restore the compromised controller's
  grants to finish ordinary drain. Preserve unresolved work for explicit review.
6. Before restart, the security owner reviews containment, credential invalidation,
  data integrity and restored admission/CNI authority. Reconcile the installation
  inventory, run supported recovery where applicable, and obtain fresh
  exact-target acceptance and approval under the activation contract. Old local
  reports, expired approvals and a healthy Deployment do not authorize restart.
  Production activation remains blocked until its separate gates are satisfied.

Planned maintenance retains required old cleanup authority until drain completes.
Compromise response prioritizes containment and may interrupt that ordering; it
does not certify a successful normal upgrade or create a recovery bypass.

## Reviewed Plan

There is no automatic plan approval or inventory adoption. An administrator creates
a strict version 1 JSON plan using the exported schema and reviews every identity.
Unknown, duplicate and case-variant fields are rejected. Relative paths resolve
against the plan directory. Preserve the exact original plan bytes for recovery.

| Fields | Review requirement |
| --- | --- |
| `version`, `kubeconfig`, `endpoint` | Version `1`, explicit credential file and exact HTTPS API origin |
| `clusterUID`, `namespaceUID` | kube-system UID and installation namespace UID |
| `authorityNamespace`, `authorityNamespaceUID` | Separate protected authority namespace and exact UID |
| `release`, `releaseRevision` | Exact Helm release; `0` for a fresh install, otherwise current reviewed revision |
| `chart`, `values`, `manifest` | Each contains `path` and lowercase SHA256 of exact file bytes; chart is a reviewed `.tgz` |
| `previousManifest` | Omit for fresh install; otherwise exact current Helm manifest artifact |
| `workloadNamespaceUIDs` | Complete desired enrolled namespace-name to UID map |
| `controllers` | Old controller `deployment` ObjectRef plus exact `serviceAccount` |
| `grants`, `legacyPolicies`, `retainedObjects` | Exact reviewed resource inventories; explicit empty arrays for required cleanup lists |
| `completedMigrationJobs` | Optional exact ObjectRefs for completed content-addressed migration Jobs removed by the new release |
| `cleanupAttestations` | Non-Helm cleanup lineage: `resourceUID`, `controllerUID`, `evidence`; invocation policies also require `invocationID` |

An ObjectRef contains `apiVersion`, `kind`, `namespace`, `name`, `uid`, `sha256`.
Calculate its hash using exported `installation.ObjectDigest` on the live
unstructured object. It removes status, resourceVersion and managedFields while
retaining UID and generation. A raw `kubectl` JSON hash is not equivalent.
Object identity includes API group/kind and namespace/name. Labels alone do not
authorize cleanup. Shared bindings and unexplained previous resources fail closed.

Render the packaged chart with the exact values, release, namespace and actual
server version, `--no-hooks`, and `--set k8sController.activated=false`; include
`--is-upgrade` when `releaseRevision` is positive. Review the resulting manifest
under the [trusted-render contract](bootstrap-verification-contract.md). Package
without rebuilding dependencies or refreshing unrelated archives. Pin production
images by digest and retain provenance.

Values must render deterministically without live lookup: provision existing
credentials/TLS Secrets where supported and provide stable reviewed values for
enabled components. Generated random Secrets or lookup-dependent templates can
produce different offline and apply renders; mismatch is a rejection, not grounds
to weaken verification. The local fixture disables optional memory/tool gateway
components; it does not establish acceptance for every default chart profile.

The installer snapshots verified artifacts and a flattened static kubeconfig,
validates the actual server render, then validates Helm's output again through its
post-renderer. Hash equality detects drift; it does not establish trust in inputs.

## Bootstrap Commands

Both wrappers invoke the same command and require affirmative freeze confirmation:

```bash
make deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
```

```powershell
.\make.ps1 deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
```

The equivalent manual path uses the installer, not a direct Helm upgrade:

```powershell
go build -o ./bin/kognition-install.exe ./tools/kognition-install
./bin/kognition-install.exe bootstrap --plan ./reviewed/install.json --writers-frozen
```

The common ordered stages are preflight, workload drain, controller stop, observed
stop, grant revocation, drain verification, exact legacy policy deletion, completed migration retirement, admission
bootstrap and bootstrap verification. A held Invocation or finalizer blocks before
controller shutdown. Old and new identities must be drained before admission changes.
Unsupported migration requires operator reconciliation and a new review.

When a schema change creates a differently named migration Job, inventory the
old Job in `completedMigrationJobs` and every other previous release object in
the appropriate retained or cleanup inventory. Cleanup accepts only an
independently Helm-owned `batch/v1` migration Job from the exact previous
manifest, in the installation namespace, with one successful completed execution,
no active executions and API token automount disabled. All owned Pods must be
terminal before deletion and absent before admission bootstrap continues.
Pending, failed, changed, replaced and unrelated Jobs fail closed.

The `remove-completed-migrations` stage runs after drain, controller stop and
grant revocation. It journals the exact UID/hash deletion intent and uses UID and
resource-version preconditions. Recovery requires the same reviewed plan,
matching claim, settled requests and retained intent; missing Jobs without that
intent are not treated as successful cleanup. This stage never changes SQL
ConfigMaps, database volumes or migration ledger entries, and never authorizes
controller activation.

Helm uses the Secret storage driver, reset values, forced inactive activation,
no hooks, skipped CRDs and the exact post-renderer. It never uses `--atomic`,
`--reuse-values` or automatic rollback. Saved `activated=true` cannot start the
controller. The chart withholds controller grants; reviewed inactive values must
also leave the legacy `k8sController.replicas` override at zero. The installer
rejects a rendered nonzero controller before mutation.
Failures retain the claim, freeze and diagnostic stage; there is no automatic retry.

### Retire A Completed Bootstrap Claim Before Activation

A successful bootstrap retains its installation claim. After independently
establishing executor and request settlement, use the original plan, frozen
installer and exact claim UID with the recovery command below while the target
is still inactive. Require `verified-inactive`, then confirm claim absence and
the matching immutable retirement receipt before preparing activation.
Retirement is an explicit reviewed operation, not an automatic success cleanup.

Otherwise a later upgrade cannot acquire its claim, and original-plan recovery
cannot retire the earlier claim while controller grants and replicas are active.
If this has already happened, complete controlled workload finalization, then use
the original activation executable, artifacts and approval to recover that
activation inactive. Only afterward retire the original installation claim.
Never delete authority records or restore grants manually to bypass this order.

The drain inventory must cover every Kognition/provider resource kind, including
unowned terminal AgentInstances and controller-created default AutonomyClasses.
Removing parent Agents alone does not establish a complete drain. Preserve the
exact unchanged Helm-owned default MemoryClass only through the supported
`retainedObjects` review.

## Explicit Recovery

Stop every prior installer/recovery executor and independently establish that its
outstanding API requests have settled. Process death, a timeout, NotFound and a
claim resourceVersion do not prove settlement or fence a paused executor.
Do not acknowledge these conditions until operational evidence supports them.

```powershell
./bin/kognition-install.exe recover --plan ./reviewed/install.json --writers-frozen --executors-stopped --requests-settled --claim-uid EXACT_REVIEWED_UID
```

Use the original plan and printed claim UID. The claim pins target and exact plan
bytes in protected administrator storage. Before admission begins, recovery can
finish only exact durable stop/delete intents. Once admission begins it verifies
drain without replaying legacy cleanup. No recovery restores old grants or replicas.

| Outcome | Meaning |
| --- | --- |
| `aborted-before-mutation` | The journal reached no mutation stage; existing work may still run with its old privileges |
| `drained-retry` | Old/new controller identities are drained; admission may remain incomplete and needs a newly reviewed retry |
| `verified-inactive` | Current admission and stopped state passed verification; activation is still separate |

Recovery creates an immutable retirement-intent receipt before conditionally
deleting the exact claim UID/resourceVersion. The receipt alone does not prove
deletion completed. A repeat with an absent claim confirms absence and the matching
receipt; it does not revalidate current admission or authorize a new plan. Preserve
claims, receipts and the kube-system authority anchor. Never delete them to bypass
review. Direct administrator edits and legacy rollback are unsupported bypasses.

### Interrupted Helm Storage

If an interrupted Helm process left the exact next revision pending, review its
Secret UID and `installation.ObjectDigest` after request settlement. Add paired
`--helm-record-uid EXACT_UID --helm-record-sha256 LOWERCASE_SHA256` to recovery.
Only an active admission-stage claim can authorize settlement. Exact release,
revision, payload, inactive manifest and history must match. Settlement changes
pending-install/pending-upgrade to failed through a durable intent; it never
replays apply, deletes Helm history or restores a deployed revision.

### Retrying A Failed Release

After recovery, create a new reviewed plan for the current failed revision:

- Set `failedRelease` to the exact failed Helm Secret ObjectRef and
  `previousManifest` to that revision's stored manifest.
- Record `previousDeployedRevision` and `previousDeployedManifest` for the most
  recent earlier deployed revision, or omit them when no deployed revision exists.
- Inventory the union of the target, current failed and last-deployed manifests.
  Review live resources with exact UID/hash; list missing identities in
  `absentObjects` using API version/kind/namespace/name. Absence is allowed only
  under this explicit failed-release review.
- Review removed resources and cleanup lineage. Render the retry as an upgrade.
  Recheck all writers remain frozen and bootstrap with the new plan.

An ordinary subsequent deployed-release upgrade must clear failed-release-only
fields and review the new current revision. Never restore old activation using
`helm rollback`, `--atomic`, direct RBAC mutation or inherited values.

## Verification Boundary

The [SG-05 local acceptance report](../../development-plans/features/knowledge-provider-execution/sg05-local-acceptance.md) records exact local
scenario identities, commands, results, quality checks and outstanding gates.

[Deployed tests](../../../internal/traceability/installation_deployed_integration_test.go)
exercise real CLI/Helm fresh/legacy installs, quota-rejected install/upgrade,
certificate readiness failure, a real installer process kill during read-only
verification after Helm exit, and explicit retry/recovery. The legacy controller
is a BusyBox fixture with a minimal grant, not a production finalization test.
[API tests](../../../internal/traceability/installation_journal_integration_test.go)
exercise durable cleanup and Helm settlement with injected lost responses; they
do not claim killing an in-flight Helm process settles its requests. SG-07 owns enforcing-CNI/full stdio and
SG-08 owns production producer lifecycles. Missing gates continue to block activation.
