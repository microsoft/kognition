---
title: AKS setup
description: Install Kognition on an existing Azure Kubernetes Service cluster.
---

# AKS setup

Use the [Kubernetes installation workflow](kubernetes.md) with an AKS kubeconfig
and images available to the cluster. This guide uses an existing AKS cluster,
Microsoft Entra authentication and Azure Container Registry (ACR).

## Prerequisites

- Azure CLI, kubelogin, kubectl, Helm 3 and the repository's Go toolchain.
- Python 3.12 or newer when packaging the chart from source.
- Permission to retrieve cluster credentials and perform the Kubernetes
  installation operations.
- Azure CNI powered by Cilium, including the Cilium policy APIs.
- cert-manager installed and ready.
- Application images and required third-party mirrors in a registry reachable
  by the nodes, with image-pull permissions already configured.

Cluster provisioning is separate. The repository provides
[Bicep infrastructure](../../infra/main.bicep) and
[parameter tooling](../../deploy/aks/New-FoundationParameters.ps1) when a new
foundation is needed.

## 1. Select the cluster

Use a dedicated kubeconfig rather than changing the default one:

```powershell
New-Item -ItemType Directory -Force .\.local | Out-Null
$ResourceGroup = '<resource-group>'
$ClusterName = '<aks-cluster>'
$Kubeconfig = Join-Path (Get-Location) '.local\aks.kubeconfig'
$Context = 'kognition-aks'

az aks get-credentials --resource-group $ResourceGroup --name $ClusterName `
    --file $Kubeconfig --context $Context
kubelogin convert-kubeconfig --kubeconfig $Kubeconfig -l azurecli
kubectl --kubeconfig $Kubeconfig --context $Context get nodes
```

**Bash (Linux/macOS):**

```bash
mkdir -p .local
resource_group='<resource-group>'
cluster_name='<aks-cluster>'
kubeconfig="$PWD/.local/aks.kubeconfig"
context='kognition-aks'

az aks get-credentials --resource-group "$resource_group" --name "$cluster_name" \
    --file "$kubeconfig" --context "$context"
kubelogin convert-kubeconfig --kubeconfig "$kubeconfig" -l azurecli
kubectl --kubeconfig "$kubeconfig" --context "$context" get nodes
```

Use the Azure CLI account and subscription that own the selected cluster.
The installer supports the kubeconfig's kubelogin exec authentication.

## 2. Verify networking and registry access

Inspect the configured network profile:

```powershell
az aks show --resource-group $ResourceGroup --name $ClusterName `
    --query '{plugin:networkProfile.networkPlugin,dataplane:networkProfile.networkDataplane}' `
    --output table
kubectl --kubeconfig $Kubeconfig --context $Context api-resources --api-group=cilium.io
```

**Bash (Linux/macOS):**

```bash
az aks show --resource-group "$resource_group" --name "$cluster_name" \
    --query '{plugin:networkProfile.networkPlugin,dataplane:networkProfile.networkDataplane}' \
    --output table
kubectl --kubeconfig "$kubeconfig" --context "$context" api-resources --api-group=cilium.io
```

The profile must provide Cilium network and cluster-wide network policies.
Kognition uses scoped Cilium policies for workload isolation, API access and
node-health probes. On AKS, `ipBlock` rules cannot select node or Pod IPs.
See [Azure CNI powered by Cilium](https://learn.microsoft.com/en-us/azure/aks/azure-cni-powered-by-cilium).

Configure registry connectivity and pull permissions through the cluster's normal
platform process. Do not change registry network access as an installation shortcut.
For a private mirror, the [image synchronization helper](../../deploy/aks/Sync-ThirdPartyImages.ps1)
copies the declared third-party image digests from an authorized publication runner.

## 3. Prepare the chart and runtime profile

Package the chart and install its CRDs using the kubeconfig and context already
selected:

```powershell
New-Item -ItemType Directory -Force .\.local\charts | Out-Null
python .\tools\distribution_notices.py charts
helm package .\deploy\helm\kognition --destination .\.local\charts
$Chart = '.\.local\charts\kognition-0.1.0.tgz'
helm show crds $Chart | kubectl --kubeconfig $Kubeconfig --context $Context apply -f -
```

**Bash (Linux/macOS):**

```bash
mkdir -p .local/charts
python3 ./tools/distribution_notices.py charts
helm package ./deploy/helm/kognition --destination ./.local/charts
chart='./.local/charts/kognition-0.1.0.tgz'
helm show crds "$chart" | kubectl --kubeconfig "$kubeconfig" --context "$context" apply -f -
```

Use the archive version emitted by `helm package`. See
[Kubernetes setup](kubernetes.md#2-package-the-chart-and-install-its-crds) for
the shared prerequisites.

The AKS profile uses a single persistent PostgreSQL instance, the gateway services,
AKS disk storage and no public application ingress. Configure separate
availability, backup and recovery requirements before production use.

Set your published image references and run the profile installer:

```powershell
$Registry = '<registry>.azurecr.io'
$ImageTag = '<release-image-tag>'
$ControllerDigest = 'sha256:<controller-image-digest>'

.\deploy\aks\Install-RuntimeBase.ps1 `
    -Kubeconfig $Kubeconfig -Context $Context -ChartPackage $Chart `
    -Namespace kognition-system -WorkloadNamespace team-a `
    -ImageRegistry $Registry -ImageTag $ImageTag `
    -ControllerImageDigest $ControllerDigest `
    -CreateWorkflowBindingAdminGroup `
    -StateDirectory .\.local\aks-kognition-install -Exclusive
```

**Bash (Linux/macOS):**

```bash
registry='<registry>.azurecr.io'
image_tag='<release-image-tag>'
controller_digest='sha256:<controller-image-digest>'

pwsh -NoProfile -File ./deploy/aks/Install-RuntimeBase.ps1 \
    -Kubeconfig "$kubeconfig" -Context "$context" -ChartPackage "$chart" \
    -Namespace kognition-system -WorkloadNamespace team-a \
    -ImageRegistry "$registry" -ImageTag "$image_tag" \
    -ControllerImageDigest "$controller_digest" \
    -CreateWorkflowBindingAdminGroup \
    -StateDirectory ./.local/aks-kognition-install -Exclusive
```

The AKS profile helper requires PowerShell 7 on Linux/macOS as well as Windows;
see [command shells](../README.md#command-shells). It generates the same AKS
values in both environments. Do not substitute a bare Helm install.

This wrapper uses the same checked fresh installer. It does not provision Azure
resources, publish images or configure inference endpoints.

### Workflow binding administrators

`-CreateWorkflowBindingAdminGroup` enables Workflow execution and makes
[Initialize-BindingAdminGroup.ps1](../../deploy/aks/Initialize-BindingAdminGroup.ps1)
create (or reuse) the Microsoft Entra security group
`kognition-workflow-binding-admins`. It adds the signed-in Azure CLI user and
passes the group's object ID to the chart as the only binding administrator.

- Run it with an Azure CLI account that may create Entra groups. Without that
  permission, ask an Entra administrator to create the group and pass its object
  ID with `-WorkflowBindingAdminGroupId <guid>` instead.
- The cluster must use AKS-managed Entra integration (the repository's
  [Bicep foundation](../../infra/modules/cluster.bicep) does). AKS then presents
  group object IDs to Kubernetes, which the admission webhook matches. On a
  cluster without Entra integration no user can administer bindings.
- Add other operators with `az ad group member add --group <object-id> --member-id <user-object-id>`.
  Membership is the approval authority; keep it small and reviewed.
- Omit both options to install without Workflow execution.
- Add `-EnableWorkflowCollaboration` to also enable Workflow collaboration steps
  (see [Workflow collaboration](../workflow-collaboration.md)). It requires a binding administrator group.

## 4. Verify and configure an agent

```powershell
kubectl --kubeconfig $Kubeconfig --context $Context -n kognition-system get pods
kubectl --kubeconfig $Kubeconfig --context $Context -n kognition-system rollout status `
    deployment/kognition-k8s-controller --timeout=120s
```

**Bash (Linux/macOS):**

```bash
kubectl --kubeconfig "$kubeconfig" --context "$context" -n kognition-system get pods
kubectl --kubeconfig "$kubeconfig" --context "$context" -n kognition-system rollout status \
    deployment/kognition-k8s-controller --timeout=120s
```

Then configure an approved model and its credentials in `team-a`, preferably using
workload identity where supported by the selected provider. Follow
[First agent](first-agent.md) to create the Agent and ModelGrant and verify a turn.

For additional profile details, see the [AKS deployment reference](../../deploy/aks/README.md)
and [kubelogin authentication guidance](https://learn.microsoft.com/en-us/azure/aks/kubelogin-authentication).
