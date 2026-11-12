// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { verifyKubernetesRuntime } = require("./live-pilot");
const { verifyIntent, parseContinuationOptions, readContinuation, runProduction } = require("./live-production-session");
const { GOOD, UNKNOWN, JUDGE, MALFORMED_JUDGE, fixtures, executionSpec, verifyEvaluationOutcome } = require("./live-evaluation");

const POLICY = Object.freeze({ batch: "evaluation-final-contract", kinds: ["Agent", "Evaluation"] });

function completionCount(usage) {
  assert(Number.isSafeInteger(usage.promptTokens) && usage.promptTokens >= 0);
  assert(Number.isSafeInteger(usage.totalTokens) && usage.totalTokens >= usage.promptTokens);
  const completed = usage.totalTokens - usage.promptTokens;
  if (usage.completionTokens !== undefined) assert.equal(usage.completionTokens, completed);
  return completed;
}

function verifyUnexpectedValidJudge(run, jobs) {
  assert.equal(run.status.phase, "Ready");
  assert.equal(run.status.passed, true);
  assert.equal(run.status.caseCount, 2);
  assert.equal(jobs.length, 4);
  for (const job of jobs) {
    assert(job.metadata.ownerReferences?.some(owner => owner.uid === run.metadata.uid));
    assert.equal(job.status.phase, "Ready");
    if (job.metadata.labels["kognition.dev/evaluationJob"] === "judge") {
      const actual = JSON.parse(job.status.output);
      assert(typeof actual.score === "number" && actual.score >= 0.8 && actual.passed === true);
    }
  }
}

function verifyFailedJudgeRuntime(run, jobs) {
  assert.equal(run.status.phase, "Failed");
  assert(run.status.conditions.some(item => item.reason === "TargetJobFailed"));
  assert.equal(jobs.length, 4);
  const targets = jobs.filter(item => item.metadata.labels["kognition.dev/evaluationJob"] !== "judge");
  const judges = jobs.filter(item => item.metadata.labels["kognition.dev/evaluationJob"] === "judge");
  assert.equal(targets.length, 2);
  assert.equal(judges.length, 2);
  assert.deepEqual(targets.map(job => job.status.output.trim()).sort(), [GOOD, UNKNOWN].sort());
  for (const job of jobs) assert(job.metadata.ownerReferences?.some(owner => owner.uid === run.metadata.uid));
  assert(targets.every(job => job.status.phase === "Ready"));
  for (const job of targets) {
    assert([GOOD, UNKNOWN].includes(job.status.output.trim()));
    assert(completionCount(job.status.tokenUsage) > 0);
  }
  assert(judges.every(job => job.status.phase === "Failed" && /inactive session.*502/i.test(job.status.output)));
  assert(!(run.status.caseResults || []).some(result => result.metrics?.some(metric => metric.metric === "judge_score")),
    "A runtime failure must not be relabeled as a parsed judge-score failure");
}

async function runEvaluationClosure(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  if (options.target !== RANCHER_TARGET || !options.continuationPath) {
    throw new Error("Failure closure requires the exact Rancher target and preserved failed final-contract report");
  }
  const previous = readContinuation(options, POLICY.batch, "evaluation-");
  const desired = fixtures(options.runId);
  return runProduction(options, { batch: "evaluation-failure-closure", kinds: ["Agent"] }, async session => {
    const prior = previous.report;
    assert.equal(prior.created.length, 1);
    assert.equal(prior.created[0].kind, "Evaluation");
    const evaluation = await session.get("evaluations", prior.created[0].name);
    assert.equal(evaluation.metadata.uid, prior.created[0].uid);
    const run = await session.get("evaluationruns", evaluation.status.lastRunRef.name);
    const jobs = await session.list("agentjobs", `kognition.dev/evaluationRun=${run.metadata.name}`);
    verifyFailedJudgeRuntime(run, jobs);
    const instances = (await session.list("agentinstances")).filter(item =>
      item.metadata.ownerReferences?.some(owner => jobs.some(job => job.metadata.uid === owner.uid)));
    assert.equal(instances.length, 6);
    assert(instances.every(item => ["Ready", "Failed"].includes(item.status.phase)),
      "All original attempts must settle before restoring the mutable judge configuration");
    const priorEvaluationJobs = await session.list("agentjobs", `kognition.dev/evaluation=${evaluation.metadata.name}`);
    session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
      sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false };
    session.report.settledRuntimeFailure = { evaluation, run, jobs, instances,
      graderParsingFailureDemonstrated: false, failedJudgeUsageUnavailable: true };
    session.report.unproducedMalformedOutput = prior.unproducedMalformedOutput;
    session.report.priorRuns = prior.priorRuns;
    session.report.coverage = { entireScenesCompleted: [], scenes: ["S28", "S29"],
      qualifications: [
        "The one-token fixture failed at runtime with 502 before judge grading; it is not evidence of malformed-JSON score rejection.",
        "The runtime's configured bounded retries are retained as six actual terminal attempts across four jobs.",
        "Failed judge calls did not provide reliable usage; namespace quota projection is not a provider invoice.",
        "This closure reads settled work and restores one owned Agent; it does not create or repeat an evaluation.",
      ] };
    await session.scene("E13", ["S29"], "Inspect the actual settled judge-runtime failure without calling it a grader-parse result.",
      "The two target jobs completed, but the one-token judge jobs failed at runtime with an inactive-session 502. The configured retry attempts have settled. No judge-score parsing result was produced, and we preserve that distinction.", async () => {
        await session.inspectObserved("evaluationruns", run.metadata.name, ["Failed", "502"], "actual-judge-runtime-failure");
      });
    await session.scene("E14", ["S29"], "Restore the healthy judge only after every previous attempt is terminal.",
      "We now restore the normal judge instruction and 256-token ceiling on the same Agent identity. This is configuration recovery, not another evaluation attempt. All prior failed and passing results remain unchanged.", async () => {
        let judge = await session.get("agents", desired.judge.metadata.name);
        const archived = prior.evidence.filter(item => item.type === "verified-judge-runtime").at(-1).agent;
        verifyIntent(judge, archived);
        assert.equal(judge.spec.runtime.maxTokens, 1);
        judge.spec.instructions.inline = JUDGE;
        judge.spec.runtime.maxTokens = 256;
        const saved = await session.save(judge, false);
        judge = await session.wait("restored healthy judge runtime", async () => {
          const current = await session.get("agents", saved.metadata.name);
          const deployment = await session.get("deployments", `agent-${saved.metadata.name}`);
          const container = deployment.spec.template.spec.containers.find(item => item.name === "agent");
          const env = Object.fromEntries(container.env.map(item => [item.name, item.value]));
          if (current.status?.phase !== "Ready" || current.metadata.generation !== saved.metadata.generation ||
              env.KOGNITION_SYSTEM_PROMPT !== JUDGE || env.KOGNITION_MAX_TOKENS !== "256" ||
              deployment.status?.observedGeneration !== deployment.metadata.generation ||
              !["replicas", "availableReplicas", "readyReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
          assert.equal(current.metadata.uid, archived.metadata.uid);
          session.report.restoredJudgeRuntime = deployment;
          return current;
        });
        session.report.restoredJudge = judge;
        const afterJobs = await session.list("agentjobs", `kognition.dev/evaluation=${evaluation.metadata.name}`);
        assert.deepEqual(afterJobs.map(item => item.metadata.uid).sort(), priorEvaluationJobs.map(item => item.metadata.uid).sort());
        const unchanged = await session.get("evaluationruns", run.metadata.name);
        assert.equal(unchanged.metadata.uid, run.metadata.uid);
        assert.equal(unchanged.status.phase, "Failed");
      });
    session.report.datasetAfter = await session.get("evaluationdatasets", desired.dataset.metadata.name);
    assert.deepEqual(session.report.datasetAfter.spec, desired.dataset.spec);
    session.report.totalObservedTargetAndJudgeJobs = prior.priorRuns.reduce((sum, item) => sum + item.jobs.length, 0) +
      prior.unproducedMalformedOutput.jobs.length + jobs.length;
    assert.equal(session.report.totalObservedTargetAndJudgeJobs, 32);
  });
}

async function runEvaluationFinal(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  if (options.target !== RANCHER_TARGET || !options.continuationPath) {
    throw new Error("Final evaluation capture requires the exact Rancher target and preserved preceding report");
  }
  const previous = readContinuation(options, "evaluation-quality", "evaluation-");
  const desired = fixtures(options.runId);
  return runProduction(options, POLICY, async session => {
    const prior = previous.report;
    assert.equal(prior.runs.length, 7);
    assert.deepEqual(prior.runs.map(item => item.variant), ["pinned", "configuration", "good", "manual", "bad", "fixed", "metric"]);
    const jobsFor = run => session.list("agentjobs", `kognition.dev/evaluationRun=${run.metadata.name}`);
    for (const result of prior.runs) {
      const live = await session.get("evaluationruns", result.run.metadata.name);
      assert.equal(live.metadata.uid, result.run.metadata.uid);
      assert.deepEqual(live.spec, result.run.spec);
      verifyEvaluationOutcome(live, result.variant, desired.dataset.metadata.name);
      assert.equal((await jobsFor(live)).length, result.jobs.length);
    }
    const dataset = await session.get("evaluationdatasets", desired.dataset.metadata.name);
    assert.deepEqual(dataset.spec, desired.dataset.spec);
    const target = await session.get("agents", desired.target.metadata.name);
    const archivedTarget = prior.runtimeRevisions.filter(item => item.agent.metadata.name === target.metadata.name).at(-1).agent;
    verifyIntent(target, archivedTarget);
    let judge = await session.get("agents", desired.judge.metadata.name);
    const archivedJudge = prior.runtimeRevisions.filter(item => item.agent.metadata.name === judge.metadata.name).at(-1).agent;
    verifyIntent(judge, archivedJudge);
    assert.equal(judge.spec.instructions.inline, MALFORMED_JUDGE);
    const observed = await session.get("evaluations", desired.evaluation("judge").metadata.name);
    const unexpected = await session.get("evaluationruns", observed.status.lastRunRef.name);
    assert(unexpected.metadata.ownerReferences?.some(owner => owner.uid === observed.metadata.uid));
    const unexpectedJobs = await jobsFor(unexpected);
    verifyUnexpectedValidJudge(unexpected, unexpectedJobs);
    const truncatedDefinition = desired.evaluation("trunc");
    assert.equal(await session.get("evaluations", truncatedDefinition.metadata.name, session.namespace, true), null,
      "Never replay the truncation evaluation");
    session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
      sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false };
    session.report.priorRuns = prior.runs;
    session.report.unproducedMalformedOutput = { run: unexpected, jobs: unexpectedJobs,
      actualResultPassed: true, requestedMalformedOutputProduced: false };
    session.report.coverage = { entireScenesCompleted: [], scenes: ["S28", "S29"],
      qualifications: [
        "A model instruction requesting malformed output did not guarantee it; the actual valid, passing result is retained.",
        "The separate negative case deliberately restricts judge output to one token and verifies actual completion usage.",
        "No target outputs, expected data, grader scores or completed run states are rewritten.",
        "The original preceding runner report remains failed; its completed functional prefix is independently reverified.",
      ] };
    const ready = async (agent, content, tokens) => session.wait("judge runtime configuration", async () => {
      const current = await session.get("agents", agent.metadata.name);
      const deployment = await session.get("deployments", `agent-${agent.metadata.name}`);
      const container = deployment.spec.template.spec.containers.find(item => item.name === "agent");
      const env = Object.fromEntries(container.env.map(item => [item.name, item.value]));
      if (current.status?.phase !== "Ready" || current.metadata.generation !== agent.metadata.generation ||
          env.KOGNITION_SYSTEM_PROMPT !== content || env.KOGNITION_MAX_TOKENS !== String(tokens) ||
          deployment.status?.observedGeneration !== deployment.metadata.generation ||
          !["replicas", "availableReplicas", "readyReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
      assert.equal(current.metadata.uid, agent.metadata.uid);
      assert.equal(container.image, session.target.runtimeImage);
      verifyKubernetesRuntime(current, { name: current.metadata.name, namespace: session.namespace, uid: current.metadata.uid,
        generation: current.metadata.generation, deploymentUID: deployment.metadata.uid, replicas: 1, maxTokens: tokens }, deployment);
      session.report.evidence.push({ type: "verified-judge-runtime", agent: current, deployment });
      return current;
    });
    await session.scene("E10", ["S29"], "Inspect the actual valid result from the unfulfilled malformed-output request.",
      "The preceding instruction asked for malformed judge output, but the model returned valid JSON and the run passed. We preserve that observed result. It is not evidence that malformed-output rejection was demonstrated.", async () => {
        await session.inspectObserved("evaluationruns", unexpected.metadata.name, ["Ready", "passed"], "unexpected-valid-judge-result");
      });
    await session.scene("E11", ["S29"], "Inspect the frozen profile's separate one-token judge-runtime failure.",
      "This separate evaluation uses a deliberately one-token judge ceiling. On the qualified frozen profile, judge jobs fail at runtime with 502 before grading. We retain the terminal attempts and do not claim a malformed-JSON parsing result or reliable failed-call usage.", async () => {
        judge.spec.instructions.inline = JUDGE;
        judge.spec.runtime.maxTokens = 1;
        judge = await ready(await session.save(judge, false), JUDGE, 1);
        const evaluation = await session.save(truncatedDefinition);
        const parent = await session.wait("truncation evaluation creation", async () => {
          const current = await session.get("evaluations", evaluation.metadata.name);
          return current.status?.lastRunRef?.name ? current : false;
        });
        const run = await session.wait("truncation evaluation terminal", async () => {
          const current = await session.get("evaluationruns", parent.status.lastRunRef.name);
          return ["Ready", "Failed"].includes(current.status?.phase) ? current : false;
        }, 360000);
        assert.deepEqual(run.spec, executionSpec(evaluation));
        const jobs = await session.wait("all one-token attempts settled", async () => {
          const current = await jobsFor(run);
          if (current.length !== 4 || !current.every(item => ["Ready", "Failed"].includes(item.status.phase))) return false;
          const attempts = (await session.list("agentinstances")).filter(item =>
            item.metadata.ownerReferences?.some(owner => current.some(job => job.metadata.uid === owner.uid)));
          if (!attempts.every(item => ["Ready", "Failed"].includes(item.status.phase))) return false;
          return current;
        });
        const settled = await session.get("evaluationruns", run.metadata.name);
        verifyFailedJudgeRuntime(settled, jobs);
        assert.equal(settled.status.datasetDigest, prior.runs.find(item => item.variant === "good").run.status.datasetDigest);
        session.report.oneTokenRuntimeFailure = { evaluation, run: settled, jobs, failedJudgeUsageUnavailable: true };
        await session.inspectObserved("evaluationruns", settled.metadata.name, ["Failed", "502"], "actual-one-token-runtime-failure");
      });
    await session.scene("E12", ["S29"], "Restore the healthy judge configuration without replaying any completed evaluation.",
      "After the negative test, we restore the normal judge instruction and 256-token ceiling on the same Agent identity. Historical failed and passing results remain unchanged, and no completed evaluation is repeated.", async () => {
        const restored = await session.get("agents", judge.metadata.name);
        assert.equal(restored.metadata.uid, judge.metadata.uid);
        restored.spec.instructions.inline = JUDGE;
        restored.spec.runtime.maxTokens = 256;
        judge = await ready(await session.save(restored, false), JUDGE, 256);
        session.report.restoredJudge = judge;
        session.report.datasetAfter = await session.get("evaluationdatasets", dataset.metadata.name);
        assert.deepEqual(session.report.datasetAfter.spec, dataset.spec);
        assert.equal(session.report.datasetAfter.metadata.uid, dataset.metadata.uid);
      });
    session.report.totalObservedTargetAndJudgeJobs = prior.runs.reduce((sum, item) => sum + item.jobs.length, 0) +
      unexpectedJobs.length + session.report.oneTokenRuntimeFailure.jobs.length;
    assert.equal(session.report.totalObservedTargetAndJudgeJobs, 32);
  });
}

if (require.main === module) {
  const args = process.argv.slice(2);
  const closeExisting = args[0] === "--close-existing-failure";
  const run = closeExisting ? runEvaluationClosure : runEvaluationFinal;
  run(parseContinuationOptions(closeExisting ? args.slice(1) : args)).catch(error => {
    console.error(error.message); process.exitCode = 1;
  });
}

module.exports = { completionCount, verifyUnexpectedValidJudge, verifyFailedJudgeRuntime,
  runEvaluationFinal, runEvaluationClosure };
