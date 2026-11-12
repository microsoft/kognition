// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const path = require("node:path");
const { chromium } = require("@playwright/test");
const { RANCHER_TARGET } = require("./live-authoring");
const {
  extractSandboxSource, fixtureObjects, teamObjects, parseOptions, verifyStoryRuntime,
  verifyMemoryEvidence, verifyGroundedToolEvidence, publicationGateReady, verifyReviewedBulletin, storyApprovalDecision, verifyStoryChatView,
  verifyStoryRun, verifyStoryApproval, showStoryStepOutput, readingWindowSeconds,
} = require("./live-story");

test("story execution requires explicit target, unique run and live-write consent", () => {
  assert.throws(() => parseOptions([]), /authorize-live-writes/);
  const args = ["--runtime", "runtime", "--output", "take", "--run-id", "story-1", "--stage", "rehearse", "--authorize-live-writes"];
  assert.equal(parseOptions(args).runId, "story-1");
  assert.equal(parseOptions(args.map(value => value === "rehearse" ? "record" : value)).stage, "record");
  assert.equal(parseOptions([...args, "--target", RANCHER_TARGET]).target, RANCHER_TARGET);
  assert.throws(() => parseOptions([...args, "--target", "unreviewed"]), /exact reviewed target/);
  assert.throws(() => parseOptions(args.map(value => value === "story-1" ? "../escape" : value)));
});

test("Rancher publication storage uses its existing local-path provisioner without changing historical fixtures", () => {
  const repository = path.resolve(__dirname, "..", "..");
  const original = fixtureObjects(repository);
  const rancher = fixtureObjects(repository, RANCHER_TARGET);
  assert.equal(original.find(object => object.kind === "PersistentVolumeClaim").spec.storageClassName, "standard");
  assert.equal(rancher.find(object => object.kind === "PersistentVolumeClaim").spec.storageClassName, "local-path");
  const normalized = structuredClone(rancher);
  normalized.find(object => object.kind === "PersistentVolumeClaim").spec.storageClassName = "standard";
  assert.deepEqual(normalized, original);
  assert.throws(() => fixtureObjects(repository, "unreviewed"), /exact reviewed target/);
});

test("reading windows start after a completed answer and scale with all its words without a time cap", () => {
  assert.equal(readingWindowSeconds("391"), 5);
  assert.equal(readingWindowSeconds(Array(120).fill("word").join(" ")), 50);
  assert.equal(readingWindowSeconds(Array(300).fill("word").join(" ")), 122);
  assert.equal(readingWindowSeconds("  one\n two   three  "), 5);
  assert.throws(() => readingWindowSeconds("  "), /non-empty/);
  assert.throws(() => readingWindowSeconds(null), /non-empty/);
});

test("reviewed sandbox extraction stops before unrelated YAML resources", () => {
  const input = "data:\n  server.py: |\n    from http.server import HTTPServer\n    path = '/calculator'\n---\nkind: Secret\n";
  const result = extractSandboxSource(input);
  assert.match(result, /HTTPServer/);
  assert.doesNotMatch(result, /Secret|---/);
  assert.throws(() => extractSandboxSource("data: {}"));
});

test("integration fixtures retain restricted security, exact preapproved egress labels and pinned image", () => {
  const objects = fixtureObjects(path.resolve(__dirname, "..", ".."));
  const deployment = objects.find(object => object.kind === "Deployment");
  assert.equal(deployment.metadata.namespace, "tutorial-support");
  assert.equal(deployment.spec.template.metadata.labels["app.kubernetes.io/part-of"], "kognition-tutorial-tools");
  assert.equal(deployment.spec.template.spec.automountServiceAccountToken, false);
  assert.equal(deployment.spec.template.spec.securityContext.runAsNonRoot, true);
  for (const container of deployment.spec.template.spec.containers) {
    assert.match(container.image, /@sha256:[a-f0-9]{64}$/);
    assert.equal(container.securityContext.readOnlyRootFilesystem, true);
    assert.equal(container.securityContext.allowPrivilegeEscalation, false);
    assert.deepEqual(container.securityContext.capabilities.drop, ["ALL"]);
  }
  assert.equal(objects.filter(object => object.kind === "Namespace" || object.kind === "Secret").length, 0);
  for (const tool of objects.filter(object => object.kind === "Tool")) {
    assert.equal(tool.spec.executionPolicy, undefined);
    assert.equal(tool.spec.sandbox.timeout, "20s");
  }
});

test("workflow publication depends on actual reviewed output and an explicit gate", () => {
  const objects = teamObjects("rehearsal-story");
  const workflow = objects.find(object => object.kind === "Workflow");
  const publish = workflow.spec.steps.find(step => step.name === "publish");
  assert.match(publish.input, /Steps.review.Output/);
  assert.match(publish.input, /printf "%q"/);
  assert.deepEqual(workflow.spec.approvalGates, [{
    from: "review", to: "publish", requiredApprovers: 1, timeout: "10m",
  }]);
  assert.equal(workflow.spec.maxParallelSteps, 2);
  assert.equal(workflow.spec.maxConcurrentRuns, 1);
  const agent = objects.find(object => object.kind === "Agent" && object.metadata.name.endsWith("-support"));
  assert.equal(agent.spec.memory.claims[0].name, "story-customer-memory");
  assert.equal(agent.spec.knowledgeBaseClaims[0].required, true);
  assert.ok(objects.some(object => object.kind === "MemoryGrant"));
  assert.ok(objects.some(object => object.kind === "ToolGrant"));
});

test("team communication permits only the two directed Agent handoffs and retains default deny", () => {
  const policies = teamObjects("rehearsal-story").filter(object => object.kind === "AgentCommunicationPolicy");
  assert.deepEqual(policies.map(policy => [policy.spec.allowFrom[0].name, policy.spec.allowTo[0].name]), [
    ["rehearsal-story-support", "rehearsal-story-writer"],
    ["rehearsal-story-writer", "rehearsal-story-reviewer"],
  ]);
  for (const policy of policies) {
    assert.equal(policy.metadata.namespace, "tutorial-support");
    assert.equal(policy.metadata.labels["kognition.dev/story-fixture"], "true");
    assert.equal(policy.spec.defaultDeny, true);
    assert.equal(policy.spec.allowFrom.length, 1);
    assert.equal(policy.spec.allowTo.length, 1);
  }
});

test("story calls require a physically available one-replica runtime, not zero-replica Ready metadata", () => {
  const agent = {
    kind: "Agent", metadata: { name: "story-support", namespace: "tutorial-support", uid: "agent-uid", generation: 2 },
    spec: { replicas: 1, runtime: { maxTokens: 512 } },
    status: { phase: "Ready", replicas: 1, readyReplicas: 1 },
  };
  const deployment = {
    kind: "Deployment", metadata: { name: "agent-story-support", namespace: "tutorial-support", uid: "runtime-uid", generation: 1 },
    spec: { replicas: 1 }, status: { observedGeneration: 1, availableReplicas: 1, readyReplicas: 1, updatedReplicas: 1 },
  };
  verifyStoryRuntime(agent, deployment);
  assert.throws(() => verifyStoryRuntime({ ...agent, spec: { ...agent.spec, replicas: 0 } }, deployment));
  assert.throws(() => verifyStoryRuntime(agent, { ...deployment, status: { ...deployment.status, availableReplicas: 0 } }));
  assert.throws(() => verifyStoryRuntime(agent, undefined));
});

test("Memory storage evidence requires a confirmed write, exact tag and actual tool arguments", () => {
  const args = { content: "Cedar Labs requests incident updates every 30 minutes.", tag: "story-new" };
  const call = { name: "memory_store", args: JSON.stringify(args), result: "Stored." };
  const body = { response: "I remembered the 30-minute preference.", toolCallsDetail: [call] };
  assert.deepEqual(verifyMemoryEvidence(body, "memory_store", "story-new"), { args });
  assert.throws(() => verifyMemoryEvidence({
    ...body, toolCallsDetail: [{ ...call, result: JSON.stringify({ runtime_approval_state: "Unavailable" }) }],
  }, "memory_store", "story-new"), /did not confirm/);
  assert.throws(() => verifyMemoryEvidence(body, "memory_store", "another-tag"), /exact fresh memory tag/);
  assert.throws(() => verifyMemoryEvidence({ ...body, toolCallsDetail: [] }, "memory_store", "story-new"));
  assert.throws(() => verifyMemoryEvidence({ ...body, toolCallsDetail: [call, call] }, "memory_store", "story-new"));
});

test("calculator proof uses the real echoed input and numeric tool result rather than model prose", () => {
  const call = { name: "story_calculator", args: '{"expression":"17 * 23"}',
    result: '{"expression":"17 * 23","result":391}' };
  verifyGroundedToolEvidence({ toolCallsDetail: [call] }, "story_calculator");
  assert.throws(() => verifyGroundedToolEvidence({
    response: "391", toolCallsDetail: [{ ...call, result: '{"error_code":"ToolInvocationUnavailable"}' }],
  }, "story_calculator"), /actual calculator result/);
  assert.throws(() => verifyGroundedToolEvidence({
    toolCallsDetail: [{ ...call, result: '{"expression":"17 * 23","result":390}' }],
  }, "story_calculator"));
});

test("knowledge proof requires actual inline chunks from the intended KnowledgeBase", () => {
  const hit = { kb: "story-orbital-docs", source_type: "inline", chunk: "Brazil South is not bookable yet." };
  const call = { name: "knowledge_search", result: JSON.stringify([hit]) };
  verifyGroundedToolEvidence({ toolCallsDetail: [call] }, "knowledge_search");
  for (const result of ['{"error":"retrieval unavailable"}', "[]", JSON.stringify([{ ...hit, kb: "another" }])]) {
    assert.throws(() => verifyGroundedToolEvidence({
      response: "Brazil South is not bookable yet.", toolCallsDetail: [{ ...call, result }],
    }, "knowledge_search"), /Actual inline Knowledge retrieval/);
  }
});

test("Memory recall evidence requires actual structured records with the requested tag", () => {
  const records = [{ content: "Cedar Labs requests incident updates every 30 minutes.", tag: "story-new", score: 0.9 }];
  const call = { name: "memory_recall", args: JSON.stringify({ query: "Cedar Labs update preference", tag: "story-new" }),
    result: JSON.stringify(records) };
  const body = { toolCallsDetail: [call] };
  assert.deepEqual(verifyMemoryEvidence(body, "memory_recall", "story-new").records, records);
  for (const result of ["[]", '{"runtime_approval_state":"Unavailable"}', '[{"content":"30 minutes","tag":"old"}]']) {
    assert.throws(() => verifyMemoryEvidence({
      toolCallsDetail: [{ ...call, result }],
    }, "memory_recall", "story-new"), /actual records/);
  }
  assert.throws(() => verifyMemoryEvidence({
    toolCallsDetail: [{ ...call, result: "No relevant memories found." }],
  }, "memory_recall", "story-new"));
});

test("publication approval waits for the real WaitingApproval condition, not merely a completed review", () => {
  const state = { status: { phase: "Pending", activeStep: "publish",
    stepStatuses: [{ name: "review", phase: "Ready" }, { name: "publish", phase: "Pending" }],
    conditions: [{ type: "Ready", status: "False", reason: "WaitingApproval" }] } };
  assert.equal(publicationGateReady(state), true);
  assert.equal(publicationGateReady({ status: { ...state.status, phase: "Running", conditions: [] } }), false);
  assert.equal(publicationGateReady({ status: { ...state.status, activeStep: "review" } }), false);
  assert.equal(publicationGateReady({ status: { ...state.status,
    stepStatuses: [{ name: "review", phase: "Ready" }, { name: "publish", phase: "Ready" }] } }), false);
});

test("scripted approval requires the actual reviewer's complete approved bulletin", () => {
  const bulletin = "INC-1042 affects Cedar Labs. Impact: 391. Brazil South is not bookable. Updates every 30 minutes.";
  const state = output => ({ status: { stepStatuses: [{ name: "review", phase: "Ready", output }] } });
  assert.equal(verifyReviewedBulletin(state(bulletin + "\nREVIEW: APPROVED")), bulletin + "\nREVIEW: APPROVED");
  assert.equal(verifyReviewedBulletin(state(bulletin + "\n**REVIEW: APPROVED**")), bulletin + "\n**REVIEW: APPROVED**");
  assert.throws(() => verifyReviewedBulletin(state(bulletin)), /did not approve/);
  assert.throws(() => verifyReviewedBulletin(state(bulletin + "\nREVIEW: REJECTED")), /did not approve/);
  assert.throws(() => verifyReviewedBulletin(state("Missing incident evidence.\nREVIEW: APPROVED")), /complete/);
});

test("scripted approval binds the authenticated requester and discloses automation without impersonation", () => {
  const decision = storyApprovalDecision("story-new", { status: { userInfo: { username: "kubernetes-admin" } } });
  assert.equal(decision.spec.approver, "kubernetes-admin");
  assert.equal(decision.spec.workflowRunRef.name, "story-new");
  assert.equal(decision.metadata.namespace, "tutorial-support");
  assert.equal(decision.metadata.labels["kognition.dev/scripted-approval"], "true");
  assert.match(decision.spec.reason, /Scripted demonstration/);
  assert.throws(() => storyApprovalDecision("story-new", {}), /authenticated Kubernetes requester/);
  assert.throws(() => storyApprovalDecision("story-new", { status: { userInfo: { username: "" } } }));
});

test("Studio-generated run names remain bound to the exact story and fresh publication parameters", () => {
  const run = { kind: "WorkflowRun", metadata: { name: "workflow-studio-unique", namespace: "tutorial-support", uid: "run-uid" },
    spec: { workflowRef: { name: "story-workflow" }, parameters: { memoryTag: "story-new", publicationID: "story-new" } } };
  verifyStoryRun(run, "story-workflow", "story-new");
  assert.throws(() => verifyStoryRun(run, "another-workflow", "story-new"), /exact story/);
  assert.throws(() => verifyStoryRun(run, "story-workflow", "old"), /fresh parameters/);
  assert.throws(() => verifyStoryRun({ ...run, metadata: { ...run.metadata, namespace: "another" } }, "story-workflow", "story-new"));
});

test("Studio approval evidence requires the actual requester, run and scripted reason", () => {
  const decision = storyApprovalDecision("studio-generated-run", { status: { userInfo: { username: "kubernetes-admin" } } });
  decision.metadata.uid = "decision-uid";
  verifyStoryApproval(decision, "studio-generated-run", "kubernetes-admin");
  assert.throws(() => verifyStoryApproval(decision, "studio-generated-run", "fake-reviewer"), /authenticated/);
  assert.throws(() => verifyStoryApproval(decision, "another-run", "kubernetes-admin"));
  assert.throws(() => verifyStoryApproval({ ...decision, spec: { ...decision.spec, reason: "independent human approval" } },
    "studio-generated-run", "kubernetes-admin"));
});

test("chat verification accepts rendered Markdown and requires the new completed assistant bubble", async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent(
      '<div class="chat-message assistant"><div class="chat-bubble">Previous response</div></div>' +
      '<div class="chat-message assistant"><div class="chat-bubble"><p>Remembered: ' +
      'Cedar Labs requests updates every <strong>30 minutes</strong>. Tag: <code>story-new</code></p></div></div>' +
      '<div class="chat-message assistant"><div class="chat-bubble typing">Pending response</div></div>',
    );
    const rendered = await verifyStoryChatView(page, 1, ["30 minutes", "story-new"]);
    assert.match(rendered, /Cedar Labs requests updates every 30 minutes/);
    assert.doesNotMatch(rendered, /<strong>|<code>|`|Pending response/);
  } finally {
    await browser.close();
  }
});

test("step output inspection ignores a hidden duplicate left in the other Studio view", async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    const card = '<div class="run-step-card"><span class="run-step-name">publish</span>' +
      '<details class="run-step-output"><summary>Output</summary><pre class="run-output">' +
      '{"publicationCount":1,"content":"INC-1042: 391"}</pre></details></div>';
    await page.setContent('<div id="preview-host" hidden>' + card + '</div><div id="builder-host">' + card + '</div>');
    await showStoryStepOutput(page, "publish", ["publicationCount", "391"]);
    assert.equal(await page.locator("#builder-host details").getAttribute("open"), "");
    assert.equal(await page.locator("#preview-host details").getAttribute("open"), null);
  } finally {
    await browser.close();
  }
});
