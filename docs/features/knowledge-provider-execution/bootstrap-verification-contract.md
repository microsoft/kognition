# Bootstrap Verification Contract

- Scope: SG-01 structural bootstrap verification with SG-02 drain prerequisites.
- Updated: 2026-09-24.
- Release gate: BLOCKED. Structural verification does not authorize activation.

## Trusted Input

An installation administrator must review the chart source, version, images,
values, release identity, installation namespace, and ordered workload namespace
list before providing the rendered admission-only manifest. Preserve those inputs
and the exact render for review. Input from a controller-writable location or an
unreviewed render is outside this contract.

`activation.Parse` validates the supported inventory and safety constraints, then
seals the exported `Manifest` fields with an internal digest. `activation.Verify`
revalidates that inventory and rejects manually constructed or subsequently
modified manifests before accessing Kubernetes. This digest detects mutation; it
does not authenticate the source or prove arbitrary CEL expressions, images, or
Service selectors safe. Matching unsafe live and desired state cannot establish
authorization. Administrator review of the render remains a prerequisite.

Enrolled workload namespaces must be separate from the installation namespace and
dedicated to Kognition workloads and credentials. Controller compromise can expose
credentials in those namespaces; admission cannot constrain Secret GET. SG-09
owns the complete durable trust decision and operator responsibilities.

## Supported Inventory

The controller Deployment must use `apps/v1`, an explicit non-default service
account, and explicit zero replicas. It identifies the installation; the verifier
does not attest the controller's complete Pod template or arbitrary extra chart
resources.

The protected inventory contains exactly:

| Objects | API version | Requirement |
| --- | --- | --- |
| Six policies and six bindings | `admissionregistration.k8s.io/v1` | Same-name pairs; Fail policies with rules, conditions and Forbidden validations; Deny bindings without narrowed scope or parameters |
| Guard Deployment | `apps/v1` | Independent guard service account; positive replica count |
| Guard Service and ServiceAccount | `v1` | Exact guard name and installation namespace |
| Guard webhook configuration | `admissionregistration.k8s.io/v1` | Exact two ordered webhook identities, service target, controller/namespace/name conditions, rules and delivery settings |
| Baseline NetworkPolicy per workload namespace | `networking.k8s.io/v1` | Exact stdio selector; Egress-only deny baseline |
| Guard Role and RoleBinding per workload namespace | `rbac.authorization.k8s.io/v1` | Only GET Pods, Jobs and NetworkPolicies; exact guard subject and Role reference |

Unknown GVKs in this inventory, duplicates, missing objects, owner references,
deleting objects, identity mismatches, and duplicate or installation workload
namespaces fail verification. The parser selects the chart's named guard and
baseline resources and all policies/bindings; it is not a general-purpose manifest
or cluster-wide security audit.

## Live State Checks

Verification first requires the existing drain preflight to pass. It then reads
the controller and each protected object and uses server-side dry-run updates to
normalize API defaults. It compares spec, webhook, permission, subject, account and
ownership fields. Baseline labels and annotations must also match the render.

Only omitted Service allocation fields (`clusterIP`, `clusterIPs`, `ipFamilies`,
`ipFamilyPolicy`) may be copied from live state. Only omitted webhook `caBundle`
values may be copied, after base64 and PEM certificate parsing. Explicit desired
values are preserved and differences fail. Certificate parsing does not establish
serving trust, DNS identity, endpoint reachability, or a successful handshake;
those have separate [SG-03 local acceptance evidence](../../development-plans/features/knowledge-provider-execution/sg03-local-acceptance.md).

Every policy must report a positive `status.observedGeneration` equal to its
metadata generation and a present, non-null `status.typeChecking` object.
`expressionWarnings` may be absent or an empty array; null, malformed or nonempty
values fail. Conditions may be absent or an empty array because supported APIs
need not emit them. Every reported condition must have a unique nonempty type,
status `True`, the current observed generation, a valid nonzero transition time,
and a nonempty reason. Malformed, stale, False or Unknown conditions fail closed.

The guard rollout must have a positive desired replica count, a current observed
generation, and matching updated and available replicas. Read failures, deletion,
normalization errors, cancellation and expired deadlines fail verification.

## Invocation And Limits

Run from the repository with administrator-reviewed paths:

```powershell
go run ./tools/check-admission-activation --manifest <trusted-render.yaml> --kubeconfig <explicit-kubeconfig>
```

This default structural mode never uses an implicit kubeconfig. Requests have a 15-second timeout
and verification has a two-minute deadline. Argument failures return 2;
verification failures return 1. Success returns 0 and prints the input render's
SHA256 while leaving the controller inactive. No grant, activation, deletion or
persisted repair is performed; updates use `DryRunAll`.

The separately opted-in `--policy-behavior --writers-frozen` mode creates and
cleans up temporary protected fixtures and exact-name grants. Its bounds, JSON
report, cleanup failures and rerun contract are documented in the
[SG-03 report](../../development-plans/features/knowledge-provider-execution/sg03-local-acceptance.md). The non-mutating statement above applies
to structural mode, not the behavioral fixture lifecycle.

Reads are sequential snapshots, not a transactional authorization decision.
Concurrent administrators or reconcilers can change state after verification.
The [SG-02 drain contract](drain-verification-contract.md) defines the mandatory
writer freeze, migration inventory and pre-bootstrap drain-only invocation.
The [SG-04 transaction](activation-transaction-contract.md) binds activation to fresh verified state and acceptance evidence.
A printed hash is not an activation token.

## Evidence Boundaries

- [Manifest matrix](../../../internal/traceability/activation_manifest_test.go): chart-derived inventory, identity, fail-closed settings and post-parse mutation regressions.
- [Status matrix](../../../internal/traceability/activation_status_test.go): public verifier with intercepted synthetic responses, including malformed shapes an API server would reject. Normalization is intercepted; this matrix does not prove API defaulting.
- [API fixture](../../../internal/traceability/activation_integration_test.go): isolated Kubernetes 1.30.3 API server, real defaulting/dry-run persistence checks, explicit allocation/CA handling, deletion, drift restoration, read errors and bounded failure propagation.

The API fixture simulates policy type-check and Deployment rollout status. It
does not run a policy controller-manager, guard Pods, TLS Service endpoints, stdio
streams or an enforcing CNI. SG-02 now retains all default shared grants, including
`system:service-account-issuer-discovery`, and verifies fresh and drained-upgrade
preflight against real API defaulting. Separate SG-03 acceptance now records actual
typechecking on 1.30.3/1.31.1, deployed guards and a real kubelet stream.
SG-07 and SG-08 retain enforcing-CNI, full stdio and producer acceptance
requirements.

See the [security gate backlog](../../development-plans/features/knowledge-provider-execution/security-gate-backlog.md) for completion evidence
and the remaining blocked tasks.
