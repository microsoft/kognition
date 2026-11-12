// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { assertResourceScope } = require("./live-production-session");
const { POLICY, fixtures, verifyGate, runRelease } = require("./live-release");

test("release fixtures keep candidates, grants and immutable checks separate and bounded", () => {
  const runId = "rancher-release-unit";
  const desired = fixtures(runId);
  for (const item of [desired.good, desired.bad, ...desired.grants, desired.dataset, desired.evaluation, desired.solution]) {
    assertResourceScope(item, { ...POLICY, runId, target: RANCHER_TARGET });
  }
  assert.notEqual(desired.good.metadata.name, desired.bad.metadata.name);
  assert.equal(desired.good.spec.runtime.maxTokens, 256);
  assert.equal(desired.bad.spec.runtime.maxTokens, 256);
  assert.equal(desired.solution.spec.target.ref.name, desired.good.metadata.name);
  assert.equal(desired.evaluation.spec.targetRef.revision, undefined);
  assert.equal(desired.evaluation.spec.schedule, undefined);
  assert.throws(() => fixtures("../escape"));
});

test("promotion evidence requires the exact Solution revision, owner, dataset, bounds and actual case outcome", () => {
  const { solution, evaluation } = fixtures("rancher-release-unit");
  solution.metadata.uid = "owned-solution";
  const run = { kind: "EvaluationRun", metadata: { uid: "owned-run", namespace: "tutorial-support",
    ownerReferences: [{ kind: "Solution", uid: solution.metadata.uid }] },
    spec: { targetRef: { kind: "Solution", name: solution.metadata.name, revision: 3 },
      evaluationRef: { name: evaluation.metadata.name }, datasetRef: evaluation.spec.datasetRef,
      graders: evaluation.spec.graders, metricThresholds: evaluation.spec.metricThresholds },
    status: { phase: "Ready", passed: true, completedCases: 2, caseResults: [
      { id: "region", targetOutput: "Brazil South is not bookable yet.", passed: true },
      { id: "unknown", targetOutput: "I do not have evidence for that region.", passed: true },
    ] } };
  verifyGate(run, solution, evaluation, 3, true);
  for (const change of [
    changed => { changed.spec.targetRef.revision = 2; },
    changed => { changed.metadata.ownerReferences = []; },
    changed => { changed.spec.datasetRef.version = "weakened"; },
    changed => { changed.spec.metricThresholds[0].min = 0; },
    changed => { changed.status.passed = false; },
  ]) {
    const changed = structuredClone(run);
    change(changed);
    assert.throws(() => verifyGate(changed, solution, evaluation, 3, true));
  }
  const failed = structuredClone(run);
  failed.spec.targetRef.revision = 2;
  delete failed.status.passed;
  delete failed.status.caseResults[0].passed;
  failed.status.caseResults[0].targetOutput = "Brazil South is bookable now.";
  verifyGate(failed, solution, evaluation, 2, false);
  failed.status.passed = true;
  assert.throws(() => verifyGate(failed, solution, evaluation, 2, false));
});

test("release capture refuses missing live-write consent before preparing resources", async () => {
  await assert.rejects(runRelease({ runtime: "runtime", output: "unused-release",
    runId: "rancher-release-unit", target: RANCHER_TARGET }), /authorize-live-writes/);
});
