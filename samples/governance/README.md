# Governance Samples

Kubernetes-mode Agents receive one controller-managed `AgentIdentity` during
reconciliation. The identity has the deterministic name
`agent-identity-<agent-name>` and carries the Agent's SPIFFE URI.

The governance manifests do not create additional manual identities for the
same Agents. Tool Gateway and runtime authorization use the controller-managed
identity, and duplicate identities make legacy identity lookups ambiguous.

Use `kubectl get agentidentities -A` to inspect the identities created for the
Agents in these samples.