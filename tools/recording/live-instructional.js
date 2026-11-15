// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const path = require("node:path");
const fs = require("node:fs");
const crypto = require("node:crypto");
const { expect } = require("@playwright/test");
const { RANCHER_TARGET, BASELINE_V3_TARGET, recordingTarget } = require("./live-authoring");
const { verifyKubernetesRuntime } = require("./live-pilot");
const { holdReadableConfiguration } = require("./reading");
const { runProduction, parseContinuationOptions, readContinuation, PLURALS, verifyIntent,
  verifyStartedWorkflow, WORKFLOW_UI_V1 } = require("./live-production-session");

const POLICY = Object.freeze({ batch: "builder-first-instructional", uiRevision: "create-picker-v1",
  naturalTyping: true, manifestPreviewSeconds: 4,
  kinds: ["Prompt", "Agent", "ModelGrant", "Tool", "ToolGrant", "AgentRole", "AgentRoleBinding",
    "KnowledgeBase", "KnowledgeBaseClaim", "MemoryClaim", "MemoryGrant", "AgentCommunicationPolicy", "Workflow"] });
const PURPOSES = Object.freeze({
  Prompt: "stores a reusable response instruction",
  Agent: "defines the support assistant's model, instructions and capabilities",
  ModelGrant: "authorizes the assistant to use a specific Model",
  Tool: "connects the assistant to an arithmetic service",
  ToolGrant: "authorizes the assistant to invoke that Tool",
  KnowledgeBase: "holds the Orbital product documentation",
  KnowledgeBaseClaim: "attaches the product corpus to the assistant",
  MemoryClaim: "requests persistent storage for customer preferences",
  MemoryGrant: "authorizes the assistant to read and write that storage",
  AgentCommunicationPolicy: "permits this assistant's result to pass between the two Workflow steps",
  Workflow: "connects work steps and passes each result to the next step",
});
const CORPUS = "# Orbital Mark 7\nBrazil South is not bookable yet. It is targeted for the Mark 7.1 dot-release in November 2026.";
const BASE = "You help fictional Cedar Labs with Orbital Mark 7. Reply only to the current request. Keep answers under 90 words.";
const TOOL_INSTRUCTION = BASE + " Use full_lesson_calculator for arithmetic and report its actual result.";
const KB_INSTRUCTION = TOOL_INSTRUCTION + " Call knowledge_search before product answers and quote the retrieved evidence. Do not invent availability.";
const MEMORY_INSTRUCTION = KB_INSTRUCTION + " Use memory_store only when asked to remember, storing the exact note and tag. Use memory_recall with the supplied tag for customer preferences. Do not use Memory for conversation history.";
const APPROVAL_POLL = " If a tool returns runtime_approval_state pending, repeat that same tool with exactly the same arguments to retrieve the decision, up to three total calls. Do not claim execution until a completed result is returned.";
const NARRATION = Object.freeze({
  A01: "An Agent defines a model, instructions and capabilities. We will build support-assistant for Cedar Labs, then add calculation, product knowledge and customer preferences one capability at a time.",
  A02: "A Prompt stores reusable instructions. We name this Prompt support-instructions and define the assistant's response style. The saved version identifies the instruction content. This runtime uses the same text as inline Agent instructions.",
  A03: "The Model resource tutorial-support-model connects to the configured inference provider. ModelGrant support-model authorizes support-assistant to use that Model. A model reference and permission to invoke it are separate resources.",
  A04: "We create Agent support-assistant in the Builder, select its Model and set the instructions, timeout and completion limit. YAML shows the resulting resource definition before deployment.",
  A05: "The deployed support-assistant can answer a basic support question. Studio shows the completed response and reported usage. We will now give it a Tool for calculations.",
  T01: "A Tool connects an Agent to a callable capability. We create impact-calculator in the Builder and select the existing HTTP ToolClass. Its endpoint serves arithmetic, and its schema requires an expression.",
  T02: "Before attaching impact-calculator to the Agent, we test it directly. Seventeen times twenty-three returns three hundred ninety-one. The schema, request and result show the Tool's contract independently of the model.",
  T03: "ToolGrant calculator-access permits support-assistant to invoke impact-calculator. We attach the Tool to the Agent and update its instructions to use it for arithmetic.",
  T04: "The assistant now calls impact-calculator to compute incident impact. The Tool details show the actual expression and result, and the assistant returns three hundred ninety-one.",
  K01: "A KnowledgeBase holds a corpus for retrieval. We create orbital-product-docs in the Builder using the inline KnowledgeBaseClass, and add the product passage about Brazil South availability.",
  K02: "KnowledgeBaseClaim product-docs-reader attaches orbital-product-docs to support-assistant with read-only retrieval. We add that claim to the Agent before asking it a product question.",
  K03: "The assistant searches orbital-product-docs. The retrieved passage says Brazil South is not bookable yet. The answer cites that evidence instead of guessing product availability.",
  M01: "A MemoryClaim requests persistent storage. We create customer-preferences with the default MemoryClass. Its bound MemoryVolume provides the backing storage; it is separate from the current chat history.",
  M02: "MemoryGrant preference-access authorizes support-assistant to read and write customer-preferences. We attach the claim and define when the Agent may store or recall a preference.",
  M03: "Cedar Labs requests incident updates every thirty minutes. The assistant stores that note with a unique tag. After clearing the conversation, it recalls the same tagged preference from persistent Memory.",
  W00: "AgentCommunicationPolicy full-lesson-step-handoff authorizes the handoff between this assistant's Workflow steps. It allows full-lesson-support as both source and destination, without granting access to other Agents. The calculator still requires its own runtime approval.",
  W01: "A Workflow connects steps and passes data between them. We create incident-summary in the visual Builder, add an impact step and a summary step, and connect their ports. Both steps use the capabilities we have already introduced.",
  W02: "The Workflow YAML records the step targets, inputs and connection. The summary input references the impact step's output. A WorkflowRun is a distinct execution of this saved definition.",
  W03: "We run incident-summary with the fictional incident input. The impact step calls the calculator, and the summary step uses its result. Studio displays each completed step and the final summary.",
});

function verifyLessonQuota(quota, targetName, continuation = false) {
  const maximum = recordingTarget(targetName).tutorialTokenBudget || 100000;
  assert.equal(quota.spec.tokenBudget.maxTotalTokens, maximum);
  assert.equal(quota.status?.phase, "Ready", "Quota must have reconciled successfully");
  // The API omits empty usage maps and the unused token-usage pointer.
  const agents = quota.status.used?.agents ?? 0;
  const tokens = quota.status.tokenUsage === undefined ? 0 : quota.status.tokenUsage?.totalTokens;
  assert(Number.isSafeInteger(agents) && agents >= 0 && Number.isSafeInteger(tokens) && tokens >= 0,
    "Quota usage counters must be nonnegative integers");
  assert((continuation || agents < 20) && tokens < maximum * 0.9,
    "The lesson needs bounded Agent capacity and token headroom");
  return maximum;
}

function fixtures(runId) {
  if (!/^(rancher|isolated)-lesson-[a-z0-9-]+$/.test(runId) || runId.length > 30) throw new Error("Use a fresh bounded rancher-lesson-* or isolated-lesson-* ID");
  const resource = (kind, suffix, spec) => ({ apiVersion: "kognition.dev/v1alpha1", kind,
    metadata: { name: `full-lesson-${suffix}`, namespace: "tutorial-support",
      labels: { "kognition.dev/recording-run": runId } }, ...(spec ? { spec } : {}) });
  const prompt = resource("Prompt", "instructions", { content: BASE });
  const agent = resource("Agent", "support", { modelRef: { name: "tutorial-support-model" },
    instructions: { inline: BASE }, runtime: { class: "default", timeout: "120s", maxTokens: 384 }, replicas: 1 });
  const tool = resource("Tool", "calculator", { className: "story-http", protocol: "OpenAPI",
    endpoint: "http://story-integrations.tutorial-support.svc.cluster.local:8080/calculator",
    description: "Calculate a pure arithmetic expression.", metadata: { allowPrivateNetwork: "true" },
    inputSchema: { type: "object", properties: { expression: { type: "string" } }, required: ["expression"] } });
  const modelGrant = resource("ModelGrant", "model", { agentRef: { name: agent.metadata.name }, modelRef: { name: "tutorial-support-model" } });
  const toolGrant = resource("ToolGrant", "calculator-access", { agentRef: { name: agent.metadata.name },
    toolRef: { name: tool.metadata.name }, verbs: ["invoke", "discover", "inspect"] });
  const role = resource("AgentRole", "capabilities");
  role.rules = [{ resources: ["Tool"], verbs: ["get", "invoke", "discover", "inspect"] },
    { resources: ["Memory"], verbs: ["get", "update"] }];
  const binding = resource("AgentRoleBinding", "capabilities", { roleRef: { name: role.metadata.name },
    subjects: [{ kind: "Agent", name: agent.metadata.name }] });
  const kb = resource("KnowledgeBase", "product-docs", { knowledgeBaseClassName: "story-inline", sources: [{ type: "inline", data: CORPUS }] });
  const kbClaim = resource("KnowledgeBaseClaim", "docs-reader", {
    knowledgeBaseName: kb.metadata.name, accessMode: "ReadOnly", retrieval: { topK: 3 } });
  const memory = resource("MemoryClaim", "preferences", { memoryClassName: "default", type: "episodic", capacity: "16Mi", accessModes: ["ReadWrite"] });
  const memoryGrant = resource("MemoryGrant", "preference-access", { agentRef: { name: agent.metadata.name },
    memoryClaimRef: { name: memory.metadata.name }, accessMode: "ReadWrite" });
  const communicationPolicy = resource("AgentCommunicationPolicy", "step-handoff", { defaultDeny: true,
    allowFrom: [{ name: agent.metadata.name }], allowTo: [{ name: agent.metadata.name }] });
  const workflow = resource("Workflow", "incident", {
    budgets: {}, emergent: {},
    steps: [
      { name: "impact", type: "agent", agentRef: { name: agent.metadata.name },
        input: "Use full_lesson_calculator to compute 17 * 23. Return the result.", timeout: "120s" },
      { name: "summary", type: "agent", agentRef: { name: agent.metadata.name },
        input: "Summarize fictional Cedar Labs incident INC-1042 using only this impact result: {{ .Steps.impact.Output }}. Do not call tools.", timeout: "120s" },
    ], edges: [{ from: "impact", to: "summary" }] });
  return { prompt, agent, tool, modelGrant, toolGrant, role, binding, kb, kbClaim, memory, memoryGrant,
    communicationPolicy, workflow };
}

async function expandBuilderObjects(page) {
  for (let opened = 0; opened < 100; opened++) {
    const closed = page.locator("#builder-host details.schema-object:not([open])");
    if (!await closed.count()) return;
    await closed.first().locator("summary").first().click();
  }
  throw new Error("Builder object expansion exceeded its bounded field count");
}

function builderSection(page, title) {
  const escaped = title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return page.locator("#builder-host details.form-section").filter({
    has: page.locator("summary").filter({ hasText: new RegExp(`^${escaped}(?:advanced)?$`, "i") }),
  }).first();
}

function builderTypingOptions(value) {
  // Allow native Builder updates between keystrokes without changing typing speed.
  return { delay: 35, timeout: Math.max(30000, String(value).length * 100 + 10000) };
}

function runtimeApprovalFailure(actionError, decisionError) {
  if (!actionError) return decisionError;
  if (!decisionError) return actionError;
  return new AggregateError([actionError, decisionError],
    `${actionError.message}; runtime approval decision failed: ${decisionError.message}`);
}

function workflowDraftNeedsReconstruction(completed, liveWorkflow) {
  if (completed.has("W02")) return false;
  assert.equal(liveWorkflow, null, "An unrecorded Workflow save must be settled before continuation");
  return completed.has("W01");
}

function verifyFailedWorkflowAttempts(runs, workflow, input) {
  assert(runs.length <= 1, "Repeated Workflow failures require separate review");
  for (const run of runs) {
    verifyStartedWorkflow(run, workflow, input, {}, workflow.metadata.namespace);
    assert.equal(run.status?.phase, "Failed", "Never replay an active or successful Workflow");
    assert(run.status.completionTime && run.status.conditions?.some(c => c.reason === "CommunicationDenied"),
      "Only the settled policy-denied handoff may be retried");
    assert.equal(run.status.stepStatuses?.length, 1);
    assert.equal(run.status.stepStatuses[0].name, "impact");
    assert.equal(run.status.stepStatuses[0].phase, "Ready");
    assert.equal(run.status.stepStatuses[0].attempts, 1);
  }
}

async function runInstructional(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target ? ["--target", input.target] : []),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites ? ["--authorize-live-writes"] : [])]);
  if (![RANCHER_TARGET, BASELINE_V3_TARGET].includes(options.target) ||
      path.dirname(options.output).toLowerCase() !== options.runtime.toLowerCase() ||
      !path.basename(options.output).startsWith("instructional-")) throw new Error("Use a fresh instructional-* capture on an exact reviewed target");
  const desired = fixtures(options.runId);
  const previous = readContinuation(options, POLICY.batch, "instructional-");
  const chain = [];
  let source = previous;
  while (source) {
    chain.unshift(source);
    const parent = source.report.continuation;
    source = parent ? readContinuation({ ...options, continuationPath: parent.reportPath }, POLICY.batch, "instructional-") : undefined;
    if (source) assert.equal(source.reportSHA256, parent.reportSHA256);
    assert(chain.length <= 6, "Continuation chain exceeds its bounded recovery scope");
  }
  const completed = new Set(chain.flatMap(entry => entry.report.scenes.filter(s => s.passed).map(s => s.id)));
  for (const id of ["A05", "T02", "T04", "K03", "M03"]) {
    if (!completed.has(id) && chain.some(entry => entry.report.scenes.some(s => s.id === id))) {
      throw new Error(`Cannot resume unfinished invocation scene ${id}; independently settle its completed or uncertain action without replay`);
    }
  }
  const policy = { ...POLICY,
    uiRevision: options.target === BASELINE_V3_TARGET ? WORKFLOW_UI_V1.revision : POLICY.uiRevision };
  return runProduction(options, policy, async session => {
    const page = session.page;
    session.report.resourceIntroductions = [];
    session.report.builderActions = [];
    session.report.narrativeOrder = ["Agent", "Prompt", "Model", "ModelGrant", "Tool", "ToolGrant",
      "KnowledgeBase", "KnowledgeBaseClaim", "MemoryClaim", "MemoryGrant", "AgentCommunicationPolicy",
      "Workflow", "WorkflowRun"];
    const section = title => builderSection(page, title);
    const field = label => page.locator("#builder-host").getByLabel(new RegExp(`^${label}\\s*\\*?$`));
    const type = async (control, value) => {
      await control.scrollIntoViewIfNeeded();
      const startSeconds = session.seconds();
      await control.fill(""); await control.pressSequentially(String(value), builderTypingOptions(value));
      session.report.typingIntervals ||= [];
      session.report.typingIntervals.push({ kind: "builder", startSeconds, endSeconds: session.seconds(),
        characters: String(value).length, playbackSpeed: 1 });
    };
    const open = async title => { const box = section(title); if (await box.getAttribute("open") === null) await box.locator("summary").click(); };
    const begin = async (object, generic = true) => {
      await session.click(page.locator("#btn-create")); await session.click(page.locator(`#btn-new-${object.kind.toLowerCase()}`));
      await session.click(page.locator('[data-view="builder"]'));
      if (generic) { const all = page.getByRole("button", { name: "All API fields", exact: true }); if (await all.isVisible()) await all.click(); }
      session.report.resourceIntroductions.push({ kind: object.kind, name: object.metadata.name,
        purpose: PURPOSES[object.kind] || "defines permission rules for the assistant", seconds: session.seconds() });
    };
    const expand = () => expandBuilderObjects(page);
    const reviewConfiguration = async (control, target) => {
      const startSeconds = session.seconds();
      const reading = await holdReadableConfiguration(page, control);
      session.report.readingHolds.push({ target: `configuration-${target}`, kind: "configuration",
        startSeconds, endSeconds: session.seconds(), ...reading });
      session.persist();
    };
    const set = async (key, value) => {
      await expand();
      const control = page.locator(`[data-path="${key}"]`);
      await expect(control).toHaveCount(1);
      if (await control.evaluate(el => el.tagName) === "SELECT") await control.selectOption(String(value));
      else await type(control, value);
      session.report.builderActions.push({ path: key, seconds: session.seconds() });
      if (key === "spec.content" || key === "spec.instructions.inline" || /^spec\.sources\.\d+\.data$/.test(key)) {
        await reviewConfiguration(control, key);
      }
    };
    const addArray = async (label, object) => {
      await expand();
      await page.getByRole("button", { name: `Add ${label} item`, exact: true }).click();
      for (const [key, value] of Object.entries(object)) await set(key, value);
    };
    const identity = async object => {
      await set("metadata.name", object.metadata.name);
      await set("metadata.namespace", "tutorial-support");
      const labels = page.locator("#builder-host details.schema-object").filter({
        has: page.locator("summary").filter({ hasText: /^Labels$/ }) }).first();
      await type(labels.getByRole("textbox", { name: "Labels entry key", exact: true }), "kognition.dev/recording-run");
      await labels.getByRole("button", { name: "Add entry", exact: true }).click();
      await set("metadata.labels.kognition.dev/recording-run", options.runId);
    };
    const guidedIdentity = async object => {
      await type(field("Name"), object.metadata.name);
      const labels = section("Identity").locator(".field").filter({
        has: page.locator("label").filter({ hasText: /^Labels$/ }) });
      await labels.getByRole("button", { name: "+ add entry", exact: true }).click();
      await type(labels.getByPlaceholder("key", { exact: true }), "kognition.dev/recording-run");
      await type(labels.getByPlaceholder("value", { exact: true }), options.runId);
    };
    const reviewDeploy = async (object, create = true) => {
      await session.click(page.locator('[data-view="yaml"]'));
      const text = await page.locator("#yaml-area").inputValue();
      assert(/^apiVersion:/m.test(text) && !/^\s*\{/.test(text));
      await page.waitForTimeout(4000);
      return session.deployBuiltResource(object, create);
    };
    const generic = async (object, fields) => {
      await begin(object); await identity(object);
      for (const [key, value] of Object.entries(fields)) await set(key, value);
      return reviewDeploy(object);
    };
    const ready = async saved => session.wait("lesson Agent physical rollout", async () => {
      const agent = await session.get("agents", saved.metadata.name);
      const d = await session.get("deployments", `agent-${saved.metadata.name}`, session.namespace, true);
      if (!d || agent.status?.phase !== "Ready" || d.status.observedGeneration !== d.metadata.generation ||
          !["readyReplicas", "availableReplicas", "updatedReplicas"].every(k => d.status[k] === 1)) return false;
      if (d.spec.template.spec.containers.find(c => c.name === "agent").env.find(e => e.name === "KOGNITION_SYSTEM_PROMPT")?.value !== saved.spec.instructions.inline) return false;
      verifyKubernetesRuntime(agent, { name: agent.metadata.name, namespace: session.namespace, uid: saved.metadata.uid,
        generation: saved.metadata.generation, deploymentUID: d.metadata.uid, replicas: 1, maxTokens: 384 }, d);
      session.report.evidence.push({ type: "runtime-verified", agent, deployment: d });
      return agent;
    });
    let agent, tool, kb, kbClaim, memory, workflow;
    const withApprovals = async action => {
      if (input.authorizedRuntimeApprovals !== true) {
        throw new Error("Default runtime approval requires separate explicit authorization for the scripted approver");
      }
      const file = path.join(options.runtime, "lesson-runtime-approver.cjs");
      const digest = crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
      session.report.runtimeApprovalHelper = { path: file, sha256: digest, userApprovedScriptedApprover: true };
      const { createApprover } = require(file);
      const approver = await createApprover();
      const existing = new Set((await session.list("runtimeapprovals")).map(r => r.metadata.uid));
      let finished = false;
      let decisionFailure;
      const decisions = (async () => {
        while (!finished) {
          const records = await session.list("runtimeapprovals");
          for (const record of records) {
            if (existing.has(record.metadata.uid) || record.spec.agentRef.name !== agent.metadata.name) continue;
            (session.report.runtimeApprovalCandidates ||= []).push({
              name: record.metadata.name, uid: record.metadata.uid, resourceVersion: record.metadata.resourceVersion,
              plane: record.spec.plane, agentRef: record.spec.agentRef, agentInstanceRef: record.spec.agentInstanceRef,
              targetRef: record.spec.targetRef, decisionState: record.status?.decisionState,
              executionState: record.status?.executionState, seconds: session.seconds(),
            });
            session.persist();
            assert.equal(record.spec.agentRef.uid, agent.metadata.uid);
            assert(record.spec.plane === "Tool" ? record.spec.targetRef.name === tool.metadata.name :
              record.spec.plane === "Memory" && !!memory &&
                [memory.metadata.name, memory.status.boundVolumeName].includes(record.spec.targetRef.name),
            "Only the currently demonstrated Tool or Memory operation may receive a decision");
            const proof = await approver.approve(record);
            (session.report.runtimeApprovalDecisions ||= []).push({ ...proof, seconds: session.seconds() });
            existing.add(record.metadata.uid); session.persist();
          }
          await new Promise(resolve => setTimeout(resolve, 150));
        }
      })().catch(error => {
        decisionFailure = error; finished = true;
        session.report.runtimeApprovalFailure = { message: error.message, stack: error.stack, seconds: session.seconds() };
        session.persist();
      });
      let failure;
      try { await action(); }
      catch (error) { failure = error; }
      finally {
        finished = true;
        try { await decisions; }
        catch (error) { failure ||= error; }
        failure = runtimeApprovalFailure(failure, decisionFailure);
        approver.close();
      }
      if (failure) throw failure;
    };
    const changeAgent = async (instruction, extra) => {
      const current = await session.get("agents", agent.metadata.name);
      await session.select("agents", agent.metadata.name); await page.locator('[data-view="builder"]').click();
      const all = page.getByRole("button", { name: "All API fields", exact: true });
      if (await all.isVisible()) await all.click();
      await set("spec.instructions.inline", instruction);
      current.spec.instructions.inline = instruction;
      await extra(current);
      agent = await ready(await reviewDeploy(current, false));
    };
    const showDetails = async () => {
      await page.locator(".chat-message.assistant").last().locator(".tool-call-badge").click();
      const modal = page.locator("#tool-calls-modal"); await expect(modal).toBeVisible();
      await page.waitForTimeout(4000); await modal.locator("button[data-modal-close]").first().click();
    };
    const call = name => body => {
      const matching = (body.toolCallsDetail || []).filter(c => c.name === name || c.name === name.replaceAll("-", "_"));
      assert.equal(matching.length, 1, `Expected exactly one ${name}`);
      return matching[0];
    };
    const quota = await session.get("cognitivequotas", "tutorial-budget");
    const tokenBudget = verifyLessonQuota(quota, options.target, !!previous);
    session.report.tokenBudget = { maximum: tokenBudget, before: quota.status.tokenUsage?.totalTokens ?? 0 };
    if (previous) {
      for (const object of Object.values(desired)) {
        const history = chain.flatMap(entry => entry.report.evidence).filter(e =>
          e.resource?.kind === object.kind && e.resource?.metadata.name === object.metadata.name);
        if (!history.length) continue;
        const latest = history.at(-1).resource;
        const live = await session.get(PLURALS[object.kind], object.metadata.name);
        verifyIntent(live, latest);
        if (object.kind === "Agent") agent = live;
        if (object.kind === "Tool") tool = live;
        if (object.kind === "KnowledgeBase") kb = live;
        if (object.kind === "KnowledgeBaseClaim") kbClaim = live;
        if (object.kind === "MemoryClaim") memory = live;
        if (object.kind === "Workflow") workflow = live;
      }
      session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
        sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false, completedScenes: [...completed] };
      const liveWorkflow = await session.get("workflows", desired.workflow.metadata.name, session.namespace, true);
      if (workflowDraftNeedsReconstruction(completed, liveWorkflow)) {
        completed.delete("W01");
        session.report.continuation.draftReconstructions = [{ scene: "W01",
          resource: desired.workflow.metadata.name, reason: "Unsaved browser draft was not persisted",
          backendActionsReplayed: false }];
      }
    } else {
      for (const object of Object.values(desired)) assert.equal(await session.get(PLURALS[object.kind], object.metadata.name, session.namespace, true), null);
    }
    const sc = (id, action) => completed.has(id) ? Promise.resolve() : session.scene(id, [], id, NARRATION[id], action);
    await sc("A01", async () => { await session.click(page.locator("#btn-create")); await page.waitForTimeout(3000); await page.keyboard.press("Escape"); });
    await sc("A02", async () => { await generic(desired.prompt, { "spec.content": BASE }); });
    await sc("A03", async () => {
      await session.select("models", "tutorial-support-model"); await page.locator('[data-view="yaml"]').click(); await page.waitForTimeout(3000);
      await generic(desired.modelGrant, { "spec.agentRef.name": desired.agent.metadata.name, "spec.modelRef.name": "tutorial-support-model" });
    });
    await sc("A04", async () => {
      agent = await generic(desired.agent, { "spec.modelRef.name": "tutorial-support-model", "spec.instructions.inline": BASE,
        "spec.runtime.class": "default", "spec.runtime.timeout": "120s", "spec.runtime.maxTokens": 384, "spec.replicas": 1 });
      agent = await ready(agent);
    });
    await sc("A05", async () => { await session.chat(agent.metadata.name, "What should Cedar Labs collect when reporting incident INC-1042?", b => assert(b.response && b.usage.completionTokens <= 384)); });
    await sc("T01", async () => {
      await begin(desired.tool, false); await guidedIdentity(desired.tool);
      await field("Protocol").selectOption("OpenAPI"); await field("Tool class").selectOption("story-http");
      await type(field("Endpoint"), desired.tool.spec.endpoint); await type(field("Description"), desired.tool.spec.description);
      await open("Metadata");
      const meta = section("Metadata"); await meta.getByRole("button", { name: "+ add entry", exact: true }).click();
      await type(meta.getByPlaceholder("key", { exact: true }), "allowPrivateNetwork");
      await type(meta.getByPlaceholder("value", { exact: true }), "true");
      await open("Legacy");
      const schema = section("Legacy").locator("textarea");
      await type(schema, JSON.stringify(desired.tool.spec.inputSchema));
      await schema.press("Tab");
      tool = await reviewDeploy(desired.tool);
    });
    await sc("T02", async () => {
      await session.select("tools", tool.metadata.name); await page.locator('[data-view="preview"]').click();
      await page.getByRole("button", { name: "Raw JSON", exact: true }).click();
      const area = page.locator("#preview-host textarea").first(); await type(area, '{"expression":"17 * 23"}');
      const endpoint = `/api/invoke/tool/${tool.metadata.name}`;
      session.arm({ method: "POST", path: endpoint, body: { input: { expression: "17 * 23" } } });
      const [r] = await Promise.all([page.waitForResponse(x => x.request().method() === "POST" && new URL(x.url()).pathname === endpoint),
        session.click(page.getByRole("button", { name: "Invoke", exact: true }))]);
      const body = await r.json(); assert(r.ok());
      session.report.toolTest = body;
      assert(JSON.stringify(body).includes("391"), "Independent calculator must return its actual arithmetic result");
      await page.waitForTimeout(5000);
    });
    await sc("T03", async () => {
      const existingRole = await session.get("agentroles", desired.role.metadata.name, session.namespace, true);
      if (existingRole) {
        verifyIntent(existingRole, desired.role);
        await session.select("agentroles", desired.role.metadata.name);
      } else {
      await begin(desired.role); await identity(desired.role);
      for (const [i, rule] of desired.role.rules.entries()) {
        await page.getByRole("button", { name: "Add Rules item", exact: true }).click(); await expand();
        for (const [j, resource] of rule.resources.entries()) {
          await page.getByRole("button", { name: "Add Resources item", exact: true }).nth(i).click();
          await set(`rules.${i}.resources.${j}`, resource);
        }
        for (const [j, verb] of rule.verbs.entries()) {
          await page.getByRole("button", { name: "Add Verbs item", exact: true }).nth(i).click();
          await set(`rules.${i}.verbs.${j}`, verb);
        }
      }
      await reviewDeploy(desired.role);
      }
      const existingBinding = await session.get("agentrolebindings", desired.binding.metadata.name, session.namespace, true);
      if (existingBinding) {
        verifyIntent(existingBinding, desired.binding);
      } else {
      await begin(desired.binding); await identity(desired.binding); await set("spec.roleRef.name", desired.role.metadata.name);
      await addArray("Subjects", { "spec.subjects.0.kind": "Agent", "spec.subjects.0.name": agent.metadata.name });
      await reviewDeploy(desired.binding);
      }
      await begin(desired.toolGrant); await identity(desired.toolGrant);
      await set("spec.agentRef.name", agent.metadata.name); await set("spec.toolRef.name", tool.metadata.name);
      while (await page.getByRole("button", { name: "Remove Verbs item 1", exact: true }).count()) {
        await page.getByRole("button", { name: "Remove Verbs item 1", exact: true }).click();
      }
      for (const [i, verb] of desired.toolGrant.spec.verbs.entries()) {
        await page.getByRole("button", { name: "Add Verbs item", exact: true }).click(); await set(`spec.verbs.${i}`, verb);
      }
      await reviewDeploy(desired.toolGrant);
      await changeAgent(TOOL_INSTRUCTION, async current => {
        await addArray("Tools", { "spec.tools.0.ref.name": tool.metadata.name, "spec.tools.0.required": true });
        current.spec.tools = [{ ref: { name: tool.metadata.name }, required: true }];
      });
    });
    await sc("T04", async () => {
      if (input.authorizedRuntimeApprovals === true && !agent.spec.instructions.inline.includes(APPROVAL_POLL.trim())) {
        await changeAgent(TOOL_INSTRUCTION + APPROVAL_POLL, async () => {});
      }
      await withApprovals(async () => { await session.chat(agent.metadata.name,
        "Calculate incident impact: use full_lesson_calculator for 17 * 23.", body => {
        const evidence = (body.toolCallsDetail || []).filter(c => c.name === "full_lesson_calculator");
        assert(evidence.length > 0 && evidence.length <= 3);
        const results = evidence.map(c => JSON.parse(c.result));
        assert(results.some(r => r.result === 391), "Approved calculator must actually execute");
        assert(results.filter(r => r.runtime_approval_state).every(r => r.runtime_approval_state === "pending"));
        assert(body.response.includes("391"));
      });
      await showDetails(); });
    });
    await sc("K01", async () => {
      await begin(desired.kb, false); await guidedIdentity(desired.kb);
      await field("Knowledge base class").selectOption("story-inline");
      await page.getByRole("button", { name: "+ Add source", exact: true }).click();
      await type(field("Inline data"), CORPUS); kb = await reviewDeploy(desired.kb);
      kb = await session.wait("inline KnowledgeBase ready", async () => {
        const current = await session.get("knowledgebases", kb.metadata.name);
        return current.status?.phase === "Ready" ? current : false;
      });
    });
    await sc("K02", async () => {
      await begin(desired.kbClaim, false); await guidedIdentity(desired.kbClaim);
      await field("Knowledge base").selectOption(kb.metadata.name);
      await open("Retrieval overrides"); await field("Top K").fill("3");
      kbClaim = await reviewDeploy(desired.kbClaim);
      await session.wait("knowledge claim bound", async () => {
        const c = await session.get("knowledgebaseclaims", kbClaim.metadata.name); return c.status?.phase === "Bound" || c.status?.phase === "Ready" ? c : false;
      });
      await changeAgent(KB_INSTRUCTION + APPROVAL_POLL, async current => {
        await addArray("Knowledge Base Claims", { "spec.knowledgeBaseClaims.0.name": kbClaim.metadata.name,
          "spec.knowledgeBaseClaims.0.required": true });
        current.spec.knowledgeBaseClaims = [{ name: kbClaim.metadata.name, required: true }];
      });
    });
    await sc("K03", async () => {
      await session.chat(agent.metadata.name, "Search the product documentation: is Orbital Mark 7 bookable in Brazil South?", body => {
        const hits = JSON.parse(call("knowledge_search")(body).result);
        assert(hits.some(hit => hit.kb === kb.metadata.name && hit.chunk.includes("not bookable yet")));
        assert(body.response.includes("Brazil South"));
      });
      await showDetails();
    });
    await sc("M01", async () => {
      await begin(desired.memory); await identity(desired.memory);
      for (const [key, value] of Object.entries({ "spec.memoryClassName": "default", "spec.type": "episodic", "spec.capacity": "16Mi" })) await set(key, value);
      await page.getByRole("button", { name: "Add Access Modes item", exact: true }).click();
      await set("spec.accessModes.0", "ReadWrite"); memory = await reviewDeploy(desired.memory);
      memory = await session.wait("MemoryClaim bound", async () => {
        const c = await session.get("memoryclaims", memory.metadata.name); return c.status?.phase === "Bound" && c.status.boundVolumeName ? c : false;
      });
      session.report.memoryVolume = await session.get("memoryvolumes", memory.status.boundVolumeName);
    });
    await sc("M02", async () => {
      const existingGrant = await session.get("memorygrants", desired.memoryGrant.metadata.name, session.namespace, true);
      if (existingGrant) {
        verifyIntent(existingGrant, desired.memoryGrant);
        await session.select("memorygrants", desired.memoryGrant.metadata.name);
      } else {
      await begin(desired.memoryGrant, false); await guidedIdentity(desired.memoryGrant);
      await field("Agent").selectOption(agent.metadata.name); await field("Memory claim").selectOption(memory.metadata.name);
      await field("Access mode").selectOption("ReadWrite"); await reviewDeploy(desired.memoryGrant);
      }
      await changeAgent(MEMORY_INSTRUCTION + APPROVAL_POLL, async current => {
        await addArray("Claims", { "spec.memory.claims.0.name": memory.metadata.name });
        current.spec.memory = { claims: [{ name: memory.metadata.name }] };
      });
    });
    await sc("M03", async () => {
      const tag = options.runId;
      let stored;
      await withApprovals(async () => {
      stored = await session.chat(agent.metadata.name,
        `Use memory_store to remember exactly: Cedar Labs requests incident updates every 30 minutes. Use tag ${tag}.`, body => {
          const evidence = (body.toolCallsDetail || []).filter(c => c.name === "memory_store" && /Stored/i.test(c.result));
          assert.equal(evidence.length, 1);
          const args = JSON.parse(evidence[0].args);
          assert.equal(args.tag, tag); assert(args.content.includes("30 minutes"));
          assert(/Stored/i.test(evidence[0].result));
        });
      await showDetails();
      });
      const recalled = await session.chat(agent.metadata.name, `Use memory_recall with tag ${tag} to retrieve Cedar Labs' update preference.`, body => {
        const evidence = call("memory_recall")(body); assert(evidence.result.includes("30 minutes"));
        assert(body.response.includes("30"));
      }, true);
      assert.notEqual(stored.sessionID, recalled.sessionID);
      session.report.memorySessions = { store: stored.sessionID, recall: recalled.sessionID, tag };
      await showDetails();
    });
    await sc("W00", async () => {
      await begin(desired.communicationPolicy); await identity(desired.communicationPolicy);
      await set("spec.defaultDeny", true);
      await addArray("Allow From", { "spec.allowFrom.0.name": agent.metadata.name });
      await addArray("Allow To", { "spec.allowTo.0.name": agent.metadata.name });
      await reviewDeploy(desired.communicationPolicy);
    });
    await sc("W01", async () => {
      await begin(desired.workflow, false);
      await type(field("Name"), desired.workflow.metadata.name);
      for (const step of desired.workflow.spec.steps) {
        await page.getByRole("button", { name: "+ Agent", exact: true }).first().click();
        const inspector = page.locator("#inspector-body");
        await inspector.getByLabel(/^Name\s*\*?$/).fill(step.name);
        await inspector.getByLabel(/^Agent\s*\*?$/).selectOption(agent.metadata.name);
        await type(inspector.getByLabel("Input (Go template)", { exact: true }), step.input);
        await reviewConfiguration(inspector.getByLabel("Input (Go template)", { exact: true }), `workflow-${step.name}-input`);
        await inspector.getByLabel("Timeout", { exact: true }).fill(step.timeout);
      }
      await page.getByRole("button", { name: "Arrange steps automatically", exact: true }).click();
      const source = page.locator('.node[data-step="impact"] .port.out');
      const destination = page.locator('.node[data-step="summary"] .port.in');
      await source.dragTo(destination);
      await page.waitForTimeout(2500);
      const closeInspector = page.locator("#inspector-close");
      if (await closeInspector.isVisible()) await closeInspector.click();
      await page.getByRole("button", { name: "All API fields", exact: true }).click();
      await identity(desired.workflow);
    });
    await sc("W02", async () => { workflow = await reviewDeploy(desired.workflow); });
    await sc("W03", async () => {
      const input = "Prepare the fictional incident impact summary.";
      const priorRuns = await session.list("workflowruns", `kognition.dev/workflow=${workflow.metadata.name}`);
      verifyFailedWorkflowAttempts(priorRuns, workflow, input);
      session.report.priorWorkflowAttempts = priorRuns;
      session.persist();
      await withApprovals(async () => {
      const run = await session.startWorkflow(workflow, input, {});
      const terminal = await session.wait("workflow completes", async () => {
        const current = await session.get("workflowruns", run.metadata.name);
        return ["Ready", "Failed"].includes(current.status?.phase) ? current : false;
      }, 360000);
      assert.equal(terminal.status.phase, "Ready");
      const steps = terminal.status.stepStatuses;
      assert.equal(steps.find(s => s.name === "impact").phase, "Ready");
      assert(steps.find(s => s.name === "impact").output.includes("391"));
      assert(steps.find(s => s.name === "summary").output.includes("391"));
      session.report.workflowRun = terminal;
      await session.inspectObserved("workflowruns", terminal.metadata.name, ["Ready", "391"], "completed-incident-summary");
      });
    });
    session.report.quotaAfter = await session.get("cognitivequotas", "tutorial-budget");
    assert(session.report.quotaAfter.status.tokenUsage.totalTokens < tokenBudget);
    if (!previous) assert.equal(session.report.calls.length, 5);
    session.report.completeCore = true;
    session.persist();
  });
}
if (require.main === module) runInstructional(parseContinuationOptions(process.argv.slice(2))).catch(e => { console.error(e.message); process.exitCode = 1; });
module.exports = { POLICY, PURPOSES, NARRATION, fixtures, verifyLessonQuota, expandBuilderObjects,
  builderSection, builderTypingOptions, runtimeApprovalFailure, workflowDraftNeedsReconstruction,
  verifyFailedWorkflowAttempts, runInstructional };
