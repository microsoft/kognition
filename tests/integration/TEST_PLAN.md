# Integration Test Plan — Kognition Platform

## Purpose

Validate Functional Requirements (FRs) from `BACKLOG.md` by exercising the real
Kognition system code with deterministic, reproducible tests. These tests instantiate
the actual apiserver, admission chain, reconcilers, CPI registry, and runtime with
in-memory implementations — proving correctness without external dependencies.

All files use the `//go:build integration` build tag.

---

## Layout

```
tests/integration/
├── TEST_PLAN.md            ← this file
├── helpers_test.go         ← shared test harness, mock CPI driver, mock runtime, seed helpers
├── run_tests.ps1           ← PowerShell runner
├── api_crud_test.go        ← Core API CRUD, revisions, events, health
├── admission_test.go       ← Validation, defaulting, quota enforcement, SecretRef checks
├── lifecycle_test.go       ← Agent/AgentInstance reconciliation, dispatch, autoscaling
├── models_cpi_test.go      ← Model health, capabilities, and provider routing
├── tools_mcp_test.go       ← Tool HTTP health probes, thresholds, and recovery
├── memory_embedding_test.go ← Environment-configured memory-service embedding flow
├── gateway_test.go         ← IntentGateway goal submission and routing
├── governance_test.go      ← Approval decisions and governance broker
├── memory_test.go          ← MemoryRecord Right-To-Be-Forgotten and derived-record cascade
├── observability_test.go   ← PII redaction, cost attribution, audit log, CloudEvents, retention
├── watch_test.go           ← SSE watch streams: create events, multi-event ordering, headers
└── chat_test.go            ← End-to-end chat: session create/reuse, error paths
```

---

## Current Test Coverage

The catalog below lists tests actually present in the suite. Items in the **Planned**
section at the bottom track FRs that are not yet exercised here.

### 1. Core API CRUD (`api_crud_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestAPI_CreateAgent | FR-0001 | POST creates resource, 201 returned |
| TestAPI_CreateDuplicate | FR-0001 | Duplicate POST rejected |
| TestAPI_GetAgent | FR-0002 | GET retrieves by name |
| TestAPI_GetNotFound | FR-0002 | GET on missing returns 404 |
| TestAPI_ListAgents | FR-0003 | LIST returns resources in namespace |
| TestAPI_UpdateAgent | FR-0004 | PUT with resourceVersion succeeds |
| TestAPI_UpdateConflict | FR-0004 | Stale resourceVersion returns 409 |
| TestAPI_DeleteAgent | FR-0005 | DELETE removes resource |
| TestAPI_DeleteNotFound | FR-0005 | DELETE on missing returns 404 |
| TestAPI_HealthEndpoints | FR-0009 | `/healthz`, `/readyz` return 200 |
| TestAPI_AuthenticationProtectsResourcesButNotHealth | NFR-0002 | API resources require a valid bearer token while health endpoints remain probe-accessible |
| TestAPI_MultiResourceTypes | FR-0010 | CRUD works across multiple resource kinds |
| TestAPI_Revisions | FR-0007 | Revisions created, listed, retrieved by number |
| TestAPI_Events | FR-0008 | Events appended and listed for an instance |

### 2. Admission (`admission_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestAdmission_MissingName | FR-0100 | Resource without name rejected |
| TestAdmission_InvalidJSON | FR-0100 | Malformed JSON rejected |
| TestAdmission_SecretRefValidation | FR-0104 | SecretRef format and references validated |
| TestAdmission_ToolValidator | FR-0101 | Tool spec validation (protocol, endpoint) |
| TestAdmission_LimitRangeDefaulter | FR-0454 | Defaults injected when agent has no limits |
| TestAdmission_QuotaEnforcer | FR-0453 | Exceeding quota rejected |
| TestAdmission_CognitivePolicyDeniesOnlyItsNamespace | FR-0452 | Namespace-scoped OPA policy denies a matching API create |
| TestAdmission_CognitivePolicyRespectsMatchOperations | FR-0452 | OPA policy evaluates only its declared CREATE or UPDATE operations |

### 3. Lifecycle & Reconciliation (`lifecycle_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestLifecycle_PendingToRunning | FR-0200, FR-0201 | Agent transitions Pending → Initializing → Ready → Running |
| TestLifecycle_RunningToCompleted | FR-0210 | Job-style agents complete and persist final status |
| TestLifecycle_DispatchFailure | FR-0202 | Dispatch error surfaces failure on the instance |
| TestLifecycle_AutoscalerScaleUp | FR-0230 | Autoscaler increases replicas under load |

### 4. Intent Gateway (`gateway_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestGateway_SubmitGoal_Success | FR-0400 | Goal matches a route, Solution created |
| TestGateway_SubmitGoal_GatewayNotFound | FR-0400 | Unknown gateway returns 404 |
| TestGateway_SubmitGoal_GatewayNotReady | FR-0400 | Non-Ready gateway rejects submission |
| TestGateway_SubmitGoal_EmptyGoal | FR-0400 | Empty goal returns 400 |
| TestGateway_SubmitGoal_NoRouteMatched | FR-0400 | Unmatched goal returns 404 |

### 5. Governance (`governance_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestGovernance_ApprovalDecision_NoBroker | FR-0313 | Decision without broker fails cleanly |
| TestGovernance_ApprovalDecision_Success | FR-0313 | Approval decision recorded, agent unblocked |
| TestGovernance_ApprovalDecision_NotFound | FR-0313 | Decision on unknown request returns 404 |
| TestGovernance_ApprovalDecision_InvalidState | FR-0313 | Decision in terminal state rejected |
| TestGovernance_ApprovalDecision_InvalidJSON | FR-0313 | Malformed decision payload rejected |

### 6. Memory (`memory_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestMemory_RTBF_SingleRecord | FR-0255 | Right-To-Be-Forgotten removes a single record |
| TestMemory_RTBF_CascadeDerived | FR-0255 | RTBF cascades to derived records |
| TestMemory_RTBF_NotFound | FR-0255 | RTBF on missing record returns 404 |
| TestMemory_RTBF_DifferentMemoryRef | FR-0255 | RTBF scoped to the correct MemoryRef |

### 7. Observability (`observability_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestObs_PIIRedaction_Email | FR-0700 | Email addresses redacted |
| TestObs_PIIRedaction_SSN | FR-0700 | SSNs redacted |
| TestObs_PIIRedaction_CreditCard | FR-0700 | Credit-card numbers redacted |
| TestObs_PIIRedaction_StrictMode | FR-0700 | Strict mode redacts additional patterns |
| TestObs_PIIRedaction_NoneMode | FR-0700 | None mode disables redaction |
| TestObs_CostAttribution | FR-0701 | Session costs attributed to workspace/agent |
| TestObs_AuditLog_AppendAndQuery | FR-0702 | Audit entries appended and queryable |
| TestObs_CloudEvents_Format | FR-0703 | Emitted events conform to CloudEvents spec |
| TestObs_Retention_SweepResult | FR-0704 | Retention sweep removes expired entries |

### 8. Watch / SSE (`watch_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestWatch_ReceivesCreateEvent | FR-0006 | Watch stream receives ADDED event on create |
| TestWatch_SSEFormat | FR-0006 | SSE frames are well-formed |
| TestWatch_ContentTypeSSE | FR-0006 | Response uses `text/event-stream` |
| TestWatch_MultipleEvents | FR-0006 | Multiple events delivered in order |
| TestWatch_StoreNotifiesOnCreate | FR-0006 | Store layer notifies watchers on create |

### 9. Chat End-to-End (`chat_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestChat_Success | E2E | Chat creates session, calls CPI, returns response |
| TestChat_SessionReuse | E2E | Subsequent messages reuse the same session |
| TestChat_AgentNotFound | E2E | Chat to unknown agent returns 404 |
| TestChat_AgentNotReady | E2E | Chat to non-Ready agent returns 503 |
| TestChat_EmptyMessage | E2E | Empty message rejected with 400 |
| TestChat_ModelNotFound | E2E | Missing model returns 404 |
| TestChat_DriverError | E2E | CPI driver error surfaces as 502 |
| TestChat_NoCPIRegistry | E2E | Missing CPI registry surfaces a clean error |
| TestChat_ModelGrantDeniesByDefaultAndAllowsExactGrant | FR-0405 | Chat reaches CPI only after an exact ModelGrant authorizes the Agent and Model |

### 10. Models & CPI (`models_cpi_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestModelsCPI_HealthyProviderReportsAvailability | FR-0304 | Valid credentials reach the provider and healthy status reports capabilities |
| TestModelsCPI_InvalidEndpointReportsUnavailable | FR-0304 | Invalid endpoint persists unavailable status and diagnostic |
| TestModelsCPI_RoutesModelsToIndependentProviders | FR-0304 | Agents route completion requests independently through Models and ModelClasses |

### 11. Memory Service (`memory_embedding_test.go`)
| Test | Trace | What it validates |
|------|-----|-------------------|
| TestMemoryService_EmbedsStoreAndSearch | US-0084 | Environment-configured Foundry-compatible embeddings flow through memory-service store and semantic search endpoints |

### 12. Tools & MCP (`tools_mcp_test.go`)
| Test | FR | What it validates |
|------|-----|-------------------|
| TestToolsMCP_HTTPHealthProbeDegradesAndRecovers | FR-0310 | API-created Tool executes HTTP GET probes and persists threshold-based degradation and recovery through the production reconciler |
| TestToolsMCP_TCPHealthProbeDegradesWhenEndpointCloses | FR-0310 | API-created Tool executes TCP connect probes and persists endpoint reachability loss through the production reconciler |
| TestToolsMCP_PingHealthProbeDegradesOnRPCError | FR-0310 | API-created MCP Tool executes JSON-RPC ping probes and persists RPC health failures through the production reconciler |
| TestToolsMCP_UnsupportedRuntimeProtocolsFailClosed | FR-0312 | API-created Browser and Shell Tools remain unavailable while no production runtime executor exists |

---

## Planned (not yet implemented)

The following FR areas have plan entries but no integration tests yet. Add them as
the implementations land:

- **Tools & MCP** (FR-0300..FR-0312): MCP schema discovery, OpenAPI compilation,
  digest verification, sandbox isolation, tool grants, invocation tracing, and
  exec health probes. The exec argv contract and reconciler runner boundary have unit coverage; tagged integration awaits a production sandbox runner. MCP ping, HTTP GET, and TCP health probing are covered above.
- **Models & CPI** (FR-0304): Model-backed embedding routing through a production
  memory-service boundary; registration, health, completion, and provider routing are covered above.
- **Workflows** (FR-0250..FR-0262): linear and DAG execution, approval gates,
  budget enforcement, failure cascades.
- **Identity & Workspace** (FR-0401..FR-0406): workspace isolation, SPIFFE
  identity provisioning, RBAC bindings, and cross-namespace grants. ModelGrant
  enforcement has tagged integration coverage.
- **Cognitive Admission Policy / OPA** (FR-0452): mutation behavior and
  Kubernetes webhook coverage. Tagged integration covers denial, namespace
  isolation, and operation filtering.

---

## Implementation Strategy

1. **Shared helpers** (`helpers_test.go`): Full memStore implementation, mock CPI driver
   (returns canned completions), mock Runtime, test server factory that wires everything.

2. **Each test file** is self-contained — creates resources, exercises logic, asserts outcomes.

3. **No external dependencies** — all tests use in-memory store, mock CPI, mock runtime.

4. **Build tag**: `//go:build integration` — separated from unit tests but runnable locally.

5. **Deterministic**: No sleeps, no flaky timing — uses channels and synchronous reconcile loops.

---

## Running

```powershell
# Run all integration tests
.\tests\integration\run_tests.ps1

# Or manually:
go test -tags=integration -v -count=1 ./tests/integration/...
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./tests/integration/run_tests.ps1
# Or invoke the same test package directly:
go test -tags=integration -v -count=1 ./tests/integration/...
```
