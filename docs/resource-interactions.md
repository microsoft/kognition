# Kognition Resource Interaction Map

> **Status:** Current implementation map, reviewed 2026-08-31.
> **Scope:** All 47 canonical `kognition.dev/v1alpha1` resource kinds.

This map answers how Kognition resources reference, create, govern, and feed
status to one another. It includes the native Kubernetes workloads created by
Kognition at the outer boundary. Those native objects are not part of the
43-kind canonical count.

## Relationship Legend

| Line | Meaning |
| --- | --- |
| `A --> B` | `A.spec` references, routes to, or binds `B`. |
| `A ==> B` | The controller for `A` creates or owns `B`. |
| `A -.-> B` | `A` selects, governs, authorizes, or mutates `B`. |

Kubernetes mode installs a CRD for each canonical kind, including `ToolGrant`.
Embedded `AgentPolicy.spec.toolGrants` entries remain available as a runtime
authorization compatibility path.

## Domain Overview

The overview keeps domain boundaries visible. The focused maps below show the
individual resources and exact edges.

```mermaid
flowchart LR
  Intent["Intent and orchestration<br/>5 resources"]
  Execution["Agent execution<br/>6 resources"]
  MemoryPlane["Memory plane<br/>6 resources"]
  KnowledgePlane["Knowledge plane<br/>5 resources"]
  Evaluation["Evaluation and quality<br/>3 resources"]
  Capabilities["Models, tools, config<br/>8 resources"]
  Identity["Identity and access<br/>6 resources"]
  Governance["Governance and admission<br/>4 resources"]
  Autonomy["Autonomy and trust<br/>3 resources"]
  Kubernetes["Native Kubernetes<br/>Deployment, Service, Job"]

  Intent -->|starts work| Execution
  Intent -->|declares run memory| MemoryPlane
  Execution -->|uses| MemoryPlane
  Execution -->|searches| KnowledgePlane
  Evaluation -->|creates test jobs| Execution
  Evaluation -->|grades with| Capabilities
  Execution -->|invokes| Capabilities
  Governance -.->|constrains| Intent
  Governance -.->|constrains| Execution
  Identity -.->|authorizes and isolates| Execution
  Identity -.->|controls cross namespace refs| MemoryPlane
  Identity -.->|controls cross namespace refs| KnowledgePlane
  Identity -.->|controls cross namespace refs| Capabilities
  Autonomy -.->|gates actions| Execution
  Autonomy -.->|restricts tools| Capabilities
  Execution ==>|materializes| Kubernetes
  KnowledgePlane ==>|launches ingestion| Kubernetes
```

## Intent And Agent Lifecycle

```mermaid
flowchart LR
  subgraph intent["Intent and workflow"]
    Solution["Solution"]
    IntentGateway["IntentGateway"]
    Workflow["Workflow"]
    WorkflowRun["WorkflowRun"]
    ApprovalDecision["ApprovalDecision"]
  end

  subgraph runtime["Agent execution"]
    Agent["Agent"]
    AgentJob["AgentJob"]
    AgentInstance["AgentInstance"]
    AgentService["AgentService"]
    AgentAutoscaler["AgentAutoscaler"]
    ModelAutoscaler["ModelAutoscaler"]
  end

  Model["Model"]
  Tool["Tool"]
  MemoryClaim["MemoryClaim"]
  KnowledgeBaseClaim["KnowledgeBaseClaim"]
  Prompt["Prompt"]
  AutonomyClass["AutonomyClass"]
  Deployment["Deployment<br/>(apps/v1)"]
  Service["Service<br/>(v1)"]

  Solution -->|planner model| Model
  Solution ==>|compiles and owns| Agent
  IntentGateway -->|routes intent| Agent
  IntentGateway -->|routes intent| Workflow
  Workflow -->|agent step| Agent
  Workflow -->|tool step| Tool
  WorkflowRun -->|snapshots and executes| Workflow
  WorkflowRun ==>|creates step jobs| AgentJob
  WorkflowRun ==>|creates run or step memory| MemoryClaim
  ApprovalDecision -->|votes on gate| WorkflowRun

  Agent -->|modelRef| Model
  Agent -->|instructions.ref| Prompt
  Agent -->|tools| Tool
  Agent -->|memory claims| MemoryClaim
  Agent -->|knowledge claims| KnowledgeBaseClaim
  Agent -->|autonomyClassRef| AutonomyClass
  Agent -->|companions| Agent
  Agent ==>|maintains replicas| AgentInstance
  AgentJob -->|runs agent| Agent
  AgentJob -->|optional memory| MemoryClaim
  AgentJob ==>|creates attempts| AgentInstance
  AgentInstance -->|invokes revision| Agent
  AgentService -->|exposes revision| Agent
  AgentService -.->|counts readiness| AgentInstance
  AgentAutoscaler -.->|updates replicas| Agent
  ModelAutoscaler -.->|updates modelRef| Agent
  ModelAutoscaler -->|preferred model| Model
  Agent ==>|materializes| Deployment
  Agent ==>|materializes| Service
```

## Memory Plane

```mermaid
flowchart LR
  Agent["Agent"]
  WorkflowRun["WorkflowRun"]
  Model["Model"]
  MemoryBackend[("Memory backend")]

  subgraph memory["Memory resources"]
    MemoryClass["MemoryClass"]
    MemoryClaim["MemoryClaim"]
    MemoryVolume["MemoryVolume"]
    Memory["Memory"]
    MemoryRecord["MemoryRecord"]
    MemoryGrant["MemoryGrant"]
  end

  Agent -->|memory claims| MemoryClaim
  WorkflowRun ==>|creates run or step claims| MemoryClaim
  MemoryClaim -->|memoryClassName| MemoryClass
  MemoryClaim ==>|creates pending volume| MemoryVolume
  MemoryVolume -->|claimRef| MemoryClaim
  MemoryVolume -->|memoryClassName| MemoryClass
  MemoryClaim ==>|provision / deprovision| MemoryBackend
  MemoryVolume -.->|backendHandle| MemoryBackend
  Memory -->|claimRef| MemoryClaim
  MemoryRecord -->|memoryRef| Memory
  MemoryClass -->|summarization model| Model
  MemoryGrant -.->|authorizes| Agent
  MemoryGrant -.->|authorizes| MemoryClaim
```

## Knowledge Plane

```mermaid
flowchart LR
  Agent["Agent"]
  Model["Model"]
  SecretRef["SecretRef"]
  IngestJob["Ingestion Job<br/>(batch/v1)"]
  VectorStore[("pgvector")]

  subgraph knowledge["Knowledge resources"]
    KnowledgeBaseClass["KnowledgeBaseClass"]
    KnowledgeBase["KnowledgeBase"]
    KnowledgeBaseClaim["KnowledgeBaseClaim"]
    KnowledgeBaseIndexRun["KnowledgeBaseIndexRun"]
    KnowledgeBaseIndexSchedule["KnowledgeBaseIndexSchedule"]
  end

  Agent -->|knowledge claims| KnowledgeBaseClaim
  KnowledgeBaseClaim -->|knowledgeBaseName| KnowledgeBase
  KnowledgeBase -->|knowledgeBaseClassName| KnowledgeBaseClass
  KnowledgeBase -->|embedding override| Model
  KnowledgeBase -->|blob credentials| SecretRef
  KnowledgeBaseClass -->|default embedding model| Model
  KnowledgeBaseClass -->|vector credentials| SecretRef
  KnowledgeBaseIndexRun -->|reindexes| KnowledgeBase
  KnowledgeBaseIndexSchedule -->|targets| KnowledgeBase
  KnowledgeBaseIndexSchedule ==>|creates on cron tick| KnowledgeBaseIndexRun
  KnowledgeBaseIndexRun ==>|launches external ingest| IngestJob
  IngestJob -->|writes chunks| VectorStore
  Agent -->|knowledge_search| VectorStore
```

## Evaluation And Quality

```mermaid
flowchart LR
  Agent["Agent"]
  AgentJob["AgentJob"]

  subgraph evaluation["Evaluation resources"]
    EvaluationDataset["EvaluationDataset"]
    Evaluation["Evaluation"]
    EvaluationRun["EvaluationRun"]
  end

  Evaluation -->|datasetRef| EvaluationDataset
  Evaluation ==>|creates immutable run| EvaluationRun
  EvaluationRun -->|reads and records digest| EvaluationDataset
  EvaluationRun -->|targetRef| Agent
  EvaluationRun ==>|creates one target job per case| AgentJob
  EvaluationRun ==>|creates judge job per llm_judge grader| AgentJob
  AgentJob -->|runs target or judge| Agent
```

EvaluationRuns record the SHA-256 digest of the dataset specification as 64
lowercase hexadecimal characters. A changed dataset fails an in-progress run
closed before additional target or judge jobs are created.

When upgrading from SHA-1 dataset digests, non-terminal runs with legacy digests
fail with `DatasetDigestUpgradeRequired`. Create a new EvaluationRun against the
reviewed dataset; do not overwrite the old run's digest to bypass the check.
Completed historical runs are left unchanged. Generated names longer than 63
characters now use a truncated SHA-256 suffix, so long EvaluationRun, target job,
judge job and Solution promotion run names differ from pre-upgrade names.
Drain in-flight evaluations before upgrading where possible, and inspect retained
jobs and promotion runs before rerunning to avoid unintended duplicate model calls.

## Models, Tools, And Configuration

```mermaid
flowchart LR
  Agent["Agent"]
  AgentPolicy["AgentPolicy"]
  ModelAutoscaler["ModelAutoscaler"]
  ExternalSecret["Kubernetes Secret or<br/>external secret store"]

  subgraph modelResources["Model resources"]
    ModelClass["ModelClass"]
    Model["Model"]
    ModelGrant["ModelGrant"]
  end

  subgraph toolResources["Tool resources"]
    ToolClass["ToolClass"]
    Tool["Tool"]
    ToolGrant["ToolGrant"]
  end

  subgraph configuration["Configuration"]
    Prompt["Prompt"]
    SecretRef["SecretRef"]
  end

  Agent -->|modelRef| Model
  Agent -->|instructions.ref| Prompt
  Agent -->|tools| Tool
  Model -->|className| ModelClass
  Model -->|credentialRef| SecretRef
  ModelGrant -.->|authorizes| Agent
  ModelGrant -.->|authorizes| Model
  ModelAutoscaler -->|preferred model| Model
  Tool -->|className| ToolClass
  Tool -->|secretRefs| SecretRef
  ToolGrant -.->|selects or references| Agent
  ToolGrant -.->|authorizes| Tool
  AgentPolicy -.->|embedded grant compatibility| Tool
  SecretRef -->|resolves value| ExternalSecret
```

## Identity, Governance, And Autonomy

```mermaid
flowchart LR
  Agent["Agent"]
  Tool["Tool"]
  ToolClass["ToolClass"]
  Model["Model"]
  Memory["Memory"]
  KnowledgeBase["KnowledgeBase"]
  Solution["Solution"]
  ToolGrant["ToolGrant"]
  MatchedWrite["Matched Kognition<br/>resource write"]

  subgraph access["Identity and access"]
    Workspace["Workspace"]
    AgentIdentity["AgentIdentity"]
    AgentRole["AgentRole"]
    AgentRoleBinding["AgentRoleBinding"]
    AgentCommunicationPolicy["AgentCommunicationPolicy"]
    CrossNamespaceGrant["CrossNamespaceGrant"]
  end

  subgraph policy["Governance and admission"]
    AgentPolicy["AgentPolicy"]
    CognitiveAdmissionPolicy["CognitiveAdmissionPolicy"]
    CognitiveQuota["CognitiveQuota"]
    CognitiveLimitRange["CognitiveLimitRange"]
  end

  subgraph trust["Autonomy and trust"]
    AutonomyClass["AutonomyClass"]
    TrustProfile["TrustProfile"]
    AutonomyReview["AutonomyReview"]
  end

  AgentIdentity -->|agentRef| Agent
  AgentRoleBinding -->|roleRef| AgentRole
  AgentRoleBinding -->|identity subject| AgentIdentity
  AgentRoleBinding -->|agent subject| Agent
  AgentCommunicationPolicy -.->|allows or denies peers| Agent
  Workspace ==>|creates default deny| AgentCommunicationPolicy
  Workspace -.->|scopes namespaced agents| Agent
  Workspace -.->|requires signatures| Tool
  Workspace -.->|requires signatures| Model
  CrossNamespaceGrant -.->|authorizes cross namespace refs| Tool
  CrossNamespaceGrant -.->|authorizes cross namespace refs| Model
  CrossNamespaceGrant -.->|authorizes cross namespace refs| Memory
  CrossNamespaceGrant -.->|authorizes cross namespace refs| KnowledgeBase

  AgentPolicy -.->|targets| Agent
  AgentPolicy -.->|embedded grant compatibility| Tool
  CognitiveAdmissionPolicy -.->|validates or mutates| MatchedWrite
  CognitiveQuota -.->|limits namespace usage| Workspace
  CognitiveQuota -->|supplies inherited budget| Solution
  CognitiveLimitRange -.->|defaults and caps sessions| Agent

  Agent -->|autonomyClassRef| AutonomyClass
  TrustProfile -->|agentRef| Agent
  TrustProfile -->|initial and maximum class| AutonomyClass
  AutonomyReview -->|agentRef| Agent
  AutonomyReview -->|trustProfileRef| TrustProfile
  AutonomyReview -->|current or requested class| AutonomyClass
  AutonomyClass -.->|allows tool classes| ToolClass
```

## Domain Coverage

| Domain | Canonical resources |
| --- | --- |
| Intent and orchestration | `Solution`, `IntentGateway`, `Workflow`, `WorkflowRun`, `ApprovalDecision` |
| Agent execution | `Agent`, `AgentInstance`, `AgentJob`, `AgentService`, `AgentAutoscaler`, `ModelAutoscaler` |
| Evaluation and quality | `EvaluationDataset`, `Evaluation`, `EvaluationRun` |
| Knowledge | `KnowledgeBaseClass`, `KnowledgeBase`, `KnowledgeBaseClaim`, `KnowledgeBaseIndexRun`, `KnowledgeBaseIndexSchedule` |
| Memory | `MemoryClass`, `MemoryClaim`, `MemoryVolume`, `Memory`, `MemoryRecord`, `MemoryGrant` |
| Models and tools | `ModelClass`, `Model`, `ModelGrant`, `ToolClass`, `Tool`, `ToolGrant` |
| Configuration | `Prompt`, `SecretRef` |
| Identity and namespace access | `Workspace`, `AgentIdentity`, `AgentRole`, `AgentRoleBinding`, `AgentCommunicationPolicy`, `CrossNamespaceGrant` |
| Governance and admission | `AgentPolicy`, `CognitiveAdmissionPolicy`, `CognitiveQuota`, `CognitiveLimitRange` |
| Autonomy and trust | `AutonomyClass`, `TrustProfile`, `AutonomyReview` |

## Current Boundaries

- Agent controllers create `AgentInstance` resources. In Kubernetes mode, the
  dispatcher also ensures one runtime Deployment and Service per Agent.
- WorkflowRun creates AgentJobs for agent-backed steps and MemoryClaims for
  declared run or step memory. Tool-backed steps invoke Tool directly.
- Evaluation creates immutable EvaluationRuns. Each run snapshots its grader
  configuration, reads its dataset, creates one target AgentJob per case, and
  creates judge AgentJobs after target completion for `llm_judge` graders.
- MemoryClaim creates a pending MemoryVolume, provisions its generated handle
  through `kognition-memory-service`, then marks the volume and claim Bound.
  `ReclaimDelete` deprovisions the handle before deleting the volume.
- KnowledgeBaseIndexRun launches a Kubernetes Job only when the class has an
  external vector store and the ingester is enabled. Inline sources use the
  runtime keyword-search path without a Job.
- CognitiveAdmissionPolicy can match any Kognition kind. The governance view
  uses one `Matched Kognition resource write` boundary node instead of drawing
  43 duplicate policy arrows.
- CrossNamespaceGrant accepts arbitrary resource kinds. The diagram shows the
  currently important Tool, Model, Memory, and KnowledgeBase families.
