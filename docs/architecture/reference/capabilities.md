# Capability coverage and acceptance criteria

Every implementation assessment should assign an owner, applicability decision,
measurable target, and evidence to each capability below. These reference IDs
are local to this document set; they do not replace Kognition's FR/NFR registry.

**Baseline** means the responsibility must be addressed, possibly by an existing
enterprise service. **Conditional** means the feature may be disabled, but its
absence and rejected entry points must be explicit. A check in this matrix is
an acceptance scenario to execute, not a claim that it has passed.
Baseline controls apply to enabled features; they do not require every product
to offer every channel, model modality, memory backend, or orchestration pattern.

View IDs are linked through the [diagram index](README.md#read-by-level).
Source IDs resolve in the [official source register](sources.md). Local evidence
families resolve in the [Kognition evidence map](kognition-evidence.md).
The [OWASP coverage map](owasp-coverage.md) adds page-cited ASI01-ASI10 cases to
the linked capability rows. Include those cases when assessing applicable
capabilities; they do not change the existing C01-C30 identifiers.

The Basis column identifies sources and code examples that informed a row;
it does not assert that each cited source mandates the entire row or that
Kognition implements it completely. Views include diagrams and their adjacent
contracts. Role names identify responsibilities; each adoption must name one
accountable owner per row and the collaborating roles.

## Experience and execution

| ID | Required capability and applicability | Accountable role | Views | Acceptance scenario | Basis |
| --- | --- | --- | --- | --- | --- |
| C01 | Baseline: typed input/result, progress, cancellation, accessible errors and feedback for enabled interactive/API/event/scheduled channels; select channels and modalities explicitly. | Application owner | D1, D4 | Verify durable acceptance, duplicates, missed schedule/event handling, bounded streaming and declared detach-or-cancel behavior across supported channels. | S01, S16; K01, K02 |
| C02 | Baseline: catalog, authoring, validation, non-executing review previews, separately authorized sandbox tests, version comparison, controlled publication and administrative UX. | Application and platform owners | D1, D3, D7 | Opening a preview cannot execute tools or fetch external content; a changed definition requires a reviewed revision before publication. | S03, S04, S24; K01, K03 |
| C03 | Baseline: version, provenance and machine-readable inventory of agents, workflows, prompts, tools, model bindings, runtimes, adapters, datasets, retrieval configuration and dynamically discovered dependencies. | Release owner | D2, D3, D12 | Resolve a historical or active run to exact dependencies; reject altered artifacts and identify all consumers of a recalled digest. | S02, S22, S23, S24; K03, K11 |
| C04 | Baseline: tenant/workspace hierarchy, separate human/workload/automation identities, immutable incarnations, scoped runtime and administrative roles, explicit cross-boundary grants. | Identity and platform owners | D2, D6 | Reject cross-tenant or recreated-workload reuse; verify workers cannot edit their grants, trust roots, enrollment, or evidence retention. | S13, S14, S21; K05, K06 |
| C05 | Baseline: durable tasks, idempotent admission, recoverable queue publication, fenced ownership and distinct task/effect/delivery/accounting outcomes. | Runtime owner | D2, D4, D5, D16 | Crash between acceptance and enqueue, or after dispatch; recover original task identity without inventing success, losing accepted work or resetting effect claims. | S10, S11, S16; K02, K07 |
| C06 | Baseline for workflows: ordered/conditional execution, parallel branches, join semantics, loops, typed dataflow, errors, deadlines and cancellation. | Workflow owner | D3, D4, D5 | Verify branch failure, straggler timeout, condition error, loop termination, and deterministic conflict/partial-result handling. | S01, S04, S08; K02 |
| C07 | Baseline: bounded reasoning turns, recursion, fan-out, queueing, retries, concurrency, time, context, tokens and cost. | Runtime and service owners | D4, D5, D10 | Adversarial non-converging work stops at each configured bound without starving cancellation or other tenants. | S01, S04; K02, K08 |
| C08 | Conditional: team orchestration, supervisor/worker, handoff, shared-workspace or review/revise patterns with explicit convergence rules. | Application owner | D4, D14 | Independent branches remain isolated; joins reject conflicting output; handoff chains cannot loop indefinitely. | S01, S04, S05, S09; K02 |
| C09 | Baseline for delegation: explicit goal/purpose and context subset, initiator/actor distinction, child task identity, attenuated authority and parent-budget allocation. | Runtime and identity owners | D6, D14 | A child cannot exceed the parent's granted scope or reclaim budget by delegating again; switching the user or purpose requires fresh authority. | S06, S09, S13, S21, S24; K05, K06 |
| C10 | Baseline: durable human decisions, independently derived risk summary, non-executing exact action preview/binding, approver scope, separation of duties, expiry, escalation and revalidation. | Business and risk owners | D1, D5, D7, D16 | Race approval with expiry/revocation; alter inputs or fabricate persuasive rationale; verify no stale execution and that reviewers can reject or report manipulation. | S07, S12, S21, S24; K06 |

## Capability access and data

| ID | Required capability and applicability | Accountable role | Views | Acceptance scenario | Basis |
| --- | --- | --- | --- | --- | --- |
| C11 | Baseline: per-operation authentication/authorization, approved-intent and tool-composition checks, target-specific enforcement, bounded authority freshness, and equivalent controls across all call paths. | Security and integration owners | D2, D6, D7, D16 | Worker, UI test, workflow, evaluation and peer paths reject specified unsafe actions; unverifiable mandatory authority or expired policy snapshots fail closed. | S07, S13, S15, S21, S24; K01, K05 |
| C12 | Baseline: hierarchical quotas, enforceable cost bounds, durable reservations, measured/estimated usage distinction, idempotent settlement and explicit uncertain accounting. | Platform and finance owners | D16, D10, D11 | Concurrent cells cannot exceed shared allowances; missing/late usage and failed settlement do not trigger another provider call or an unjustified refund. | S01, S02; K08 |
| C13 | Baseline: model capability/version registry, approval and data-use rules, authorized routing, usage and error normalization; self-hosted serving conditional. | Model and risk owners | D3, D6, D12 | Wrong model binding is rejected; a fallback respects tested quality, residency and output-schema constraints; canary costs are accounted. | S02, S03; K05, K11 |
| C14 | Baseline: approved tool identity, semantic argument validation, isolated execution, filesystem/network limits and effect receipts; generation/validation/execution separation when code execution is enabled. | Tool and security owners | D6, D7, D15, D16 | Reject ambiguous aliases, altered approved code, unsafe deserialization, install hooks and disallowed egress; uncertain delivery blocks until target retry safety is established. | S14, S15, S21, S24; K05, K06 |
| C15 | Conditional: authenticated peer federation, protected discovery, context-bound message integrity/anti-replay, version/capability negotiation, task/artifact/status/stream/cancel contracts and secure callbacks. | Integration owner | D6, D14 | Deny foreign task access, cross-context replay and protocol downgrade; deduplicate callbacks and reconcile reconnect state without assuming event replay. | S13 through S16, S24; K05, K12 |
| C16 | Baseline for resumable work: safe-point checkpoints, protected storage keys, authorized plaintext restore, binding/version checks, fencing, expiry and retained replay-denial facts. | Runtime and data owners | D4, D5, D9, D16 | Wrong tenant/version/key fails; neither payload expiry, a new run ID nor an older backup permits replay of an unresolved effect. | S10 through S12; K04, K07 |
| C17 | Baseline when memory is enabled: scoped memory, provenance, verification/expiry state, controlled promotion, consistency, conflict resolution, quarantine, recovery and forgetting. | Data and application owners | D9 | Shared writes follow policy; self-generated output cannot promote itself to trusted memory; quarantine invalidates derived context without resetting effect claims. | S09, S10, S20, S24; K09 |
| C18 | Baseline when retrieval is enabled: source/ACL lineage, isolated ingestion, generation publication, freshness, filtered retrieval, citations and verified cleanup. | Data owner | D8, D9 | Partial indexing does not publish; an ACL change takes effect despite stale indexes; source deletion reaches derived data. | S01, S03, S20; K10 |
| C19 | Baseline: purpose limitation, classification, minimization, permitted processing, retention, correction, erasure and legal-hold handling. | Privacy and data owners | D8, D9, D11 | Trace a deletion through memory, summaries, embeddings, caches and backups; preserve only lawfully retained evidence. | S16, S18, S20; K09, K10 |
| C20 | Conditional caches; baseline context limits: tenant/authority-aware caching, version invalidation, bounded retrieval/history and safe summaries. | Data and runtime owners | D9, D10 | Revocation invalidates cached access; long context stays within budget; summaries cannot promote untrusted text into authority. | S01, S09, S10; K09 |

## Assurance and enterprise operations

| ID | Required capability and applicability | Accountable role | Views | Acceptance scenario | Basis |
| --- | --- | --- | --- | --- | --- |
| C21 | Baseline: end-to-end traces/metrics/logs, asynchronous causal links, health/quality/cost views, bounded cardinality and content redaction. | Service owner | D1, D11 | Trace fan-out, handoff and resume; detect exporter failure without publishing synthetic healthy or zero-usage signals. | S02, S17, S18; K13 |
| C22 | Baseline: protected decision/change/effect evidence, tamper detection, required pre-effect durable fences, recoverable outboxes, safe queries and independent retention. | Security and audit owners | D6, D7, D16, D11 | Unacknowledged mandatory evidence blocks dispatch; eventual outbox delivery is idempotent and unauthorized result reads remain denied. | S16, S19, S20; K06, K13 |
| C23 | Baseline: representative and adversarial evaluations, full workflow/tool/retrieval testing, versioned datasets/rubrics/judges, human calibration and drift monitoring. | Quality and business owners | D3, D11, D12 | A regression hidden by average score fails a required slice; unavailable required metrics block promotion. | S03, S04, S19, S20; K11, K14 |
| C24 | Baseline: provenance verification, separate release/installation activation, safe canaries, compatible rollback/migration and verified tenant/provider retirement. | Release and platform owners | D3, D12 | Stale evidence blocks activation; canaries do not duplicate production effects; retirement preserves claims and confirms data/credential disposition. | S02, S22, S23; K03, K11, K14 |
| C25 | Baseline: capacity/fairness, partitioning, independent scaling, bulkheads, overload rejection, semantic propagation caps and tested service objectives; multiple regions conditional. | Service and infrastructure owners | D4, D10, D11 | Saturate one tenant, lose a cell, and inject a faulty peer result under nominal load; meet fairness, recovery and semantic blast-radius limits. | S01 through S03, S24; K08, K14 |
| C26 | Baseline: independent behavioral/incident detection, component recall, quarantine, kill/revocation paths, uncertain-effect reconciliation, notification and verified reintegration. | Incident commander and business owner | D5, D10, D11, D13 | Exercise rogue behavior within nominal permissions, unauthorized replication and in-flight effects; contain descendants and require fresh identity, dependency and human review evidence before resumption. | S16, S19, S20, S24; K06, K14 |
| C27 | Baseline: encryption, workload/peer verification, key ownership/rotation, residency and inactive restore with lost-claim reconciliation. | Identity, data and infrastructure owners | D6, D9, D10 | Restore an older backup with a claim gap; verify old owners are fenced and potentially completed work cannot automatically redispatch. | S13, S14, S20; K07, K10, K12 |
| C28 | Baseline: versioned APIs/events/adapters, capability negotiation, conformance, typed errors, migration and provider offboarding without silent fallback. | Platform and integration owners | D2, D3, D10 | Unsupported runtime/checkpoint/retrieval mode rejects before contact; provider uninstall with live bindings follows safe drain/cleanup. | S08, S13 through S16; K03, K10, K12 |
| C29 | Baseline: use-case/risk inventory, named owners, acceptable-use/data/vendor policy, access review, residual-risk acceptance, accessibility, calibrated human oversight and redress. | Business, legal, privacy and risk owners | D1, D3, D7, D13 | Each use case has accountable owners; test misleading explanations, approval fatigue and reporting of manipulation alongside emergency procedures. | S19, S20, S21, S24; K14 |
| C30 | Baseline: operational efficiency, service economics, retention/capacity optimization; environmental reporting conditional on organizational policy. | Service and finance owners | D10, D11, D12 | Measure idle capacity and cost per successful task; verify optimization does not violate quality, privacy, latency or recovery objectives. | S02, S03; K08, K13 |

## Deployment profiles and required decisions

| Profile | Minimum additional decision |
| --- | --- |
| Single-team platform | Map identities and data scopes explicitly; retain durable effects and enforcement even if services are co-located. |
| Pooled multi-tenant service | Define isolation tiers, fair scheduling, per-tenant quotas/keys, cache/query separation and noisy-neighbor tests. |
| Dedicated or regulated workload | Decide independent administrative/credential domains, data residency, hold/erasure, private connectivity, evidence access and human oversight. |
| Federated peers | Establish trust enrollment, delegated scopes, supported protocol versions, external incident ownership and data-sharing agreements. |
| High-impact autonomous actions | Set authority ceilings, preventive approvals, non-bypassable containment, receipt reconciliation and business remediation. |

## Assessment completion rule

For every row, record one of: **implemented and evidenced**, **required gap**, or
**not applicable with an approved reason**. A "not applicable" decision must not
leave an enabled ungoverned execution path. For applicable rows record the
implementation boundary, acceptance threshold, test environment, exact artifact
and configuration versions, evidence date, owner, and unresolved risks.

This reference supplies the checklist and design, not an assessment verdict.
Use the [service-objective worksheet](06-scale-and-resilience.md#service-objective-worksheet)
to quantify performance and recovery expectations.

## Cross-view acceptance walkthroughs

These walkthroughs test agreement between contracts, rather than testing each
box in isolation. They are required scenario templates, not executed results.

| Scenario | Required end-to-end disposition | Primary views |
| --- | --- | --- |
| Lost acceptance response and queue failure | One durable task, recoverable publication, matching retry resolution, and no new effect identity. | D2, D4, D5 |
| Human wait followed by revocation | Durable pending/approved decision remains inspectable under read authorization, but the next execution is denied by current policy. | D7, D16 |
| Paid effect succeeds but receipt acknowledgement is lost | Preserve claim, reconcile target and ledger, never infer non-execution; settle without another call. | D5, D16, D11 |
| Tenant cancellation with running branches | Stop new child work, collect stop/effect dispositions, retain uncertainty for unacknowledged targets, and label partial results. | D4, D5, D14 |
| Poisoned knowledge crosses a peer boundary | Current source access and provenance checks, validated peer envelope, controlled result propagation, quarantine of derivatives. | D8, D9, D14 |
| Failover from an older backup | Restore inactive, fence previous owners, reconcile the lost-claim interval and current data revocations before dispatch. | D9, D10, D16 |
| Model or tool release rollback during active work | Future routing uses approved compatible bindings; active run history is preserved and live revocation still applies. | D3, D12, D16 |
| Tenant/provider removal with uncertain effects | Stop admission and new access, retain evidence and ownership, authorize cleanup separately, and report incomplete retirement explicitly. | D2, D9, D13 |
