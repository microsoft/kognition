// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { GOOD, UNKNOWN } = require("./live-evaluation");
const { completionCount, verifyUnexpectedValidJudge, verifyFailedJudgeRuntime,
  runEvaluationFinal, runEvaluationClosure } = require("./live-evaluation-finalize");

function fixture(passed) {
  const run = { metadata: { uid: "run-uid" }, status: { phase: "Ready", passed, caseCount: 2, completedCases: 2,
    caseResults: [["region", GOOD], ["unknown", UNKNOWN]].map(([id, output]) => ({
      id, targetOutput: output, passed, metrics: [{ metric: "judge_score", value: passed ? 1 : 0,
        passed, message: passed ? "Actual valid grade." : "judge output score must be between 0 and 1" }],
    })) } };
  const jobs = [false, false, true, true].map((judge, index) => ({
    metadata: { name: `job-${index}`, ownerReferences: [{ uid: run.metadata.uid }],
      labels: judge ? { "kognition.dev/evaluationJob": "judge" } : {} },
    status: { phase: "Ready", output: judge ? '{"score":1,"passed":true}' : index === 0 ? GOOD : UNKNOWN,
      tokenUsage: { completionTokens: judge ? 1 : 12, promptTokens: judge ? 199 : 188, totalTokens: 200 } },
  }));
  return { run, jobs };
}

test("a valid passing judge result cannot be relabeled as a malformed-output failure", () => {
  const { run, jobs } = fixture(true);
  verifyUnexpectedValidJudge(run, jobs);
  assert.throws(() => verifyFailedJudgeRuntime(run, jobs));
  jobs[2].status.output = "not JSON";
  assert.throws(() => verifyUnexpectedValidJudge(run, jobs));
});

test("completion usage is measured from consistent totals and supports omitted canonical zero counts", () => {
  assert.equal(completionCount({ promptTokens: 200, totalTokens: 201, completionTokens: 1 }), 1);
  assert.equal(completionCount({ promptTokens: 200, totalTokens: 200 }), 0);
  assert.throws(() => completionCount({ promptTokens: 200, totalTokens: 201, completionTokens: 2 }));
  assert.throws(() => completionCount({ totalTokens: 200 }));
});

test("final evaluation capture refuses missing live-write consent before recovery or execution", async () => {
  await assert.rejects(runEvaluationFinal({ runtime: "runtime", output: "unused-evaluation-final",
    runId: "rancher-eval-unit", target: RANCHER_TARGET }), /authorize-live-writes/);
});

test("runtime failure proof is distinct from a judge-score parsing failure", () => {
  const { run, jobs } = fixture(false);
  run.status.phase = "Failed";
  run.status.conditions = [{ reason: "TargetJobFailed" }];
  for (const result of run.status.caseResults) delete result.metrics;
  for (const job of jobs.filter(item => item.metadata.labels["kognition.dev/evaluationJob"] === "judge")) {
    job.status.phase = "Failed";
    job.status.output = "runtime reported inactive session: Error code: 502";
  }
  verifyFailedJudgeRuntime(run, jobs);
  assert.throws(() => verifyUnexpectedValidJudge(run, jobs));
  run.status.caseResults[0].metrics = [{ metric: "judge_score", value: 0, passed: false }];
  assert.throws(() => verifyFailedJudgeRuntime(run, jobs));
});

test("failure closure requires live-write consent before inspecting or restoring an Agent", async () => {
  await assert.rejects(runEvaluationClosure({ runtime: "runtime", output: "unused-evaluation-closure",
    runId: "rancher-eval-unit", target: RANCHER_TARGET }), /authorize-live-writes/);
});
