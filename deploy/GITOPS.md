# Guarded GitOps Without A CI Pipeline

This reference covers a manually controlled Argo CD deployment and its external
activation protocol. It is separate from the
[fresh command-line installer](../docs/getting-started/kubernetes.md).

Argo CD can deploy reviewed, manually built artifacts without a GitHub Actions
pipeline. Do not let Argo concurrently manage the same release while the fresh
installer or a recovery transaction is running. Keep release qualification
separate from installation readiness.

## Required Inputs

| Input | Requirement |
| --- | --- |
| Git source | Operator-reviewed chart and non-secret values at an immutable commit |
| Images | Available registry artifacts pinned by digest, with retained source and scan evidence |
| Target | Explicit kubeconfig and reviewed cluster, namespace and resource identities |
| Credentials | Provisioned out of band; no private keys, tokens or database passwords in Git |
| Argo | Restricted AppProject and native RBAC; no authority-namespace access |

For an isolated local exercise, a read-only Git daemon and local registry can
serve these inputs without external publication. Disable Git receive-pack and
upload-archive, mount the served repository read-only and expose no public ports.
Update the local bare repository only through its separately controlled owner.

## Prepare admission before manual activation

1. Build and verify the images manually. Retain exact platform digests, source,
   SBOM, provenance and unresolved scanner findings.
2. Provision dedicated installation, workload and activation-authority namespaces.
   Enroll reviewed CRDs separately. Preprovision serving TLS, runtime binding keys
   and gateway/database Secrets.
3. Use the [Application example](argocd-application.yaml). Supply the immutable
   commit, reviewed repository and non-secret values. Keep controller replicas at
   zero, admission fail-closed, automated sync disabled and pruning disabled.
4. Compare actual Argo repository-server output with the trusted Helm render.
   Argo label tracking may add only the matching Application identity,
   `app.kubernetes.io/instance=kognition`. Match Application and Helm release names;
   do not ignore admission, RBAC, images or baseline metadata differences.
   The example enables `ServerSideDiff=true` to compare Kubernetes API defaults
   through dry-run, including StatefulSet claim templates, without adding field
   ignores.
5. Manually synchronize admission prerequisites and wait for the independent
   webhooks to become ready before admitting chart-owned custom resources.
   Kubernetes-mode migrations are ordinary Jobs; require successful completion.
   The example retains their content-addressed completion records with
   `migrations.retainCompletedJobs=true`. Otherwise the ordinary ten-minute TTL
   deletes the Job and produces expected GitOps drift. Ordinary installs keep
   their existing cleanup default.
6. Verify actual current-generation rollouts and database prerequisites. Keep the
   reconciliation controller at zero with no workload grants or workload Pods.
7. Physically scale the Argo application-controller to zero. Wait for its exact
   Pods to disappear and confirm that no operation or earlier CLI executor remains.
   A sync window or disabled automatic sync is not a physical writer freeze.

Argo Core can refresh an expired managed-resource cache only while its controller
is running. A comparison-only refresh must have no pending operation or automated
sync. Stop that controller again before preparing acceptance or approval.

## Kubernetes API Connectivity On Cilium

With enforcing network policies, supply reviewed API Service and backing endpoint
CIDRs in `networkPolicy.kubernetesApiEgressCIDRs`. The optional policy grants only
installation-scoped control-plane consumers TCP 443 and 6443. The API server
validates CIDR syntax.

Cilium can assign a local API endpoint the reserved `host` identity. A native
`ipBlock` rule for its exact IP does not select that identity. For the supported
Cilium profile, explicitly enable:

```yaml
mode: kubernetes
networkPolicy:
  enabled: true
  ciliumApiServerEgress: true
admissionGuard:
  stdioIsolationProfile: cilium
```

Continue supplying reviewed kubelet probe CIDRs and API endpoint CIDRs for the
rest of the installation. This option adds one CiliumNetworkPolicy in the
installation namespace:

- It selects the exact release and only the controller, Memory Service and four
  gateway components.
- It permits only `host` and `kube-apiserver` entities on TCP 443 and 6443.
- It adds no ingress and selects no enrolled worker namespace.
- It does not change Cilium configuration or the per-workload stdio deny policy.

The activation validator accepts only this exact named policy, scope, ownership
and specification. Broader selectors, peers, ports, ingress, extra policy specs
and missing workload deny policies fail closed. Use the updated safety executable;
older validators reject the additional installation policy.

## Activation And Recovery

Use the existing [activation transaction](../docs/features/knowledge-provider-execution/activation-transaction-contract.md)
with the exact executable, chart, actual Argo manifest, kubeconfig and target.
Collect genuine fresh SG-03/07/08/09 evidence and obtain explicit owner approval.
Successful rendering, manual sync and Pod readiness do not satisfy these gates.

On a failed inactive sync, preserve the revision, logs, resource identities and
physical freeze. Review a new immutable correction and compare its real Argo
render before another explicit manual sync. Do not silently retry unknown writes,
enable self-healing, automatically roll back or replay retained claims.

After activation starts, use only the original transaction's supported recovery
protocol. An inactive Git correction is not activation recovery.
