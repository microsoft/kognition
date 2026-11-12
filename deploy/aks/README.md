# Deploy Kognition to Azure Kubernetes Service

This runbook deploys the Kognition Kubernetes runtime and the Foundry-backed
sample to Azure Kubernetes Service (AKS). It records the sequence validated for
the single-node POC in East US 2. The environment-specific approval, cost, and
deployment evidence are in [the Azure deployment plan](../../.azure/deployment-plan.md).

## Scope

The deployed profile includes:

- one CPU-only AKS node;
- Microsoft Entra authentication, Azure RBAC, OIDC, and Workload Identity;
- Azure CNI Overlay with Cilium;
- a private Premium Azure Container Registry (ACR);
- cert-manager for admission webhook certificates;
- the Kognition Kubernetes runtime;
- one in-cluster PostgreSQL and pgvector instance with a 10-GiB disk;
- the Foundry-backed sample and governance resources.

This is a POC, not a production topology. It has no high availability,
automated backup, public application ingress, or disaster-recovery guarantee.
Azure AI Search is not deployed: the repository does not yet contain a
deployable Azure AI Search provider and governed execution route. Knowledge
storage remains on the existing pgvector path.

## Prerequisites

Run all commands from the repository root in PowerShell 7. Install:

- Azure CLI with Bicep;
- Docker Desktop or another Docker engine;
- Go;
- Helm 3;
- kubectl;
- kubelogin.

The Azure identity running the foundation deployment must be able to create
resources and role assignments in the target subscription. The operator object
ID is an Entra object ID, not an application/client ID.

Confirm the local tools before making Azure changes:

```powershell
$ErrorActionPreference = 'Stop'

az version
az bicep version
docker version
go version
helm version
kubectl version --client
kubelogin --version
```

## Deployment variables

Set the approved values once. Replace the operator ID and current public IP;
do not copy the historical address from the deployment plan.

```powershell
$SubscriptionId = '<approved-subscription-id>'
$ExpectedTenantId = '<approved-tenant-id>'
$ResourceGroup = 'rg-kognition'
$Location = 'eastus2'
$RuntimeNamespace = 'kognition-system'
$SampleNamespace = 'kognition-demo'

$OperatorObjectId = '<entra-object-id>'
$OperatorPublicIp = (Invoke-RestMethod 'https://api.ipify.org').Trim()
$OperatorPublicCidr = "$OperatorPublicIp/32"

$GitSha = (git rev-parse --short=12 HEAD).Trim()
$ImageTag = 'aks-{0}-{1}' -f (Get-Date).ToUniversalTime().ToString('yyyyMMdd-HHmmss'), $GitSha
```

Use an immutable image tag. `New-RuntimeValues.ps1` rejects `latest`, `dev`,
and `main`.

## 1. Verify the Azure account and approval

Select the subscription and fail if the active tenant or subscription is not
the approved one:

```powershell
az account set --subscription $SubscriptionId
$Account = az account show --output json | ConvertFrom-Json

if ($Account.id -ne $SubscriptionId) {
    throw "Wrong Azure subscription: $($Account.id)"
}
if ($Account.tenantId -ne $ExpectedTenantId) {
    throw "Wrong Microsoft Entra tenant: $($Account.tenantId)"
}

$Account | Select-Object name, id, tenantId, user | Format-List
```

Before provisioning, obtain explicit approval for the resources and current
cost estimate. The validated POC estimate was USD 373.33 per month in fixed
costs, under an approved USD 425 ceiling. Recalculate current pricing and stop
if the estimate or ARM what-if exceeds the approved boundary.

## 2. Register providers and create the resource group

Provider registration and resource-group creation are Azure mutations:

```powershell
$Providers = @(
    'Microsoft.Authorization',
    'Microsoft.ContainerRegistry',
    'Microsoft.ContainerService',
    'Microsoft.ManagedIdentity',
    'Microsoft.Network'
)

foreach ($Provider in $Providers) {
    az provider register --namespace $Provider --subscription $SubscriptionId --wait
    $State = az provider show --namespace $Provider --subscription $SubscriptionId `
        --query registrationState --output tsv
    if ($State -ne 'Registered') {
        throw "$Provider is not registered: $State"
    }
}

az group create --subscription $SubscriptionId --name $ResourceGroup `
    --location $Location --output table
```

## 3. Generate parameters and run repository checks

The parameter generator accepts only IPv4 networks of `/24` or narrower. A
single-operator `/32` is preferred.

```powershell
./deploy/aks/New-FoundationParameters.ps1 `
    -OperatorPrincipalId $OperatorObjectId `
    -ApiServerAuthorizedIpRanges $OperatorPublicCidr `
    -Location $Location `
    -KubernetesVersion '1.35' `
    -NodeVmSize 'Standard_D8s_v5' `
    -NodeCount 1 |
    Set-Content ./foundation.parameters.json

pwsh -NoProfile -File deploy/aks/tests/profile-tests.ps1
pwsh -NoProfile -File deploy/aks/tests/foundation-tests.ps1
pwsh -NoProfile -File deploy/aks/tests/secret-tests.ps1
pwsh -NoProfile -File deploy/aks/tests/runtime-values-tests.ps1
pwsh -NoProfile -File deploy/aks/tests/install-tests.ps1
pwsh -NoProfile -File deploy/helm/kognition/tests/helm-regression-tests.ps1
go build ./...
```

`foundation.parameters.json` and the compiled `infra/main.json` are generated
artifacts and must not be committed.

## 4. Validate the foundation before deployment

Compile and lint Bicep, ask ARM to validate the deployment, and review what-if:

```powershell
az bicep build --file ./infra/main.bicep
az bicep lint --file ./infra/main.bicep

az deployment group validate `
    --subscription $SubscriptionId `
    --resource-group $ResourceGroup `
    --template-file ./infra/main.bicep `
    --parameters ./foundation.parameters.json `
    --output table

az deployment group what-if `
    --subscription $SubscriptionId `
    --resource-group $ResourceGroup `
    --template-file ./infra/main.bicep `
    --parameters ./foundation.parameters.json
```

Stop before deployment if validation reports a policy or authorization denial,
quota or SKU unavailability, an unexpected replacement/deletion, public ACR
access, a broader Kubernetes API allowlist, or costs above the approved ceiling.

## 5. Deploy and capture foundation outputs

```powershell
$DeploymentName = 'kognition-foundation-{0}' -f `
    (Get-Date).ToUniversalTime().ToString('yyyyMMdd-HHmmss')

az deployment group create `
    --subscription $SubscriptionId `
    --resource-group $ResourceGroup `
    --name $DeploymentName `
    --template-file ./infra/main.bicep `
    --parameters ./foundation.parameters.json `
    --output table

$Outputs = az deployment group show `
    --subscription $SubscriptionId `
    --resource-group $ResourceGroup `
    --name $DeploymentName `
    --query properties.outputs --output json | ConvertFrom-Json

$ClusterName = $Outputs.clusterName.value
$RegistryName = $Outputs.registryName.value
$RegistryLoginServer = $Outputs.registryLoginServer.value
$AksContext = $ClusterName

[pscustomobject]@{
    ClusterName = $ClusterName
    RegistryName = $RegistryName
    RegistryLoginServer = $RegistryLoginServer
    OidcIssuerUrl = $Outputs.oidcIssuerUrl.value
} | Format-List
```

Do not place deployment output JSON containing secrets in `.azure`. The current
foundation outputs are non-secret, but generated `.azure/*.json` remains ignored
to prevent future leakage.

## 6. Configure and verify kubectl

```powershell
az aks get-credentials `
    --subscription $SubscriptionId `
    --resource-group $ResourceGroup `
    --name $ClusterName `
    --overwrite-existing

kubelogin convert-kubeconfig -l azurecli

if ((kubectl config current-context) -ne $AksContext) {
    throw 'kubectl is not using the expected AKS context.'
}

kubectl --context $AksContext get nodes -o wide
```

If kubectl starts a device-code flow or times out in `kubelogin`, rerun
`az login --tenant $ExpectedTenantId`, reselect the subscription, and repeat
`kubelogin convert-kubeconfig -l azurecli`.

## 7. Publish immutable images through a bounded ACR window

The registry is private by default. Hosted `az acr build` cannot use this
registry without a dedicated task agent pool. For this POC, temporarily enable
the public endpoint with default action `Deny` and permit only the operator's
current `/32`. Never use `0.0.0.0/0`.

Keep this terminal open until the closure check succeeds. If any login, build,
push, or manifest check fails, stop that work and run the closure block below
immediately. Treat closure as the `finally` action for the publication window.

```powershell
az acr update --subscription $SubscriptionId --resource-group $ResourceGroup `
    --name $RegistryName --default-action Deny --output none
az acr update --subscription $SubscriptionId --resource-group $ResourceGroup `
    --name $RegistryName --public-network-enabled true --output none
az acr network-rule add --subscription $SubscriptionId --resource-group $ResourceGroup `
    --name $RegistryName --ip-address $OperatorPublicCidr --output none

az acr login --subscription $SubscriptionId --name $RegistryName
```

Build and push the seven root-Dockerfile targets and the two runtime images:

```powershell
$RootImages = @(
    @{ Repository = 'kognition-k8s-controller'; Target = 'k8s-controller' },
    @{ Repository = 'kognition-intent-gateway'; Target = 'intent-gateway' },
    @{ Repository = 'kognition-memory-service'; Target = 'memory-service' },
    @{ Repository = 'kognition-memory-policy-gateway'; Target = 'memory-policy-gateway' },
    @{ Repository = 'kognition-tool-gateway'; Target = 'tool-gateway' },
    @{ Repository = 'kognition-model-gateway'; Target = 'model-gateway' },
    @{ Repository = 'kognition-agent-communication-gateway'; Target = 'agent-communication-gateway' }
)

foreach ($Image in $RootImages) {
    $Reference = "$RegistryLoginServer/$($Image.Repository):$ImageTag"
    docker build --target $Image.Target --tag $Reference .
    if ($LASTEXITCODE -ne 0) { throw "Build failed: $Reference" }
    docker push $Reference
    if ($LASTEXITCODE -ne 0) { throw "Push failed: $Reference" }
}

$RuntimeImages = @(
    @{ Repository = 'kognition-agent-runtime'; Context = '.'; Dockerfile = 'runtime/agent/Dockerfile' },
    @{ Repository = 'kognition-kb-ingester'; Context = '.'; Dockerfile = 'runtime/ingester/Dockerfile' }
)

foreach ($Image in $RuntimeImages) {
    $Reference = "$RegistryLoginServer/$($Image.Repository):$ImageTag"
    docker build --file $Image.Dockerfile --tag $Reference $Image.Context
    if ($LASTEXITCODE -ne 0) { throw "Build failed: $Reference" }
    docker push $Reference
    if ($LASTEXITCODE -ne 0) { throw "Push failed: $Reference" }
}
```

Verify all nine manifests before closing the window:

```powershell
$Repositories = @($RootImages.Repository) + @($RuntimeImages.Repository)
foreach ($Repository in $Repositories) {
    $Digest = az acr manifest show-metadata --registry $RegistryName `
        --name "${Repository}:$ImageTag" --query digest --output tsv
    if (-not $Digest) { throw "Missing manifest: ${Repository}:$ImageTag" }
    Write-Host "$Repository $Digest"
}
```

Disable public access even if a later deployment step fails. Azure can normalize
a `/32` rule to a host address, so read the stored rule and remove that exact
value:

```powershell
az acr update --subscription $SubscriptionId --resource-group $ResourceGroup `
    --name $RegistryName --public-network-enabled false --output none

$StoredRules = @(az acr show --subscription $SubscriptionId `
    --resource-group $ResourceGroup --name $RegistryName `
    --query 'networkRuleSet.ipRules[].value' --output tsv)
foreach ($Rule in $StoredRules) {
    az acr network-rule remove --subscription $SubscriptionId `
        --resource-group $ResourceGroup --name $RegistryName `
        --ip-address $Rule --output none
}

$RegistryPosture = az acr show --subscription $SubscriptionId `
    --resource-group $ResourceGroup --name $RegistryName `
    --query '{publicNetworkAccess:publicNetworkAccess,adminUserEnabled:adminUserEnabled,defaultAction:networkRuleSet.defaultAction,ipRules:networkRuleSet.ipRules,privateEndpoints:privateEndpointConnections[].privateLinkServiceConnectionState.status}' `
    --output json | ConvertFrom-Json
$RegistryPosture | ConvertTo-Json -Depth 5

if ($RegistryPosture.publicNetworkAccess -ne 'Disabled' -or
    $RegistryPosture.adminUserEnabled -or
    @($RegistryPosture.ipRules).Count -ne 0) {
    throw 'ACR was not restored to private-only access.'
}
```

After closure, prove that AKS can pull from the private registry. The probe only
needs a non-empty image ID; the controller process may exit because this bare Pod
does not have the runtime configuration used by the Helm release.

```powershell
$ProbeNamespace = 'kognition-pull-probe'
$ProbeName = 'acr-private-pull'

kubectl --context $AksContext create namespace $ProbeNamespace `
    --dry-run=client --output yaml |
    kubectl --context $AksContext apply -f -
kubectl --context $AksContext --namespace $ProbeNamespace delete pod `
    $ProbeName --ignore-not-found --wait=true
kubectl --context $AksContext --namespace $ProbeNamespace run $ProbeName `
    --image="$RegistryLoginServer/kognition-k8s-controller:$ImageTag" `
    --restart=Never
kubectl --context $AksContext --namespace $ProbeNamespace wait `
    --for=jsonpath='{.status.containerStatuses[0].imageID}' `
    "pod/$ProbeName" --timeout=120s

$PulledImageId = kubectl --context $AksContext --namespace $ProbeNamespace get `
    "pod/$ProbeName" -o jsonpath='{.status.containerStatuses[0].imageID}'
if (-not $PulledImageId) { throw 'AKS did not pull the private ACR image.' }
Write-Host $PulledImageId

kubectl --context $AksContext delete namespace $ProbeNamespace --wait=true
```

## 8. Install cert-manager

The live deployment used cert-manager v1.21.2. Pin the version so a rerun does
not silently introduce a different dependency:

```powershell
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm upgrade --install cert-manager jetstack/cert-manager `
    --kube-context $AksContext `
    --namespace cert-manager `
    --create-namespace `
    --version v1.21.2 `
    --set crds.enabled=true `
    --wait `
    --timeout 5m

kubectl --context $AksContext --namespace cert-manager rollout status `
    deployment/cert-manager --timeout=120s
kubectl --context $AksContext --namespace cert-manager rollout status `
    deployment/cert-manager-cainjector --timeout=120s
kubectl --context $AksContext --namespace cert-manager rollout status `
    deployment/cert-manager-webhook --timeout=120s
kubectl --context $AksContext wait --for=condition=Established `
    crd/certificates.cert-manager.io crd/issuers.cert-manager.io --timeout=60s
```

## 9. Install the Kognition runtime

```powershell
./deploy/aks/Install-RuntimeBase.ps1 `
    -Context $AksContext `
    -Namespace $RuntimeNamespace `
    -ImageRegistry $RegistryLoginServer `
    -ImageTag $ImageTag

helm --kube-context $AksContext status kognition --namespace $RuntimeNamespace
kubectl --context $AksContext --namespace $RuntimeNamespace get pods -o wide
kubectl --context $AksContext --namespace $RuntimeNamespace get statefulset,deployments
```

The installer creates the namespace, lints generated values, initializes
database and runtime-auth Secrets through kubectl stdin, installs or upgrades
Helm, and verifies rollouts. It intentionally uses `--wait=false`: the migration
hook must complete before dependent workloads are awaited. Do not add `--atomic`,
which implicitly enables Helm workload waiting and breaks this ordering.

Secret initialization is rerunnable and preserves valid existing credentials.
It refuses unsafe regeneration when a database PVC already exists. Deleting the
database Secret does not change the password stored in an existing PostgreSQL
volume; restore the original Secret rather than generating a new one.

The generated values set `networkPolicy.kubeletProbeIngressCIDRs` to the Bicep
foundation's node subnet (`10.240.0.0/22`) and `global.aksAdmissionsEnforcerOptOut`
to `true`. For a cluster created another way, such as `az aks create` with its
managed virtual network, pass that cluster's node CIDR:

```powershell
./deploy/aks/Install-RuntimeBase.ps1 `
    -Context $AksContext `
    -Namespace $RuntimeNamespace `
    -ImageRegistry $RegistryLoginServer `
    -ImageTag $ImageTag `
    -KubeletProbeIngressCidr 10.224.0.0/16
```

The opt-out labels each Kognition webhook configuration with
`admissions.enforcer/disabled: "true"`. Without it the AKS Admissions Enforcer
adds `control-plane` and `kubernetes.azure.com/managedby` exclusions to every
webhook selector: a namespace carrying either label would then escape the
admission guard, and the live objects would stop matching a reviewed manifest.
Guarded activation on AKS uses the reviewed `reviewed-aks-cilium-1.18.15`
network profile described in
[Reviewed Local Target Acceptance](../../docs/testing.md#reviewed-local-target-acceptance).

## 10. Configure Foundry credentials and samples

The sample expects a repository-root `.env` with `AZURE_TENANT_ID`,
`AZURE_CLIENT_ID`, and `AZURE_CLIENT_SECRET` for an identity authorized to call
the configured Foundry deployment. Never print these values in logs or commands.
Rotate any credential that has been disclosed.

Generate and apply the sample credential only after the SecretRef CRD exists:

```powershell
./make.ps1 foundry-creds AGENT_NAMESPACE=$SampleNamespace
```

This command writes the ignored `foundry-creds.yaml` and applies it. The current
sample stores credential material in a `SecretRef` annotation for POC use. That
is not a production secret-management design; protect Kubernetes API access and
replace it with a governed external secret provider before production use.

Apply resources in dependency order:

```powershell
kubectl --context $AksContext apply -f samples/workflows/tool-sandbox.yaml
kubectl --context $AksContext --namespace default rollout status `
    deployment/tool-sandbox --timeout=120s

kubectl --context $AksContext apply -f samples/k8s-mode-demo.yaml
kubectl --context $AksContext apply -f samples/governance-demo.yaml
kubectl --context $AksContext apply -f samples/model-grants.yaml
```

The broad `samples/model-grants.yaml` also contains grants for other sample
namespaces. For a tightly scoped environment, extract or maintain only the three
`kognition-demo` grants for `researcher`, `writer`, and `reviewer`.

Runtime authorization and model authorization are separate controls. A healthy
runtime can still deny an AgentInstance until its Agent has an explicit
ModelGrant for `gpt-4-1`.

## 11. Verify the deployment

```powershell
helm --kube-context $AksContext status kognition --namespace $RuntimeNamespace
kubectl --context $AksContext --namespace $RuntimeNamespace get pods
kubectl --context $AksContext get crd | Select-String 'kognition.dev'
kubectl --context $AksContext --namespace $SampleNamespace get `
    models,memoryclaims,tools,agents,workflows,modelgrants,secretrefs,workspaces,agentidentities,cognitivequotas,agentpolicies
```

Create a bounded Foundry smoke invocation through the Kubernetes API, not by
port-forwarding directly to a runtime `/invoke` endpoint:

```powershell
$SmokeName = 'aks-foundry-smoke'
kubectl --context $AksContext --namespace $SampleNamespace delete `
    agentinstance $SmokeName --ignore-not-found --wait=true

@"
apiVersion: kognition.dev/v1alpha1
kind: AgentInstance
metadata:
  name: $SmokeName
  namespace: $SampleNamespace
spec:
  agentRef:
    name: researcher
  revision: 1
  sessionContext:
    input: "Reply with exactly: Kognition AKS ready."
"@ | kubectl --context $AksContext apply -f -

kubectl --context $AksContext --namespace $SampleNamespace wait `
    --for=jsonpath='{.status.phase}'=Ready `
    "agentinstance/$SmokeName" --timeout=180s
kubectl --context $AksContext --namespace $SampleNamespace get `
    "agentinstance/$SmokeName" `
    -o jsonpath='{.status.output}{"`n"}{.status.tokenUsage.totalTokens}{"`n"}'
```

Acceptance requires a deployed Helm release, Ready runtime workloads, reconciled
sample resources, a terminal smoke invocation with non-empty output, and ACR
still reporting `publicNetworkAccess=Disabled` with admin access disabled.

## 12. Launch Studio in Kubernetes mode

Build Studio from the current source tree so an older CLI on `PATH` cannot serve
stale embedded assets:

```powershell
go build -o ./bin/kognitionctl.exe ./cmd/kognitionctl
./bin/kognitionctl.exe `
    --mode kubernetes `
    --kube-context $AksContext `
    --namespace $SampleNamespace `
    studio --open
```

Studio binds to `127.0.0.1` by default. Keep the terminal open while using it.
Explicit Kubernetes mode is required for this deployment; auto-detection can
otherwise select an unintended standalone endpoint.

## Troubleshooting

### `runtime authorization is required`

Do not invoke a runtime pod or Service directly. Runtime endpoints require an
internal bearer token. Invoke through an AgentInstance or use Studio/CLI in
explicit Kubernetes mode so the controller-owned authorization path is used.

### AgentInstance reports model access denied

Check that the Agent, Model, and namespace-specific ModelGrant exist:

```powershell
kubectl --context $AksContext --namespace $SampleNamespace get `
    agent/researcher model/gpt-4-1 modelgrants
kubectl --context $AksContext --namespace $SampleNamespace describe `
    "agentinstance/$SmokeName"
```

Apply the `kognition-demo` ModelGrants and recreate the one-shot AgentInstance.

### Migration cannot resolve PostgreSQL or remains pending

Inspect the migration hook and Cilium policies:

```powershell
kubectl --context $AksContext --namespace $RuntimeNamespace get pods,jobs
kubectl --context $AksContext --namespace $RuntimeNamespace logs `
    -l app.kubernetes.io/component=migration --all-containers --tail=200
kubectl --context $AksContext --namespace $RuntimeNamespace get `
    networkpolicy,ciliumnetworkpolicy
```

Do not broaden all workload egress. The API-server egress exception must select
only the Kubernetes controller; migration still needs cluster DNS and PostgreSQL.

### `ImagePullBackOff`

Confirm the immutable tag exists, the kubelet identity has `AcrPull`, private DNS
resolves the registry, and ACR's private endpoint connection is approved. Do not
reenable public ACR access as a pull workaround.

### kubectl is forbidden

The operator needs both Azure Kubernetes Service Cluster User Role and Azure
Kubernetes Service RBAC Cluster Admin at the AKS scope. Azure Kubernetes Service
Cluster Admin Role is the local-account role and does not grant the required
Azure RBAC data-plane access when local accounts are disabled.

### Foundry authentication fails

Confirm `.env` points to the expected tenant and client without printing the
secret, regenerate `foundry-creds.yaml`, and reapply it. A Foundry failure does
not invalidate a healthy model-free runtime base; do not substitute a different
tenant or service without approval.

### Webhook or certificate errors

Verify all three cert-manager deployments and inspect Certificate and Issuer
conditions before rerunning the runtime installer. CRD presence alone does not
prove cert-manager is operational.

## Reruns and cleanup controls

Bicep deployments, cert-manager installation, the runtime installer, and sample
`kubectl apply` operations are designed to be rerun. Reuse the existing immutable
image tag only when its manifests are known and unchanged; publish a new tag for
new source. Secret initialization preserves valid values and deliberately stops
when persisted database state makes regeneration unsafe.

Cleanup is destructive and is not part of this runbook. Obtain explicit approval
before any of these actions:

- deleting an AgentInstance or sample namespace that may contain evidence;
- uninstalling the Helm release;
- deleting PostgreSQL PVCs or Secrets;
- removing Azure role assignments;
- deleting `rg-kognition` or its AKS-managed resource group.

Before resource-group deletion, verify the group contains only this POC and that
no sample data, logs, or deployment evidence requires retention. Never delete the
AKS-managed `MC_*` resource group independently.