# Workflow Collaboration Implementation Plan

> **Superseded in part.** The shipped design runs each member turn as an
> AgentJob and keeps the run ledger in an owner-referenced ConfigMap. The
> receiver, task-plane and receipt stack described below was not built and has
> been removed. See the [workflow collaboration guide](../../../workflow-collaboration.md)
> for the implemented behavior. This plan is retained as history.

- Status: approved implementation plan. All decision gates resolved on 2026-11-17.
- Date: 2026-11-16. Gates resolved: 2026-11-17.
- Scope: extend Workflow with governed, A2A-based multi-agent collaboration.
- Delivery boundary: implementation proceeds on a feature branch behind a
  disabled feature. No interoperability, deployment, security-owner approval, or
  release acceptance is established here.
- Readiness: the [decision gates](#decision-gates) are resolved, and the A2A
  SDK compatibility spike (gate 13) passed on the candidate graph below.
  Dependent implementation may proceed in delivery order behind the disabled
  feature. The SDK is now a root dependency for an unwired executor adapter.
- SDK preflight: `github.com/a2aproject/a2a-go/v2@v2.9.0` passed its upstream
  gRPC, card cryptography, and extension tests on Go `1.26.10`.
  `govulncheck` reported [GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859)
  on that toolchain. A repeat on `1.26.11` found no reachable vulnerabilities
  in the scanned SDK packages. The root dependency was added on 2026-11-22.
- SDK qualification evidence, 2026-11-19: the [isolated gRPC probe](../../../../tools/a2a-qualification/grpc_test.go)
  passed six race-enabled tests on Go `1.26.11`. It verified task, context and
  artifact serialization across reconnects, typed errors, card signing,
  extension negotiation, and task access and list isolation for fixture
  principals. Resending a message ID created a new task and context; persist
  Kognition operation identity and do not resubmit ambiguous operations.
  `govulncheck` found no reachable or imported-package vulnerabilities; the
  two module-only findings, [GO-2026-6866](https://pkg.go.dev/vuln/GO-2026-6866)
  and [GO-2026-6860](https://pkg.go.dev/vuln/GO-2026-6860), are in `golang.org/x/net`.
  The probe's 11 dependency modules have Apache-2.0 or BSD-3-Clause texts, with
  the applicable gRPC notice and Go patent grants present.
- Release toolchain qualification, 2026-11-19: the root Go pin and both
  digest-pinned release builders moved to `1.26.11`. The 15 Linux/amd64
  application commands built without changes to their 104 dependency module
  selections, and the separate Linux/amd64 ingester transport built. The
  release command scan and the Linux root `./...` scan no longer report
  `GO-2026-6859`. They report no reachable or imported-package findings and
  one existing module-only finding,
  [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932).
  The refreshed [Go notice inventory](../../../../go-notice-inventory.json)
  matches the root manifest and records legal texts for 111 modules.
- SDK candidate graph: in a disposable workspace with the root selections
  (`x/net` `v0.62.0`, gRPC `v1.83.2`, protobuf `v1.36.12`), all six probe
  tests passed with `-race` and the Linux probe scan reported no findings.
  The SDK and `x/mod` were the only additions to the tested command module
  set, with Apache-2.0 and BSD-3-Clause legal texts respectively. This is SDK
  compatibility evidence, not production authorization or interoperability.
  The root graph was subsequently scanned and notices regenerated for the unwired
  adapter described below. These checks do not qualify built images or approve
  deployment.
- Unwired A2A adapter progress, 2026-11-22: the official SDK now sends exact raw
  signed payload bytes behind the existing fresh-commit gate and polls immutable
  retained task/context IDs without resend. A calibrated retry-policy control
  proves ordinary SDK retry while the gated lost-acknowledgment case sends once.
  Coordinator-owned observations preserve the existing effect and claim owners.
  Both backend contracts, real PostgreSQL revocation regressions, and five affected
  full consumer suites passed with race detection and standard vet. The actual
  Linux adapter source graph scan found no vulnerabilities across 49 modules.
  The refreshed notice inventory records legal texts for 113 modules and zero
  unresolved texts, but distribution review remains pending. Production wiring,
  receiving-runtime qualification, real transport identity and extension
  enforcement, strategy/member execution, accounting, retention, deployed
  qualification, and independent approval remain unfinished. Tasks 1 through 5
  remain open. See the [adapter evidence](execution-contract.md#unwired-a2a-adapter-and-task-observations-2026-11-22).
- Unwired receiving-store progress: PostgreSQL and native ConfigMap adapters now
  share the official SDK task contract and the same server-restart test. Polling
  preserves exact task/context/artifact/raw payload without another execution.
  PostgreSQL uses real database persistence; native restart and concurrent-version
  tests use a fake client. Native task metadata is reserved by installed private
  admission and remains deny-by-default, with no controller write exemption.
  Receiving identities, real authenticated runtime wiring, installed native task
  persistence, recovery across stop/uncertainty, and retention remain unqualified.
  The execution contract records the exact test evidence and its boundaries.
- Unwired receiving-scope progress: fresh dispatch carries a bounded signed
  initiation capability, and strict verification rejects ambiguous metadata and
  caller injection. A read-only resolver derives the exact action from protected
  approval evidence and rejects unsigned message/configuration scope. It checks
  live authority and the existing committed claim, bounds authorization by the
  capability lifetime, and reverifies controller identity and expiry afterward.
  Thirty-three scope cases and the official SDK-decoded request fixture pass.
  The four affected full consumer suites pass with race detection and standard
  vet. No new dispatch owner or grant is introduced. Durable receiving assembly,
  genuine receiver identity, TLS, extensions, and deployment remain unfinished.
- Unwired receiving-handler progress: duplicate SDK initiation after restart
  reproduced a second member execution despite a task-store duplicate error.
  Initial task persistence now precedes SDK dispatch, with protected owner checks
  and signed-action revalidation after creation and at executor entry. A separate
  task-load revocation reproducer passes after repair. Native concurrent SDK
  handlers accept one initiation and execute once, and unsupported methods fail
  closed. All four affected consumer suites and all four real PostgreSQL receiving
  tests pass with race detection and standard vet. Production
  runtime assembly, real receiver identity, TLS, extension qualification, and
  recovery remain unfinished. No task or dispatch owner is replaced.
- Unwired trusted task-scope progress: scope derivation now binds protected action,
  activation, invocation, owner, and approved byte/message limits. The scoped task
  authenticator verifies current executor/controller identity and retained effect
  evidence without reusing initiation capabilities. The receiving SDK fixture
  uses both helpers and rejects post-create identity and limit changes. A final
  effect-read controller race was reproduced and repaired with live revalidation.
  All four full consumer suites pass. The unwired receiving assembly rejects
  unsupported or mismatched backends, enforces the protected authenticator, and
  revalidates scope after construction under the signed initiation deadline.
  Eight constructor checks and the existing receiving checks pass. The final full
  RuntimeApproval suite and consumer compile checks pass after formatting.
  Native task writes remain denied, and receiving runtime,
  receiver/TLS authentication, and maintenance recovery remain unqualified.
- Sender polling now revalidates live activation authority and the original
  controller incarnation after the SDK response before returning content. Three
  in-flight revocation, stop, and replacement regressions were reproduced and
  repaired through the official gRPC SDK. Each denial performs one poll and no
  resend. All four full consumer suites pass with race detection and standard vet
  after formatting. Injected authority and insecure
  loopback transport do not qualify deployed receiving or stopped-run recovery.
- Unwired polling reconstruction now authenticates a retained protected scope,
  signed payload, exact owner, fixed Agent/AgentService, and approved limits before
  and after bounded backend construction. Its SDK handler has no executor and
  polls the same task without initiation capability or mutation. Reconstruction
  and eleven denial cases pass, including late revocation and a backend attempting
  to replace the protected authenticator. All four affected full suites pass with
  race detection and standard vet. The official gRPC SDK also polls the same task,
  context, state, and signed payload without initiation metadata and cannot
  initiate or mutate the retained record. The final full RuntimeApproval suite
  passes after that wire check. Its injected authentication, in-memory store, and
  insecure loopback transport do not qualify a deployed receiver or TLS identity.
  Active authority and unexpired action evidence
  remain required, so stopped, revoked, and long-term recovery are unqualified.
- Unwired per-RPC receiving progress: sender polling supplies a bounded canonical
  retained-task locator which grants no authority. The SDK receiving handler
  requires genuine private live workload proof before lookup, uses the existing
  signed initiation constructor for send, and derives polling ownership and
  limits from protected evidence before reading a candidate record. It verifies
  the retained signed payload, exact task/context, action digest, effect and live
  authority before disclosure without a new capability or execution. Eighteen
  parser cases, twenty-seven dispatch/poll scenarios and an official SDK receiving
  test with ten denial cases pass. The latter uses injected token verification,
  fake Kubernetes objects, in-memory persistence and insecure loopback transport.
  All five formatted auth/consumer suites pass with race detection and standard
  vet. See the [receiving and lookup evidence](execution-contract.md#unwired-per-rpc-receiving-and-retained-lookup).
  The same SDK fixture now denies a separate committed initiation when genuine
  Pod proof is revoked during backend construction, before task creation or
  execution. Restoring readiness cannot resend that initiation or change its
  retained activation. The focused check and formatted full RuntimeApproval suite
  pass with race detection and standard vet after correcting the fixture's
  retained-commit expectation.
  Production wiring, protected native writer, receiver/TLS/extensions, member
  execution, recovery, strategies, accounting, retention and independent approval
  remain unfinished. Tasks 1 through 5 remain open and collaboration stays disabled.
- Dedicated-receiver preparation: read-only discovery in context `kognition`
  identified `research-service` and `summarizer-service` in `kognition-work`.
  Their current Deployments use the shared default ServiceAccount. The user
  authorized a [disabled dedicated-receiver design](execution-contract.md#disabled-dedicated-receiver-design),
  not principal approval, RBAC, schema changes, deployment or activation. The
  draft separates native receiver identities from a protected control-plane task
  writer, requires independent per-write admission, original-owner preservation
  and real CAS, and records executable qualification gates for both stores.
  Actual image provenance, AgentInstance/runtime binding, receipt schema and
  independent security-owner approval remain open. No live workload changed.
  Tasks 1 through 5 remain open and collaboration stays disabled.
- Isolated native task receipt progress: the user authorized the unwired contract
  and bounded record derivation. Exact mutation HMAC, canonical encoding,
  expiry and shared history/version transition checks pass focused and full
  RuntimeApproval tests plus all five native and four real PostgreSQL receiving
  adapter tests with race detection and standard vet. The [receipt evidence](execution-contract.md#isolated-native-task-mutation-receipt-contract)
  distinguishes injected authorization and raw-record binding from full ConfigMap
  metadata, live writer authorization, key custody and installed admission.
  Native task writes remain denied. Tasks 1 through 5 remain open and collaboration
  stays disabled. No principal, RBAC, deployment or activation approval is inferred.
- Full ConfigMap receipt progress: the user's subsequent finalization request
  authorized advancing the [reviewed representation](execution-contract.md#proposed-full-configmap-receipt-representation)
  to unwired source code. The dedicated annotation, v2 domain, bounded canonical
  envelope, exact raw-record digest and actual prior UID/resourceVersion/digests
  are implemented. Focused adversarial receipt tests, the formatted full
  RuntimeApproval suite, all five native adapter checks and two installed
  deny-by-default admission tests pass with race detection and standard vet.
  All four real PostgreSQL receiving checks also pass with clean fixture shutdown.
  See the [source evidence and AKS hold](execution-contract.md#full-object-receipt-source-evidence-and-aks-hold).
  Read-only AKS revalidation still finds default-ServiceAccount candidates on
  the prior runtime digest. Genuine writer/key custody, positive installed v2
  acceptance, native receiving runtime/TLS/extensions, member execution,
  recovery, strategies, accounting, retention and independent approval remain
  unfinished. No accepted execution gate, native admission rule or cluster
  resource changed. Tasks 1 through 5 remain open and collaboration stays disabled.
- Writer/admission remediation: the confirmed ambiguous-field parser defect and
  canonical integer receipt-decoding defect are repaired with permanent
  regressions. The protected writer's public source contract covers signed
  CREATE/UPDATE, non-consuming preflight, lost-acknowledgement no-resend and denied
  writes. Shared marker persistence and concurrent CAS now pass against real
  PostgreSQL and a real Kubernetes API server. The
  [current review disposition](security-review-code.md#implementation-follow-up)
  keeps the high finding open for transport/startup, positive installed admission,
  all-writer admission selection, deployment custody and native provenance.
  No AKS resource changed, tasks 1 through 5 remain open, and collaboration stays
  disabled pending qualification and independent activation approvals.
- Unwired gRPC caller-admission progress: paired unary and stream interceptors
  require one bounded same-token live-controller credential for send and polling
  and deny streaming before SDK execution. Anonymous streaming and malformed
  control/whitespace credential regressions were reproduced and repaired. The
  official SDK fixture covers exact authenticated task polling and unsupported
  methods without granting ownership. Actual signed bound-token installed checks
  cover both methods, distinct Pod/ServiceAccount UIDs, rotation, wrong audience,
  unbound tokens, and live readiness revocation. Full consumer suites pass with
  race detection and standard vet after the parser repair, including the final
  installed webhook suite in 26.525 seconds. The [caller-admission evidence](execution-contract.md#unwired-grpc-caller-admission-2026-11-22)
  distinguishes installed source proof from injected SDK authentication and
  in-memory persistence. No production listener is wired or receiver/TLS profile
  qualified. Tasks 1 through 5 remain open and collaboration remains disabled.
- Finding remedies: the [execution contract](execution-contract.md) specifies
  the authority, data-release, and final dispatch-admission designs under
  [Accepted ADR-0029](../../../adr/0029-workflow-execution-boundaries.md).
- Implementation progress: a typed activation-gate persistence foundation is
  recorded in the [execution contract](execution-contract.md#disabled-persistence-foundation-2026-11-16).
  The shared Workflow planner rejects disabled collaboration before scheduling.
  No collaboration execution, capability, or deployment behavior is enabled.
- Implementation progress, 2026-11-20: the administrator-protected standalone
  policy API accepts separately typed, exact `Workflow/use` and `Agent/invoke`
  execution rules. Race-enabled API and authorization tests pass. The
  [policy evidence and proposed executor-action schema](execution-contract.md#standalone-execution-policy-foundation-2026-11-20)
  keep schema review, dispatcher implementation, all-plane enforcement, and
  independent production approval explicitly outstanding. Collaboration remains
  disabled.
- Disabled execution foundation, 2026-11-20: a typed executor-action binding,
  signed member activation context, shared claimed-effect dispatch primitive,
  and fail-closed Gate3 integration now have race and backend persistence tests.
  See the [implementation evidence and outstanding work](execution-contract.md#disabled-executor-binding-and-dispatch-foundation-2026-11-20).
  The primitive is not wired into production gateways and the four-strategy
  runtime is not implemented. This progress does not enable collaboration.
- Disabled foundation progress, 2026-11-21: private stores, atomic standalone
  acceptance with final Workflow/use authorization, explicit native identity
  assembly, live pinned-step member preflight, and real PostgreSQL HTTP
  decision/preparation/claim/non-replay evidence are implemented. Nine affected
  suites and the focused PostgreSQL integration passed with `-race`. Native
  signed submission receipts, verified finalization, and acceptance-only public
  reconciliation are now assembled. Installed race tests prove actual resource
  UID, bound JWT identity, protected private writes, and rendered state policy
  enforcement. The disabled opt-in chart profile supplies explicit identity,
  rotating token, review RBAC, separate admission routing, and certificate SANs.
  Its 31 focused regressions pass, and the installed private-record filter keeps
  ordinary bootstrap state available during executor webhook downtime.
  Standalone controller attestation and initiation authorization are now assembled
  under an explicit bound-token profile and consume a private live proof. Default
  initiation and unqualified target dispatch stay unavailable. Installed signature,
  rotation, readiness, and cancellation checks and production claim-only denial
  regressions pass. Production A2A transport, member interaction and delegation,
  cumulative accounting, and deployed qualification remain unfinished. Envtest pod
  readiness and authority remain fixtures. See the
  [standalone evidence](execution-contract.md#standalone-bound-token-attestation-and-initiation).
  The latest full native suites, focused real PostgreSQL acceptance test, and full
  API-server suite passed with race detection and standard vet. The standalone
  binding API now independently rejects stale live Workflow definitions before
  audit and persistence and preserves revocation after Workflow deletion. All
  546 chart regressions and the six-chart notice freshness check passed.
  The expanded PostgreSQL regression passed with all four binding/source-use
  revocation orderings against atomic acceptance. Installed native tests now
  prove actual operator administration and durable attribution, and four
  channel-controlled acceptance orderings through the real private admission
  boundary. The full native webhook integration suite passed with race detection
  and standard vet. This does not establish native cross-resource atomicity or
  deployed runtime qualification.
  Selected tasks 1 through 5 are not closed. See the
  [current evidence](execution-contract.md#disabled-executor-authorization-progress-2026-11-21).
- Design elaboration, 2026-11-18: the execution contract now details
  [governance and trust enforcement](execution-contract.md#governance-and-trust-enforcement),
  [strategy safety](execution-contract.md#strategy-safety-and-runtime-state), and
  [governance acceptance scenarios](execution-contract.md#governance-acceptance-scenarios).
  These elaborate the approved gates without substituting for runtime evidence.

## Recommendation And Confirmed Requirements

Add a `collaboration` step to Workflow and retain WorkflowRun as the execution
resource. A Workflow containing one collaboration step represents a team solving
one task. Larger Workflows can compose that collaboration with preparation,
ordinary Agent or Tool steps, and approval gates.

The following requirements were confirmed during design discussion:

- Authors select the collaboration strategy in YAML.
- The first release includes Supervisor, Parallel, Handoff, and Peer strategies.
- Members reference existing Kognition Agents, including Agents in other
  namespaces.
- Definitions are reusable and each task has an independent durable execution.
- Workflow and WorkflowRun provide those definition and execution boundaries.
  AgentPool and AgentPoolRun are deferred.

The [approved resolutions](decision-gate-resolutions.md) govern strategy rules,
schema names, limits, and release scope. Older proposed wording below is design
history where the resolution supplies a precise choice. Implementation details
must satisfy that choice or receive a recorded amendment. Completing a phase
does not remove any of the four strategies from release scope.

## Scope And Boundaries

| In scope | Deferred |
| --- | --- |
| Inline team membership inside a collaboration step | Independently managed AgentPool or another team-definition resource |
| Four YAML-selectable strategies | User-installed strategy plugins or arbitrary executable strategy code |
| Managed Kognition Agents across namespaces | External-agent federation and arbitrary member URLs |
| Governed A2A messages, tasks, results, and cancellation | Every optional A2A transport, callback, or extension |
| Independent sessions, bounded context, durable progress and recovery | Automatic sharing of credentials, complete histories, or all member memory |
| Workflow composition and existing public consumer compatibility | A general Workflow engine rewrite or removal of legacy `companions` |

Agents retain their own lifecycle, runtime, models, capabilities, identities, and
authorization. A collaboration step neither clones nor owns its member Agents.
Cross-namespace membership does not enroll an Agent into a new trust domain.

## Existing Evidence And Gaps

This is a representative source review on the date above. It is not a passing
test receipt or a claim that every backend implements identical behavior.

| Existing surface | Reuse and required qualification |
| --- | --- |
| [Workflow API](../../../../api/v1alpha1/workflow.go) | Reuse graph composition, parameters, step input, retries, approval gates, run concurrency, history, and budget configuration. Add a discriminated collaboration step. |
| [WorkflowRun reconciler](../../../../internal/controller/workflow_run_reconciler.go) | Reuse definition snapshots and run ownership. Extend persistence, cancellation, status projection, history, and cleanup for collaboration. |
| [Workflow execution](../../../../internal/controller/workflow_reconciler.go) | Reuse dependency scheduling and step completion. Introduce a focused executor boundary instead of adding conversational logic to both cyclic and acyclic loops. |
| [Workflow admission](../../../../internal/admission/workflow_validator.go) | Current step kinds are Agent and Tool. Validate the new type and its exclusive configuration without changing old defaults. |
| [Dataflow](../../../../internal/controller/workflow_dataflow.go) | Preserve existing input templates and bounded string outputs. Store structured artifacts and conversation data outside the legacy string projection. |
| [Agent API](../../../../api/v1alpha1/agent.go) | Reuse Agent identities and immutable execution revisions. `companions` is a declaration, not evidence of a complete collaboration executor. |
| [AgentService API](../../../../api/v1alpha1/service.go) | Resolve member serving bindings without treating a service name as the Agent identity. |
| [Agent communication gateway](../../../../internal/agentgateway/server.go) | Reuse current authorization and exact delegation. Its custom invocation acknowledgement does not implement the A2A task and artifact lifecycle. |
| [Communication policy](../../../../internal/controller/communication_policy_engine.go) | Cross-workspace dispatch checks a grant but source matching is name-based. Design and test namespace-qualified callers. |
| [Reference types and grants](../../../../api/v1alpha1/grant.go) | Reuse namespace-aware reference concepts and destination-owned grants. Preserve existing reference semantics. |
| [Runtime contract](../../../../internal/runtime/contract.go) | Reuse create, step, streaming, cancel, and capability boundaries. Qualify supported runtimes rather than assuming capacity or checkpoint parity. |

The emergent mechanism currently consumes annotation proposals and appends graph
nodes and edges. Its checks include proposal-supplied token and cost estimates.
It is not a task ledger, an authorization grant, or authoritative accounting.
Before reusing graph mutation, test accepted changes through the production
WorkflowRun path after serialization and a fresh reconciler. The original
definition snapshot and evolving execution plan need separate persisted records.

## Architectural Decisions

| Decision | Basis | Alternative and tradeoff |
| --- | --- | --- |
| One WorkflowRun owns the overall execution | Confirmed Workflow-first direction and existing definition/run separation | AgentPoolRun duplicates public lifecycle and integration work. |
| Collaboration is a step type | Allows a complete team task or composition with ordinary processing | A whole-Workflow mode makes mixed orchestration less natural. |
| Separate strategy semantics from A2A transport | The protocol supplies interactions rather than scheduling or termination policy | Encoding strategy in transport handlers couples policy to wire behavior. |
| Reuse governed member invocation | Existing capability, live policy, approval, and receipt boundaries remain authoritative | Direct member HTTP access introduces a bypass path. |
| Separate fixed definition from evolving task state | Enables restart recovery and reproducible configuration binding | Mutating the template or hiding history in annotations loses execution evidence. |
| Defer a separate team resource | No independent team lifecycle requirement has been established | A definition-only resource may later support separately owned reusable teams. |

ADR-0029 and the gate resolutions were accepted by the feature owner on
2026-11-17. Dependent implementation may proceed under their disabled-feature
and evidence boundaries. New authority or policy semantics require the existing
ADR/amendment process. Existing gateway and recovery decisions remain authoritative.

## API Proposal

### Workflow authoring

The following illustrates the selected collaboration shape, not an enabled or
complete executable fixture. API types and validation are being implemented, and
collaboration remains disabled pending qualification. The numeric limits are
explicit examples rather than defaults. Required dataflow declarations and
operator execution bindings are omitted here and must satisfy the approved
contract before dispatch can be enabled.

```yaml
apiVersion: kognition.dev/v1alpha1
kind: Workflow
metadata:
  name: research-review
  namespace: investigations
spec:
  budgets:
    maxDuration: 10m
    maxTokens: 20000
  steps:
    - name: investigate
      type: collaboration
      input: "{{ .Input }}"
      timeout: 8m
      collaboration:
        strategy: Supervisor
        protocol: A2A
        members:
          - name: lead
            agentRef:
              name: planner
              namespace: planning
          - name: analyst
            agentRef:
              name: researcher
              namespace: research
        allowedInteractions:
          - from: lead
            to: analyst
        supervisor:
          coordinator: lead
        limits:
          maxTurns: 20
          maxMessages: 100
          maxDelegations: 20
          maxDelegationDepth: 3
          maxParallelTasks: 2
          maxTokens: 16000
```

An ordinary WorkflowRun references this Workflow and supplies its input through
the existing API. Applying the definition does not start execution. Existing
edges connect `investigate` to other steps without exposing its internal message
graph as Workflow dependency edges.

### Parallel Review Draft

Open the [Parallel review draft](parallel-review.draft.yaml) in the Studio
Workflow YAML tab, then select **Apply YAML** to load it into the Builder.
Select `review-request` in the graph to edit the team. This sample references
three existing Agents in the Workflow namespace. Replace the Agent names with
those in your workspace. Two required branches review the same task and a
separate synthesizer combines their results. Parallel does not declare direct
member-to-member interactions.

This example is for authoring only. The default admission policy rejects it,
and Studio disables deployment and invocation. The Dashboard Teams tab shows
declared configuration for collaboration definitions returned by the API. It
does not attest to agent reachability, Agent Card discovery, or execution.
After qualification and activation, an authorized `WorkflowRun` is the intended
on-demand trigger. The Workflow definition itself does not start a team.

### Proposed schema rules

- Add `collaboration` to the step discriminator. Preserve empty type as Agent.
  Require exactly the selected Agent, Tool, or collaboration configuration.
- Use a dedicated namespace-qualified Agent reference, or constrain the existing
  resource-reference type to kind Agent. Do not globally change
  `LocalObjectReference`. An omitted namespace means the Workflow namespace.
- Require a finite, nonempty member list and unique stable aliases. Define a
  reviewed member-count ceiling and duplicate-Agent policy before schema freeze.
- Resolve `coordinator`, `entryMember`, `finalizer`, and interaction aliases only
  within that step. Reject missing members and irrelevant strategy blocks.
- `allowedInteractions` describes permitted delegation requests. It does not
  replace communication policy or automatically add Workflow dependency edges.
  Replies to a child task are authorized as results of that task, not new reverse
  delegations.
- Require an explicit allowed interaction graph for dynamic strategies. Reject
  undeclared recipients, accidental self-delegation, and unsupported recursion.
- Require positive finite collaboration limits. Existing unlimited graph fields
  retain their compatibility behavior, but cannot make a collaboration unbounded.
- Expose one supported A2A profile initially. Do not accept arbitrary protocol
  names, runtime modules, URLs, credentials, or agent-produced schema fragments.
- Preserve existing step-type update rules. Template edits affect new runs.
  Accepted runs retain their resolved definition and execution bindings.

Author-supplied input and runtime proposals cannot enlarge the member set,
interaction graph, privileges, limits, or accepted output contract. Existing
WorkflowRun parameters remain data rather than privileged configuration overrides.

### Serving resolution

Membership remains an Agent reference. Resolve each member to an authorized,
ready AgentService for that Agent. Allow an optional explicit service binding
only if needed to disambiguate multiple serving configurations.

- Never choose an arbitrary service by list order or trust a member-provided URL.
- Missing or ambiguous serving bindings block dispatch with an actionable reason.
- Record the resolved Agent and service namespaces, UIDs, execution revision,
  configuration binding, protocol profile, and advertised capabilities.
- Service replacement or Agent recreation is a changed target, not a transparent
  continuation. Resolve that condition under the approved recovery contract.
- Resolve credentials in their owning namespace through existing trusted paths.
  Do not copy secrets into Workflow definitions, status, or member context.

### Initial dispatch and outer Workflow boundaries

Use the [execution contract](execution-contract.md#workflow-initiation-authority)
for the distinct submitter, operator-bound executor, and actual member identities.
The existing gateway still requires a genuine source AgentInstance for member
delegation. The controller does not fabricate that identity for initial dispatch.

- Bind executor authority to an operator-approved Workflow UID, revision, digest,
  and finite target/operation scope. Permission to author or submit a Workflow
  cannot select an arbitrary privileged execution identity.
- Authenticate submission, check permission to use the approved revision, retain
  original initiator attribution, and persist the run's accepted authority.
  Do not retain human credentials. Recheck live executor and destination policy
  for new initiation, including after waits and recovery.
- Use a distinct exact initiation capability and authenticated route for
  controller-originated starts. Use real member identities for member-originated
  delegation. Reference grants and create permission alone do not authorize
  invocation.
- Require [explicit data bindings](execution-contract.md#explicit-data-release-and-mixed-steps)
  in collaboration-bearing definitions, including their ordinary Agent/Tool
  steps. Render from authorized inputs only and propagate all inherited content
  restrictions. A finalizer cannot erase worker provenance.
- The current inbound-edge authorizer skips steps without `agentRef`. New
  collaboration boundaries must enter explicit initiation and release checks,
  including template access and final result publication.

ADR acceptance, protected policy configuration, and the contract's authorization
tests remain prerequisites for dispatcher and authorization implementation.

## Execution Ownership And Persistence

### Ownership hierarchy

| Level | Responsibility |
| --- | --- |
| Workflow | Reusable definition, outer dependencies, and author limits |
| WorkflowRun | Overall execution state, approved snapshot, cancellation, and result |
| Collaboration step activation and attempt | Strategy state, logical tasks, member sessions, and child allocations |
| Member task and invocation | Actual Agent identity, exact target, effect claims, results, and accounting |

WorkflowRun retains platform ownership during handoff. A transfer changes which
member may act next, not which controller or resource owns the run. Existing
RuntimeApproval records retain exact-action execution ownership. The collaboration
ledger must not become an alternative route to claim the same protected effect.

### Persisted data

Use the shared store abstraction with PostgreSQL and Kubernetes implementations.
Keep the following logically separate even if an implementation stores some in
one protected record:

| Record | Required contents |
| --- | --- |
| Accepted definition | Workflow snapshot, selected strategy, limits, member aliases, allowed graph, and resolved revision bindings |
| Step execution | Run UID, step name, activation ID, graph iteration where applicable, attempt, strategy state, owner epoch, deadlines, cancellation intent, and terminal disposition |
| Child task ledger | Stable logical task and parent IDs, source and recipient identity, target-specific A2A IDs, invocation IDs, input binding, state, and allocations |
| Messages and artifacts | Bounded content or protected content references, origin, media type, schema version, and retention metadata |
| Accounting and evidence references | Reservations, actual usage, receipts, approvals, audit delivery, and uncertainty references |

- Derive logical identity from run UID, step, activation ID, attempt, and an
  allocated task or turn ID. An activation is one scheduled visit to a step and
  includes its graph iteration where applicable. Persist it before dispatch.
  A later cyclic visit cannot reuse an earlier visit's tasks, completion,
  sessions, allowances, or receipts. Keep parent-run accounting cumulative.
  Do not use Agent names or timestamps alone as execution identity.
- Persist admission and task intent before acknowledging durable acceptance.
- Persist the next operation before dispatch and conditionally claim execution.
  Reconciliation and gateway retries resolve the same operation binding.
- Store evolving task state and plan revisions separately from the immutable
  initial definition. Do not use annotations as the authoritative conversation.
- Use conditional writes and fencing for concurrent reconcilers. Queue delivery
  and scheduler leases do not independently authorize a target effect.
- Bound status summaries, task counts, event buffers, and content size. Store
  substantial messages and artifacts in protected backend storage rather than
  unbounded CRD status. Define size ceilings and schema-safe counter types.
- Protect internal Kubernetes records from generic CRUD and unauthorized writers.
  Internal storage objects are not a new public run resource or execution owner.

Checkpointing is optional per runtime capability. Resume must bind the run,
member identity, Agent revision, runtime configuration, session, and checkpoint
expiry. Do not promise restart of arbitrary in-flight model or SDK execution.
When safe continuation is unavailable, preserve evidence and fail or pause for
reconciliation without replaying an uncertain effect.

### Reconcile contract

1. Load the run snapshot and the current collaboration activation and attempt
  under conditional ownership. Block new execution for a terminal, deleting,
  cancellation-requested, or uncertain run. Continue the maintenance path for
  status retrieval, cancellation, retained-result settlement, audit, and cleanup.
2. Resolve and verify referenced identities, serving bindings, and capabilities.
   Perform current authorization before dispatch rather than trusting admission.
3. Reconcile retained approvals, child task status, known outcomes, reservations,
   and uncertain effects before proposing more work.
4. Evaluate parent and step deadlines and remaining limits. Record stop or wait
   intent before changing execution.
5. Ask the selected strategy for bounded operations or a completion proposal.
   Validate model-produced proposals against the fixed definition.
6. Persist task intent, allocate child allowance, and invoke through the governed
  A2A adapter using stable operation identities and the mandatory final
  dispatch-admission fence after exact plane ownership is acquired.
7. Persist results and usage idempotently. Reconcile notifications and polling
   against known target task IDs rather than submitting replacement tasks.
8. Complete the step only when its result contract and required-effect settlement
   permit completion. Project its bounded output to existing Workflow dataflow.

Keep this path asynchronous. A controller reconciliation must not wait for a
complete multi-agent conversation or hold a database transaction during inference.
Route deletion and cancellation through maintenance before existing early returns
or history cleanup. A fresh reconciler must keep unresolved work visible without
resuming its strategy or blocking other runs.

## Strategy Contracts

Implement explicit strategy handlers against one shared ledger and dispatcher.
Strategy handlers propose operations. Trusted execution code validates authority,
claims effects, allocates budgets, and persists results.

### Typed operation proposals

Define a versioned, bounded proposal contract for delegation, handoff, completion,
and explicit failure. Include the logical operation ID, parent task, selected
member alias, expected result schema, context references, and requested allocation
where relevant. Derive the acting identity, owner epoch, run binding, and effective
limits from trusted execution state rather than member-supplied values.

- Runtime adapters expose only operations valid for the member's current role
  and strategy. Enforce the same restrictions at the trusted dispatcher even
  when the model emits a malformed or unauthorized proposal.
- Parse proposals as structured data and validate their schema and semantic
  constraints. Ordinary text containing words such as "complete" or "handoff"
  cannot change task ownership or finish the run.
- Handoff acknowledgement, finalizer selection, and team completion are Kognition
  coordination semantics. A2A does not define those strategy operations.
  Specify whether their versioned payload uses ordinary task data or a negotiated
  extension, and test that contract before claiming interoperability.
- A transport acknowledgement, a member's declaration of success, and a validated
  final artifact are distinct observations. Only the shared executor commits a
  strategy transition or terminal step outcome.

| Strategy | Proposed required configuration | Ownership and completion |
| --- | --- | --- |
| Supervisor | `supervisor.coordinator` | The coordinator proposes bounded subtasks and receives scoped results. Only its validated final artifact completes the collaboration after required tasks settle. |
| Parallel | Explicit worker list, join policy, optional synthesizer | Dispatch independent branches within capacity. Join admitted outcomes and either publish an artifact manifest or invoke the designated synthesizer. |
| Handoff | `handoff.entryMember` and allowed transfers | Exactly one active member owns the conversation turn. A durable, acknowledged transfer changes the owner epoch and forwards filtered context. |
| Peer | Entry participants, allowed interaction graph, `peer.finalizer` | Members may create bounded subtasks along approved edges. The designated finalizer proposes the final result after the declared required work settles. |

### Supervisor

- Distinguish delegation from handoff. A worker completes a child task while the
  coordinator retains conversation responsibility.
- Require a child goal, recipient, expected result, filtered context, deadline,
  allocation, and escalation disposition on each proposal.
- Validate results as untrusted input. Coordinator output cannot override current
  policy or authorize effects merely by describing them.
- Exhaustion of turns or delegation limits fails or explicitly escalates the
  step. It does not count as successful completion.

### Parallel

- Proposed initial join contract: all declared required branches must succeed.
  An optional branch may fail only when explicitly marked optional, with its
  omission recorded in the output manifest.
- Any-success and quorum joins require separate approved semantics before being
  accepted by the schema. They are not inferred from `continueOnError`.
- Give each branch isolated context and a child allocation. A synthesizer cannot
  see unauthorized branch data or conceal incomplete results.
- A fail-fast choice prevents new dispatch and requests cancellation of active
  branches. Preserve already executed effects and wait for their dispositions.
- Fixed parallel tasks can continue using ordinary Workflow branches. Use the
  collaboration form when team-level task, result, or A2A semantics are needed.

### Handoff

- Persist the transfer proposal, recipient acceptance, and new owner epoch before
  the recipient can act. Fence future work from the previous owner.
- Establish a safe transfer boundary for the previous owner and its outstanding
  effects. An epoch change does not terminate a claimed operation. Activate the
  recipient only after required old-owner work has a known disposition, or a
  separately reviewed rule proves that retained work cannot conflict. Uncertainty
  blocks activation rather than granting both owners authority.
- Transfer only allowed task context and artifact references. Do not transfer
  bearer tokens, hidden model state, or unrestricted conversation history.
- A recipient rejection leaves ownership with the current member only if no
  conflicting activation occurred. An ambiguous transfer freezes further action
  until reconciliation establishes the owner.
- Bound transfers and repeated visits. An owner may complete only its current
  task and epoch. A late prior-owner response cannot complete the run.

### Peer

- Separate concurrent collaboration from authority. Peer outputs do not become
  instructions with higher privileges and cannot create new members.
- Proposed first-release completion uses a designated finalizer, not model votes
  or an implicit consensus detector. Finalizer authority does not grant extra
  model, tool, memory, or cross-namespace permissions.
- Multiple peers may work, but final completion requires resolved required child
  tasks, a validated artifact, and a single conditional completion winner.
- Bound cycles, total messages, delegation depth, active tasks, and wall time.
  Detect quiescence without an acceptable result as an explicit incomplete or
  failed outcome rather than success.

## A2A Integration

Use the [A2A 1.0 specification](https://a2a-protocol.org/v1.0.0/specification/)
as the reviewed baseline. Select and pin an SDK and exact supported binding only
after a compatibility spike verifies its task, error, and serialization behavior.
Prefer supported libraries over hand-written protocol models.

| Operation or concept | Required behavior |
| --- | --- |
| Agent Card | Derive supported skills, media types, authentication, and capabilities from trusted serving configuration. Declare only implemented features. |
| Send message | Validate input and identity, durably bind the operation, and handle either a direct Message or a Task response. Never treat HTTP success as completed work. |
| Target task identity | Map each server-assigned task and context ID to the local logical task. Do not assume IDs or contexts are shared between member servers. |
| Status retrieval | Reconcile long-running work using authenticated polling. Polling is the baseline recovery mechanism independent of streaming. |
| Streaming | Apply ordering and bounded buffers if enabled. A disconnected stream is not a failed task or permission to resubmit it. |
| Artifacts | Validate origin, media type, result schema, size, and authorized access before use. Preserve provenance and content references. |
| Input required | Persist a wait tied to the exact task. Resume with new authorized input without changing the original task binding. |
| Authentication required | Preserve the task's wait state. Acquire authority through a trusted out-of-band path and re-evaluate the exact operation. |
| Cancellation | Request cancellation of known child tasks and reconcile their outcomes. Acceptance of the request does not prove remote termination or reverse effects. |
| Version and extensions | Reject unsupported required capabilities and protocol versions. Do not silently downgrade into different execution semantics. |

Scope message deduplication to the authenticated actor, target binding, and
canonical request content. A2A message IDs alone do not guarantee idempotent task
creation. If a submission might have reached the server but its task ID was lost,
query a supported retained mapping or preserve uncertainty. Resubmit only when
the pinned server contract proves that matching retries resolve the same task.

Initial polling support does not imply optional streaming, push notifications,
or every binding is implemented. Unsupported operations must follow the selected
profile's error contract. A compatibility spike must verify which operations
are mandatory for the claimed profile, including list and access scoping.
Push notifications remain disabled unless destination validation, authentication,
deduplication, and retention are explicitly implemented and tested.

Adapt the existing communication gateway rather than creating a direct runtime
egress path. Preserve the legacy invocation contract during migration. The
current short request timeout cannot serve as the collaboration deadline.
Distinguish a bounded submission/status request from a durable long-running task.

The allowed recipients, run identity, step activation and attempt, parent task,
deadline, and remaining scope must be bound through trusted execution context and
validated at the gateway. A model-provided member alias or copied run-ID header
is not authority. Every nested delegation must remain inside this contract.

## Authorization And Threat Controls

This feature changes exposed operations, persistent data, and namespace
boundaries. It requires an architectural security review and targeted
implementation review. This plan specifies controls rather than granting
security or deployment approval.

### Distinct authorization checks

| Boundary | Required decision |
| --- | --- |
| Author creates a Workflow | Existing control-plane permission plus admission of references and limits |
| Caller starts, views, resumes, or cancels a run | Authenticated permission for that operation and the particular run |
| Workflow references a foreign Agent | Destination-owned CrossNamespaceGrant and authorized reference resolution |
| Member invokes another member | Actual source Agent namespace and UID, recipient, allowed interaction, live communication policy, and required grants |
| Protected member effect | Existing authentication, capability, exact-action RuntimeApproval, receipt, and quota protocol |
| Caller reads a message or artifact | Authorization for the content, not possession of task or context IDs |

Cross-namespace membership and communication are separate checks. For example,
a Workflow in namespace A referencing Agents in B and C may need A-to-B and
A-to-C reference permission, while B-to-C delegation needs its own live
communication authority. Do not impersonate namespace A as the sender.

Extend communication policy to distinguish namespaced callers. Preserve legacy
same-namespace behavior without allowing unqualified legacy entries to authorize
new foreign callers by name alone. Define migration handling for existing
cross-workspace users before changing their semantics. Bind recreated objects
by UID, and re-evaluate grants and policy before further dispatch after a wait.

### Threat review checklist

| Threat | Preventive and detective controls |
| --- | --- |
| Spoofed member or confused deputy | Workload-bound capabilities, exact audience and target binding, actual caller attribution, and namespace-qualified policies |
| Tampered task, transfer, or replay | Canonical input binding, stable IDs, conditional claims, owner epochs, and late-result rejection |
| Repudiation or missing evidence | Durable invocation and approval references, attribution of original initiator and acting Agent, and write-ahead audit where required |
| Cross-run data disclosure | Independent sessions, filtered context, task/content authorization, redaction, and protected storage |
| Runaway or recursive work | Parent allocations, finite limits, bounded queues, capacity checks, and endpoint rate limits |
| Privilege escalation through peers | Treat proposals and artifacts as untrusted data, validate recipients and effects, and recheck live authorization |
| Unsafe discovery or artifact download | Trusted serving resolution, TLS profile, bounded responses, egress policy, redirect control, and content validation |
| Unauthorized internal-record writes | Restricted store methods and Kubernetes RBAC/admission protection for execution and accounting records |

Run cancellation permission must not implicitly grant permission to delete
shared Agents, revoke unrelated grants, or cancel another run's member sessions.
Workflow edge approval and per-effect RuntimeApproval remain distinct. A wait or
approval decision never substitutes for live authentication and authorization.

Run, activation, attempt, and owner-epoch restrictions must reach every applicable
effect plane, including model, tool, memory, knowledge, and communication gateways.
Otherwise a cancelled or prior-owner session could still invoke tools or spend
model allowance while AgentInstance remains Running. Recheck execution eligibility
before a new effect claim and immediately before dispatch under the existing
exact-action protocol. Pending approval cannot revive a stopped activation.
Preserve already claimed or completed effects and their replay/settlement rules.
Authorize maintenance operations separately so stopping execution does not block
status reconciliation, cancellation, or evidence recovery.

Use the [final dispatch-admission fence](execution-contract.md#stop-handoff-and-effect-admission)
to order stop or handoff against permission to contact the target. An exact
plane claim remains the effect's sole owner but is not sufficient for dispatch.
`CommitDispatch` and stop/transfer update the same bounded activation gate.
PostgreSQL uses a conditional row transaction and Kubernetes one-object CAS.

If stop wins before CommitDispatch, target contact is forbidden even if a racing
plane claim succeeded. Finalize known no-dispatch failure under the exact plane
protocol rather than resetting its claim. If CommitDispatch wins first, retain
the operation as admitted in flight until settled or explicitly uncertain.
Cancellation acceptance is not conclusive drain, and a handoff recipient waits
for the prior owner's required outcomes. This refines the earlier unattainable
cross-object promise that stop prevents every later plane claim. Prove both
orderings and crash windows in each backend before enabling the feature.

## Limits, Capacity, And Accounting

- Effective limits are the tighter of run, step, member, workspace, and runtime
  constraints. The run cannot increase the template's authority or ceilings.
- Wall-clock deadlines include approval and input waits unless a separately
  reviewed policy defines pause accounting. Child deadlines cannot outlive the
  parent. Expiry prevents new dispatch and starts outcome reconciliation.
- Define turns as strategy-level decisions and messages as accepted logical A2A
  messages. Count retries of the same accepted message once. Rate-limit polling
  and rejected requests separately rather than letting them consume task scope.
- Count logical delegations and maximum ancestry depth independently of retry
  attempts. Self-routing and handoff do not reset counters or allowances.
- Reserve child tokens and other constrained usage from remaining parent
  allowance before dispatch. Multiple children cannot each receive the full run
  allowance. Reserve synthesis work rather than spending every token on workers.
- Record actual provider usage using the existing authoritative accounting path.
  Proposal estimates and unreported usage do not establish budget compliance.
  Unknown consumption remains conservatively charged pending reconciliation.
- Define cross-namespace charge ownership before implementation. Proposed model:
  the run aggregates logical usage and each actual effect is charged once under
  its existing workspace policy. Any additional origin allowance is a separately
  named policy dimension, not another cost record for the same provider call.
- Share capacity limits across concurrent runs using the same Agent. Honor
  runtime capabilities and queue bounds rather than assuming replicas or
  available sessions permit concurrent execution.
- Child task, message, content, and run retention need independent finite limits.
  Cleanup must retain replay-denial facts and exact-effect evidence for their
  required security period.

## Lifecycle, Failure, And Recovery

Use typed collaboration state rather than repurposing every global resource
phase. Proposed step states are Pending, Running, WaitingForInput,
WaitingForApproval, Cancelling, OutcomeUncertain, Succeeded, Failed, and Cancelled.

| Observation | Required transition or disposition |
| --- | --- |
| Member temporarily unavailable before dispatch | Wait or retry within deadline and capacity policy without claiming an effect. |
| Permanent invalid reference or denied operation | Reject or fail with a specific safe reason and no target call. |
| Approved input or runtime approval arrives | Recheck current bindings and authority, then continue the same task/invocation. |
| Required child fails conclusively | Apply the strategy's declared failure policy and preserve completed sibling effects. |
| Controller or adapter loses acknowledgement | Query known task state or retain OutcomeUncertain. Do not create a replacement invocation. |
| Limit or deadline exhausted | Stop new work and request cancellation. Report failure after required effect dispositions permit it. |
| Client disconnects | Continue accepted durable work unless the caller explicitly requested cancellation. |
| Cancellation requested | Persist intent, stop scheduling, cancel known owned tasks, and retain Cancelling or uncertainty until resolved. |
| Agent, service, policy, or configuration changes | Fence stale bindings and require the approved continuation or recovery procedure. |
| Duplicate or late result | Resolve the existing task, invocation, and owner epoch. Reject conflicts and do not overwrite a terminal outcome. |

Track result completion, target-effect disposition, evidence delivery, and
accounting settlement separately. A failed collaboration can contain successful
earlier effects. A known result with pending settlement must remain distinguishable
from an unknown target outcome. Define which outstanding obligations block the
outer terminal projection and expose the remainder explicitly.

Proposed outer projection keeps existing success as `Ready` and failure as
`Failed`. Waiting and uncertain collaboration keep the run nonterminal with
explicit conditions. Review `Terminated` with reason `Cancelled` for acknowledged
run cancellation before API freeze. All terminal predicates, history selection,
metrics, CLI, Studio, and cleanup must agree on the accepted mapping.

Existing step retries must not blindly restart a partially executed conversation.
Retry the same logical operation when supported. A new collaboration attempt
requires conclusive prior-attempt disposition, safe effect handling, a new attempt
identity, and a remaining parent allowance. Never replay a protected uncertain
effect or interpret exhausted iteration limits as successful task completion.

Deleting a run requests bounded cleanup rather than declaring cancellation
complete. Kubernetes cross-namespace owner references cannot own remote children.
Track those children by immutable run identity and explicit references, use
authorized cleanup and an orphan-reconciliation path, and retain unresolved
evidence. Review content access when deleting the parent so dangling IDs cannot
bypass authorization. Deleting a Workflow template must preserve the existing
snapshot-based behavior for accepted runs.

## Implementation Surfaces And Compatibility

| Surface | Planned work and compatibility obligation |
| --- | --- |
| [Canonical API](../../../../api/v1alpha1) | New step schema, strategy blocks, namespace-aware members, bounded status summaries, and validation tests |
| [Kubernetes API](../../../../api/k8s/v1alpha1) and [conversion](../../../../internal/convert) | Equivalent native types, deep-copy behavior, structural schema, CEL/admission constraints, and round-trip tests |
| [Admission](../../../../internal/admission) | Discriminated validation, reference access, limits, supported protocol profile, and side-effect-free dry-run |
| [Controllers](../../../../internal/controller) and [Kubernetes controllers](../../../../internal/k8s/controller) | Shared strategy execution boundary, dependency watches, run recovery, child tracking, cancellation, and history |
| [Stores](../../../../internal/store) | Protected persistence, atomic transitions, indexes, migrations, retention, and backend parity tests |
| [Communication gateway](../../../../internal/agentgateway) and [runtime authority](../../../../internal/runtimeauth) | A2A adapters, namespaced identities, run/task bindings, legacy transport compatibility, and exact invocation reuse |
| [Runtime](../../../../internal/runtime) and [Agent workers](../../../../runtime/agent) | Member session isolation, capability reporting, bounded context, wait/resume behavior, and capacity qualification |
| [API server](../../../../internal/apiserver) and [CLI](../../../../cmd/kognitionctl) | Run submission and lifecycle operations, validation errors, watch/status, and content authorization |
| [Helm](../../../../deploy/helm) | CRD parity, minimal RBAC and network changes, internal-record protection, packaging, and upgrade guards |
| [Observability](../../../../internal/observability) | Run, step, member, task, invocation, usage, wait, and uncertainty projections without sensitive default content |

- Preserve existing Agent and Tool steps, default graph behavior, workflow
  snapshots, templating, approvals, and legacy member invocation endpoints.
- Avoid changing all AgentJob references to cross-namespace references. Route
  collaboration through a reviewed dispatcher that respects each target's local
  execution model and authority.
- Keep `companions` unchanged initially. Document its relationship to the new
  explicit collaboration contract without treating it as an authorization grant.
- Do not require a separate team resource or nested Workflow support to deliver
  one collaboration step.
- Separate data migration and feature activation. Updated schemas must precede
  new writers, and readers/controllers must understand retained run state before
  activation. A disabled feature rejects new collaboration definitions while
  preserving safe recovery of already accepted runs.
- Rollback must not discard new execution records or hand them to an old
  controller that cannot fence them. Document a guarded drain or supported-reader
  upgrade path before release.
- Do not assume a generic CRD generation command works for this repository.
  Verify markers, checked-in schemas, conversions, and deep copies explicitly.
  Chart changes require review of both first-party consumer archives and their
  existing traceability checks without unrelated generated churn.

## Delivery Sequence

Every phase adds tests with the implementation. Keep new execution disabled until
the complete reviewed release contract is qualified. A phase boundary is not
permission to ship a subset of the four requested strategies.

| Phase | Deliverable | Exit evidence and dependency |
| --- | --- | --- |
| Contract and baseline | Resolve decision gates, create reviewed ADRs, choose A2A profile/SDK, and record existing Workflow behavior | Production-path baseline for Agent/Tool graphs, cycles, approvals, snapshots, retries, and accepted emergent proposals across restart. |
| API and validation | Collaboration schema, all strategy blocks, namespaced member references, and public status contract | Canonical/native conversion, validation and dry-run tests. Old manifests preserve behavior and unsupported configurations fail closed. |
| Durable execution core | Protected ledger, operation identity, conditional ownership, scheduling, allocations, and cancellation intent | PostgreSQL and Kubernetes store contracts plus fresh-reconciler and concurrent-owner tests before external effects. |
| Governed A2A boundary | AgentService serving adapter, task mapping, result access, live policy, approvals, and receipt integration | Independent SDK interoperability and exact invocation tests, including namespace collisions, waits, revocation, and ambiguous submission. |
| Strategy implementations | Supervisor, Parallel, Handoff, and Peer handlers using shared core | Deterministic behavioral acceptance for every strategy, result contract, exhaustion path, and failure policy. |
| Composition and operations | Mixed Workflow graphs, CLI/Studio status, lifecycle controls, telemetry, retention, and cleanup | End-to-end mixed graphs and cross-namespace child cleanup with no regression in existing consumers. |
| Release qualification | Supported runtime/backend matrix, upgrade and rollback procedures, packaging, security deltas, and operator documentation | Reviewed change-based evidence, isolated installed acceptance where required, and separate release/activation approval. |

Implement only the executor abstraction needed by the first real strategy.
Extract shared dispatch and persistence behavior when a second strategy needs it.
Avoid a generic plugin framework or unrelated cleanup of the Workflow engine.

## Verification Plan

Use deterministic member fixtures and public execution interfaces to verify
observable behavior. Do not rely solely on schema tests, mocked strategy calls,
or a model-generated success statement. Reuse existing Workflow and gateway test
helpers where practical.

| Acceptance scenario | Observable evidence |
| --- | --- |
| Existing workflows remain compatible | Old Agent/Tool manifests, conditional graphs, cycles, retries, approvals, output templates, snapshots, and history retain their behavior. |
| Supervisor completes a real team task | Coordinator delegates to two scoped specialists, consumes their artifacts, and produces a validated final artifact with one accounting chain. |
| Parallel branches join correctly | Required and optional branch outcomes, synthesis, capacity bounds, failure behavior, and incomplete-result disclosure match configuration. |
| Handoff transfers responsibility | Old owner is fenced, recipient receives only permitted context, late prior-owner work is denied, and transfer uncertainty blocks double activation. |
| Peers converge under explicit rules | Approved peer requests run concurrently within limits, only the configured finalizer completes, and deadlock or limit exhaustion is explicit. |
| Cross-namespace access is precise | Same-named Agents in different namespaces remain distinct, reference and communication grants are independently enforced, and revoked authority blocks new dispatch. |
| Runs are isolated | Concurrent runs using shared member Agents cannot read or cancel each other's sessions, tasks, messages, artifacts, or checkpoints. |
| Configuration is reproducible | New template edits do not change accepted runs, evolving task state survives restart, and Agent/service recreation rejects stale bindings. |
| Cyclic visits remain independent | A second visit to the same collaboration step creates a distinct activation while sharing the parent allowance. Old callbacks, tasks, and completion cannot satisfy the new visit. |
| Initial and composed dispatch is authorized | First-member and synthesizer dispatch use the approved executor or real member authority. Mixed Workflow edges and input templates cannot skip origin or recipient checks. |
| Stop and handoff fence every effect plane | Race stop/transfer against CommitDispatch in every affected backend. A winning stop blocks final dispatch admission, a losing plane claimant cannot contact its target, previously committed effects remain tracked, and maintenance stays available. |
| Recovery preserves effects | Crash injection before and after submission, task-ID persistence, effect claim, result persistence, audit, and settlement never causes blind redispatch. |
| Budgets and capacity are bounded | Concurrent children cannot oversubscribe parent allowance, actual usage is accounted once, unknown usage is retained, and worker capacity is respected. |
| A2A interoperability is genuine | Independent SDK client/server fixtures verify the pinned profile, direct Message and Task results, artifacts, errors, waits, task access, and cancellation. |
| Delivery interruptions are safe | Stream loss, duplicate updates, out-of-order observations, and polling recovery do not create new tasks or overwrite terminal outcomes. |
| Cancellation and deletion are scoped | Other runs and shared Agents remain untouched, remote children are explicitly tracked, and unresolved effects remain visible. |
| Public consumers agree | Standalone and Kubernetes paths, CLI, Studio, admission, schemas, store projections, and observability use the same reviewed lifecycle semantics. |
| Resource and payload abuse is bounded | Oversized content, unsupported media, unauthorized artifacts, recursive routing, malformed proposals, and polling floods are rejected or throttled. |
| Upgrade and rollback preserve evidence | Schema/writer ordering, disabled-feature behavior, active-run drain, old-reader fencing, and retained-record recovery are demonstrated. |

Run focused package and runtime tests at each phase, applicable race checks, and
existing required CI. Render and scan changed Helm profiles and review exact
RBAC/network deltas. Real API-server admission, concurrency, cross-namespace
cleanup, and network-boundary behavior need reviewed isolated acceptance when
mocks cannot establish them. Local deterministic fixtures do not certify paid
providers, production installations, or all runtime combinations.

Follow the [change-based validation gates](../../../development-gates.md) and
[testing guidance](../../../testing.md). Separate source validation, artifact
qualification, and installation authorization. Record exact source, dependency,
profile, and environment scope for each receipt rather than inheriting broad
certification claims.

## Decision Gates

All sixteen gates were resolved on 2026-11-17 as recorded in
[decision-gate-resolutions.md](decision-gate-resolutions.md), which is the
authoritative source for exact names, ceilings, release matrix, and proofs. The
starting points below are retained for traceability. Resolution closes the design
gap, not backend implementation or test evidence.

| Decision | Proposed starting point | Must be resolved before |
| --- | --- | --- |
| Exact schema and strategy names | Typed collaboration step with exclusive strategy blocks and constrained Agent references | API and CRD implementation |
| Initial dispatch authority | Operator-bound approved-revision executor, distinct exact initiation route, submitter use permission, and live destination authority per execution contract | ADR acceptance and protected policy schema before dispatcher/authorization implementation |
| Outer boundary and data-release policy | Explicit input/result bindings, conservative lineage, restricted templates, and protected payload publication per execution contract | ADR acceptance and binding/content schema before composition implementation |
| Peer completion | Explicit finalizer, required-task settlement, no voting-based authorization or implicit consensus | Strategy schema freeze |
| Parallel joins and partial results | All required branches, explicit optional branches, optional synthesizer | Strategy schema freeze |
| Handoff transfer contract | Acknowledged, fenced owner epochs with filtered context | Ledger and handoff implementation |
| Member proposal contract | Versioned structured operations with trusted actor binding and explicit A2A payload or extension mapping | Runtime and strategy adapter implementation |
| Service selection | Authorized, pinned AgentService resolution with explicit ambiguity handling | Dispatcher and member readiness implementation |
| Persistence and protection | Shared logical store contract with protected backend records and bounded status | Store and chart implementation |
| Cross-namespace policy migration | Namespaced callers, live grants, and reviewed legacy compatibility | Authorization changes and activation |
| Cross-plane stop and owner fencing | Single-gate CommitDispatch after the plane claim, explicit drain semantics, and separate maintenance per execution contract | ADR acceptance and gate store schema before handoff/cancellation/gateway implementation, then backend race acceptance |
| Accounting ownership | Single provider cost record with run aggregation and explicitly separate policy allowances | Reservations and usage implementation |
| Protocol and SDK | Pinned A2A 1.0 profile, independently tested SDK coverage | Transport implementation |
| Limits and retention | Required finite configuration until reviewed defaults and operational ceilings exist | Validation and cleanup implementation |
| Cancellation projection and retry | Explicit nonterminal uncertainty, reviewed terminal mapping, and effect-safe attempts | Lifecycle and public consumer implementation |
| Runtime release matrix | Enumerated backend/runtime/profile support with explicit unsupported-capability errors | Release qualification |

## Completion Criteria And Follow-Up

The feature is complete only when all four strategies execute through the
governed A2A path, compose within Workflow, and pass their declared backend and
runtime qualification. All decision gates need recorded outcomes, and no
unresolved required effect may be presented as successful completion.

Deliver authoring examples, operation and recovery procedures, protocol support
limits, migration notes, and reviewed acceptance records with the implementation.
Add a separate team-definition resource later only when independent ownership,
versioning, readiness, or reuse across multiple consumers warrants that lifecycle.
Such a resource does not by itself require another public run kind.

### Related contracts

- [Agent Gateway Fabric](../../../adr/0007-agent-gateway-fabric.md).
- [Runtime Autonomy Gate Completion and Recovery](../../../adr/0014-runtime-autonomy-gate-completion-and-recovery.md).
- [Runtime Checkpoint Recovery](../../../adr/0024-runtime-checkpoint-recovery.md).
- [Runtime Binding Profiles](../../../runtime-binding-profiles.md).
- [Durable Execution and Multi-Agent Collaboration](../../../architecture/reference/03-execution.md).
- [Recommended Execution Contract](execution-contract.md).
- [Decision Gate Resolutions](decision-gate-resolutions.md).
- [Accepted Workflow Execution Boundaries](../../../adr/0029-workflow-execution-boundaries.md).
- [Final Readiness and Architectural Security Review](security-review-architecture.md).
- [Development Plans and Review Records](../../README.md).
