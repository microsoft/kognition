# Kognition Platform Umbrella Chart

Deploys the full Kognition platform including:

- **Platform**: CRDs, k8s-controller, memory-service
- **Governance**: Workspace, quotas, limit ranges, policies, admission control

## Usage

This combined chart is not a supported Kubernetes installation entry point under
the [guarded installation contract](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md).
It combines platform changes with governance producers that must not run during
the bootstrap writer freeze. Direct Helm install/upgrade would bypass reviewed
ordering, target checks and recovery claims.

Install the core platform through the guarded installer using exact reviewed
artifacts and values. Governance/sample installation requires separate review,
namespace enrollment and approved activation after all required security gates.
Do not rebuild or refresh the bundled archive as part of installation recovery.

## Environment-Specific Values

Review separate complete core-platform values and exact target inventories for
each environment. Development settings do not waive admission, enrollment,
writer-freeze or activation requirements. This sample is not production acceptance
evidence.

## Dependencies

| Chart | Description | Condition |
|-------|-------------|-----------|
| kognition | Platform controllers and CRDs | `platform.enabled` |
| kognition-governance | Workspace, quotas, policies, RBAC | `governance.enabled` |
