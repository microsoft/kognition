# Official source register

Reviewed on **2026-10-08**. The reference synthesizes primary-source architecture
guidance, protocol specifications, observability conventions, risk frameworks,
and software-supply-chain guidance. Their scopes differ; none is a complete
enterprise multi-agent platform specification.

The diagrams and capability matrix are original architectural synthesis. Source
recommendations do not become universal normative requirements merely by being
cited. Protocol requirements apply only when implementing the specified version
and feature. No compliance, certification, or implementation claim follows from
using these documents.

S01-S24 record the sources inspected during this reference's preparation.
Unversioned documentation links can change after review; protocol versions and
repository snapshots are identified where available. The clarity pass reconciles
the architecture with those recorded claims rather than asserting a fresh
verification of every external page or every source-linked incident.

## Architecture, orchestration, and runtime

| ID | Official source and publisher | Verified contribution | Scope or caveat |
| --- | --- | --- | --- |
| S01 | Microsoft, [AI agent orchestration patterns](https://learn.microsoft.com/en-us/azure/architecture/ai-ml/guide/ai-agent-design-patterns) | Simplicity baseline; sequential, concurrent, group-chat, handoff, adaptive task-ledger patterns; bounded execution, external state, human checkpoints. | Architecture recommendations, not a multi-agent performance guarantee. |
| S02 | Microsoft, [AI workload operations on Azure](https://learn.microsoft.com/en-us/azure/well-architected/ai/operations) | Availability plus output quality/freshness; actionable observability; safe deployments; resource efficiency. | Applies to AI workloads broadly. Product-specific examples are translated into generic responsibilities here. |
| S03 | Microsoft, [Test and evaluate AI workloads on Azure](https://learn.microsoft.com/en-us/azure/well-architected/ai/test) | Representative datasets; full agentic-path validation; concurrent load, dependency failure, endpoint throttling, and human review. | Model evaluation alone does not validate an entire platform. |
| S04 | Anthropic, [Building effective agents](https://www.anthropic.com/engineering/building-effective-agents) | Workflow versus agent distinction; routing, parallelization, supervisor/worker and review/revise patterns; stopping conditions and sandbox testing. | Experience-based guidance; the page notes that its tooling landscape has changed. |
| S05 | OpenAI, [Agent orchestration](https://openai.github.io/openai-agents-python/multi_agent/) | Code-driven versus model-driven coordination; agents-as-tools versus handoff; independent parallel tasks. | SDK examples do not establish distributed durability or authorization. |
| S06 | OpenAI, [Handoffs](https://openai.github.io/openai-agents-python/handoffs/) | Structured handoff metadata, history filters, destination enablement, and argument-dependent authorization boundaries. | Handoff availability is not permission for a particular action; function-tool guardrails do not cover the handoff pipeline. |
| S07 | OpenAI, [Guardrails](https://openai.github.io/openai-agents-python/guardrails/) | Input/output/tool-check scope; blocking versus parallel checks; detection after an effect differs from prevention. | Coverage differs among handoffs, function tools, and hosted/built-in tools. |
| S08 | Google, [Workflows: multi-agent, multi-node applications](https://adk.dev/workflows/) | Graph, programmatic, collaborative, and template composition; sequences, loops, parallel execution. | Routing is described as experimental; capabilities vary by version/language. The older multi-agent documentation redirected here. |
| S09 | Google, [Build collaborative agent teams](https://adk.dev/workflows/collaboration/) | Coordinator and subagent modes; conversation ownership versus subtask execution; isolated session branches and result collection. | Context isolation is not an authorization boundary; mode restrictions are implementation-specific. |
| S10 | LangChain, [Persistence](https://docs.langchain.com/oss/python/langgraph/persistence) | Thread-scoped execution state versus long-term memory; restart durability; retention/pruning. | In-memory checkpointing is not durable. The durable-execution URL redirected to this page during review. |
| S11 | LangChain, [Checkpointers](https://docs.langchain.com/oss/python/langgraph/checkpointers) | Step-boundary snapshots, retained parallel writes, replay, and synchronous/asynchronous/exit durability tradeoffs. | Replay can repeat model/API calls; no universal exactly-once external-effect guarantee. |
| S12 | LangChain, [Interrupts](https://docs.langchain.com/oss/python/langgraph/interrupts) | Durable pause/resume, stable interaction identity, human review, and idempotent pre-interrupt effects. | Resumption can restart a node; approval expiry and downstream authorization remain application responsibilities. |

## Interoperability, observability, and risk

| ID | Official source and publisher | Verified contribution | Version and caveat |
| --- | --- | --- | --- |
| S13 | MCP project, [Authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization) | Resource-owner/client/server roles, resource indicators, audience validation, least-privilege scopes, controlled step-up. | Observed version 2026-07-28. Protocol authorization is optional; the enterprise reference requires authenticated protected access independently. Not a task-delegation credential specification. |
| S14 | MCP project, [Security Best Practices](https://modelcontextprotocol.io/docs/2026-07-28/tutorials/security/security_best_practices) | Confused-deputy prevention, per-client consent, token-passthrough prohibition, discovery SSRF, restricted local execution, principal-bound state handles. | Observed version 2026-07-28 describes no protocol-level sessions; older session guidance must not be mixed silently with this version. Platform sessions remain a separate concept. |
| S15 | MCP project, [Tools](https://modelcontextprotocol.io/specification/2026-07-28/server/tools) | Input validation, access control, rate limiting, output sanitization, untrusted annotations, sensitive-operation confirmation. | Observed version 2026-07-28. Schema validity does not establish semantic safety. |
| S16 | A2A project, [Agent2Agent Protocol Specification](https://a2a-protocol.org/v1.0.0/specification/) | Tasks/messages/artifacts; status retrieval, subscriptions, cancellation, authorization of every operation, notification security, audit and retention. | Observed release 1.0.0; sections 3.1.3 through 3.1.6 and 13. Streaming is capability-dependent; cancellation is an attempt; universal crash durability, missed-event replay, and exactly-once effects are not established. |
| S17 | OpenTelemetry, [Semantic Conventions for GenAI agent and framework spans](https://github.com/open-telemetry/semantic-conventions-genai/blob/06ec68e722c45a7218e23ea1bc1339fe4e21ecae/docs/gen-ai/gen-ai-agent-spans.md) | Agent/workflow/tool spans, local versus remote calls, correlation identifiers. | Development status. The [documentation entry](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/) redirected readers to the maintained repository; this register pins the inspected commit. |
| S18 | OpenTelemetry, [Semantic conventions for generative client AI spans](https://github.com/open-telemetry/semantic-conventions-genai/blob/06ec68e722c45a7218e23ea1bc1339fe4e21ecae/docs/gen-ai/gen-ai-spans.md) | Sensitive content is not captured by default; external protected content storage can be referenced from telemetry. | Development status at the same commit. A configured content-upload hook can run regardless of span sampling; sampling is not a privacy control. |
| S19 | NIST, [AI RMF Core](https://airc.nist.gov/airmf-resources/airmf/5-sec-core/) | Continuous Govern, Map, Measure, Manage functions; evaluation and accountable risk management. | AI RMF 1.0, 2023 excerpt. Voluntary guidance, not an ordered certification checklist. |
| S20 | NIST, [Generative Artificial Intelligence Profile](https://nvlpubs.nist.gov/nistpubs/ai/NIST.AI.600-1.pdf) | Evidence retention, data-retention policy, adversarial testing, third-party incident ownership/rehearsal, after-action review. | NIST AI 600-1, July 2024. Verified actions GV-1.5-002/003, GV-6.2-003, MP-4.1-005, MP-5.1-005 on printed pages 16, 22, 26, 27. No universal retention period prescribed. |
| S21 | OWASP, [LLM06:2025 Excessive Agency](https://genai.owasp.org/llmrisk/llm062025-excessive-agency/) | Minimize functionality, privileges, and autonomy; user-scoped downstream authorization; human approval for high-impact effects; malicious tool/peer risks. | 2025 edition. Logging and rate limiting limit damage but do not replace preventive controls. |
| S22 | SLSA, [Build: Provenance](https://slsa.dev/spec/v1.2/build-provenance) | Artifact subjects, builder identity, build definitions, parameters, and dependencies. | Version 1.2; provenance predicate v1. Software origin/integrity evidence does not establish runtime safety or answer correctness. |
| S23 | SLSA, [Build: Verifying artifacts](https://slsa.dev/spec/v1.2/verifying-artifacts) | Verify signature, subject digest, predicate, trusted builder, and expected source/build parameters. | Version 1.2. Verifier-owned trust roots are required; a declared level is insufficient. |
| S24 | OWASP Gen AI Security Project, Agentic Security Initiative, [OWASP Top 10 for Agentic Applications 2026](../ext_sources/OWASP-Top-10-for-Agentic-Applications-2026-12.6-1.pdf) | Verified ASI01 through ASI10 descriptions and mitigations: goal hijack, tool misuse, identity abuse, dynamic supply chain, unexpected code execution, memory poisoning, peer communication, cascading failures, human trust exploitation, and rogue agents. | Cover identifies Version 2026, December 2025. Full local publication reviewed; risk sections are printed pp. 9-38. Guidance is not a compliance standard or proof that any control eliminates the risk. |

## Supplied OWASP publication verification

The user supplied the PDF linked in S24, resolving the earlier download
limitation. It contains 57 PDF pages; the cover is unnumbered, so **printed page
N is PDF page N + 1**. The [OWASP coverage map](owasp-coverage.md) cites individual
risk sections and connects them to this reference's controls and acceptance cases.

- Publisher attribution: OWASP Gen AI Security Project, Agentic Security
  Initiative, *OWASP Top 10 for Agentic Applications 2026*.
- Official [publication page](https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/).
- Source license: [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/),
  stated on printed p. 1. The supplied PDF is unchanged; the coverage map and
  architectural additions summarize and apply the guidance without reproducing
  its diagrams or incident catalog. No OWASP endorsement is implied.
- Supplied-file SHA-256:
  `A2DB94CD00B08E0B3A5E5B619AFE024BDBCD74503111085705E4F3DD886FCB5C`.
  This identifies the reviewed bytes, not independently verified publisher
  authenticity.
- Review scope: overview, all ten risk and mitigation sections, and appendices
  A through E. Appendix D is a publication-time incident snapshot; its linked
  incident claims were not independently re-investigated.

The publication's "intent capsule" is explicitly an emerging pattern (printed
p. 10). Signed message envelopes, behavioral attestations, and memory trust
signals require a defined threat model and implementation contract; neither a
signature nor an evaluator proves semantic correctness or goal alignment.
Appendices A and C provide cross-mappings, not interchangeable risk identifiers
or a claim that this review assessed every referenced framework.

## Derived decisions versus source guarantees

The following are reference design choices assembled from the sources and
Kognition evidence: protected effect classification and claims, independent
policy administration, the approval/prepare/claim/settle protocol, cell
partitioning, bounded policy-cache freshness, and retention of replay-denial
facts. D7 covers decision preparation; D16 covers dispatch and recovery.
Equivalent mechanisms are acceptable only when they preserve the specified
authority, durability, and failure behavior. No cited protocol automatically
provides those guarantees or exactly-once effects across arbitrary targets.

Revisit versioned protocol profiles and evolving telemetry schemas during each
adapter upgrade. Revisit risk controls after material changes to data, models,
tools, business effects, regulation, or deployment assumptions.
