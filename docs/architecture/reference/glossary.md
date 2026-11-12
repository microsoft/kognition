# Reference terminology

These terms are proposed for this vendor-neutral reference. Kognition resource
names remain unchanged; [the evidence map](kognition-evidence.md) provides the
translation to implementation terminology.

| Term | Definition | Avoid conflating with |
| --- | --- | --- |
| Agent definition | A versioned specification of an agent's purpose, instructions, capabilities, and execution constraints. | A running process |
| Agent instance | An execution of a particular agent definition under a particular workload identity. | A reusable agent name |
| Workload incarnation | An immutable identity for one creation of a workload, distinct from later creations with the same name. | Session or tenant |
| Task | A durable unit of requested work with identity, owner, lifecycle, and completion criteria. | A transport request |
| Run | One execution of a pinned workflow or agent configuration for a task. | A revision |
| Step | A bounded execution unit within a run. | An individual token |
| Attempt | One execution attempt for a step, recorded separately from repeated delivery of the same invocation. | A new right to repeat an effect |
| Invocation | One identified request to a model, tool, data operation, or peer capability. | A retry delivery |
| Effect | An externally observable change, disclosure, paid operation, or delegated action whose outcome matters to safe retry. | Only a database write |
| Session | A bounded interaction context that may contain multiple turns and tasks. | Permanent memory |
| Workflow | A declared control-flow structure coordinating deterministic steps, agents, and decisions. | An unrestricted conversation |
| Team | A set of agents collaborating under one bounded task contract. | A shared authorization principal |
| Handoff | An explicit transfer of execution responsibility with a defined context and authority boundary. | A normal function return |
| Delegation | A request for another principal to perform scoped work without expanding the caller's authority. | Credential forwarding |
| Capability catalog | An approved inventory of discoverable agents, models, tools, data services, and adapters. | Permission to invoke |
| Capability credential | A scoped authorization artifact binding a caller to specified operations, targets, and validity conditions. | An advertised feature or a task ID |
| Capability gateway | An enforcement boundary mediating one class of runtime capability access. | An API ingress alone |
| Policy authority | The independently administered source and decision logic for authorization and constraints. | An agent's generated plan |
| Approval | A durable, expiring human decision about an exactly bound action or release. | Authentication or blanket authorization |
| Tenant | An administrative, contractual, and data-isolation boundary. | A label or a user session |
| Workspace | A project-scoped administrative subdivision mapped to an explicit tenant and isolation policy. | Always a separate physical tenant |
| Cell | A bounded deployment and failure domain containing runtime capacity and scoped services. | An authorization boundary by itself |
| Task journal | Authoritative durable execution state, ownership generations, transitions, and result references. | Sampled telemetry |
| Effect ledger | Durable invocation claims, target receipts, and reconciliation facts used to prevent unsafe duplicate effects. | A trace store |
| Execution claim | A durable conditional ownership record that must be acquired before dispatching an effect. | An approval or a quota reservation |
| Fencing generation | A monotonically advancing ownership value that an enforcing boundary uses to reject stale owners. | A lease timeout alone |
| Uncertain outcome | A state in which retained evidence cannot establish whether an effect occurred or remains in progress. | A conclusive failure |
| Quiescent checkpoint | A checkpoint at a supported boundary with no unresolved effect that could be repeated by restoration. | Merely an idle process |
| Outbox | Durable pending publication work retained atomically with its originating state transition for recoverable delivery. | An in-memory buffer or an exactly-once delivery guarantee |
| Release authorization | A scoped decision that a reviewed artifact is eligible for deployment. | Activation of a particular installation |
| Activation | Enabling execution or traffic in an installation after its required controls and dependencies are verified. | Artifact publication |
| Review preview | A non-executing display of an already captured proposed action and its risk evidence. | A live sandbox test or provider call |
| Checkpoint | Sensitive serialized runtime state that is restorable only under its identity, version, and quiescence contract. | Evidence an effect did not happen |
| Working context | The bounded, temporary information presented to an agent for a turn or step. | Long-term memory |
| Memory | Retained interaction-derived information with explicit scope, provenance, and permitted uses. | Curated knowledge |
| Knowledge | Governed source-derived information published for retrieval with provenance and access controls. | Automatically trusted instructions |
| Audit evidence | Protected records of decisions, authority, configuration changes, and effects. | Debug logs or hidden reasoning |
| Accounting ledger | Authoritative reservations, usage, settlement, and unresolved charges. | Estimated dashboard cost |
| Error budget | An agreed allowance of SLO misses within a defined measurement window used to guide reliability and change decisions. | A spending allowance or permission to violate safety constraints |
| Circuit breaker | A dependency-protection mechanism that temporarily suppresses calls after defined failures and controls checks for recovery. | Authorization, deduplication, or proof that an earlier operation did not happen |
| Backpressure | A mechanism that regulates incoming work to respect downstream processing capacity. | Silent loss of accepted responsibility |
| Unit cost | Cost within a declared boundary divided by an explicit work count for the same population and period. | Necessarily the marginal price or cost per successful business outcome |
| Resource intensity | Resource consumption per defined functional unit within a stated system boundary. | Measured carbon emissions or total environmental impact |

## Relationships and ambiguity rules

- A **tenant** can own many **workspaces**. Each **run** has exactly one owning
  tenant and workspace; cross-boundary access requires explicit delegation.
- A **task** can have multiple **runs**, each with many **steps** and
  **invocations**. Retrying delivery preserves the original invocation identity.
- An **outbox** relay can publish the same event more than once. Consumer
  idempotence or deduplication must preserve one logical disposition; publication
  alone does not establish exactly-once processing or a remote business effect.
- A new run or attempt cannot bypass an unresolved **effect** from prior work.
  New effect identity is permitted only for a separately authorized operation;
  it must not disguise an unsafe retry.
- A **session** can span tasks; session continuity does not imply permission to
  reuse another task's authority or memory.
- An **agent instance** can make a delegation; the receiving agent authenticates
  independently and receives a reduced, explicit authority scope.
- "Trust" can mean cryptographic identity assurance or evaluated behavioral
  reliability. A behavioral score never replaces authentication or permission.
- "Replay" can mean inspection of recorded history or re-execution. This
  reference permits inspection freely under read authorization; re-execution
  requires a separate effect-safety decision.
- "Gateway" describes a responsibility. Logical gateways may share a deployment
  while retaining target-specific authorization, credentials, and receipts.
- "Resource" can mean a registered capability or its allocation; name the exact
  target and version when authorization or deletion depends on the distinction.
- SLI means a measured service indicator; SLO means its target over a defined
  window. RPO is tolerated recovery-point data loss; RTO is time to restore the
  specified service. None of these alone proves safe recovery of unknown effects.
