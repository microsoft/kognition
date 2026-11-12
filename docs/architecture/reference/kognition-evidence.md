# Kognition review and abstraction map

The reference uses Kognition as architectural evidence, not as the shape that
every enterprise platform must reproduce. The review covered the repository's
major domains through inventory, public contracts, representative implementation
paths, tests, deployment definitions, and operational documentation.

- Original implementation review: 2026-10-08.
- Original repository HEAD observed: `03dacb4`, with uncommitted runtime changes.
- Method: breadth-first architecture review with focused traces across execution,
  trust, data, provider, and operations boundaries.
- Limits: representative source/test inspection, not a line-by-line audit of
  every file, vulnerability assessment, load test, or production certification.
- Application tests, live providers, cluster installation, and deployment
  acceptance were **not run** for this documentation task.
- Credential/environment files and local binary/coverage artifacts were excluded.
  Existing runtime edits were preserved.

This is a **dated evidence snapshot**, not a live feature-status dashboard.
The later clarity pass observed HEAD `2666be6` and concurrent working-tree
changes; it did not repeat the full implementation or deployed acceptance review.
Relative source links navigate the current checkout and can therefore show
newer behavior. Consult exact test receipts and their artifact/environment scope
before using any operational guide as a current conformance claim.

## Resource inventory

The [canonical registry](../../../api/v1alpha1/register.go) contained **50
registered kinds** at review time. Its native-kind predicate excludes the two
standalone control-plane role kinds; chart-internal records are a separate
inventory. The resource model below informed responsibility coverage rather
than a one-box-per-kind reference design.

| Domain | Canonical resources | Generic abstraction |
| --- | --- | --- |
| Execution | Agent, AgentInstance, AgentJob, AgentService, AgentAutoscaler, ModelAutoscaler | Definition, execution instance, job, service binding, workload/model capacity controls |
| Intent and workflow | Solution, IntentGateway, Workflow, WorkflowRun, ApprovalDecision | Goal intake, compiled plan, workflow definition/run, workflow approval |
| Memory | Memory, MemoryClass, MemoryClaim, MemoryVolume, MemoryRecord, MemoryGrant | Data class, scoped allocation, records, access policy |
| Knowledge | KnowledgeBaseClass, KnowledgeBase, KnowledgeBaseClaim, KnowledgeBaseIndexRun, KnowledgeBaseIndexSchedule | Source-derived data, authorized binding, refresh lifecycle |
| Models | ModelClass, Model, ModelGrant | Provider class, approved inference binding, authorization |
| Tools | ToolClass, Tool, ToolInvocationRun, ToolGrant | Execution tier, capability contract, effect record, authorization |
| Identity and isolation | Workspace, AgentIdentity, AgentRole, AgentRoleBinding, ControlPlaneRole, ControlPlaneRoleBinding, AgentCommunicationPolicy, CrossNamespaceGrant | Tenant scope, workload and human authority, explicit boundary grants |
| Governance | AgentPolicy, CognitiveAdmissionPolicy, CognitiveQuota, CognitiveLimitRange | Configuration/runtime policy, aggregate and per-action limits |
| Autonomy | AutonomyClass, TrustProfile, AutonomyReview, RuntimeApproval | Reviewed authority ceiling, behavioral evidence, promotion review, exact action decision |
| Evaluation | Evaluation, EvaluationRun, EvaluationDataset | Evaluation definition, execution, versioned test corpus |
| Configuration | Prompt, SecretRef | Versioned instructions and controlled credential indirection |

## Evidence families

Links identify inspected source areas and representative contracts. Test files
are evidence of intended verification coverage, not passing receipts from this
review. Historical ADRs record decisions at their dates, not current completion.

| ID | Inspected responsibility | Representative evidence | Reference consequence |
| --- | --- | --- | --- |
| K01 | CLI, authoring, dashboard, interactive invocation | [Studio](../../../cmd/kognitionctl/cmd/studio.go), [interactive test routes](../../../cmd/kognitionctl/cmd/studio_invoke.go), [dashboard](../../../cmd/kognitionctl/cmd/dashboard.go), [interactive tests](../../../cmd/kognitionctl/cmd/studio_invoke_test.go) | Distinguish non-executing review from effect-bearing sandbox tests; apply production-equivalent authorization to tests. |
| K02 | Agent/jobs, workflows, dataflow, retries, dynamic compilation | [agent contract](../../../api/v1alpha1/agent.go), [workflow contract](../../../api/v1alpha1/workflow.go), [workflow engine](../../../internal/controller/workflow_reconciler.go), [dataflow](../../../internal/controller/workflow_dataflow.go), [retry policy](../../../internal/controller/workflow_retry.go), [solution reconciler](../../../internal/controller/solution_reconciler.go) | Separate definitions, runs, steps, ownership, bounded planning and typed results. |
| K03 | Desired state, admission, conversion and plugin registration | [API server](../../../internal/apiserver/server.go), [controller manager](../../../internal/controller/manager.go), [conversions](../../../internal/convert), [plugin host](../../../internal/plugin/host.go), [plugin wire](../../../internal/plugin/wire.go) | Separate management from runtime and version every adapter boundary. |
| K04 | Runtime contract and concrete workers | [runtime interface](../../../internal/runtime/contract.go), [wire contract](../../../proto/kognition/v1alpha1/runtime.proto), [worker selection](../../../runtime/agent/main.py), [governed adapter](../../../runtime/agent/autogen_graph.py), [runtime conformance](../../../internal/conformance/runtime_suite.go) | Explicit create/step/status/stream/cancel/checkpoint contracts; support and concurrency vary by runtime profile. |
| K05 | Runtime gateways and current authorization | [tool gateway](../../../internal/toolgateway/gateway.go), [model gateway](../../../internal/modelgateway/server.go), [peer gateway](../../../internal/agentgateway/server.go), [knowledge gateway](../../../internal/knowledgegateway/server.go), [namespace resolver](../../../internal/namespace/resolver.go), [policy evaluator](../../../internal/opa/engine.go) | Shared invariant with specialized target checks; discovery or admission alone does not authorize execution. |
| K06 | Durable approval, execution claims and human permissions | [approval execution](../../../internal/runtimeapproval/execution.go), [control-plane authorizer](../../../internal/controlplaneauth/authorizer.go), [authorizer tests](../../../internal/controlplaneauth/authorizer_test.go), [approval/recovery decision](../../adr/0014-runtime-autonomy-gate-completion-and-recovery.md), [trust evidence contract](../../adr/0011-trust-decision-contract.md) | Keep approval separate from authentication, authorization and effect ownership; preserve uncertain outcomes. |
| K07 | Durable storage and checkpoint recovery | [store interface](../../../internal/store/interface.go), [recovery contract](../../../internal/store/runtime_recovery.go), [persistent recovery](../../../internal/store/postgres/runtime_recovery.go), [approval recovery](../../../internal/store/postgres/runtime_approval_recovery.go), [recovery tests](../../../internal/store/postgres/runtime_approval_recovery_test.go), [current runtime profiles](../../runtime-binding-profiles.md) | Separate sensitive checkpoint expiry from task/effect claims and replay-denial retention. |
| K08 | Quota and capacity controls | [quota/limit API](../../../api/v1alpha1/governance.go), [autoscaler](../../../internal/controller/agent_autoscaler_reconciler.go), [autoscaler tests](../../../internal/controller/agent_autoscaler_reconciler_test.go), [persistent store](../../../internal/store/postgres/store.go) | Durable accounting, bounded concurrency and fair capacity need explicit contracts beyond replica counts. |
| K09 | Memory and telemetry privacy | [memory backend](../../../internal/memory/backend.go), [memory service](../../../internal/memory/service/handler.go), [persistent adapter](../../../internal/memory/pgvector/adapter.go), [service contract tests](../../../internal/memory/service/handler_contract_test.go), [redaction](../../../internal/observability/redaction.go) | Memory scope/provenance, authorized writes and content minimization are independent responsibilities. |
| K10 | Knowledge lifecycle and independent providers | [knowledge guide](../../knowledge-base.md), [provider guide](../../knowledge-providers.md), [ingestion spool](../../../internal/ingestion/provider_spool.go), [provider query contract](../../../pkg/pluginsdk/knowledge.go), [index implementation](../../../providers/azuresearch/index.go), [cleanup](../../../providers/azuresearch/cleanup.go), [provider configuration](../../../providers/pgvector/config.go) | Stage/verify/publish generations, trusted scope, bounded batches, capability negotiation and confirmed cleanup. |
| K11 | Evaluation, model binding and artifact approval | [evaluation API](../../../api/v1alpha1/evaluation.go), [evaluation reconciler](../../../internal/controller/evaluation_reconciler.go), [artifact signing](../../artifact-signing.md), [runtime/model binding profiles](../../runtime-binding-profiles.md) | Version datasets and graders; distinguish signed configuration from immutable remote model weights and from evaluated behavior. |
| K12 | Workload identity and external protocols | [workload identity source](../../../internal/runtimeauth/spiffe.go), [identity proxy entry point](../../../cmd/kognition-workload-identity-proxy/main.go), [MCP client](../../../internal/mcp/client.go), [provider interface](../../../internal/cpi/driver.go), [knowledge SDK](../../../pkg/pluginsdk/knowledge.go) | Identity attribution differs from attestation; bind peer and workload incarnation, negotiate protocol versions, keep credentials outside workers. |
| K13 | Telemetry, audit and operational projections | [observability guide](../../observability.md), [exporter](../../../internal/observability/exporter.go), [workflow projection](../../../internal/observability/workflow_projection.go), [projection tests](../../../internal/observability/workflow_projection_test.go) | Distinguish sampled telemetry from retained evidence and authoritative accounting; expose unavailable observations honestly. |
| K14 | Deployment, isolation and acceptance gates | [Kubernetes manager](../../../internal/k8s/controller/manager.go), [charts](../../../deploy/helm/kognition), [installation boundary](../../adr/0016-installation-trust-boundary.md), [change-based gates](../../development-gates.md), [CI](../../../.github/workflows/ci.yml), [integration plan](../../../tests/integration/TEST_PLAN.md), [end-to-end tests](../../../e2e) | Installation, source validation, artifact release and activation have distinct assurance scopes; require target-specific evidence. |

The repository inventory also included [API schemas](../../../api/schemas),
[Kubernetes types](../../../api/k8s), [protocol definitions](../../../proto),
[provider packages](../../../providers), [deployment assets](../../../deploy),
[infrastructure](../../../infra), [samples](../../../samples), and
[validation tooling](../../../tools). Representative checks were distributed
across these areas; generated schemas, fixtures, and every test body were not
individually audited.

## Distinctions preserved during synthesis

1. **Documentation drift is visible.** The root guide and
   [resource interaction map](../../resource-interactions.md) contain differing
   historical counts and ToolGrant descriptions. The registry and
   [shipped ToolGrant definition](../../../deploy/helm/kognition/crds/kognition.dev_toolgrants.yaml)
   were used for the inventory. These unrelated documents were not rewritten.
2. **Accepted design is not completion.** The
   [checkpoint ADR](../../adr/0024-runtime-checkpoint-recovery.md) describes work
   as open at acceptance, while the newer
   [runtime profile](../../runtime-binding-profiles.md) and K07 source record
   local implementation at the original review. Later operational updates may
   add installed acceptance; this snapshot does not independently verify their
   scope. Local tests and specific installed receipts are distinct evidence.
3. **Identity metadata is not cryptographic proof.** The older
   [security guide](../../security.md) describes informational workload URIs;
   K12 code and runtime profiles at the original review included an opt-in attested transport.
   The reference requires explicit identity mode and deployment evidence rather
   than extrapolating one path to every mode.
4. **A policy-shaped API is not a full policy engine.** K05's
   [local interpreter](../../../internal/opa/interpreter.go) is a constrained
   evaluator. The reference requires defined, tested semantics and failure
   behavior without prescribing a policy language or claiming full engine parity.
5. **Stateful workers constrain scaling.** The originally inspected profiles described
   pinned single-replica stateful worker operation. The reference's cell and
   ownership model is a target pattern, not a claim that every adapter supports
   arbitrary horizontal scaling.
6. **Integration must be checked in code.** Provider query/index/cleanup paths
   exist even when older runbooks describe them as future work. Their existence
   is not proof of live-provider or production acceptance.
7. **Published metrics need evidence.** K13 distinguishes retained projections
   and export pipelines. The reference extends this into independent operational,
   audit, quality and accounting contracts rather than treating a dashboard as
   the authoritative state.

## Enterprise additions requiring explicit assessment

The source review does not establish complete implementation of global
multi-region failover, pooled-tenant fairness at target scale, all protocol
versions, end-to-end privacy erasure across backups, regulatory compliance,
calibrated behavioral autonomy, or all runtime/provider combinations.

Those capabilities remain in the [reference matrix](capabilities.md) because
they must be assessed for enterprise use. They are **not asserted defects or
confirmed missing features** solely because representative inspection did not
prove them. Likewise, deployment approval and operational SLO attainment require
fresh, environment-specific evidence.
