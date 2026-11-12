// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const path = require("node:path");
const { RANCHER_TARGET } = require("./live-authoring");
const {
  RUNTIME, PRODUCTION, RANCHER_RUNTIME, RANCHER_PRODUCTION, parseOptions, variants, labels, verifyPrincipal, verifyCreatedRun,
  verifyResult, requestAllowed, verifyMedia, narrationFor, runWorkflowVariants,
} = require("./live-workflow-variants");

const runId = "full-wv-unit-a1";
const cases = variants(runId);
const args = ["--runtime", RUNTIME, "--output", path.join(PRODUCTION, "workflow-variants-a999999"),
  "--run-id", runId, "--authorize-live-writes"];

function created(variant) {
  return { apiVersion: "kognition.dev/v1alpha1", kind: "WorkflowRun",
    metadata: { name: `${variant.workflow.metadata.name}-studio-123456`, namespace: "tutorial-support", uid: "fresh-run" },
    spec: { workflowRef: { name: variant.workflow.metadata.name }, input: variant.input, parameters: variant.parameters } };
}

function completed(variant) {
  const run = created(variant);
  run.status = { phase: ["dataflow", "cycle"].includes(variant.id) ? "Ready" : "Failed",
    startTime: "2026-11-03T20:00:00Z", completionTime: "2026-11-03T20:00:10Z",
    snapshotSpec: structuredClone(variant.workflow.spec), stepStatuses: [], completedSteps: [] };
  const output = (name, expression, result) => ({ name, phase: "Ready", attempts: 1,
    output: JSON.stringify({ expression, result }) });
  if (variant.id === "dataflow") run.status.stepStatuses = [
    output("seed", "17 * 23", 391), output("left", "17 + 23", 40),
    output("right", "17 - 23", -6), output("joined", "40 + (-6)", 34),
    { name: "skip-edge", phase: "Skipped" }, { name: "skip-step", phase: "Skipped" },
  ];
  if (variant.id === "cycle") {
    run.status.stepStatuses = [output("loop", "17 * 23", 391), output("confirm", "391 + 1", 392)];
    run.status.completedSteps = ["loop@1", "loop@2", "confirm@1", "confirm@2"];
  }
  if (variant.id === "retry-deadline") {
    run.status.stepStatuses = [{ name: "expired", phase: "Failed", attempts: 2 }];
    run.status.conditions = [{ reason: "StepFailed", status: "False", message: 'step "expired" failed' }];
  }
  if (variant.id === "cycle-budget") run.status.conditions = [
    { reason: "BudgetExceeded", status: "False", message: "iteration 1 exceeded maxTokensPerCycle budget" },
  ];
  return run;
}

function guardState() {
  return { origin: "http://127.0.0.1:12345", catalog: [
    { plural: "workflows", namespaced: true, treatment: "author" },
    { plural: "workflowruns", namespaced: true, treatment: "observe" },
    { plural: "toolclasses", namespaced: false, treatment: "author" },
  ], workflowNames: cases.map(item => item.workflow.metadata.name), runNames: [created(cases[0]).metadata.name],
  dismissals: 0, expected: cases[0], started: new Set() };
}

test("execution refuses nonboolean consent before writing evidence or reaching a service", async () => {
  for (const consent of [undefined, false, "true", 1]) {
    await assert.rejects(runWorkflowVariants({ runtime: RUNTIME, output: path.join(PRODUCTION, "workflow-variants-a999999"),
      runId, authorizedLiveWrites: consent }), /authorize-live-writes/);
  }
});

test("strict CLI options bind the exact frozen runtime, fresh take directory and owned naming", () => {
  assert.equal(parseOptions(args).authorizedLiveWrites, true);
  for (const bad of [
    [...args, "--run-id", runId], [...args, "--unknown"], [...args, "positional"],
    args.map(value => value === RUNTIME ? path.dirname(RUNTIME) : value),
    args.map(value => value === path.join(PRODUCTION, "workflow-variants-a999999") ? path.join(PRODUCTION, "other") : value),
    args.map(value => value === runId ? "unowned" : value),
  ]) assert.throws(() => parseOptions(bad));
});

test("Rancher workflow variants retain exact runtime and production paths with separate recording labels", () => {
  const rancherArgs = ["--runtime", RANCHER_RUNTIME, "--output", path.join(RANCHER_PRODUCTION, "workflow-variants-a999999"),
    "--run-id", runId, "--target", RANCHER_TARGET, "--authorize-live-writes"];
  assert.equal(parseOptions(rancherArgs).target, RANCHER_TARGET);
  assert.throws(() => parseOptions(rancherArgs.map(value => value === RANCHER_RUNTIME ? RUNTIME : value)));
  assert.throws(() => parseOptions([...args, "--target", RANCHER_TARGET]));
  const rancherCases = variants(runId, RANCHER_TARGET);
  assert.equal(rancherCases.length, cases.length);
  for (const [index, variant] of rancherCases.entries()) {
    assert.deepEqual(variant.workflow.spec, cases[index].workflow.spec);
    assert.deepEqual(variant.workflow.metadata.labels, labels(runId, RANCHER_TARGET));
    assert.equal(variant.workflow.metadata.labels["kognition.dev/recording-run"], "full-20261105");
  }
});

test("Rancher Tool-only execution rejects historical or replaced authorization identities", () => {
  const agent = { kind: "Agent", metadata: { name: "rehearsal-story-support", namespace: "tutorial-support",
    uid: "be5f84a0-8356-4fc5-86c9-bce9c71d351e" }, status: { phase: "Ready" } };
  const grant = { kind: "ToolGrant", metadata: { name: "rehearsal-story-story-calculator",
    namespace: "tutorial-support", uid: "e52e469a-9c34-490f-a627-1743549b0ced" },
  spec: { agentRef: { name: "rehearsal-story-support" }, toolRef: { name: "story-calculator" },
    verbs: ["invoke", "discover", "inspect"], rateLimits: { callsPerMinute: 20 } } };
  verifyPrincipal(agent, grant, RANCHER_TARGET);
  assert.throws(() => verifyPrincipal(agent, grant));
  assert.throws(() => verifyPrincipal({ ...agent, metadata: { ...agent.metadata, uid: "replacement" } }, grant, RANCHER_TARGET));
  assert.throws(() => verifyPrincipal(agent, { ...grant, metadata: { ...grant.metadata, uid: "replacement" } }, RANCHER_TARGET));
  assert.throws(() => verifyPrincipal(agent, { ...grant, spec: { ...grant.spec, toolRef: { name: "*" } } }, RANCHER_TARGET));
});

test("all fixtures are owned, labeled Tool-only graphs using only the existing calculator grant", () => {
  assert.equal(cases.length, 4);
  for (const variant of cases) {
    assert.ok(variant.workflow.metadata.name.startsWith("full-wv-"));
    assert.deepEqual(variant.workflow.metadata.labels, labels(runId));
    assert.equal(variant.workflow.metadata.namespace, "tutorial-support");
    assert.equal(variant.workflow.spec.maxConcurrentRuns, 1);
    for (const step of variant.workflow.spec.steps) {
      assert.equal(step.type, "tool");
      assert.deepEqual(step.toolRef, { name: "story-calculator" });
      assert.deepEqual(step.agentRef, { name: "rehearsal-story-support" });
      assert.ok(step.timeout);
      assert.match(step.input, /^\{"expression":/);
      assert.equal(step.memoryClaim, undefined);
    }
    for (const edge of variant.workflow.spec.edges || []) {
      assert.notEqual(edge.from, edge.to, "installed admission rejects self-loop edges");
    }
    for (const forbidden of ["approvalGates", "memoryClaim", "emergent", "historyLimit"]) {
      assert.equal(variant.workflow.spec[forbidden], undefined);
    }
  }
  assert.equal(cases[1].workflow.spec.budgets.maxIterations, 2);
  assert.equal(cases[2].workflow.spec.steps[0].retry.maxAttempts, 2);
  assert.equal(cases[2].workflow.spec.steps[0].timeout, "1ns");
});

test("prior JSON output comparisons use Go string quoting outside the JSON value, without escaped raw literals", () => {
  const left = cases[0].workflow.spec.steps.find(step => step.name === "left").input;
  assert.ok(left.startsWith('{"expression":{{ if eq .Steps.seed.Output "{\\"expression\\"'));
  assert.ok(left.includes('{{ printf "%q" .Params.left }}'));
  assert.equal(left.includes("`"), false);
  const joined = cases[0].workflow.spec.steps.find(step => step.name === "joined").input;
  assert.ok(joined.includes('.Steps.left.Output "{\\"expression\\"'));
  assert.ok(joined.includes('.Steps.right.Output "{\\"expression\\"'));
});

test("existing authorization cannot silently adopt a replacement or a wildcard grant", () => {
  const agent = { kind: "Agent", metadata: { name: "rehearsal-story-support", namespace: "tutorial-support",
    uid: "0e08e151-ca83-4f51-80f5-b76488a35c52" }, status: { phase: "Ready" } };
  const grant = { kind: "ToolGrant", metadata: { name: "rehearsal-story-story-calculator",
    namespace: "tutorial-support", uid: "fd9de9ec-38b2-47e2-8c42-c9d31bb97110" },
  spec: { agentRef: { name: agent.metadata.name }, toolRef: { name: "story-calculator" },
    verbs: ["invoke", "discover", "inspect"], rateLimits: { callsPerMinute: 20 } } };
  verifyPrincipal(agent, grant);
  assert.throws(() => verifyPrincipal({ ...agent, metadata: { ...agent.metadata, uid: "replacement" } }, grant));
  assert.throws(() => verifyPrincipal(agent, { ...grant, spec: { ...grant.spec, toolRef: { name: "*" } } }));
});

test("Studio request allowlist permits only the active exact run body once and the isolated dismissal", () => {
  const state = guardState();
  const request = { method: "POST",
    url: `${state.origin}/api/invoke/workflow/${cases[0].workflow.metadata.name}?namespace=tutorial-support`,
    body: { input: cases[0].input, parameters: cases[0].parameters } };
  assert.equal(requestAllowed(request, state), true);
  for (const changed of [
    { url: request.url.replace("127.0.0.1", "localhost") },
    { url: request.url + "&namespace=tutorial-support" },
    { url: request.url.replace("tutorial-support", "default") },
    { body: { ...request.body, agentRef: { name: "another" } } },
    { method: "PUT" }, { url: `${state.origin}/api/invoke/tool/story-calculator?namespace=tutorial-support` },
    { url: `${state.origin}/api/resources/agents?namespace=tutorial-support` },
    { url: `${state.origin}/api/invoke/workflow/${cases[1].workflow.metadata.name}?namespace=tutorial-support` },
  ]) assert.equal(requestAllowed({ ...request, ...changed }, state), false);
  state.started.add(cases[0].workflow.metadata.name);
  assert.equal(requestAllowed(request, state), false);
  const dismissal = { method: "POST", url: `${state.origin}/api/studio/tutorial-dismiss`, body: {} };
  assert.equal(requestAllowed(dismissal, state), true);
  state.dismissals++;
  assert.equal(requestAllowed(dismissal, state), false);
});

test("sidebar reads remain catalog-scoped and individual reads require exact owned objects", () => {
  const state = guardState();
  const get = suffix => requestAllowed({ method: "GET", url: state.origin + suffix }, state);
  assert.equal(get("/api/resources/workflows?namespace=tutorial-support"), true);
  assert.equal(get("/api/resources/toolclasses?namespace="), true);
  assert.equal(get(`/api/resources/workflows/${cases[0].workflow.metadata.name}?namespace=tutorial-support`), true);
  assert.equal(get(`/api/resources/workflowruns/${created(cases[0]).metadata.name}?namespace=tutorial-support`), true);
  for (const suffix of ["/api/resources/secrets?namespace=tutorial-support",
    "/api/resources/workflows?namespace=", "/api/resources/toolclasses?namespace=tutorial-support",
    "/api/resources/workflows/unowned?namespace=tutorial-support",
    "/api/resources/workflows?namespace=tutorial-support&watch=true", "/api/namespaces?namespace=default"]) {
    assert.equal(get(suffix), false);
  }
});

test("the first server-generated poll is bounded to one started workflow and later correlated", () => {
  const state = guardState();
  const url = `${state.origin}/api/resources/workflowruns/${cases[0].workflow.metadata.name}-studio-888?namespace=tutorial-support`;
  assert.equal(requestAllowed({ method: "GET", url }, state), false);
  state.started.add(cases[0].workflow.metadata.name);
  assert.equal(requestAllowed({ method: "GET", url }, state), true);
  state.pendingRunName = `${cases[0].workflow.metadata.name}-studio-777`;
  assert.equal(requestAllowed({ method: "GET", url }, state), false);
  assert.equal(requestAllowed({ method: "GET",
    url: url.replace(cases[0].workflow.metadata.name, cases[1].workflow.metadata.name) }, state), false);
});

test("created-run correlation rejects unowned IDs, changed parameters and wrong namespaces", () => {
  for (const variant of cases) verifyCreatedRun(created(variant), variant);
  for (const mutate of [
    run => { run.metadata.name = "unowned-studio-1"; },
    run => { run.metadata.namespace = "default"; },
    run => { run.metadata.uid = ""; },
    run => { run.spec.parameters = { take: "another" }; },
    run => { run.spec.input = "different"; },
  ]) {
    const run = structuredClone(created(cases[0])); mutate(run);
    assert.throws(() => verifyCreatedRun(run, cases[0]));
  }
});

test("verified observations distinguish output dataflow, skipped branches, cycle bounds, retries and budgets", () => {
  for (const variant of cases) {
    verifyResult(completed(variant), variant);
    assert.ok(narrationFor(variant, completed(variant)).includes(completed(variant).metadata.name));
  }
  const wrongJoin = completed(cases[0]); wrongJoin.status.stepStatuses[3].output = '{"result":0}';
  assert.throws(() => verifyResult(wrongJoin, cases[0]));
  const dispatchedSkip = completed(cases[0]); dispatchedSkip.status.stepStatuses[4].attempts = 1;
  assert.throws(() => verifyResult(dispatchedSkip, cases[0]));
  const extraCycle = completed(cases[1]); extraCycle.status.completedSteps.push("loop@3");
  assert.throws(() => verifyResult(extraCycle, cases[1]));
  const oneAttempt = completed(cases[2]); oneAttempt.status.stepStatuses[0].attempts = 1;
  assert.throws(() => verifyResult(oneAttempt, cases[2]));
  const appErrorAsSuccess = completed(cases[2]); appErrorAsSuccess.status.phase = "Ready";
  assert.throws(() => verifyResult(appErrorAsSuccess, cases[2]));
  const unbounded = completed(cases[3]); unbounded.status.snapshotSpec.budgets.maxTokensPerCycle = 0;
  assert.throws(() => verifyResult(unbounded, cases[3]));
});

test("the frozen reconciler may persist either template parameters or the exact run-merged parameters, never unrelated drift", () => {
  const initialSnapshot = completed(cases[0]);
  verifyResult(initialSnapshot, cases[0]);
  const mergedSnapshot = completed(cases[0]);
  mergedSnapshot.status.snapshotSpec.parameters = { ...cases[0].parameters };
  verifyResult(mergedSnapshot, cases[0]);
  mergedSnapshot.status.snapshotSpec.parameters.take = "unrelated";
  assert.throws(() => verifyResult(mergedSnapshot, cases[0]));
});

test("media acceptance requires exact full-HD 25fps source and coverage of the completed timeline", () => {
  const metadata = { streams: [{ codec_type: "video", codec_name: "vp8", width: 1920, height: 1080,
    avg_frame_rate: "25/1" }], format: { duration: "120" } };
  verifyMedia(metadata, 119);
  for (const mutate of [
    value => { value.streams[0].width = 1280; },
    value => { value.streams[0].avg_frame_rate = "30/1"; },
    value => { value.streams.push({ codec_type: "audio" }); },
    value => { value.format.duration = "30"; },
  ]) {
    const value = structuredClone(metadata); mutate(value);
    assert.throws(() => verifyMedia(value, 119));
  }
});
