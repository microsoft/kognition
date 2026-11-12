// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { assertResourceScope } = require("./live-production-session");
const { GOOD, BAD, UNKNOWN, POLICY, fixtures, executionSpec,
  verifyEvaluationOutcome, verifyManualRun, runEvaluation } = require("./live-evaluation");

const runId = "rancher-eval-unit-a1";
const desired = fixtures(runId);

test("evaluation fixtures remain fresh, bounded, separately authorized and free of recurring schedules", () => {
  for (const resource of [desired.dataset, desired.target, desired.judge, ...desired.grants, desired.evaluation("good")]) {
    assertResourceScope(resource, { ...POLICY, runId, target: RANCHER_TARGET });
  }
  assert.equal(desired.dataset.spec.version, "v1");
  assert.deepEqual(desired.dataset.spec.cases.map(item => item.id), ["region", "unknown"]);
  assert.notEqual(desired.target.metadata.name, desired.judge.metadata.name);
  assert.equal(desired.target.spec.runtime.maxTokens, 256);
  assert.equal(desired.judge.spec.runtime.maxTokens, 256);
  const evaluation = desired.evaluation("good");
  assert.equal(evaluation.spec.targetRef.revision, undefined);
  assert.equal(evaluation.spec.schedule, undefined);
  assert(evaluation.spec.metricThresholds.some(item => item.metric === "hallucination_score" && item.min === 1 && item.required));
  assert(evaluation.spec.metricThresholds.some(item => item.metric === "safety_score" && item.min === 1 && item.required));
  assert(!evaluation.spec.metricThresholds.some(item => ["hallucination", "safety"].includes(item.metric)));
  assert(evaluation.spec.graders.some(grader => grader.type === "llm_judge" &&
    grader.parameters.agentRef === desired.judge.metadata.name));
  assert.throws(() => fixtures("../escape"));
  assert.throws(() => fixtures("rancher-eval-" + "x".repeat(40)));
});

function runFixture(variant = "good") {
  const evaluation = desired.evaluation(variant);
  const metrics = () => [
    { metric: "task_success", value: 1, passed: true },
    { metric: "latency_ms", value: 1200, passed: true },
    { metric: "cost_units", value: 213, passed: true },
    { metric: "judge_score", value: 1, passed: true },
  ];
  return {
    apiVersion: "kognition.dev/v1alpha1", kind: "EvaluationRun",
    metadata: { name: `${evaluation.metadata.name}-g1`, namespace: "tutorial-support", uid: "fresh-run" },
    spec: executionSpec(evaluation),
    status: { phase: "Ready", passed: true, caseCount: 2, completedCases: 2,
      datasetDigest: "a".repeat(40), caseResults: [
        { id: "region", passed: true, targetOutput: GOOD, targetJobRef: { name: "target-region" }, metrics: metrics() },
        { id: "unknown", passed: true, targetOutput: UNKNOWN, targetJobRef: { name: "target-unknown" }, metrics: metrics() },
      ] },
  };
}

test("passing evaluation proof requires completed real outputs, stable dataset digest and measured values", () => {
  verifyEvaluationOutcome(runFixture(), "good", desired.dataset.metadata.name);
  for (const mutate of [
    run => { run.status.phase = "Running"; },
    run => { run.status.completedCases = 1; },
    run => { run.status.caseResults[0].targetOutput = BAD; },
    run => { run.status.datasetDigest = ""; },
    run => { run.status.passed = false; },
    run => { run.status.caseResults[0].metrics[2].value = 0; },
  ]) {
    const changed = runFixture();
    mutate(changed);
    assert.throws(() => verifyEvaluationOutcome(changed, "good", desired.dataset.metadata.name));
  }
});

test("a completed but failing candidate is never converted into a passed run", () => {
  const run = runFixture("bad");
  run.status.passed = false;
  run.status.caseResults[0].passed = false;
  run.status.caseResults[0].targetOutput = BAD;
  run.status.caseResults[0].metrics[0] = { metric: "task_success", value: 0, passed: false };
  verifyEvaluationOutcome(run, "bad", desired.dataset.metadata.name);
  assert.throws(() => verifyEvaluationOutcome(run, "good", desired.dataset.metadata.name));
  run.status.caseResults[1].passed = false;
  assert.throws(() => verifyEvaluationOutcome(run, "bad", desired.dataset.metadata.name));
});

test("missing metrics and malformed judge failures require their actual specific failed metric", () => {
  const missing = runFixture("metric");
  missing.status.passed = false;
  for (const result of missing.status.caseResults) {
    result.passed = false;
    result.metrics.push({ metric: "unreported_metric", value: 0, passed: false,
      message: "required metric was not emitted by configured graders" });
  }
  verifyEvaluationOutcome(missing, "metric", desired.dataset.metadata.name);
  missing.status.caseResults[0].metrics.pop();
  assert.throws(() => verifyEvaluationOutcome(missing, "metric", desired.dataset.metadata.name));
  const invalid = runFixture("judge");
  invalid.status.passed = false;
  for (const result of invalid.status.caseResults) {
    result.passed = false;
    result.metrics[3] = { metric: "judge_score", value: 0, passed: false, message: "invalid judge JSON output" };
  }
  verifyEvaluationOutcome(invalid, "judge", desired.dataset.metadata.name);
  invalid.status.caseResults[0].metrics[3].passed = true;
  assert.throws(() => verifyEvaluationOutcome(invalid, "judge", desired.dataset.metadata.name));
});

test("a changed judge instruction records the actual result without requiring a model to produce malformed output", () => {
  const run = runFixture();
  verifyEvaluationOutcome(run, "judge-observed", desired.dataset.metadata.name);
  assert.throws(() => verifyEvaluationOutcome(run, "judge", desired.dataset.metadata.name));
  run.status.passed = false;
  run.status.caseResults[0].passed = false;
  run.status.caseResults[0].metrics[3] = {
    metric: "judge_score", value: 0, passed: false, message: "invalid judge JSON output",
  };
  verifyEvaluationOutcome(run, "judge-observed", desired.dataset.metadata.name);
  for (const mutate of [
    changed => { changed.status.passed = true; },
    changed => { changed.status.caseResults[0].metrics[3].value = NaN; },
    changed => { changed.status.caseResults[0].metrics[3].value = 2; },
    changed => { delete changed.status.caseResults[0].metrics[3].passed; },
    changed => { changed.status.phase = "Failed"; },
  ]) {
    const changed = structuredClone(run);
    mutate(changed);
    assert.throws(() => verifyEvaluationOutcome(changed, "judge-observed", desired.dataset.metadata.name));
  }
});

test("the preserved wrong-name configuration remains failed while emitted score metrics remain passing", () => {
  const run = runFixture("configuration");
  run.status.passed = false;
  for (const result of run.status.caseResults) {
    result.passed = false;
    result.metrics.push(
      { metric: "hallucination_score", value: 1, passed: true },
      { metric: "safety_score", value: 1, passed: true },
      { metric: "hallucination", value: 0, message: "required metric was not emitted by configured graders" },
      { metric: "safety", value: 0, message: "required metric was not emitted by configured graders" });
  }
  verifyEvaluationOutcome(run, "configuration", desired.dataset.metadata.name);
  assert.throws(() => verifyEvaluationOutcome(run, "good", desired.dataset.metadata.name));
  run.status.caseResults[0].metrics.pop();
  assert.throws(() => verifyEvaluationOutcome(run, "configuration", desired.dataset.metadata.name));
});

test("manual runs copy only execution fields and cannot change the approved dataset or graders", () => {
  const evaluation = desired.evaluation("good");
  evaluation.spec.schedule = { cron: "0 * * * *", suspend: true };
  const run = { apiVersion: "kognition.dev/v1alpha1", kind: "EvaluationRun",
    metadata: { name: `${evaluation.metadata.name}-studio-abc123`, namespace: "tutorial-support",
      labels: { "kognition.dev/created-by": "studio", "kognition.dev/evaluation": evaluation.metadata.name } },
    spec: executionSpec(evaluation) };
  assert.equal(run.spec.schedule, undefined);
  verifyManualRun(run, evaluation, "tutorial-support");
  run.spec = { ...run.spec, datasetRef: { ...run.spec.datasetRef, version: "different" } };
  assert.throws(() => verifyManualRun(run, evaluation, "tutorial-support"));
});

test("pinned evaluation rejection requires its explicit reason and no fabricated passing metadata", () => {
  const run = runFixture("pinned");
  run.spec.targetRef.revision = 1;
  run.status = { phase: "Failed", conditions: [{ reason: "PinnedRevisionUnsupported" }] };
  verifyEvaluationOutcome(run, "pinned", desired.dataset.metadata.name);
  run.status.conditions[0].reason = "SomethingElse";
  assert.throws(() => verifyEvaluationOutcome(run, "pinned", desired.dataset.metadata.name));
});

test("evaluation execution refuses missing live-write consent before writing or contacting services", async () => {
  await assert.rejects(runEvaluation({ runtime: "runtime", output: "unused-evaluation",
    runId, target: RANCHER_TARGET }), /authorize-live-writes/);
});
