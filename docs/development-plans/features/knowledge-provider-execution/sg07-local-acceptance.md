# SG-07 Local Stdio And Network Acceptance

- Status: SG-07 complete locally for the pinned Cilium profile; production activation remains BLOCKED.
- Date: 2026-09-25.
- Scope: task-owned single-node kind, Kubernetes v1.31.1, Cilium 1.17.8, IPv4 VXLAN.
- Decision: user-approved Cilium-backed isolation, recorded in [ADR-0016](../../../adr/0016-installation-trust-boundary.md).

The full deployed matrix passed with current-source guard binaries, rendered
guard network restrictions, real controller-identity kubelet streams and reachable
network controls. This closes the deployed-image and enforcing-CNI gaps retained
in the historical [SG-06 report](sg06-local-acceptance.md). It does not certify
production producer lifecycles, another CNI or the final publication candidate.

## Supported Profile And Provenance

| Item | Observed value |
| --- | --- |
| Cluster / node | `kognition-sg07-20260925` / `kognition-sg07-20260925-control-plane` |
| Node image / server | `kindest/node:v1.31.1` / `v1.31.1` |
| Explicit API endpoint | `https://127.0.0.1:63564` |
| Explicit kubeconfig | `$env:TEMP/kognition-sg07-20260925.kubeconfig` |
| Pod / Service subnet | `10.244.0.0/16` / `10.96.0.0/16` |
| Cilium Helm version / revision | `1.17.8` / `1` |
| Cilium live version | `1.17.8 (v1.17.8-03b5572f)` |
| Cilium DaemonSet UID / readiness | `b0c1aec4-f0f1-4566-8e48-72c576c0af62` / `1 of 1` |
| Cilium ConfigMap UID | `458ad423-136b-4e18-90f3-e50b618b00ca` |
| Cilium Pod / UID | `cilium-f7m7x` / `05785a34-9bb2-4639-baeb-53bd1e44b2f6` |
| Cilium runtime image ID | `quay.io/cilium/cilium@sha256:6d7ea72ed311eeca4c75a1f17617a3d596fb6038d30d00799090679f82a01636` |
| Cilium container image config | `sha256:ab0dfe84bb2a4643486d7c8e0caea7e06bf003d93ab9ca61de487ba126b935c8` |
| Guard tag | `kognition-sg07-guard:20260925-01` |
| Guard OCI index | `sha256:a6468b3170e76714ff48518c53c9b90ec529ad598687e2dc8b4a99a9c056d90c` |
| Guard observed runtime image ID | `sha256:97457dfb2d0a9dbb94161493f7b2399bdb611ebac721cfb5efa050e99b843dd5` |
| Guard exported archive SHA256 | `12e2f0d93d2b158e9480a92e1d9c0de797adb6021040f7be3c842d3c117a4254` |
| Stream tag / runtime ID | `busybox:1.37.0` / `docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e` |

The harness asserts `enable-policy=default`, `enable-k8s-networkpolicy=true`,
`enable-ipv4=true`, `enable-ipv6=false`, `ipam=kubernetes`,
`kube-proxy-replacement=false`, `routing-mode=tunnel`, `tunnel-protocol=vxlan`
and the exact Cilium DaemonSet image. It logs the entire ConfigMap data in the
deployed run output, which is retained privately and excluded from the current
published tree. The summary here is historical, not current release acceptance.

Live `cilium-dbg status --verbose` additionally reported Cilium and health daemon
OK, 36/36 controllers healthy, no CNI chaining, successful CNI configuration,
veth, TCX, tunnel VXLAN, legacy host routing, IPv4 iptables masquerading,
host firewall disabled and external Envoy OK. Live `cilium-dbg config` reported
`PolicyEnforcement: default`, `PolicyAuditMode: Disabled`,
`SourceIPVerification: Enabled`, `PolicyAccounting: Enabled`,
`MonitorAggregationLevel: Medium` and `MonitorNumPages: 64`.
NodePort, hostPort, externalIPs and LoadBalancer were disabled; ClusterIP enabled.

## Acceptance Matrix

Final full run: `a8e04a80`, package PASS in **166.579s**, exit **0**.
The parent test took 165.82s; its attachment case took 134.14s.
The preserved raw output SHA256 is
`D153947FE1567CC0769C6F449526C1B5A8F8C02FF56B722E982D3BDEB6DF2E6E`.

| Case | Discriminating result |
| --- | --- |
| Six native policies | State, leases, credentials, workload, services and network each allowed a positive object, denied the exact negative and confirmed cleanup |
| Service/TLS under rendered policy | Restricted sandbox accepted; hostPID rejected; API TCP 6443 reachable after Service translation |
| Wrong CA / wrong DNS / no endpoints | Each failed closed; restored Service path worked after each fault |
| First sandbox command | Both reachable targets blocked before any invocation policy existed |
| Positive network control | Both targets reachable; positive-only deny blocked; additive allow restored reachability |
| Additive allow | All-Pod, all-egress Kubernetes NetworkPolicy did not override Cilium deny; three rounds remained blocked |
| Real stdio | Exact stdin echoed independently to stdout and nonempty stderr through controller-identity SPDY and kubelet |
| Invalid attachment options | TTY, wrong container and missing stdin/stdout/stderr denied by the attach guard |
| Baseline deleting / absent | New attachments denied; both target paths remained blocked |
| Invocation policy deleting / absent / permissive replacement | New attachments denied; both target paths remained blocked |
| UID replacement | Delete with stale UID conflicted; same-name replacement survived |
| Guard permission loss | GET failure denied attachment with sanitized `cannot verify attachment Pod` |
| Guard API stall | Real TLS stall failed closed at webhook timeout, within asserted 12s bound; rollout recovered |
| Concurrent streams | Two separate Jobs/Pods echoed distinct complete stdout/stderr without cross-talk |
| Replacement during open streams | Fresh messages echoed on both established streams while permissive policy remained installed; new attach denied; all three sandboxes remained isolated |
| Cancellation / deadline | First stream settled with cancellation; neighbor continued echoing and later settled at its own deadline |
| Cleanup failure / recovery | Exact Job UID observed terminating with held test finalizer; bounded cleanup returned deadline error; fresh-context recovery removed only that finalizer and confirmed absence |
| Live Cilium drift | Weakened deny and changed release label each rejected activation verification; exact same-UID restoration verified |
| Final state | Controller replicas zero; temporary grants removed; reviewed bootstrap verified; all tracked resources and both namespaces absent |

The first generic NetworkPolicy run (`e43ef165`) failed in 51.385s when additive
allow UID `6fc8b6ce-5054-45d4-bd31-7d59b87fe5f9` made the sandbox reachable.
After the approved Cilium deny change, run `f2b77a24` passed in 82.063s.
The strengthened stream/cleanup run `b69ca969` passed in 163.012s before the final
full run. These are incremental red/green evidence, not separate certifications.

### Final Run Identities

| Resource | UID or address |
| --- | --- |
| Cilium deny `sg03-a8e04a80-stdio-deny` | `d341b7bb-1374-413a-ac0a-d4fcb3e161d7` |
| Additive allow | `4f9dc4c6-b91c-4ce2-80df-dd9756bb8994` |
| Cross-namespace Pod target | `http://10.244.0.156:18080/hostname` |
| Host-network target | `http://172.18.0.12:18080/hostname` |
| Main sandbox Pod `kognition-stdio-invocation-a8e04a80-tp9wp` | `38de3e36-8068-4d97-b82e-7f6b6dae3a15` |
| Main Job | `e0ad13dc-ecd8-42ed-ab4c-d1b3ae34b58a` |
| Stream 0 Pod `kognition-stdio-invocation-a8e04a80-stream-0-2xh29` | `2dacf2ac-4bc7-42f4-9e66-b6e0420ee1ad` |
| Stream 1 Pod `kognition-stdio-invocation-a8e04a80-stream-1-tbgt6` | `dbf5b0e0-fa0e-4c41-8685-388ea4719cb3` |
| Held-finalizer Job | `2382ca64-3e7c-4ac1-a6e7-d9dc89b1f1a8` |
| Active-stream old / replacement policy | `4041af99-00bd-409f-af61-041e79eb52f3` / `30423f1b-1b22-4b8d-ae38-bd04fce0e67b` |
| Baseline old / replacement policy | `5da53117-553b-4374-8d68-af4a45971875` / `3dfdb453-d5a7-49ee-be11-cd51a3e1caff` |
| Workload namespace `sg03-work-a8e04a80` | `38468213-b899-4798-8f53-4a2eed0e112a` |
| Installation namespace `sg03-control-a8e04a80` | `ba0e2450-a3bf-4ee2-999a-bce4ef3c095d` |

The final cleanup error was `client rate limiter Wait returned an error: context
deadline exceeded`, after observing the exact held terminating Job. The preceding
focused run returned plain `context deadline exceeded` for the same assertion.
Both required fresh-context absence confirmation. Two client-go closed-pipe
diagnostics occurred during deliberate stream teardown; both stream results and
complete separate outputs were asserted, and no failed assertion was suppressed.

## Reproduction

### Current-profile qualification

The historical default remains pinned to the Cilium 1.17.8 image above.
An explicit `reviewed-cilium-1.20.2` candidate profile pins
`quay.io/cilium/cilium:v1.20.2@sha256:2939231d0d3e3ebddcd80fffa168b7ddcc78fdf0dc864d1c8c126ff523c54f01`.
This opt-in selects a qualification target; it does not mark that version accepted.
All eight enforcement-setting assertions and the original positive/negative,
additive-allow, drift, stream and cleanup checks remain in place.

The candidate requires a reviewed target manifest, isolated target and frozen
writers, plus these explicit operator-fixture inputs:

| Environment variable | Required value |
| --- | --- |
| `KOGNITION_ADMISSION_NETWORK_PROFILE` | `reviewed-cilium-1.20.2` |
| `KOGNITION_ADMISSION_NETWORK_FIXTURE_NAMESPACE` | Separately approved operator namespace outside installation, workloads, authority and system namespaces |
| `KOGNITION_ADMISSION_NETWORK_FIXTURE_NAMESPACE_UID` | Exact live namespace UID |
| `KOGNITION_ADMISSION_RECORDING_RUN` | Exact `kognition.dev/recording-run` label |
| `KOGNITION_ADMISSION_AUTHORITY_NAMESPACE` | Protected authority namespace, which cannot host these fixtures |

The operator namespace must explicitly carry
`kognition.dev/acceptance-operator=true` and
`pod-security.kubernetes.io/enforce=privileged` for controlled host-network
reachability targets. Production namespace labels are read-only and remain
unchanged. Unknown profiles, replaced/terminating namespaces, missing opt-ins and
production/system fixture scopes fail closed. Twenty-two regression cases cover
profile selection and namespace boundaries.

### Dedicated remote cluster (AKS-managed Cilium)

The gate accepts a non-loopback API endpoint only for the explicit
`reviewed-aks-cilium-1.18.15` profile. Every other profile still requires a
loopback endpoint. The profile pins
`mcr.microsoft.com/containernetworking/cilium/cilium-distroless:v1.18.15-261022`
and requires the running `cilium-agent` container on every node to report
`sha256:c7fd124f9b0c01ac4ae2fec56047e22e2860fc112535dc52aac7cfa2878575db`, because the
managed image is tag-pinned by the provider. It also replaces the tunnel
enforcement settings with the AKS ones: `ipam=delegated-plugin`,
`kube-proxy-replacement=true`, `routing-mode=native`, `datapath-mode=veth`,
`enable-endpoint-routes=true` and `identity-allocation-mode=crd`, plus the
unchanged `enable-policy=default`, `enable-k8s-networkpolicy=true`,
`enable-ipv4=true` and `enable-ipv6=false`.

A remote target additionally requires all of the following, and fails closed
otherwise:

| Environment variable | Required value |
| --- | --- |
| `KOGNITION_ADMISSION_NETWORK_PROFILE` | `reviewed-aks-cilium-1.18.15` |
| `KOGNITION_ADMISSION_REMOTE_ENDPOINT` | Exactly the API endpoint pinned in the reviewed installation plan and the kubeconfig |
| `KOGNITION_ADMISSION_DEDICATED_CLUSTER_UID` | Operator attestation: the exact `kube-system` UID of a cluster dedicated to this installation |
| `KOGNITION_ADMISSION_PERMITTED_NAMESPACES` | Optional comma-separated operator namespaces that may exist besides the reviewed ones |

The kubeconfig must verify TLS. The cluster may contain only `default`, `kube-*`,
the installation, workload, authority and fixture namespaces, and namespaces
named in `KOGNITION_ADMISSION_PERMITTED_NAMESPACES`; any other namespace fails the
gate, which keeps unrelated workloads out of the policy and stdio tests. Fixture
images keep their digest pins but use `IfNotPresent` instead of `Never`, and the
policy-realization wait is 90 seconds instead of 15 because every probe adds
remote API and exec round trips. The blocked and reachable assertions are
unchanged.

AKS also rewrites webhook namespace selectors unless each webhook configuration
carries `admissions.enforcer/disabled: "true"`. Install the chart with
`global.aksAdmissionsEnforcerOptOut=true` (the generated AKS runtime values do);
otherwise the live objects no longer match the reviewed manifest and
`verify` fails by design.

On 2026-11-12 the unchanged native-policy, deployed-attach, production-stdio and
production-ingestion stages passed once against a dedicated AKS cluster running
Kubernetes 1.35.9. That is a single-cluster result and does not accept other
AKS versions or node images. The API server's backing address changes whenever
AKS stops and starts the cluster, so refresh `networkPolicy.kubernetesApiEgressCIDRs`
through a guarded upgrade after any stop and start.

The first two 2026-10-30 tutorial runs on Kubernetes 1.34.11/Cilium 1.20.2
failed unchanged API/exec timing bounds under measured resource pressure.
They remain failed evidence. Exact-UID recovery confirmed fixture/grant absence
and restored verified inactive state.

After user-approved reversible capacity cleanup, the unchanged network/stream
gate passed in 155.57 seconds and production stdio passed in 49.20 seconds.
The consolidated tutorial run `reviewed-complete-v8` subsequently passed all
required scoped stages in 268.12 seconds. The separate ingestion fixture was
hardened to restricted PodSecurity requirements without changing producer,
negative-case, timing or cleanup assertions. Twenty-three profile/fixture
regressions pass. Optional provider-binding and AutoGen mutable-credential
fixtures were explicitly skipped.

The separately reviewed exact tutorial activation passed on 2026-10-31.
These results qualify only that frozen tutorial target; the historical
acceptance below remains limited to its original target, and neither result
certifies every runtime mode or a production publication candidate. Tutorial
cold-start rehearsal subsequently found a lifecycle defect; the detailed
recording status is retained in local-only preparation notes.

Use a new task-owned name and explicit kubeconfig for a fresh run. Never select
`desktop` or `kognition-cilium-e2e`, and never change the global context. Run one
harness at a time: the local host-network control uses fixed port 18080.
Prerequisites: Docker, kind, Helm, kubectl, Go 1.26.6 and existing chart dependencies.
Do not run `helm dependency build` or refresh the protected umbrella archive.

The setup used [this kind configuration](../../../../e2e/sg07-kind.yaml):

```powershell
kind create cluster --name kognition-sg07-20260925 --image kindest/node:v1.31.1 --config e2e/sg07-kind.yaml --kubeconfig "$env:TEMP/kognition-sg07-20260925.kubeconfig" --wait 30s
helm upgrade --install cilium cilium --repo https://helm.cilium.io --version 1.17.8 --namespace kube-system --kubeconfig "$env:TEMP/kognition-sg07-20260925.kubeconfig" --set ipam.mode=kubernetes --set kubeProxyReplacement=false --set operator.replicas=1 --set image.pullPolicy=IfNotPresent --set operator.image.pullPolicy=IfNotPresent --set-string debug.verbose= --wait --timeout 5m
kubectl --kubeconfig "$env:TEMP/kognition-sg07-20260925.kubeconfig" wait node --all --for=condition=Ready --timeout=120s
```

The initial kind Ready wait timed out before installing CNI; that is expected
with `disableDefaultCNI`. Continue only if this command created the intended
task-owned node, then require successful Cilium rollout and node readiness.
The Helm installation completed successfully.

Build the guard from the current source using the production Dockerfile target.
Changed source may produce a new digest; record it rather than reusing old evidence.
The SG-03 image is unsuitable because it predates the current release-name flag.

```powershell
docker build --target k8s-controller -t kognition-sg07-guard:20260925-01 .
docker save -o "$env:TEMP/sg07-guard.tar" kognition-sg07-guard:20260925-01
docker cp "$env:TEMP/sg07-guard.tar" kognition-sg07-20260925-control-plane:/var/sg07-guard.tar
docker exec kognition-sg07-20260925-control-plane ctr --namespace=k8s.io images import --platform linux/amd64 /var/sg07-guard.tar
kind load docker-image busybox:1.37.0 --name kognition-sg07-20260925
docker exec kognition-sg07-20260925-control-plane crictl images
```

Require successful commands and verify both images in node CRI inventory before
running tests. The guard archive was hashed and imported successfully from `/var`;
an earlier kind image-load attempt did not make it available. Do not infer
availability from Docker host inventory alone. Pull `busybox:1.37.0` first if
absent locally. The test uses `PullNever` for its restricted fixtures.

```powershell
$env:KOGNITION_ADMISSION_KUBECONFIG="$env:TEMP/kognition-sg07-20260925.kubeconfig"
$env:KOGNITION_ADMISSION_GUARD_IMAGE="kognition-sg07-guard:20260925-01"
$env:KOGNITION_ADMISSION_STREAM_IMAGE="busybox:1.37.0"
$env:KOGNITION_ADMISSION_EXPECTED_VERSION="v1.31.1"
$env:KOGNITION_ADMISSION_ISOLATED="true"
$env:KOGNITION_ADMISSION_CNI="cilium"
go test -tags=integration ./internal/traceability -run '^TestDeployedAdmissionPreflight$' -count=1 -timeout=10m -v
kubectl --kubeconfig "$env:TEMP/kognition-sg07-20260925.kubeconfig" -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status --verbose
kubectl --kubeconfig "$env:TEMP/kognition-sg07-20260925.kubeconfig" -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg config
kubectl --kubeconfig "$env:TEMP/kognition-sg07-20260925.kubeconfig" -n kube-system get pods -l k8s-app=cilium -o json
```

All four final commands returned exit 0. The harness creates short-lived local
TLS material, keeps the controller stopped, and grants only the temporary fixture
capabilities needed after admission preflight. It uses unique ownership markers,
UID-precondition deletes and bounded GET absence checks. Namespace deletion alone
is not its cleanup evidence. The task-owned cluster remains available for replay;
all run-specific fixtures were removed. Optional teardown is
`kind delete cluster --name kognition-sg07-20260925`.

## Scoped Quality Gates

| Command or check | Result |
| --- | --- |
| `go test ./internal/k8s/activation ./internal/k8s/admissionguard -count=1` | Exit 0; 2.719s and 1.117s |
| `go test ./internal/traceability -run '^(TestAdmissionBootstrapWithholdsControllerPrivileges\|TestActivationParsesBootstrapChart\|TestAdmissionGuardChart\|TestAdmissionGuardChartRejectsUnsafeConfiguration\|TestAdmissionGuardSandboxBaseline\|TestAdmissionGuardNetworkPolicy\|TestActivationCiliumIsolationInventory)$' -count=1` | Exit 0; 8.646s |
| Focused deployed `cilium-inventory-drift` | Exit 0; 27.249s |
| SG-07 Go formatting and editor diagnostics | Formatted; no diagnostics |
| `golangci-lint run --build-tags integration ./internal/k8s/activation/... ./internal/k8s/admissionguard/... ./internal/traceability/...` | Exit 1; only the two pre-existing SG-11 findings below |

Retained findings: `errorlint` in `admission_fixture_integration_test.go:52`
(non-wrapping read error), and `staticcheck SA1019` in the existing Endpoints
fault fixture (`admission_deployed_integration_test.go:277`). Neither is a new
SG-07 finding or suppressed. No full-repository quality clearance is claimed.
The protected archive SHA256 remains
`541F9336B2AAAD1D1D834D068CA7C2B49E656917D17C6B411BCE7CFA063BC16F`.

Targeted Markdown lint passed for this report, ADR-0016 and the security-gate
backlog; all their relative file links resolve. Code quality criteria C1-C4 pass
for this bounded slice: SG-07 scenario conformance, approved ADR alignment,
focused regressions and success/failure coverage. ADR criteria A1-A5 pass with
ranked priorities, evaluated options, justified decision, domain title and Accepted
status. Git traceability is not applicable because commits were prohibited;
dependency/advisory and full-candidate release clearance remain later gates.

## Operator Contract And Limits

- Select `admissionGuard.stdioIsolationProfile=cilium` explicitly. The default `network-policy` profile remains compatible but is not certified against additive allows.
- An administrator installs Cilium and owns one independent `egressDeny` CiliumNetworkPolicy per enrolled namespace. Activation binds its exact spec, ownership metadata, UID and hash. No Cilium API permissions or LIST reads were added to the runtime guard.
- Maintain the external writer freeze and protect Cilium configuration/policies. Admission checks are sequential; they cannot fence administrator changes or retroactively revoke an established stream. Cilium isolation protected active streams during the tested Kubernetes policy replacements.
- Cilium enforcement disabled, Cilium policy removal, administrator modification, other versions/configurations, IPv6, multiple nodes and Internet destinations are outside this result. The host-network target is local and outside the Pod network, not an Internet test.
- Labels classify sandbox endpoints, not authenticated tenant provenance. Dedicated workload namespaces and controller credential trust remain required; admission cannot constrain Secret GET.
- This fixture exercises real controller-identity streams with restricted echo Jobs. It does not invoke the production stdio runner or prove its deferred cleanup, ignored Delete error handling or complete producer lifecycle. Those remain SG-08 work.
- SG-08, SG-09 disposition review and SG-10 through SG-13 remain open. No production activation, shared-cluster mutation, cloud operation, staging, commit, push or archive rebuild was performed.
