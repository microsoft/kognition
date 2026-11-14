# Install Kognition on AKS

Start with [AKS setup](../../docs/getting-started/aks.md) for authentication,
registry prerequisites and installation commands.

## Runtime profile

[New-RuntimeValues.ps1](New-RuntimeValues.ps1) generates Kubernetes-mode values for:

- An explicitly selected ACR and versioned images, with a digest-pinned controller.
- Dedicated Agent namespaces and Cilium isolation.
- Node-health ingress and scoped Kubernetes API egress policies.
- A persistent PostgreSQL instance using `managed-csi`.
- Memory, model, tool and communication gateways and the KnowledgeBase ingester.
- No public application ingress or GPU requirement.

The profile is a starting point, not a highly available production topology.
Configure availability, storage sizing, backup, recovery and provider requirements
for the intended deployment.

## Install a new release

Use an explicit kubeconfig and a chart archive whose images have already been
published and made accessible to AKS:

```powershell
.\deploy\aks\Install-RuntimeBase.ps1 `
    -Kubeconfig .\.local\aks.kubeconfig `
    -Context kognition-aks `
    -ChartPackage .\.local\charts\kognition-0.1.0.tgz `
    -Namespace kognition-system `
    -WorkloadNamespace team-a `
    -ImageRegistry '<registry>.azurecr.io' `
    -ImageTag '<release-image-tag>' `
    -ControllerImageDigest 'sha256:<controller-image-digest>' `
    -StateDirectory .\.local\aks-kognition-install `
    -Exclusive
```

The helper delegates to [the shared fresh installer](../../bootstrap.ps1).
That installer refuses existing releases, verifies admission and migrations, then
grants controller permissions and verifies its startup. It captures generated
credentials once instead of regenerating them during repeated Helm renders.

The state directory contains credentials and recovery artifacts. Keep it private.

## Image preparation and providers

- Build application images with the repository Dockerfiles or use a published
  release matching the chart.
- Use [Sync-ThirdPartyImages.ps1](Sync-ThirdPartyImages.ps1) to populate required
  digest-pinned mirrors from an authorized publication runner.
- Configure model endpoints, provider identity and grants separately after the
  platform is ready. See [First agent](../../docs/getting-started/first-agent.md).
- For infrastructure provisioning, see [infra/main.bicep](../../infra/main.bicep)
  and [New-FoundationParameters.ps1](New-FoundationParameters.ps1).

## Existing installations

The fresh helper does not upgrade, reset or adopt an existing installation.
Use the [installation and recovery procedure](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md)
for upgrades or a partial deployment, and retain the original installation state.
