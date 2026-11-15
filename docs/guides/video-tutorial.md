---
title: Build, govern and verify an assistant
description: Watch a captioned Studio tutorial covering Agents, Tools, Knowledge, Memory, Workflows and trust evidence.
---

# Build, govern and verify an assistant

Follow a fictional Cedar Labs support assistant from its first definition to
connected capabilities, permission boundaries and inspectable execution evidence.

**Duration:** 23 minutes 50 seconds. **Recorded:** November 14, 2026.
The recording uses development builds and the `kognition.dev/v1alpha1` API.
Screens and supported behavior may change; follow current written guides for
installation, authentication and operational contracts.

[Watch in the documentation website](https://laughing-adventure-mknz6g7.pages.github.io/guides/video-tutorial/).
The [versioned media release](https://github.com/microsoft/kognition/releases/tag/tutorial-2026-11-14)
also provides the video and English caption downloads for repository readers.

## What you will learn

- Define an Agent, its instructions and its Model authorization.
- Connect a calculator Tool and distinguish grants from action approvals.
- Ground answers in a KnowledgeBase and retain preferences through a MemoryClaim.
- Connect steps and input templates in a visual Workflow.
- Understand TrustProfile scoring, promotion, demotion and review requirements.
- Find resource status and boundary decisions in Studio and Trust Focus.

## Before following along

- Complete [Kubernetes setup](../getting-started/kubernetes.md) or
  [AKS setup](../getting-started/aks.md), then [First agent](../getting-started/first-agent.md).
- Configure a supported model provider, credentials and any required storage or
  Tool services. Installation alone does not configure these dependencies.
- Use an enrolled workload namespace. Creating a namespace does not add it to
  an installation's managed scope.
- Review [Studio](../studio.md), [the resource model](../concepts/resource-model.md)
  and the [sample matrix](../sample-matrix.md) for current behavior and limitations.
- Set explicit budgets before model calls or workflows that can use paid providers.

English narration captions are already visible in the video. An optional
selectable English text track and the transcript below provide additional ways
to follow the tutorial. Search this page or the documentation search for a topic.
The extended configuration views are intentional reading pauses.

## Recording scope

The tutorial combines newly authored AKS definitions with retained execution and
trust examples. It distinguishes configuration, permissions and actual outcomes.
The trust experiment uses a separate disposable Agent. Scripted approval is not
independent human review; the recording does not claim an approved Kubernetes
autonomy review or production readiness.

Use the [capability guides and samples](README.md) for maintained written
walkthroughs. The video is a dated example, not an instruction to replay
side-effecting or paid actions against an existing installation.

## Transcript

<!-- tutorial-transcript:start -->

### 00:00 Welcome and build the assistant

**00:00** Welcome to this Kognition tutorial. By the end, you'll know how to define an Agent, connect a Tool, ground answers in product knowledge, retain customer preferences, and build a Workflow. You'll also learn how permissions and TrustProfiles govern behavior, and where to inspect their evidence. We'll follow a fictional Cedar Labs support assistant from configuration to recorded execution. We use a dedicated, already-managed AKS namespace for configuration.

**00:30** Studio's Create picker groups the resources we'll build. Completed execution examples retain their original tutorial namespace, outputs and evidence. A Prompt stores reusable instructions. We create full-lesson-instructions and define how the assistant should respond. Its version identifies the saved content. This runtime uses that content as inline Agent instructions. A Model describes the inference provider and invocation configuration. Here we select tutorial-support-model.

**01:20** An Agent will reference this Model, while a separate ModelGrant authorizes permission to invoke it. We create Agent full-lesson-support in the Builder. We choose its Model, set inline instructions, and bound each completion and timeout. The replica count stays at zero while we author this new definition. Deploy saves the configuration. The recorded execution examples later show the assistant running with its required permissions. Labels and annotations are optional key and value pairs.

**02:35** Labels help group and select resources; annotations hold descriptive metadata. This label groups our tutorial resources, and this annotation explains their purpose. The same controls apply to Agents, Tools, MemoryClaims and grants. ModelGrant full-lesson-model authorizes full-lesson-support to invoke tutorial-support-model. A reference identifies the Model; this grant supplies permission to use it. Keeping these responsibilities separate makes access explicit and reviewable.

**03:24** We ask full-lesson-support what Cedar Labs should collect for incident INC-1042. The assistant answers using its configured instructions. Its completed response and token usage appear in Preview.

### 03:44 Connect and authorize a calculator

**03:44** A Tool connects an Agent to a callable capability. We create full-lesson-calculator in the Builder and select the existing HTTP ToolClass, story-http. We set the arithmetic endpoint and its required expression argument, then review the YAML and deploy. We test full-lesson-calculator independently before giving it to the Agent. Its input is seventeen times twenty-three. Invoke returns three hundred ninety-one, showing the Tool's request and result without relying on a model to choose the calculation.

**04:34** AgentRole full-lesson-capabilities lists allowed capability verbs. AgentRoleBinding full-lesson-capabilities assigns that role to full-lesson-support. ToolGrant full-lesson-calculator-access permits the named calculator. We attach the Tool to the Agent and instruct it to use the calculator for arithmetic. AutonomyClass supervised requires approval for Tool use, Memory writes and external communication. Grants authorize resource access; RuntimeApproval authorizes one bound action.

**05:14** Workflow approval gates control a step transition. These are separate decisions. Higher autonomy does not create missing grants. A RuntimeApproval binds one decision to a particular action, execution identity, target and arguments. A separate scripted approver submits the authenticated decision. The assistant retrieves it for that same action and obtains three hundred ninety-one. This is scripted oversight, not independent human review.

### 05:46 Ground answers in product knowledge

**05:46** A KnowledgeBase holds a corpus for retrieval. We create full-lesson-product-docs with the inline class, story-inline. In the Builder, we add an Orbital Mark 7 passage describing Brazil South availability. The YAML identifies the class and source content. A KnowledgeBaseClaim connects an Agent to a KnowledgeBase. We create full-lesson-docs-reader for the product corpus and set the retrieval limit. We attach this claim to full-lesson-support before asking a product question.

**06:55** The assistant searches its attached product documentation. The retrieved passage says Brazil South is not bookable yet. We inspect the source chunk and the answer to see how the KnowledgeBase supplies evidence.

### 07:15 Retain customer preferences

**07:15** A MemoryClaim requests persistent storage. We create full-lesson-preferences using the default MemoryClass, with episodic storage and read-write access. The controller binds it to a MemoryVolume. This storage is separate from chat history. MemoryGrant full-lesson-preference-access authorizes the assistant to read and write the preference claim. We attach full-lesson-preferences to the Agent and define when it should store and recall customer information.

**08:15** Cedar Labs requests updates every thirty minutes. The assistant stores that preference under a unique tag. We clear the conversation, then ask for the same preference. Memory recall returns the stored note in the new session.

### 08:47 Build and run a connected Workflow

**08:47** AgentCommunicationPolicy full-lesson-step-handoff permits the handoff between this assistant's Workflow steps. It names full-lesson-support as both source and destination, without granting access to other Agents. The calculator action retains its separate runtime approval. A Workflow connects steps and passes data between them. We add impact and summary, assign the assistant to both, and enter their input templates. The summary references the impact output.

**09:33** Next we connect the ports, review the complete YAML, and deploy the definition. A WorkflowRun records a separate execution. A WorkflowRun executes a saved Workflow. We submit the incident-summary request. The impact step returns three hundred ninety-one, and the summary step uses that output. Studio shows the completed steps and final result.

### 10:20 Understand TrustProfiles and trace their evidence

**10:20** A TrustProfile controls how an Agent's autonomy can change as evidence accumulates. It connects observed behavior to scoring, promotion, demotion and review requirements. This complements permission grants: a higher score still operates within the granted capabilities. We'll inspect a separate, recorded trust experiment so the support assistant's own autonomy stays unchanged. The recorded experiment uses disposable Agent full-trust-actor. Its ModelGrant authorizes the Model, and its AgentRole and binding provide Tool verbs.

**10:56** ToolGrant full-trust-calculator restricts the allowed expression to seventeen times twenty-three. This separate identity keeps the experiment distinct from the support assistant. TrustProfile full-trust-profile binds this exact Agent incarnation. Its initial score of ten is configuration. During the recorded experiment, audit evaluation ran once a minute, with at least one action and a one-minute stabilization period. Automatic promotion was capped at assisted.

**11:49** Scoring is now paused while we inspect the retained configuration and history. To find the evidence yourself, expand Trust Profile in Studio's Govern section and select full-trust-profile. Open Observed status and scroll to score history. Each entry records its time, score, class and reason. Match these records to the Agent incarnation and the event being investigated. Here is the recorded assisted-class entry and its score. Further down, the history records the lower score after the constrained Tool request was denied.

**12:41** These timestamped records are the source for the following summaries. We are inspecting retained evidence, rather than editing scores or decisions. The successful Model response provides real audit evidence. The score history records about seventy-six points. After stabilization, the controller promotes the profile from supervised to assisted. The result remains bounded by the maximum class and by the Agent's existing permissions. Future promotion now requires review.

**13:16** The Agent requests seventeen times twenty-four, which its ToolGrant forbids. The gateway records a denial without reaching the calculator. This critical policy violation resets the class to supervised, even though the score, now about fifty-nine, remains above the assisted threshold. A new successful Model response raises the observed score. AutonomyReview records the proposed promotion for full-trust-actor, while its class remains supervised. The generated review name and profile reference identify the request.

**13:53** This Kubernetes build has no authenticated review-decision endpoint, so no approved review is claimed. Without a reviewer decision, the five-minute deadline expires. The controller records Denied, with system slash TTL as the decision maker, and keeps the Agent supervised. The score history and incarnation binding explain the result. Trust scoring does not override grants or exact-action approval.

### 14:22 Inspect boundary decisions in Trust Focus

**14:22** Trust Focus inspects the actual evidence for disposable Agent full-trust-actor. We select its verified identity and recorded time window. These filters keep this experiment separate from the support assistant's history. The protection-plane counters and outcome summary describe the current results. We compare decisions that reached a target with decisions stopped before execution. An empty Communication counter means no matching evidence is present; it does not prove a communication request succeeded.

**14:55** This calculator session contains the real argument-constraint denial. The grant check is denied and the target was not reached. An earned autonomy class does not override a ToolGrant's argument constraints. The Model and Tool boundaries record different decisions. The Model completed, while the disallowed Tool request was blocked. This disposable experiment did not exercise Memory or Communication; their empty counters remain explicit. A bounded query can return a truncated result set.

**15:30** The warning means the displayed events are incomplete. We restore the wider result limit before interpreting the counters. A session filter with no matching events shows an empty view. Missing run correlation, run-proof reports or observability links remain unavailable. The dashboard does not substitute unrelated historical evidence.

### 15:53 Inspect authorization and approval outcomes

**15:53** A ToolGrant defines which Tool an Agent may use. These recorded runs show both sides of that boundary: the missing-grant run is denied, while the authorized run produces one publication. A grant can constrain arguments and expire. The changed-content request is denied, and the expired grant also prevents execution. Both recorded receiver checks show zero publications. Workflow approval gates pause a protected transition. A rejection stops publication.

**16:29** A separate gate requires two votes; one vote is insufficient, and the gate expires. The terminal status and timeout event explain why publication did not proceed.

### 16:40 Inspect evaluation outcomes

**16:41** An EvaluationDataset stores test cases and expected results. An Evaluation selects a target, dataset, graders and thresholds. These two fictional cases check a documented region and an unsupported region. Required thresholds must reference metrics that the graders emit. The incorrect-name run fails even though its answers are correct. With the score names corrected and numeric bounds unchanged, the automatic evaluation passes. An EvaluationRun records one evaluation execution.

**18:30** This manual run has its own identity and per-case results, separate from the automatic generation run. Evaluation detects a bad candidate without changing the expected data. The known-region answer fails while the unknown-region answer passes. Repairing the target produces a passing run against the same checks. A required metric cannot silently disappear. This run completes its answers but fails quality thresholds because a required metric was not emitted.

**19:09** Execution completion and evaluation success are different results. The judge's actual output determines what can be concluded. Here the malformed-output request returns valid JSON. The one-token experiment fails at runtime before grading. These results do not establish malformed-JSON grader rejection.

### 19:39 Inspect Solution revisions

**19:40** A Solution describes a goal and its execution target. This Solution selects an existing Agent. Its compiled AgentJob records the owned one-shot work; the generated multi-agent planner is a separate path. Solution promotion gates require evaluation of the candidate revision. Candidate two fails, leaving active revision one unchanged. Repaired candidate three passes its own revision-bound run and becomes active. The Solution reports active revision three.

**20:45** Its gate binds the Solution revision, while the target Agent's configuration is observed separately. Revision history and evaluation provenance explain the promotion.

### 20:57 Inspect operational resources in Studio

**20:57** Studio exposes workload attribution through AgentIdentity. Select the resource from the explorer and inspect its identity and observed status. The namespace scopes these resources. A recorded identity identifier alone is insufficient to establish verified SPIRE enrollment; check the reported verification state. CognitiveQuota defines the namespace's resource and token budgets. In Studio, review its specification and then its observed usage. The limits bound what can be admitted, while the counters report accounted usage.

**21:33** These figures are platform accounting, rather than a provider invoice. The Agent's runtime settings record its runtime class, completion limit, timeout and replica count. This new authoring Agent remains paused. For an actual invocation, inspect its AgentInstance and observed outcome separately. A saved definition describes intended behavior; the execution resource records what happened. Memory storage can also be inspected entirely in Studio. Open the MemoryClaim and check its bound volume name.

**22:08** Select that MemoryVolume to inspect the binding, then open the referenced MemoryClass. Its backend and service configuration explain where storage is provided. This connects the claim, backing volume and Memory Service without running implementation commands. A KnowledgeBase's configuration identifies its sources and storage class. Its observed status describes the retained indexing state. Inspect both in Studio when investigating retrieval. A configured source and a completed indexing result are different pieces of evidence.

**23:00** The Model resource identifies the provider, endpoint and invocation settings. Studio displays credential references by name. Review the intended Model together with its authorization grant and the actual invocation result. Configuration inspection alone does not establish that a new provider call succeeded. You have seen how an Agent, Tool, KnowledgeBase, MemoryClaim and Workflow fit together. Grants make access explicit, and TrustProfiles connect observed behavior to bounded autonomy changes.

**23:33** Use Studio for configuration and resource status, and Trust Focus for boundary decisions. Keep definitions, permissions and execution evidence connected as you build your own applications. Thank you for watching this Kognition tutorial.

<!-- tutorial-transcript:end -->
