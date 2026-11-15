# ADR-0028: First installation lifecycle

- Status: Accepted for the fresh-install command.
- Date: 2026-11-12.
- Scope: supersedes the manual approval/report requirement in decision 1 of
  [ADR-0016](0016-installation-trust-boundary.md) for a clean first installation
  only. Its independent admission, namespace trust and Cilium boundaries remain.

## Context

The existing installation implementation correctly keeps the controller stopped
until independent admission is available. However, the public quickstart was
disabled and the older AKS helper did not perform the activation transaction.
Its `replicaCount` setting also did not start a Deployment rendered with zero
`replicas`. Release-qualification reports and upgrade/recovery procedures were
presented as the normal first-install user journey.

The maintainer requested a complete fresh-install command that performs its
checks and starts the controller, while keeping existing-installation operations
separate.

## Decision

1. Keep two software phases: install and verify enforcement first, then grant
   the controller's reviewed permissions and start it. Automate both in `fresh`.
2. Refuse existing Helm release history and existing release objects, workloads
   or cleanup inventory. Capture exact cluster/namespace identities, chart,
   values and manifest hashes. Require exclusive writes to the selected scope.
3. Use the existing live admission probes, immutable target snapshots,
   single-use activation journal, UID-bound permission changes, rollout checks
   and failure compensation.
4. Issue a distinct first-install authorization only from a completed revision-zero
   bootstrap claim. Do not generate or reinterpret SG release-qualification
   reports as first-install evidence.
5. Complete database migrations and non-controller rollouts before activation.
   Generate new default credentials once and pin their rendered bytes.
6. Require Cilium isolation. Validate node-health ingress policies separately
   from workload egress-deny policies. Runtime probe ingress is limited to the
   enrolled namespaces; it cannot add egress or permit world traffic.
7. Support an explicitly selected kubeconfig and its credential exec plugin,
   including kubelogin. Capture its server, CA and command paths; retain cluster
   UID checks and reject insecure TLS or impersonation.
8. Leave upgrade, migration, drain and existing-release approval requirements
   unchanged. Recovery uses retained artifacts and the original installer bytes;
   it never authorizes automatic reactivation.

## Alternatives

| Option | Benefit | Cost |
| --- | --- | --- |
| Retain a mandatory external approval/report handoff for every first install | Reuses the existing operational procedure | Leaves no complete quickstart and conflates installation readiness with release qualification. |
| Start the controller in the initial Helm apply | Shortest implementation | Starts a privileged reconciler before enforcement and database checks; rejected. |
| Automatically complete the existing two-phase sequence for fresh targets | Working first-install UX with checks before privilege/startup | Needs a distinct, narrowly scoped authorization and explicit existing-release refusal; selected. |

## Security and consequences

- The installer already runs with operator permissions capable of creating
  admission policies and RBAC. Its authorization record documents that bounded
  operation; it is not an independent production certification.
- Controller identities must remain unable to modify installation authority,
  namespaces or the independent enforcement configuration.
- A changed target, failed probe, failed migration or uncertain request stops the
  operation. No automatic uninstall, credential rotation or replacement occurs.
- State artifacts can contain credentials and must remain private and outside
  source control. Their preservation is required for recovery.
- [AKS uses Cilium identities for node traffic](https://learn.microsoft.com/en-us/azure/aks/azure-cni-powered-by-cilium);
  an IP-block allow rule cannot substitute for the reviewed node-health policies.
- Offline tests and chart checks do not establish deployment acceptance for an
  actual AKS cluster. Production qualification remains separate.
