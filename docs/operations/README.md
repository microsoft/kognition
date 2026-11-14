---
title: Operations and troubleshooting
description: Find deployment contracts, security controls and diagnostic steps.
---

# Operations and troubleshooting

Select the API endpoint or kubeconfig and namespace explicitly. Verify resource
state and execution results for that installation.

## Operational references

| Area | Current source |
| --- | --- |
| Standalone development | [Compose setup and recovery limits](../standalone-compose.md). |
| New Kubernetes installation | [Kubernetes setup](../getting-started/kubernetes.md) or [AKS setup](../getting-started/aks.md). |
| Kubernetes installation and recovery | [Installation contract](../features/knowledge-provider-execution/installation-recovery-contract.md). |
| Inactive admission | [Bootstrap verification](../features/knowledge-provider-execution/bootstrap-verification-contract.md). |
| Existing-installation activation and recovery | [Activation transaction](../features/knowledge-provider-execution/activation-transaction-contract.md). |
| Settling producers | [Drain verification](../features/knowledge-provider-execution/drain-verification-contract.md). |
| Authentication, identity and network boundaries | [Security guide](../security.md). |
| Metrics, logs and opt-in traces | [Observability](../observability.md). |
| Sample profile and evidence | [Sample matrix](../sample-matrix.md). |
| Vulnerability reporting | [Security reporting policy](../../SECURITY.md). |
| Support boundaries | [Support](../../SUPPORT.md). |

## Diagnose in this order

1. **Target:** confirm the API endpoint or kubeconfig, namespace and source/image
   revision. Avoid automatic backend detection in repeatable operations.
2. **Control-plane readiness:** inspect service health and migration completion.
3. **Resource state:** inspect status, conditions and events for the requested
   resource and its executions.
4. **Authorization:** inspect the relevant model, tool, memory or communication
   grant and the caller's identity.
5. **Provider path:** check the configured endpoint, invocation contract,
   credential validity and network reachability.
6. **Recovery:** use the mode-specific procedure and retain the original failure
   and exact-target evidence.

| Symptom | First action |
| --- | --- |
| Compose startup fails | Inspect `docker compose ps -a` and the failing service's logs; require a successful migration. |
| A CLI request reaches an unexpected installation | Select `--mode`, endpoint or reviewed kubeconfig explicitly. |
| An Agent cannot invoke a model | Inspect the binding, ModelGrant, namespace, events and provider configuration. |
| Admission is healthy but workloads do not reconcile | Inspect the installer result and controller Deployment; a failed check leaves it stopped. Resolve that failure before recovery. |
| A KnowledgeBase indexed but retrieval fails | Verify the retrieval runtime and provider path independently from ingestion. |
| A request timed out after possible provider execution | Preserve session and accounting evidence before another invocation. |

## Handle diagnostic data carefully

Kognition trace export is opt-in. Operators choose its destination, which can be
local or self-hosted. Application logs and configured provider requests are
separate from trace export. Treat logs, trace attributes and exception text as
potentially sensitive even when tracing is disabled.

Redact tokens, credentials, prompts and private data before sharing diagnostics.
Do not attach resolved Compose configuration, Secret contents or unrestricted
cluster snapshots to public issues.
