// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const crypto = require("node:crypto");
const { RANCHER_TARGET, RANCHER_APPROVED, BASELINE_V3_TARGET } = require("./live-authoring");
const { assertResourceScope, recordingNamespaces, verifyIntent } = require("./live-production-session");
const { POLICY, PURPOSES, NARRATION, fixtures, verifyLessonQuota, builderTypingOptions,
  runtimeApprovalFailure, workflowDraftNeedsReconstruction, verifyFailedWorkflowAttempts,
  runInstructional } = require("./live-instructional");

test("builder-first fixtures use one bounded Agent with scoped prerequisites and no publication capability", () => {
  const runId = "rancher-lesson-unit";
  const desired = fixtures(runId);
  for (const item of Object.values(desired)) assertResourceScope(item, { ...POLICY, target: RANCHER_TARGET, runId });
  assert.equal(desired.agent.spec.runtime.maxTokens, 384);
  assert.equal(desired.agent.spec.replicas, 1);
  assert.equal(desired.tool.spec.className, "story-http");
  assert.match(desired.tool.spec.endpoint, /:8080\/calculator$/);
  assert.equal(desired.workflow.spec.steps.length, 2);
  assert(desired.workflow.spec.steps.every(step => step.agentRef.name === desired.agent.metadata.name));
  assert.deepEqual(desired.workflow.spec.edges, [{ from: "impact", to: "summary" }]);
  assert(!POLICY.kinds.includes("RuntimeApproval"));
  assert.throws(() => fixtures("../escape"));
});

test("the Workflow handoff policy permits only the demonstrated assistant's own step transition", () => {
  const desired = fixtures("isolated-lesson-unit");
  assert.equal(desired.communicationPolicy.kind, "AgentCommunicationPolicy");
  assert.deepEqual(desired.communicationPolicy.spec, { defaultDeny: true,
    allowFrom: [{ name: desired.agent.metadata.name }], allowTo: [{ name: desired.agent.metadata.name }] });
});

test("a failed handoff can be retried only after the prior exact run is terminal", () => {
  const workflow = fixtures("isolated-lesson-unit").workflow;
  const input = "Prepare the fictional incident impact summary.";
  const failed = { apiVersion: "kognition.dev/v1alpha1", kind: "WorkflowRun",
    metadata: { name: `${workflow.metadata.name}-studio-12345`, namespace: workflow.metadata.namespace, uid: "prior" },
    spec: { workflowRef: { name: workflow.metadata.name }, input },
    status: { phase: "Failed", completionTime: "2026-11-06T21:42:03Z",
      conditions: [{ reason: "CommunicationDenied" }],
      stepStatuses: [{ name: "impact", phase: "Ready", attempts: 1 }] } };
  verifyFailedWorkflowAttempts([failed], workflow, input);
  verifyFailedWorkflowAttempts([], workflow, input);
  for (const change of [
    { status: { ...failed.status, phase: "Running" } },
    { status: { ...failed.status, phase: "Ready" } },
    { status: { ...failed.status, completionTime: undefined } },
    { status: { ...failed.status, conditions: [{ reason: "Unknown" }] } },
    { status: { ...failed.status, stepStatuses: [...failed.status.stepStatuses, { name: "summary" }] } },
    { spec: { ...failed.spec, input: "different" } },
  ]) assert.throws(() => verifyFailedWorkflowAttempts([{ ...failed, ...change }], workflow, input));
  assert.throws(() => verifyFailedWorkflowAttempts([failed, failed], workflow, input));
});

test("a Workflow without shared Memory deploys without an empty required reference", () => {
  const desired = fixtures("isolated-lesson-unit").workflow;
  const native = structuredClone(desired);
  delete native.spec.memoryClaim;
  verifyIntent(native, desired);
  assert.equal(Object.hasOwn(desired.spec, "memoryClaim"), false);
});

test("continuation reconstructs an unsaved Workflow draft without replaying persisted actions", () => {
  assert.equal(workflowDraftNeedsReconstruction(new Set(["M03", "W01"]), null), true);
  assert.equal(workflowDraftNeedsReconstruction(new Set(["M03"]), null), false);
  const saved = { metadata: { uid: "saved-workflow" } };
  assert.equal(workflowDraftNeedsReconstruction(new Set(["W01", "W02"]), saved), false);
  assert.throws(() => workflowDraftNeedsReconstruction(new Set(["W01"]), saved), /unrecorded Workflow/);
});

test("instructional continuation refuses unfinished invocations before opening another capture", async t => {
  const runtime = fs.mkdtempSync(path.join(os.tmpdir(), "kognition-instructional-recovery-"));
  t.after(() => fs.rmSync(runtime, { recursive: true, force: true }));
  for (const [id, calls] of [
    ["M03", [{ sessionID: "stored", instance: { status: { phase: "Ready" } },
      toolCallsDetail: [{ name: "memory_store", result: "Stored" }] }]],
    ["T04", [{ sessionID: "calculated", instance: { status: { phase: "Ready" } },
      toolCallsDetail: [{ name: "full_lesson_calculator", result: '{"result":391}' }] }]],
    ["T02", []],
    ["A05", [{ sessionID: "answered", response: "Completed support answer." }]],
    ["K03", [{ sessionID: "searched", toolCallsDetail: [{ name: "knowledge_search", result: "Retrieved" }] }]],
    ["M03", []],
  ]) {
    await t.test(`${id} with ${calls.length ? "completed evidence" : "uncertain execution"}`, async () => {
      const folder = fs.mkdtempSync(path.join(runtime, "instructional-prior-"));
      const video = path.join(folder, "production.failed.webm");
      fs.writeFileSync(video, "preserved test recording");
      const reportPath = path.join(folder, "report.json");
      const report = { passed: false, failure: "Inspection failed after scene start.", batch: POLICY.batch,
        runId: "rancher-lesson-unit", approvedTarget: RANCHER_APPROVED,
        recording: { path: video, bytes: fs.statSync(video).size },
        scenes: [{ id, passed: false }], calls, evidence: [] };
      const original = JSON.stringify(report);
      fs.writeFileSync(reportPath, original);
      const output = path.join(runtime, `instructional-next-${path.basename(folder)}`);
      await assert.rejects(runInstructional({ runtime, output, runId: report.runId, target: RANCHER_TARGET,
        continuationPath: reportPath, authorizedLiveWrites: true }),
      new RegExp(`Cannot resume unfinished invocation scene ${id}.*without replay`));
      assert.equal(fs.existsSync(output), false, "Recovery must refuse before any new capture or invocation");
      assert.equal(fs.readFileSync(reportPath, "utf8"), original);
      assert.equal(fs.readFileSync(video, "utf8"), "preserved test recording");
    });
  }
});

test("instructional continuation checks ancestor reports for unfinished effects", async t => {
  const runtime = fs.mkdtempSync(path.join(os.tmpdir(), "kognition-instructional-chain-"));
  t.after(() => fs.rmSync(runtime, { recursive: true, force: true }));
  const writeReport = (suffix, scenes, continuation) => {
    const folder = path.join(runtime, `instructional-${suffix}`);
    fs.mkdirSync(folder);
    const video = path.join(folder, "production.failed.webm");
    fs.writeFileSync(video, "preserved test recording");
    const reportPath = path.join(folder, "report.json");
    const content = JSON.stringify({ passed: false, failure: "Presentation failed.", batch: POLICY.batch,
      runId: "rancher-lesson-unit", approvedTarget: RANCHER_APPROVED,
      recording: { path: video, bytes: fs.statSync(video).size }, scenes, calls: [], evidence: [], continuation });
    fs.writeFileSync(reportPath, content);
    return { reportPath, reportSHA256: crypto.createHash("sha256").update(content).digest("hex") };
  };
  const ancestor = writeReport("ancestor", [{ id: "M03" }]);
  const previous = writeReport("previous", [], ancestor);
  const output = path.join(runtime, "instructional-next");
  await assert.rejects(runInstructional({ runtime, output, runId: "rancher-lesson-unit", target: RANCHER_TARGET,
    continuationPath: previous.reportPath, authorizedLiveWrites: true }),
  /Cannot resume unfinished invocation scene M03.*without replay/);
  assert.equal(fs.existsSync(output), false);
});

test("the corpus claim intent matches the native Builder's explicit read-only access", () => {
  const desired = fixtures("isolated-lesson-unit").kbClaim;
  const built = structuredClone(desired);
  built.spec.accessMode = "ReadOnly";
  verifyIntent(built, desired);
  assert.equal(desired.spec.accessMode, "ReadOnly");
  assert.equal(desired.spec.retrieval.topK, 3);
  built.spec.accessMode = "ReadWrite";
  assert.throws(() => verifyIntent(built, desired));
});

test("long Builder instructions retain natural typing speed with sufficient bounded time", () => {
  const short = builderTypingOptions("Name");
  const long = builderTypingOptions("x".repeat(800));
  assert.equal(short.delay, 35);
  assert.equal(long.delay, short.delay);
  assert.equal(short.timeout, 30000);
  assert(long.timeout >= 800 * long.delay + 30000);
  assert(long.timeout <= 120000);
});

test("every long configuration entered in the instructional scenes is held for reading before it is deployed", () => {
  const source = fs.readFileSync(path.join(__dirname, "live-instructional.js"), "utf8");
  const scene = id => {
    const start = source.indexOf(`sc("${id}"`);
    assert(start >= 0, `${id} scene exists`);
    return source.slice(start, source.indexOf("await sc(", start + 1));
  };
  const kb = scene("K01");
  const typed = kb.indexOf('type(field("Inline data"), CORPUS)');
  const held = kb.indexOf('reviewConfiguration(field("Inline data"), "spec.sources.0.data")');
  const deployed = kb.indexOf("reviewDeploy(desired.kb)");
  assert(typed >= 0 && held > typed && deployed > held, "K01 reads the inline corpus before switching to YAML");
  const workflow = source.slice(source.indexOf("Input (Go template)"));
  assert(/reviewConfiguration\(inspector\.getByLabel\("Input \(Go template\)"/.test(workflow));
});

test("a failed action cannot hide the scripted approval watcher's actual failure", () => {
  const action = new Error("stored result absent");
  const decision = new Error("exact decision rejected");
  assert.equal(runtimeApprovalFailure(action), action);
  assert.equal(runtimeApprovalFailure(undefined, decision), decision);
  const combined = runtimeApprovalFailure(action, decision);
  assert(combined instanceof AggregateError);
  assert.deepEqual(combined.errors, [action, decision]);
  assert.match(combined.message, /stored result absent/);
  assert.match(combined.message, /exact decision rejected/);
});

test("instructional narration introduces resource names and purposes before their examples without production commentary", () => {
  for (const kind of ["Prompt", "Agent", "Tool", "KnowledgeBase", "MemoryClaim", "Workflow"]) assert(PURPOSES[kind]);
  assert(Object.keys(NARRATION).indexOf("T01") < Object.keys(NARRATION).indexOf("T04"));
  assert(Object.keys(NARRATION).indexOf("K01") < Object.keys(NARRATION).indexOf("K03"));
  assert(Object.keys(NARRATION).indexOf("M01") < Object.keys(NARRATION).indexOf("M03"));
  assert(Object.keys(NARRATION).indexOf("W01") < Object.keys(NARRATION).indexOf("W03"));
  for (const text of Object.values(NARRATION)) assert(!/paced edition|pasted|instead of json|re-record|changed during production/i.test(text));
  assert.equal(POLICY.naturalTyping, true);
});

test("builder-first capture refuses missing live-write approval before contacting the cluster", async () => {
  await assert.rejects(runInstructional({ runtime: "runtime", output: "unused-instructional",
    runId: "rancher-lesson-unit", target: RANCHER_TARGET }), /authorize-live-writes/);
});

test("the isolated lesson uses fresh namespace identities without adopting historical resources", () => {
  const runId = "isolated-lesson-unit";
  for (const item of Object.values(fixtures(runId))) {
    assertResourceScope(item, { ...POLICY, target: BASELINE_V3_TARGET, runId });
    assert.throws(() => assertResourceScope(item, { ...POLICY, target: BASELINE_V3_TARGET, runId: "historical-run" }));
  }
  const namespaces = recordingNamespaces(BASELINE_V3_TARGET);
  assert.equal(namespaces["tutorial-support"], "16dd8b57-eeb1-4cf9-a1db-d2847691719b");
  assert.equal(namespaces["tutorial-engineering"], "e24dc792-f76f-42cc-926e-40ec680e055d");
  assert.notDeepEqual(namespaces, recordingNamespaces(RANCHER_TARGET));
});

test("the isolated lesson preserves a conservative remainder of the shared tutorial token budget", () => {
  const quota = { spec: { tokenBudget: { maxTotalTokens: 50000 } },
    status: { phase: "Ready", used: { agents: 1 }, tokenUsage: { totalTokens: 44999 } } };
  assert.equal(verifyLessonQuota(quota, BASELINE_V3_TARGET), 50000);
  assert.throws(() => verifyLessonQuota(quota, RANCHER_TARGET));
  assert.throws(() => verifyLessonQuota({ ...quota, spec: { tokenBudget: { maxTotalTokens: 100000 } } },
    BASELINE_V3_TARGET));
  assert.throws(() => verifyLessonQuota({ ...quota, status: { ...quota.status, tokenUsage: { totalTokens: 45000 } } },
    BASELINE_V3_TARGET));
  const full = { ...quota, status: { ...quota.status, used: { agents: 20 } } };
  assert.throws(() => verifyLessonQuota(full, BASELINE_V3_TARGET));
  assert.equal(verifyLessonQuota(full, BASELINE_V3_TARGET, true), 50000);
  assert.equal(verifyLessonQuota({ ...quota, status: { phase: "Ready" } }, BASELINE_V3_TARGET), 50000);
  for (const status of [undefined, { phase: "Pending" }, { phase: "Ready", tokenUsage: {} },
    { phase: "Ready", tokenUsage: null }, { phase: "Ready", tokenUsage: { totalTokens: -1 } }]) {
    assert.throws(() => verifyLessonQuota({ ...quota, status }, BASELINE_V3_TARGET));
  }
});
