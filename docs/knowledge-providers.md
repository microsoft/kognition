# Knowledge Provider Query Contract

Status: SDK transport, Kubernetes binding resolution, governed gateway dispatch,
provider ingestion, and confirmed lifecycle cleanup are implemented with local
tests. Azure AI Search and pgvector implement the provider path. Pgvector also
has real database/TLS and bounded Foundry component coverage. Installed-cluster,
live Azure Search and AKS conformance remain opt-in deployment gates.

The contract implements part of
[ADR-0015](adr/0015-provider-integration-boundaries.md). It extends CPI's existing
Identity and Lifecycle services with `KnowledgeExtension.Query`, `Index` and
`Cleanup`; it does not
introduce a separate discovery framework.

## Trust Boundary

- `pluginsdk.NewKnowledgeServer` requires an adapter, a health probe, a scope
  authorization function, typed capabilities and mutual TLS credentials.
- `pluginsdk.NewKnowledgeClient` takes a provider name, fixed `host:port` address,
  trust roots, client certificate, server DNS name and exact peer SPIFFE URI.
  These settings must come from administrator-controlled registration. The SDK
  does not implement that registration resource or its RBAC policy yet.
- TLS 1.3 verifies certificate chains and server DNS identity. Both sides require
  exactly one certificate URI SAN matching the configured peer identity. A
  certificate signed by the trusted CA with a different identity is rejected.
- The secure knowledge constructor is separate from the existing local-socket
  plugin constructor. Existing unauthenticated discovery connections cannot be
  reused as authenticated query connections.
- Core must authorize the current claim, revocation state, quota and policy, audit
  the query, and derive its scope before calling the SDK. A successful TLS
  handshake or CPI capability response does not perform those governance checks.
- `AuthorizeScope` runs on every server query before backend execution. It must
  resolve the current trusted namespace-local binding, enforce the committed
  generation and configuration UID/digest, and reject missing or stale bindings.
  Its argument is a copy; changing it cannot replace the query's scope.
- The adapter must also constrain every database/search operation to the supplied
  namespace, KB UID and generation. The SDK cannot inspect backend query semantics.

## Typed Query

The protobuf contract lives in
[cpi.proto](../proto/kognition/v1alpha1/cpi.proto). Knowledge execution uses generated
protobuf/gRPC bindings. Existing local CPI discovery retains its JSON codec and
explicit wire-to-API conversion.

| Field | Contract |
| --- | --- |
| `scope.namespace` | Consuming KnowledgeBase namespace |
| `scope.knowledge_base_uid` | Immutable KnowledgeBase identity |
| `scope.generation` | Positive committed generation |
| Configuration group/kind/name | Typed, namespace-local configuration reference |
| Configuration UID | Detects deletion and recreation of configuration |
| Configuration digest | Lowercase hexadecimal SHA-256 digest using the configuration-spec algorithm below |
| `scope.operation_id` | Required immutable committed indexing operation ID, at most 128 bytes |
| `request_id` | Required correlation ID, at most 128 bytes |
| `mode` | `keyword`, `dense` or `hybrid`; must be advertised |
| `text` | Required for keyword/hybrid, at most 65,536 bytes |
| `vector` | Required for dense/hybrid, finite values, at most 65,536 dimensions |
| `top_k` | 1 through 100 |
| `filters` | At most 32 distinct `metadata.*` equality filters; each value at most 4,096 bytes |

Keyword requests reject vectors; dense requests reject text. Metadata filters
cannot replace the first-class scope. Providers must parameterize their backend
queries and treat filter values as data. Backend configuration determines actual
embedding dimension compatibility; the SDK's vector limit is only a resource bound.

The query requires advertised citation support. Filtered queries additionally
require the filter capability. Missing metadata, unknown protocols, mismatched
configuration kinds and unsupported operations fail before execution.

## Committed Backend Binding

The [Kubernetes binding resolver](../internal/k8s/knowledge/binding.go) accepts an
explicit allowlist of provider names and configuration group/version/kinds. This
is an in-process registration snapshot, not an administrator registration CRD or
endpoint directory. Replacing the allowlist requires replacing the resolver;
provider removal is not dynamically watched yet.

- `KnowledgeBase.status.backendBinding` records its index generation, provider
  name, durable reclaim policy, class UID/generation, configuration identity and
  digest, operation ID, and opaque backend operation ID.
- `Resolve` reads the KnowledgeBase, its cluster-scoped class and the allowlisted
  configuration. Configuration lookup always uses the KnowledgeBase namespace.
  It returns only the provider name and query scope, without configuration data,
  credentials or an adapter address.
- Resolution requires a positive committed `status.generation` equal to the
  binding generation. Kubernetes `metadata.generation` is a different counter.
  `Ready`, `Refreshing` and `Failed` can retain an existing committed index;
  initial indexing without a matching committed binding cannot serve queries.
- Missing or deleting objects, class replacement or spec changes, configuration
  replacement or spec changes, and unregistered providers fail closed. A failed
  refresh preserves queryability only while the committed class/configuration
  remains unchanged. No legacy backend fallback occurs.
- `AuthorizeScope` resolves again and compares every scope field. Its namespace/name
  key must come from the trusted claim/KB binding, not an agent-supplied locator.
  Use a direct, uncached Kubernetes API reader when wiring authorization. Cached
  readers can incorrectly authorize revoked or changed bindings.

`ConfigurationDigest` hashes the domain prefix
`kognition.knowledge/configuration-spec/v1` followed by one newline and Go
`encoding/json.Marshal` output for the persisted configuration's `spec` map. Map
keys sort lexicographically; array order remains significant. The encoded spec
must be at most 1 MiB. Metadata, status and Secret contents are excluded. This is a
versioned Go serialization contract, not RFC 8785 canonical JSON. Future non-Go
adapters must match its numeric and escaping rules or consume a core-computed
digest. Provider CRDs must prohibit inline credentials and place backend-routing
settings in the spec; this resolver does not validate provider-specific schemas.

The index controller publishes the binding, generation, counts, and provider
confirmation together in one conflict-checked status update after visibility is
confirmed. Only the controller identity may write that status. Query authorization
resolves current class and configuration state through a direct API reader and
compares every scope field with the committed binding.

Deletion uses the reclaim policy stored in the committed binding. A missing class
or later class policy change cannot replace that policy. Provider-backed bindings
without a valid policy or opaque backend operation ID fail closed and retain the
finalizer. This protects legacy objects from silently defaulting to Delete.

The opaque backend operation ID reconstructs provider routing after the class or
configuration has been deleted. For Azure AI Search, the versioned locator binds
the service, index, operation ID, and configuration digest. The provider validates
the locator against the exact durable scope before creating an Azure client.

The Kubernetes cleanup adapter reads controller-owned mutual TLS material and
calls the provider through `pluginsdk.KnowledgeClient.Cleanup`. Delete removes only
documents in the exact namespace, KnowledgeBase UID, generation, operation, and
configuration scope, then confirms a zero count. Retain returns a confirmed no-op.
Azure AI Search rejects Archive. The controller removes its finalizer only after
the SDK validates an exact cleanup confirmation. Missing registration, invalid
credentials, unsupported policy, transport failure, malformed confirmation, and
provider error all retain the finalizer for retry.

## Response And Failures

- Responses echo the exact scope and request ID. Both client and server validate
  this binding and reject mismatches with `DataLoss`.
- Results contain document/chunk IDs, content, finite backend score, source URI and
  optional location. Repeated document/chunk pairs and more than `top_k` results
  are rejected. Scores are backend-specific, not universal confidence values.
- Source URIs must be stable citation identifiers. Adapters must remove embedded
  credentials and signed access tokens rather than returning authenticated URLs.
- Send/receive limits are 4 MiB. Each content field is limited to 65,536 bytes.
- Calls use at most 30 seconds, respecting shorter caller deadlines. Adapter
  authorization, probes and database/search calls must honor context cancellation;
  Go cannot forcibly stop an implementation that ignores its context.
- The client does not configure application retries. The read-only query request
  ID provides correlation, not mutation idempotency or exactly-once execution.
- Expected gRPC error codes survive the boundary, but backend/transport messages
  and details are discarded. Unknown errors become `Internal`.
- HealthCheck calls the required adapter probe. Constructing a server alone cannot
  report backend readiness. Query execution independently authorizes current scope.

## Pgvector Configuration Package

The independent [pgvector chart](../deploy/helm/kognition-knowledge-pgvector/Chart.yaml)
installs `pgvector.knowledge.kognition.dev/v1alpha1`, kind `PgvectorBackend`.
The [provider-owned Go decoder](../providers/pgvector/config.go) validates the same
typed spec without importing provider configuration into core APIs. Configuration
installation remains the default. The opt-in `adapter.enabled` profile now renders
the existing adapter command, namespace-scoped backend/credential reads,
authenticated peer routes and bounded resources. It requires a reviewed pinned
image, explicit database address, certificates and network routes. It does not
provision a database, credentials, schema or core provider registration.
The core controller's provider-cleanup egress uses the same exact peer selectors
and port as Knowledge Gateway. The adapter's explicit client list must allow
the trusted gateway, controller and ingestion Jobs; it must not include Agent
runtimes.

| Field | Contract |
| --- | --- |
| `host`, `port` | DNS subdomain and integer port 1 through 65535; no URL or connection string |
| `database`, `table` | Lowercase SQL identifiers, at most 63 characters; no schema-qualified table |
| `embeddingDimensions` | 1 through 2000 for KnowledgeBase pgvector indexing; independent of each Memory volume's configured dimension |
| `credentialsSecretRef` | Local Secret `name` plus distinct `usernameKey` and `passwordKey` |
| `tls.mode` | Required `verify-full`; plaintext and certificate-verification bypass are rejected |
| `tls.caSecretRef` | Optional local Secret `name` and `key`; omission selects system roots when the adapter is wired |

All credential references belong to the configuration's namespace. No inline
credentials, arbitrary options or adapter endpoints exist in this API. Kubernetes
prunes unknown fields under its normal CRD handling; strict field validation can
reject them at submission. The Go decoder rejects unknown fields that reach it
directly, and its errors never echo supplied configuration values.

The adapter constructs connection settings through the database driver, reads
Secrets with namespace-scoped access, requires an administrator-approved database
address and verifies TLS hostnames. It rechecks configuration UID/digest before
materializing connections. Queries constrain namespace, KB UID and generation.
Schema validity does not authorize outbound access or prove database connectivity.
The existing plaintext AKS demo database cannot use this contract until database
TLS is configured. Legacy pgvector manifests continue through the existing path.

The CRD ships under the chart's `crds/` directory, outside the core chart. Helm
does not upgrade or remove it on ordinary upgrade/uninstall. Review schema upgrades
separately; never manually remove the CRD while configurations or committed
bindings exist. The supported active-binding uninstall guard remains pending.

## Pgvector Dense Retrieval Component

The [provider querier](../providers/pgvector/query.go) implements
`pluginsdk.KnowledgeQuerier` for dense cosine retrieval. `NewQuerier` accepts a
borrowed pgx pool, validated configuration and a trusted committed scope. It copies
the scope and rejects requests that differ in any binding field. It does not own
or close the pool, read Secrets, establish TLS, resolve a binding or authorize a
claim. The caller must supply the correct pool for the authorized configuration
and refresh the querier when its binding changes. The existing adapter command
wires this component through the authenticated CPI server; deployment acceptance
remains a separate gate.

- Every SQL query includes namespace, KB UID, committed generation, configuration
  UID and digest predicates. Metadata filters cannot override them.
- Table names use validated, driver-quoted identifiers under `public`. The vector
  type and cosine operator are explicitly qualified under `public` as well.
  The database role and `public` schema must be administrator-controlled.
- Filter keys and values, scope fields, vectors and limits use bound parameters.
  `metadata.nested.key` means the literal JSON object key `nested.key`, not a path.
  Missing keys do not match. Ingestion must persist filterable values as strings.
- Vector dimensions must match configuration. Float32 conversion and cosine norm
  bounds reject overflow, underflow and zero vectors. Keyword and hybrid return
  `Unimplemented`; they must not be advertised by this component.
- Results are ordered by cosine distance, then document/chunk IDs. Scores remain
  backend-specific. IDs, content, location, finite scores, duplicate citations and
  the 4 MiB response limit are validated before returning any response.
- Citation URIs allow HTTP(S) and URNs. User information, query strings, fragments
  and other schemes are rejected rather than returned. Ingestion must normalize
  source identifiers before persistence; signed URLs are not valid citations.
- SQL bounds fetched text fields before Go scans them. Invalid rows fail the whole
  response. Queries respect caller cancellation and a maximum 30-second deadline;
  raw database errors and connection details never reach the response.

The component expects a provider-owned table with text columns `namespace`,
`knowledge_base_uid`, `configuration_uid`, `configuration_digest`, `document_id`,
`chunk_id`, `content`, `source_uri`, `location`; bigint `generation`; JSONB
`metadata`; and `public.vector(embeddingDimensions)` `embedding`. Empty location
uses an empty string. Identity/citation columns must be non-null, with a unique key
over the five scope columns plus document/chunk IDs. Null embeddings are excluded.
The [lifecycle component](../providers/pgvector/lifecycle.go) supplies the reviewed
schema SQL, transactional indexing, durable generation receipts and Delete/Retain
cleanup. Apply schema through an administrator-reviewed migration, not through
the runtime database role. Its schema is not a legacy-table compatibility layer.

Index batches remain invisible until commit. An identical operation replay must
match its source/content digest and counts. Changed content, another operation,
untrusted existing rows and retired generations are rejected. Abort rolls back
staged work. Scoped cleanup preserves other generations/tenants and retains a
tombstone receipt so a deleted generation cannot be recreated through replay.
The durable backend locator contains routing and Secret references, not secrets,
and supports cleanup after the configuration object is deleted.

## Governed Text-To-Dense Queries

The gateway frontend accepts `query_text` with declared mode `dense` or `hybrid`.
It rejects simultaneous explicit `text`/`vector`, invalid bounds and unsupported
provider capabilities before embedding. Existing explicit-vector and keyword
requests retain their transport semantics. Runtime bindings carry the declared
claim/class retrieval mode rather than hardcoding keyword search.

The gateway resolves the initiating Agent's current bound claim and the indexed
embedding Model. It uses the shared model authorization boundary and CPI
invocation-profile contracts, checks ModelGrant/AgentPolicy and model quota, and
records authorization before paid work plus usage and completion/failure evidence.
Chat Model assignment checks remain unchanged. Runtime pods receive no database
or embedding-provider credentials and need no direct database/provider egress.

`status.backendBinding.embeddingModelBinding` pins the Model UID/spec digest and
profile UID/digest that produced the vectors. Status-only Model resource-version
updates do not change the embedding space; configuration/profile replacement
fails until explicit reindex/migration. Controller launch/completion verifies and
publishes this binding. Older bindings without it cannot use server-side query
embedding automatically.

Live claim retrieval mode, top-k, metadata filters and minimum score remain
enforced at the gateway. Configured per-claim query usage limits and hybrid
weight controls without an installed enforcer fail explicitly instead of being
ignored. The selected manual fixtures do not declare those optional controls.
The resource webhook converts KnowledgeBaseClaim objects in both directions
before applying claim validation. Inline claims without retrieval overrides
remain valid; unsupported modes, excessive top-k and nonfinite scores are
rejected for both creates and updates.
Provider/embedding/database failures are distinct from valid zero matches.
Provider ingestion normalizes S3/inline identifiers to citation URNs and removes
signed URL queries/fragments from returned HTTP citations.

A bounded opt-in test now covers real Foundry index/query embeddings, PostgreSQL
verified TLS, authenticated CPI indexing/query, a known cited passage and
revocation without another provider call. It used two requests and 37 tokens.
Its resource/authorization state was local test fixtures, not installed-cluster
or production acceptance.

## Pgvector Connection Material

`LoadConnectionMaterial` in the [connection material loader](../providers/pgvector/connection_material.go)
accepts a typed backend object and a Kubernetes reader. It validates the object,
reads credential and optional CA Secrets only in its namespace, and returns
credentials plus a server-authenticating TLS configuration. It does not connect
to the configured database or create a pool.

- Use an uncached API reader after authorizing the backend and its endpoint.
  The loader is not an authorization boundary and does not grant Secret access.
- Missing, deleting or invalid Secrets fail without returning partial material
  or upstream error text. Reads honor cancellation and a ten-second deadline.
- Usernames are nonempty UTF-8 up to 63 bytes; passwords are nonempty UTF-8 up to
  64 KiB. Both reject NUL. Values are preserved without trimming or DSN escaping.
- TLS requires at least 1.2 and verifies the configured hostname. A supplied CA
  bundle replaces system roots and must contain CA certificates. Without a CA
  reference, Go uses system roots. There is no insecure verification option.
- A shared credential/CA Secret is read once. Independent calls reread Secrets
  and return independent material. Separate Secrets are not an atomic snapshot.
- Default formatting redacts material. Its fields still contain credentials;
  callers must never serialize or log them. Rotation of existing pools is pending.

Loader tests use a fake Kubernetes reader and real loopback TCP TLS handshakes
to check hostname, trust, expiration and protocol rejection, namespace isolation,
missing data, rotation and sanitized errors. The adapter's pool factory
constructs explicit driver settings without ambient `PG*` configuration,
plaintext fallback or credential-bearing connection strings. A separate real
PostgreSQL test proves trusted TLS connection and rejects the wrong hostname
and trust root. These are component tests; deployed Kubernetes RBAC,
certificate lifecycle and end-to-end rotation remain separate gates.

## Opt-In Legacy Migration

The [manual shadow bundle](../deploy/examples/knowledge-pgvector/manual-shadow.yaml)
illustrates explicit migration for the Azurite, repository-document and S3
samples without replacing legacy classes, claims or persisted generations.
It lives outside automatic sample discovery and removal. Prepare the reviewed
schema, least-privilege database role, database/provider TLS, core provider
registration, Models, namespace-local credential references and source fixtures
before applying it. The example's `gpt-6-luna` Model must reference the
reviewed chat deployment/profile with the same resource name; query Agents also need the explicit embedding
Model grants supplied by the bundle.

Creating a shadow KB starts its initial embedding/indexing work. Review that
budget, validate each committed binding and actual cited Agent query, and keep
recurring schedules paused. Do not attach a provider binding to a legacy index
by editing status or silently switch existing claims. The
[blocker-resolution plan](development-plans/features/knowledge-provider-execution/remaining-sample-blockers-plan.md)
records qualification evidence and pending deployment gates.

## Verification

Run the focused SDK tests without a database, cluster or cloud subscription:

```powershell
go test ./pkg/pluginsdk -run TestKnowledge -count=1
go test ./internal/k8s/knowledge ./internal/convert -count=1
go test -tags integration ./api/k8s/v1alpha1 -run 'TestKnowledgeBackend.*CRDContract' -count=1
go test ./providers/pgvector -count=1
helm lint deploy/helm/kognition-knowledge-pgvector --strict
```

Tests use generated messages, temporary certificate authorities and real loopback
gRPC/TLS connections. They cover successful retrieval/citations, scope mismatches,
unsupported kinds/operations, peer identity and CA mismatch, anonymous clients,
server-side validation independent of the SDK client, client validation independent
of the SDK server, response size, cancellation, unhealthy probes and sanitized
errors. The adapter is a test implementation, not a pgvector/Search backend.

Binding tests use the controller-runtime fake Kubernetes client. They cover
namespace isolation, committed versus desired generations, failed-refresh
preservation, missing/recreated/changed/deleting objects, scope forgery, sanitized
read failures, registration restrictions and configuration hashing. Conversion
tests check independent copies of binding status. CRD tests inspect checked-in
schema structure; they do not establish live API-server admission or RBAC behavior.

Pgvector configuration tests run the Kubernetes OpenAPI and CEL validators against
the checked-in provider CRD, compare invalid configurations with the Go decoder,
and verify pruning of unknown fields. They cover TLS downgrade, credential scope,
identifier injection, dimensions, Secret key restrictions and sanitized errors.
These local schema tests do not exercise a live API server or PostgreSQL backend.

The separate pgvector retrieval test uses an isolated database with the real vector
extension. Set `KOGNITION_PGVECTOR_TEST_DSN` through a local environment/secret
mechanism to a disposable PostgreSQL server with pgvector available and a role
allowed to create databases and extensions. Run:

```powershell
go test ./providers/pgvector -run 'TestQuerierPostgres|TestConnectionMaterial' -count=1 -v
```

The test creates and drops only its UUID-named database. Without the environment
variable it explicitly skips; with the variable set, connection/schema/extension
errors fail. Never use a production server or application credentials for this
fixture. Local loopback plaintext test connections do not validate production TLS.
Coverage includes a known corpus, higher-ranked out-of-scope rows, old and staged
generations, config replacement/digest changes, top-k, metadata injection attempts,
citation normalization requirements, oversized responses, invalid scores and query
cancellation while waiting on a real table lock. An additional indexed-corpus
case checks scope completeness with many higher-ranked out-of-scope rows and an
HNSW index present; it does not assert a particular PostgreSQL execution plan.

## Isolated Local Manual Acceptance

The 2026-10-27 isolated kind candidate passed actual installed-Agent retrieval
through Knowledge Gateway, trusted generic-profile embeddings, CPI and
TLS-protected pgvector. Native Agents retrieved cited passages from the six-file
S3 fixture, six-file Azurite corpus and four repository documents. AutoGen
retrieved and quoted the repository manifest through the same governed path.
The three scoped indexes contain 23, 23 and 1405 chunks with 1536-dimensional
embeddings. Inline knowledge retains its existing `source_type`/`chunk` shape.

On 2026-10-28, a separate real Azure Blob profile passed authenticated source
listing/download, one ingestion generation and installed native-Agent retrieval.
Its six source documents produced 23 additional 1536-dimensional vectors.
`azure-live-docs-agent` retrieved the actual pricing passage through the gateways
and cited its real Blob HTTPS URL. The administrator-enabled account retains
disabled anonymous/shared-key access; the existing Kognition identity has only
container-scoped Storage Blob Data Reader on the dedicated corpus. The separate
Azurite profile and all earlier indexes remain unchanged.

Live checks denied revoked embedding access, restored authorized retrieval,
rejected out-of-range claim/profile values and prevented cross-namespace claim
binding. Actual Agent Pods had no raw provider/database credentials, no Kubernetes
service-account token and no direct PostgreSQL/provider TCP access. The original
cluster and indexes remained unchanged.

AutoGen requires a configured `runtimeRecovery.existingSecret` with explicit
per-workspace encryption keys before guarded activation. The opt-in manual
Agents use a declared 180-second execution budget. Keep token limits, grants and
admission enforcement enabled. Azurite establishes emulator coverage only.
Real Blob acceptance used the newly enabled public endpoint; it does not
establish access to the older private-only account or private-endpoint routing.
These results provide local manual acceptance, not production or AKS clearance.

## Remaining Gates

- Dynamic administrator-owned registration and endpoint binding; deployed RBAC,
  certificate issuance/rotation/revocation and restricted network-route validation.
- Per-claim query-usage accounting and hybrid-weight enforcement beyond the
  current fail-closed behavior; installed-cluster verification of durable
  query allowed/completed/failed audit records.
- Deployed verification of controller-only binding publication, authorization
  freshness, concurrent lifecycle operations and recovery after partial failures.
- Production pgvector deployment and administrator-reviewed legacy cutover beyond
  the isolated local shadow migration. Native Azure AI Search execution is
  implemented but its live acceptance has not been run in this local review.
- Full backend lifecycle/migration conformance and deployed adapter/AKS validation,
  including cleanup and retained ownership across configuration deletion.

The broader AKS work remains tracked in
[the deployment guide](../deploy/aks/README.md).