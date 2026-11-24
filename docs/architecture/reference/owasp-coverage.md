# OWASP agentic risk coverage

The supplied publication supports the reference's separation of planning,
execution, authority, state, and evidence. Its main additions are **behavioral
conformance within nominal permissions**, **human-review integrity**, and
**containment of compromised information and dependencies across agents**.

This is an architectural coverage review, not a vulnerability finding list for
Kognition or a claim that these controls have been implemented or tested.

## Source and citation convention

Source: OWASP Gen AI Security Project, Agentic Security Initiative,
[OWASP Top 10 for Agentic Applications 2026](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/),
Version 2026, December 2025, reviewed 2026-10-08. Attribution, source license,
file hash, and review limits are recorded in [S24](sources.md#supplied-owasp-publication-verification).

Page ranges below use **printed page numbers**. In the reviewed PDF, the PDF
page number is one higher because of the unnumbered cover. Summaries and acceptance
cases are this reference's application of the guidance, not quoted mitigation
lists. Category names follow the publication; related categories may overlap.

## All ten risks mapped to architecture

| Risk and source pages | Architectural consequence | Existing responsibility and refinement | Acceptance case |
| --- | --- | --- | --- |
| **ASI01: Agent Goal Hijack**, pp. 9-11 | An action may be permitted yet unrelated to the approved task. Preserve declared intent and check changes to goals and action sequences. | [Execution](03-execution.md#goal-and-message-integrity), D4/D14; [intent enforcement](04-trust.md#intent-and-tool-composition), D6/D7; C07, C09, C11. | A poisoned document proposes an in-permission change of goal. Pause before the effect, record the divergence, and require the applicable scope-change decision. |
| **ASI02: Tool Misuse and Exploitation**, pp. 12-14 | Legitimate tools can form an unsafe chain without privilege escalation. Check operation semantics, data destination, and ambiguous tool identity. | [Intent and tool composition](04-trust.md#intent-and-tool-composition), D6/D7; C11, C12, C14. | A permitted internal read followed by a permitted external send still violates data-use policy and is blocked; ambiguous tool aliases fail closed. |
| **ASI03: Identity and Privilege Abuse**, pp. 15-17 | Context continuity cannot transfer authority. Bind subject, resource, purpose, task, and duration; reassess on principal or purpose changes. | [Cross-view contracts](02-platform-planes.md#cross-view-contracts), [goal and message integrity](03-execution.md#goal-and-message-integrity), D6/D7; C04, C09, C11, C27. | Switch the human principal or delegated purpose during a session; reject cached authority and require fresh authentication/authorization as applicable. |
| **ASI04: Agentic Supply Chain Vulnerabilities**, pp. 18-20 | Inventory runtime-selected prompts, tools, peers, schemas, and dependencies, then support targeted recall across live deployments. | [Extension rules](02-platform-planes.md#extension-and-compatibility-rules), [delivery](07-operations-and-lifecycle.md#d12-versioned-delivery-and-continuous-evaluation), D3/D12/D13; C03, C24, C26, C28. | Revoke a compromised descriptor or prompt digest. Find affected runs and cells, prevent new use despite cached discovery, and preserve in-flight claims for reconciliation. |
| **ASI05: Unexpected Code Execution (RCE)**, pp. 21-23 | Generated text, serialized state, and package installation can become executable. Separate generation, validation, approval, and sandbox execution. | [Generated-code boundary](04-trust.md#d15-generated-code-validation-and-execution), D15; C14, C16, C24. | An artifact changes after approval, a checkpoint requests unsafe deserialization, or installation invokes an unapproved hook. Reject before execution; sandbox access cannot reach production credentials. |
| **ASI06: Memory & Context Poisoning**, pp. 24-26 | Persistent context can spread or reinforce contamination after the original input disappears. Govern promotion, aging, quarantine, and restoration. | [Memory integrity](05-data.md#memory-integrity-and-poisoning-recovery), D8/D9; C17, C18, C20. | Repeated model output cannot promote itself into trusted memory. Quarantine a source and invalidate dependent summaries/caches; restore without reviving revoked access or duplicate effects. |
| **ASI07: Insecure Inter-Agent Communication**, pp. 27-29 | Transport authentication alone does not protect relayed message context, ordering, task audience, or semantic interpretation. | [Message integrity](03-execution.md#goal-and-message-integrity), D6/D14; C09, C15, C27, C28. | Replay a valid message under another task or negotiate a weaker schema/version; reject it. Preserve issuer/audience/context integrity across a broker or relay. |
| **ASI08: Cascading Failures**, pp. 30-32 | Valid-looking but wrong outputs can spread faster than resource alerts or human review. Limit semantic propagation as well as infrastructure load. | [Cascade controls](06-scale-and-resilience.md#semantic-cascade-containment), D4/D10/D11/D13; C07, C08, C21, C23, C25, C26. | Seed one incorrect planner result or shared-memory item. Measure affected tasks/cells and stop propagation within approved caps even while CPU and latency appear healthy. |
| **ASI09: Human-Agent Trust Exploitation**, pp. 33-35 | An approval can be manipulated through persuasive explanation, false certainty, or an effect-bearing preview. | [Human-review integrity](04-trust.md#human-review-integrity), D1/D7; C02, C10, C29. | A recommendation fabricates urgency and a preview attempts a network call. Show independently derived risk and source status, block preview effects, and allow suspicious-interaction reporting. |
| **ASI10: Rogue Agents**, pp. 36-38 | Detect persistent behavioral divergence, coordinated misleading signals, and unauthorized replication even when individual calls look legitimate. | [Behavioral assurance](07-operations-and-lifecycle.md#behavioral-assurance-and-reintegration), D11/D13; C21, C23, C26, C29. | A worker suppresses alerts or requests unauthorized replicas while meeting its cost objective. Independent monitoring detects the deviation; revoke/quarantine and require fresh evidence before reintegration. |

D7 covers decisions, not target execution. Every effect-bearing path in this
mapping also follows [D16](04-trust.md#d16-effect-claim-dispatch-and-recovery).
The acceptance cases are deliberately constructed tests; passing them does not
prove detection or prevention of every variant of a risk.

## Applying the guidance without overstating guarantees

- **Least agency:** choose the minimum autonomy needed for the task, alongside
  least privilege. This reinforces the [complexity baseline](README.md#how-to-use-this-reference);
  adding agents or supervisory agents is not automatically a security benefit.
- **Independent enforcement:** evaluators and watchdogs can detect anomalies,
  but remain fallible. They cannot grant privileges, rewrite their own policy,
  or be the sole mechanism for stopping effects.
- **Authenticity versus semantics:** signed descriptors, context envelopes,
  artifacts, or behavioral manifests establish a binding to an issuer. They
  cannot prove that a goal is appropriate, a result is true, or an agent is safe.
- **Record enough to investigate:** preserve authority, goal/plan versions,
  message/action lineage and receipts in protected evidence. The publication's
  broad logging recommendations do not override data minimization; raw content
  and hidden model reasoning are not required by this reference.
- **Safe simulation:** replay histories for cascade testing only with synthetic
  or authorized data and disconnected, simulated side effects. Do not interpret
  the source's replay example as permission to repeat production actions.
- **Bounded response:** "instant" revocation or containment is an objective to
  quantify, not a distributed guarantee. Measure propagation delay, cached
  authority, existing streams, and unknown outcomes.

Appendices A and C connect agentic, language-model, non-human identity, and risk
scoring taxonomies. Appendix B motivates a machine-readable component and
relationship inventory. This reference adopts those responsibilities without
requiring a particular inventory format or importing a scoring scheme.
Appendix D's incidents are source-reported examples, not newly verified
vulnerabilities or findings against Kognition.

## Assessment use

Use this map alongside the [capability matrix](capabilities.md). Preserve the
existing C01-C30 identifiers; the cases here deepen their acceptance coverage.
Record workload-specific thresholds, implementation evidence, and residual risks
before claiming mitigation effectiveness. The ten-category map closes the source
access limitation; it does not establish exhaustive threat coverage.
