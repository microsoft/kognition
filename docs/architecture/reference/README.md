# Enterprise multi-agent reference architecture

This reference separates **platform management**, **durable task execution**,
**governed capability access**, **data**, and **assurance**. Agents may propose
plans and actions; independently enforced policy determines what can execute.
Multiple runtimes and providers can implement the contracts without changing
the trust or recovery guarantees.

- Status: architectural reference, not an approved deployment design.
- Reviewed: 2026-10-08.
- Audience: enterprise architects, platform engineers, application teams,
  security and data owners, and service operators.
- Basis: [Kognition review](kognition-evidence.md) and
  [verified official documentation](sources.md).
- Scope: interactive, asynchronous, scheduled, event-driven, and long-running
  agent applications, including multi-agent collaboration and human decisions.

## What this reference requires

Three kinds of statements appear in this set:

| Statement | Meaning |
| --- | --- |
| Baseline requirement | An applicable implementation must supply the stated behavior and evidence. It may reuse an enterprise service rather than build a new component. |
| Conditional capability | The feature may be omitted. If enabled, its associated controls become required; unsupported entry points must be rejected. |
| Deployment choice | The adopting team chooses topology, products, numeric objectives, and risk-specific controls, documenting the tradeoff and acceptance evidence. |

"Must", "require", and imperative controls describe this reference's requirements,
not universal regulatory obligations. "Consider" identifies a tradeoff to assess.
Examples and source recommendations do not establish conformance.

**Vendor-neutral does not mean mechanism-free.** Durable ownership, enforcement,
isolation, and evidence are required behaviors; their implementation technology
is replaceable. A logical separation does not require separate deployments, but
an explicitly independent trust boundary requires independently protected
credentials, policy, and administration.

## Read by level

The levels are zoom levels, not a claim of formal C4 notation. All diagrams use
portable Mermaid syntax and generic responsibilities rather than product icons.

| Level | View and diagrams | Question answered |
| --- | --- | --- |
| L0: ecosystem | [01 Context](01-context.md), D1 | Who uses, owns, supervises, and integrates with the platform? |
| L1: platform | [02 Platform planes](02-platform-planes.md), D2 | Which responsibilities and authorities must remain distinct? |
| L2: management | [02 Platform planes](02-platform-planes.md), D3 | How does an approved definition become a deployed capability? |
| L2/L3: execution | [03 Execution and collaboration](03-execution.md), D4, D5, D14 | How do tasks, teams, state, and recovery work? |
| L2/L3: trust | [04 Trust and action authorization](04-trust.md), D6, D7, D15 | Where are permissions enforced, and how are actions and generated code approved and executed? |
| L3: effects | [04 Effect execution and recovery](04-trust.md#d16-effect-claim-dispatch-and-recovery), D16 | When may an action reach its target, and what happens after a lost response? |
| L2: data | [05 Knowledge, memory, and context](05-data.md), D8, D9 | How does information enter, influence, and leave an agent system? |
| L2: deployment | [06 Scale and resilience](06-scale-and-resilience.md), D10 | How does the platform isolate load and survive failures? |
| L2/L3: assurance | [07 Operations and lifecycle](07-operations-and-lifecycle.md), D11, D12, D13 | How are quality, evidence, releases, and incidents controlled? |

Supporting references:

- [Capability and acceptance matrix](capabilities.md): stable reference IDs,
  accountable roles, diagram coverage, and verification scenarios.
- [Terminology](glossary.md): definitions used consistently across the views.
- [Source register](sources.md): official guidance, claims, and limitations.
- [OWASP agentic risk coverage](owasp-coverage.md): all ten risks mapped to
  controls and acceptance cases, with page citations to the supplied publication.
- [Kognition evidence](kognition-evidence.md): inspected surfaces, abstractions,
  and distinctions between implementation, design intent, and deployment proof.

Diagram IDs are stable references, not reading order. D14-D16 expand earlier
views without renumbering them. Read D7 then D16 for the complete action contract.

## How to use this reference

1. Start with the simplest suitable execution pattern. A deterministic workflow
   or a single agent may meet the need without a multi-agent team.
2. Select a risk and tenancy profile. Document data classes, allowed effects,
   residency, human oversight, threat assumptions, and service objectives.
3. Map existing enterprise identity, key management, integration, and operations
   services onto the responsibilities. A box is a logical responsibility, not
   necessarily a separate process, database, vendor, or purchased product.
4. Preserve the [cross-view contracts](02-platform-planes.md#cross-view-contracts)
   when co-locating or distributing components.
5. Assign measurable acceptance thresholds using the
   [service-objective worksheet](06-scale-and-resilience.md#service-objective-worksheet).
   Treat the capability matrix as an assessment checklist, not a claim that a
   diagram establishes production readiness.

An architecture adoption is ready for implementation only when it names the
tenant and authority model, supported capability profiles, state owners,
effect/retry protocol, deployment failure domains, data lifecycle, service
objectives, and accountable reviewers. Unknown values are open decisions, not
permission to use an unrestricted default.

## Architectural invariants

1. **Authenticated scope travels with work.** Tenant, initiator, workload
   incarnation, task, and delegated authority are distinct and validated.
2. **Every execution path is governed.** User interfaces, batch jobs, evaluators,
   controller-originated calls, peer agents, and retries use equivalent checks.
3. **A plan is not authority.** Generated plans, retrieved documents, model
   outputs, and peer descriptions cannot grant permissions or enroll plugins.
4. **Definitions are pinned; authority stays current.** A run records exact
   executable versions while execution gates recheck current revocation,
   policy, target binding, and budget.
5. **Effects have durable identities.** A lost response is an unknown outcome,
   not proof that nothing happened. Replaying history must not repeat an
   unconfirmed paid or externally visible operation.
6. **State has different owners.** Operational task state, sensitive checkpoints,
   memory, curated knowledge, telemetry, audit, and accounting have different
   consistency and retention contracts.
7. **Isolation is independently enforced.** Application policy complements
   network, compute, credential, and storage isolation. A compromised runtime
   must not be able to remove the controls that restrict it.
8. **Quality and safety are lifecycle concerns.** Evaluate before release and
   during operation; retain human accountability and tested containment paths.

## Reading conventions

- Rectangles represent responsibilities or systems; rounded nodes represent
  actors or lifecycle states; cylinders represent retained state.
- Containers indicate the named logical, trust, or failure boundary. They do
  not automatically imply a separate network or process.
- Flowchart arrows describe the labeled interaction from initiator to recipient.
  They may carry requests, data, or control as their labels state; they do not
  imply timing or a transaction. Return traffic is omitted unless relevant.
  Detailed views show every mandatory gateway on the illustrated path.
- Double-headed arrows summarize an exchange between the named participants;
  detailed message ordering appears in sequence views.
- Sequence diagrams show chronological requests with solid arrows and replies
  with dashed arrows.
- Diagrams do not rely on color. Each view states what it intentionally omits.

## Applicability and limits

The baseline requires owners and explicit dispositions for all capability
families. Specific client channels, memory/retrieval backends, peer federation,
adaptive autonomy, code execution, self-hosted model serving, multimodal
interfaces, and geographic failover are conditional capabilities,
not mandatory deployment choices. If omitted, disable or reject their entry
points and record the reason.

This set does not prescribe a cloud, container orchestrator, model vendor,
agent framework, policy language, database, or protocol. Source-specific names
appear in the evidence, source, and attributed risk-mapping documents. It is a
comprehensive reference checklist for the stated scope, not an exhaustive catalog of every
industry regulation or workload-specific requirement.

## Review and assurance scope

The 2026-10-08 clarity pass reviewed every reference document, the diagram
relationships and failure paths, all C01-C30 capabilities, and ASI01-ASI10 mapping.
The [cross-view walkthroughs](capabilities.md#cross-view-acceptance-walkthroughs)
identify scenarios an implementation must verify across components.

Reference validation checks documentation consistency, diagram syntax/rendering,
local links/anchors, and identifier coverage. It does not execute those acceptance
scenarios or establish implementation, deployment, security, or compliance
conformance. Numeric service objectives, target idempotency contracts, isolation
tiers, retention duties, and residual-risk acceptance remain adopting-team
decisions that must be resolved before production approval.
