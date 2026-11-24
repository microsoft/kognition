# Install Kognition on Kubernetes

Use the fresh installer for a new release. It installs and verifies the platform,
then starts the controller. Provider setup and sample installation are separate.

## 1. Prerequisites

Prepare cluster access, cert-manager, Cilium, the Kognition CRDs and images.
The operator needs Kubernetes permissions to install admission policies and RBAC
and to run the admission checks.

- [Kubernetes setup](getting-started/kubernetes.md): complete installation steps.
- [AKS setup](getting-started/aks.md): Entra authentication, registry access and
  the AKS runtime profile.
- [Standalone Compose](standalone-compose.md): local development without the
  Kubernetes installation path.

## 2. Prepare configuration

Package the [Helm chart](../deploy/helm/kognition), configure images and any
existing-secret or database references, and select dedicated workload namespaces.
Pin the controller image by digest and make all configured images available to
the nodes.

The fresh installer generates a release-managed runtime authentication Secret by
default. Set `runtimeAuth.existingSecret` to use an operator-managed Secret in
the installation namespace instead. An explicit `runtimeAuth.create: false`
without an external Secret opts out of creation; ensure the release's expected
Secret exists before enabling components that depend on it. Direct Helm renders
do not enable Secret creation by default.

Keep values and captured installation state private. A model provider or `.env`
file is not required to install the platform.

## 3. Install

```powershell
.\bootstrap.ps1 `
    -Kubeconfig .\.local\cluster.kubeconfig `
    -Chart .\.local\charts\kognition-0.1.0.tgz `
    -Values .\.local\values.yaml `
    -Namespace kognition-system `
    -WorkloadNamespace team-a `
    -StateDirectory .\.local\kognition-install `
    -Exclusive
```

**Bash (Linux/macOS):**

```bash
go run ./tools/kognition-install fresh \
    --kubeconfig ./.local/cluster.kubeconfig \
    --chart ./.local/charts/kognition-0.1.0.tgz \
    --values ./.local/values.yaml \
    --namespace kognition-system \
    --workload-namespaces team-a \
    --state-dir ./.local/kognition-install \
    --exclusive
```

Use the archive version and paths prepared for your release. `-Exclusive` /
`--exclusive` confirms
that no other installer, GitOps process or workload writer changes the selected
scope during installation.

The controller starts last:

1. The installer checks cluster identity and refuses an existing release or
   conflicting resources.
2. Admission services and database components are installed while the controller
   has no workload permissions and zero replicas.
3. Migrations, service readiness and admission behavior are checked.
4. The installer records the verified target, grants controller permissions and
   starts the controller.
5. Successful completion requires the controller rollout to be ready.

The zero-replica stage is an internal startup sequence, not a finished installation.
If a check fails, the command returns an error and does not report the platform
ready. It never resets a cluster or installs samples as a side effect.

The `make quickstart` and `make.ps1 quickstart` targets use the same installer.
See [Kubernetes setup](getting-started/kubernetes.md) for the corresponding options.

## 4. Manual step-by-step

After the installer completes, inspect the platform using the selected kubeconfig:

```powershell
kubectl --kubeconfig .\.local\cluster.kubeconfig -n kognition-system get deployments,statefulsets,jobs
go build -o .\bin\kognitionctl.exe .\cmd\kognitionctl
```

**Bash (Linux/macOS):**

```bash
kubectl --kubeconfig ./.local/cluster.kubeconfig -n kognition-system get deployments,statefulsets,jobs
go build -o ./bin/kognitionctl ./cmd/kognitionctl
```

Select the workload namespace and kubeconfig context explicitly when using the
CLI. Follow [First agent](getting-started/first-agent.md) to configure an Agent,
grant model access and verify an invocation.

For Studio, use the same target:

```powershell
$env:KUBECONFIG = (Resolve-Path .\.local\cluster.kubeconfig).Path
.\bin\kognitionctl.exe --mode kubernetes --kube-context '<context>' `
    --namespace team-a --kube-namespace kognition-system studio
```

**Bash (Linux/macOS):**

```bash
export KUBECONFIG="$PWD/.local/cluster.kubeconfig"
./bin/kognitionctl --mode kubernetes --kube-context '<context>' \
    --namespace team-a --kube-namespace kognition-system studio
```

## 5. What you get

The installed components follow the values file. The base platform does not
include a configured model provider or a successful sample invocation.

Use the [guides and samples](guides/README.md) to add tools, memory, knowledge,
workflows or evaluation. Review each sample's endpoints, namespace, credentials
and validation scope before applying it.

### Configure an Azure tenant

The optional Azure-backed samples use
[Configure.ps1](../deploy/azure-samples/Configure.ps1) to bind existing Foundry
deployments and prepare sample identity and Blob resources. It does not install
or activate Kognition.

Inspect its parameters before selecting the subscription, resource names,
deployments and workload namespaces:

```powershell
Get-Help .\deploy\azure-samples\Configure.ps1 -Detailed
```

**Bash (Linux/macOS), with PowerShell 7 installed:**

```bash
pwsh -NoProfile -Command 'Get-Help ./deploy/azure-samples/Configure.ps1 -Detailed'
```

Invoke the selected provisioning parameters from Bash with
`pwsh -NoProfile -File ./deploy/azure-samples/Configure.ps1`; this is the same
helper, not a separate Bash provisioning implementation.

Pass those selected parameters with `-ValidateOnly` before provisioning. Review
the resource changes, costs and credential outputs before running without that
switch. Generated credentials and `.env` stay local; do not commit them.
See [model invocation profiles](model-invocation-profiles.md) when selecting the
required `ChatInvocationContract`.

## 6. Test it

Complete the [first-agent walkthrough](getting-started/first-agent.md), then
inspect resource status and events. A ready Agent, successful ingestion and a
completed retrieval request are different results; verify the capability you need.

## 7. Troubleshooting

| Symptom | Action |
| --- | --- |
| Existing release or conflicting resource | Use the upgrade/recovery procedure; do not overwrite or reset it. |
| Image pull fails | Check the registry reference, node access and digest availability. |
| Migration fails | Inspect the retained Job and database logs; preserve the state directory. |
| Admission or controller check fails | Keep the failed installation state and follow the reported recovery path. |
| Controller remains at zero replicas after an error | Resolve the failed check; do not manually scale past it. |
| Model invocation fails after installation | Inspect model configuration, grants, credentials, runtime status and events. |

## 8. Uninstall

Stop new workload submissions, settle running work and apply the required data
retention or deletion policy before uninstalling.
Follow the [drain procedure](features/knowledge-provider-execution/drain-verification-contract.md)
and [installation/recovery reference](features/knowledge-provider-execution/installation-recovery-contract.md)
for an existing installation. Do not remove finalizers to bypass unfinished work.
