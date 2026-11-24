# Workflow Task Writer And Admission Code Review

> **Historical.** The native task writer and task admission guard reviewed here
> were removed with the receiver stack. Findings do not apply to the shipped
> AgentJob-based design; see the [workflow collaboration guide](../../../workflow-collaboration.md).

- Date: 2026-11-22.
- Reviewer: GitHub Copilot, performing the user-requested technical review.
- Mode: source-grounded code and contract review with focused executable probes.
- Decision: REQUEST CHANGES. Writer/admission wiring and AKS deployment remain BLOCKED.
- Scope: native task mutation receipts, full ConfigMap derivation, Kubernetes task
  persistence, and the installed native task admission guard.
- Approval boundary: this review supplies no independent security-owner or
  operator activation approval and does not amend accepted execution gates.

## Findings

The findings below preserve the original review evidence. The
[implementation follow-up](#implementation-follow-up) records the current
disposition after the user-authorized repairs.

### High: Live Writer And Admission Enforcement Remain Unimplemented

This is an activation blocker and an intentional implementation gap, not a
demonstrated bypass of the current installation.

The [receipt service](../../../../internal/runtimeapproval/workflow_a2a_task_receipt.go#L70)
depends on injected `Authorize`, `Keys` and `Clock` functions. The
[full-object verifier](../../../../internal/runtimeapproval/workflow_a2a_task_configmap.go#L119)
accepts ordinary namespace, scope, writer and raw-object arguments. Its production
authorization boundary must derive those values independently from protected
state, actual authenticated credentials and the real admission request. Source
usage inspection found no production caller or configured native task receipt service.

The [native adapter](../../../../internal/k8s/controller/workflow_a2a_task_store.go#L57)
creates and updates task records without mutation receipts. The
[admission guard](../../../../internal/k8s/webhook/webhook.go#L306)
still denies native task writes unconditionally. Replacing that denial with a
signature check alone would omit live writer identity, original-owner authority,
CREATE replay protection, operation validation and unrelated-ConfigMap denial.

Required closure evidence:

- Authenticate the dedicated writer's actual ServiceAccount and live Pod UIDs
  with the approved audience and protected binding. Authenticate the receiver
  separately and isolate writer keys, token issuance, Pod creation and exec access.
- Derive CREATE/UPDATE from actual admission operation and protected state.
  Use actual object/oldObject bytes, reject DELETE and replay recreation, and
  deny unrelated ConfigMap writes by the privileged writer.
- Prove authorized CREATE and legal UPDATE through installed admission using
  actual credentials. Cover wrong audience, identity replacement and revocation,
  unsigned writes, stale versions, concurrent CAS and unchanged denied-write bytes.

The [installed task guard test](../../../../internal/k8s/webhook/envtest_integration_test.go#L958)
proves unconditional denial. It does not prove positive v2 admission, API-assigned
metadata normalization or native writer identity enforcement. Submission writer
acceptance does not qualify the separate task writer.

### Medium: Alternate ConfigMap Field Names Bypass Ambiguity Rejection

The [ConfigMap decoder](../../../../internal/runtimeapproval/workflow_a2a_task_configmap.go#L31)
combines exact-key duplicate detection with Go `encoding/json` struct decoding.
`DisallowUnknownFields` still accepts case-insensitive field matches. Distinct
keys such as `kind` and `Kind` therefore survive duplicate detection and populate
the same typed field, with the later value overriding the earlier value.

An isolated Go test overlay exercised the public `VerifyConfigMap` method with
one valid signed CREATE receipt and otherwise unchanged inputs:

| Changed raw object fields | Required result | Observed result |
| --- | --- | --- |
| `"Kind":"ConfigMap"` instead of `"kind":"ConfigMap"` | Reject alternate encoding | Accepted |
| `"kind":"Secret","Kind":"ConfigMap"` | Reject conflicting fields | Accepted |
| `"name":"foreign","Name":"<protected-name>"` | Reject conflicting metadata | Accepted |

The unchanged signed control passed. All three rejection assertions failed in
3.153 seconds with race detection and standard vet enabled. The original receipt
remained valid because typed projection discarded the ambiguity before hashing.

This violates the [full-object representation gate](execution-contract.md#proposed-full-configmap-receipt-representation).
It does not demonstrate a persisted Secret, a foreign record write, or an AKS
bypass. Admission remains disabled, and API-server serialization may normalize
input before the future verifier sees it.

Remedy: require case-sensitive exact field names before typed projection for both
root and metadata fields, while retaining duplicate/unknown-field rejection and
the explicitly allowed API-maintained metadata. Add permanent regressions for
alternate and conflicting names on CREATE and both UPDATE objects. Prove the
installed request path rather than assuming API serialization closes this gap.

## Contract Assessment

The intended annotation and v2 domain separation, exact-byte task digest, bounded
object envelope, nil/false immutable normalization and prior UID/resourceVersion
binding are reasonable. Retained expired receipts serve integrity verification
only. Fresh UPDATE still requires current authority and an unexpired receipt.
The verifier rechecks authorization around external key reads and rejects v1
fallback, forged old receipts and altered immutable history.

Before qualification, define and prove signing-key rotation, verify-only key
retention and fail-closed key unavailability without losing required recovery or
replay evidence. Positive API-server UID/resourceVersion handling, genuine writer
credentials and key custody remain open. Component behavior does not establish
these deployment properties.

## Verification And Limits

- Fresh review run: all three existing native receipt tests passed in 1.929
  seconds on Go 1.26.11 with `-mod=readonly -race -p 1 -count=1` and standard vet.
- Review-only overlay: passing signed control and three confirmed missing
  encoding rejections. Temporary probe files were removed after evidence capture.
- Prior recorded full RuntimeApproval, native adapter, installed deny-by-default
  and real PostgreSQL checks remain evidence for their stated slices. They were
  not rerun or expanded into positive writer/admission certification.
- No production code, RBAC, credentials, cluster resource, admission exception,
  deployment, commit or push was changed by this review.
- Azure compliance and GitHub security-alert tools were previously unavailable.
  No remote vulnerability, dependency-audit or live cloud security clearance is
  claimed. This review covers the writer/admission contract, not all feature work.

## Disposition

Keep native task admission denied and collaboration disabled. Resolve the parser
defect before wiring, implement the authenticated writer/admission boundary under
the approved contract, and produce the installed acceptance evidence above.
Then repeat this scoped review and obtain the required independent security-owner
and operator approvals. Original tasks 1 through 5 remain OPEN.

## Implementation Follow-Up

- Date: 2026-11-22. The user authorized both repairs and explicitly approved the
  durable task CREATE replay guard recorded in the execution contract.
- Medium parser finding: repaired. Case-sensitive strict ConfigMap decoding
  rejects alternate and conflicting fields before projection. Nine permanent
  CREATE/proposed UPDATE/retained UPDATE regressions were reproduced and passed
  after the fix. This closes the confirmed source parsing defect, not deployment
  approval.
- The public writer test exposed a separate receipt-decoding defect. Canonical
  JSON emits `500` as `5e2`, which the integer struct decoder rejected. Receipt
  decoding now expands integer exponents exactly with bounded arithmetic and
  strict field names. A permanent regression failed in 3.576 seconds and passed
  in 4.231 seconds. Fractions, overflow and oversized exponents return no receipt
  evidence. Original canonical signature bytes and domains are unchanged.
- High writer finding: OPEN. Source now includes separate writer/receiver
  TokenReview identities, retained original-owner policy, disjoint approval and
  receipt key material, a protected writer, an opt-in admission authorizer and a
  receipted task-store callback. The public source contract passes signed CREATE,
  legal UPDATE with expired prior integrity evidence, preflight without marker
  consumption, lost-acknowledgement no-resend, and unchanged persisted bytes on
  identity, audience, revocation, dry-run and key-custody denials. Its credentials
  and admission client are fixtures, not installed qualification.

| Latest check | Result | Evidence limit |
| --- | --- | --- |
| Full RuntimeApproval suite | PASS, 14.099 seconds | Source authority and receipt behavior |
| Activation state-machine checks | PASS, 3.633 seconds | Source mutation contract |
| Native writer, identity and activation checks | PASS, 11.700 seconds | Fixture credentials and native persistence |
| Admission regression checks | PASS, 8.138 seconds | Request routing and default denial |
| Installed denial and actual run UID checks | PASS, 21.048 seconds | No positive task-writer admission |
| Formatted real API-server activation-store contract | PASS, 15.674 seconds | Independent clients, resourceVersion CAS, one-winner CREATE marker and settlement retention |
| Real PostgreSQL activation-store contract | PASS, 79.338 seconds | Independent clients and durable marker CAS, fixture listener released |

All checks use Go 1.26.11 with readonly modules, race detection, serial package
execution, a fresh test count and standard vet. Cross-client reconstruction is
not a database-process or API-server restart claim.

Later source and chart progress (disabled by default, not qualification): the
authenticated writer HTTP transport, task-plane profile/builders and controller
startup wiring exist with tests. The chart renders a dedicated task-writer
ServiceAccount, exact-name RBAC, a single-replica StatefulSet, Service,
NetworkPolicy, controller env and a writer-username admission matchCondition
(requires the native executor, a Fail-policy webhook, distinct receipt-key and
runtime-auth Secrets, and a Namespaced target in a controller-managed receiver
namespace). Chart regression (55 checks), packaged-chart parity and traceability
tests pass. The owner Pod name is pinned; replacing it needs a re-render. Live
RBAC sufficiency is untested and fails closed.

Installed identity and routing evidence (real API server, envtest, partial): a
Pod-bound writer ServiceAccount token is delivered to the `/validate` webhook
with the exact username, ServiceAccount UID and Pod name/UID extras, and its
CREATE and Get-then-UPDATE of a task ConfigMap are admitted. Unrelated and
generateName ConfigMap writes, DELETE (with RBAC delete granted), dry-run,
unbound-token and operator writes of task names are denied, and the admission
hook saw only the two permitted writes. The authorizer is a test fixture applying
the identity contract, not the production admission, so signed receipts,
activation CAS, live RBAC sufficiency and the AKS-installed webhook are not
certified. The full webhook integration suite passes (11 envtest cases, no
skips) and the race run of the controller and webhook packages passes.

Installed-qualification blockers found while preparing the AKS run (source
inspection only, no cluster change): (1) the controller Deployment template embeds
the profile `owner.podName` through `KOGNITION_WORKFLOW_A2A_TASK_PLANE_JSON`, so
the pinned Pod name changes the Pod template that produces the Pod and the render
has no fixed point; the owner pin needs a runtime-resolved or stable identity.
(2) `cmd/kognition-k8s-controller` does not assemble the executor coordinator,
dispatch gate or initiation service (only the standalone apiserver does), so no
real committed effect or activation can exist on the native AKS release, and the
writer cannot authorize a positive task CREATE there without fabricating
authoritative records. (3) No receiver binary, image or member execution adapter
exists. Positive installed admission and live RBAC sufficiency therefore remain
unqualified.

Blocker (1) is repaired in source: the controller's own profile now carries a blank
`owner.podName`, filled from the downward-API `KOGNITION_WORKFLOW_EXECUTOR_POD_NAME`
at startup and rejected when empty or mismatched; only the writer keeps the explicit
pin. Replacing the controller Pod still needs a re-render of `ownerPodName` and the
writer fails closed until then. Chart (57 checks) and command-package race tests
pass. A related fixture defect was fixed: receipt key rings must use the
`BindingDomainV1` key domain (the receipt-specific domain is used only inside signed
receipts), so earlier command tests had used a non-conforming ring. Blockers (2)
and (3) are unchanged: neither the native controller nor the standalone apiserver
(its target callback is nil) executes workflow members, so initiation, the
execution trigger and the member adapter need a design decision before assembly.

Remaining high-finding requirements are positive installed task-writer admission,
selection of all ConfigMap writes by the dedicated writer, isolated RBAC and
key/token/Pod/exec custody, and approved native receiver provenance and runtime
binding. The real API-server activation CAS check does not certify those controls.
Code quality criteria for complete specification conformance and installed
behavior coverage remain unmet for this finding. No AKS resource, RBAC,
credential, activation, commit or push changed. Collaboration remains disabled,
tasks 1 through 5 remain OPEN, and independent activation approvals remain required.