// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const path = require("node:path");
const { expect } = require("@playwright/test");
const { RANCHER_TARGET } = require("./live-authoring");
const { verifyKubernetesRuntime } = require("./live-pilot");
const { GOOD, BAD, UNKNOWN, instructions } = require("./live-evaluation");
const { PLURALS, verifyIntent, parseContinuationOptions, readContinuation, runProduction } = require("./live-production-session");

const POLICY = Object.freeze({ batch: "solution-release", uiRevision: "create-picker-v1",
  kinds: ["Agent", "ModelGrant", "EvaluationDataset", "Evaluation", "Solution"] });

function fixtures(runId) {
  if (!/^rancher-release-[a-z0-9-]+$/.test(runId) || runId.length > 30) {
    throw new Error("Release capture requires a fresh bounded rancher-release-* run ID");
  }
  const resource = (kind, suffix, spec) => ({ apiVersion: "kognition.dev/v1alpha1", kind,
    metadata: { name: `full-${runId}-${suffix}`, namespace: "tutorial-support",
      labels: { "kognition.dev/recording-run": runId } }, spec });
  const agent = (suffix, answer) => resource("Agent", suffix, {
    modelRef: { name: "tutorial-support-model" }, instructions: { inline: instructions(answer) },
    runtime: { class: "default", timeout: "120s", maxTokens: 256 }, replicas: 1 });
  const good = agent("good", GOOD), bad = agent("bad", BAD);
  const grants = [good, bad].map((item, index) => resource("ModelGrant", `model-${index}`, {
    agentRef: { name: item.metadata.name }, modelRef: { name: "tutorial-support-model" } }));
  const dataset = resource("EvaluationDataset", "data", { version: "v1", cases: [
    { id: "region", input: "Is the fictional Orbital Mark 7 bookable in Brazil South?",
      expected: "not bookable yet", reference: GOOD },
    { id: "unknown", input: "Is the fictional Orbital Mark 7 bookable in Atlantis Central?",
      expected: "do not have evidence", reference: UNKNOWN },
  ] });
  const evaluation = resource("Evaluation", "gate", {
    targetRef: { kind: "Agent", name: good.metadata.name },
    datasetRef: { name: dataset.metadata.name, version: "v1" },
    graders: [{ name: "expected-terms", type: "task_success" },
      { name: "unsupported-claims", type: "hallucination", parameters: { forbidden: "bookable now" } }],
    metricThresholds: [{ metric: "task_success", min: 1, required: true },
      { metric: "hallucination_score", min: 1, required: true }],
  });
  const solution = resource("Solution", "solution", {
    goal: dataset.spec.cases[0].input,
    target: { kind: "Agent", ref: { name: good.metadata.name } },
    constraints: ["Use only this controlled fictional response fixture."], seed: 1042 });
  return { good, bad, grants, dataset, evaluation, solution };
}

function verifyGate(run, solution, evaluation, revision, passed) {
  assert.equal(run.kind, "EvaluationRun");
  assert(run.metadata.uid);
  assert(run.metadata.ownerReferences?.some(owner => owner.uid === solution.metadata.uid && owner.kind === "Solution"));
  assert.equal(run.metadata.namespace, solution.metadata.namespace);
  assert.equal(run.spec.targetRef.kind, "Solution");
  assert.equal(run.spec.targetRef.name, solution.metadata.name);
  assert.equal(run.spec.targetRef.revision, revision);
  assert.equal(run.spec.evaluationRef.name, evaluation.metadata.name);
  assert.deepEqual(run.spec.datasetRef, evaluation.spec.datasetRef);
  assert.deepEqual(run.spec.graders, evaluation.spec.graders);
  assert.deepEqual(run.spec.metricThresholds, evaluation.spec.metricThresholds);
  assert.equal(run.status.phase, "Ready");
  if (passed) assert.equal(run.status.passed, true);
  else assert(run.status.passed === false || run.status.passed === undefined);
  assert.equal(run.status.completedCases, 2);
  const cases = Object.fromEntries(run.status.caseResults.map(item => [item.id, item]));
  assert.equal(cases.region.targetOutput.trim(), passed ? GOOD : BAD);
  assert.equal(cases.unknown.targetOutput.trim(), UNKNOWN);
  if (passed) assert.equal(cases.region.passed, true);
  else assert(cases.region.passed === false || cases.region.passed === undefined);
  assert.equal(cases.unknown.passed, true);
}

async function runRelease(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  if (options.target !== RANCHER_TARGET || path.dirname(options.output).toLowerCase() !== options.runtime.toLowerCase() ||
      !path.basename(options.output).startsWith("release-")) {
    throw new Error("Release capture requires a fresh release-* child of the reviewed Rancher runtime");
  }
  const desired = fixtures(options.runId);
  const previous = readContinuation(options, POLICY.batch, "release-");
  return runProduction(options, POLICY, async session => {
    session.report.coverage = { scenes: ["S02", "S04", "S28", "S30", "S31"], entireScenesCompleted: [],
      qualifications: [
        "User-approved qualified tutorial: unsupported and unverified behaviors remain explicitly identified.",
        "The UI is a hash-qualified four-asset derivative of the frozen CLI; cluster images and backend source remain unchanged.",
        "This Solution targets existing Agents; the generated multi-agent planning variant is not executed.",
        "The gate pins a Solution revision. Its resolved Agent configuration is observed before and after; it is not an immutable Agent revision pin.",
        "Keyword graders and two fictional cases are controlled regression checks, not universal factuality or safety certification.",
        "The stable compiled AgentJob is inspected without replaying completed work; revision promotion is not claimed to repeat it.",
      ] };
    const quota = await session.get("cognitivequotas", "tutorial-budget");
    assert.equal(quota.spec.tokenBudget.maxTotalTokens, 100000);
    assert(quota.status.tokenUsage.totalTokens <= 90000, "Reserve a bounded 10,000-token ceiling for this capture");
    if (!previous) assert(quota.status.used.agents + 2 <= quota.spec.hard.agents,
      "Two fresh bounded Agents must fit existing quota");
    session.report.quotaBefore = quota;
    if (!previous) await session.scene("R01", ["S02", "S04"], "Use the approved searchable Create picker on the real Rancher backend.",
      "The Create picker keeps all supported resource types accessible. Common choices appear first, category filters narrow the list, and search matches readable names, kind names and descriptions. Choosing a resource opens an editor; it does not deploy or start work.", async () => {
        await session.click(session.page.locator("#btn-create"));
        await expect(session.page.getByRole("menuitem")).toHaveCount(43);
        await session.click(session.page.getByRole("button", { name: "Common", exact: true }));
        await expect(session.page.getByRole("menuitem")).toHaveCount(8);
        await session.hold(session.page.locator("#create-menu"), "create-common-resources");
        await session.click(session.page.getByRole("button", { name: "All", exact: true }));
        await session.page.getByRole("searchbox", { name: "Search resource types" }).fill("memory claim");
        await expect(session.page.getByRole("menuitem")).toHaveCount(1);
        await session.hold(session.page.locator("#create-menu"), "create-search-result");
        await session.page.keyboard.press("Escape");
      });
    const ready = async agent => session.wait("bounded release Agent runtime", async () => {
      const current = await session.get("agents", agent.metadata.name);
      const deployment = await session.get("deployments", `agent-${agent.metadata.name}`, session.namespace, true);
      if (!deployment || current.status?.phase !== "Ready" ||
          deployment.status.observedGeneration !== deployment.metadata.generation ||
          !["replicas", "availableReplicas", "readyReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
      assert.equal(current.metadata.uid, agent.metadata.uid);
      assert.deepEqual(current.spec, agent.spec);
      verifyKubernetesRuntime(current, { name: current.metadata.name, namespace: session.namespace,
        uid: current.metadata.uid, generation: current.metadata.generation, deploymentUID: deployment.metadata.uid,
        replicas: 1, maxTokens: 256 }, deployment);
      session.report.evidence.push({ type: "bounded-release-runtime", agent: current, deployment });
      return current;
    });
    let dataset, evaluation, solution;
    if (!previous) {
    await session.scene("R02", ["S28", "S30"], "Prepare separate good and bad candidates and an unchanged two-case dataset.",
      "Two separately authorized Agents make the candidate boundary explicit. The bad candidate changes the known-region answer; the unknown-region answer stays unchanged. Each Agent has a 256-token completion ceiling. The dataset and required score thresholds remain fixed throughout promotion.", async () => {
        for (const grant of desired.grants) await session.save(grant);
        await ready(await session.save(desired.good));
        await ready(await session.save(desired.bad));
        dataset = await session.save(desired.dataset);
        evaluation = await session.save(desired.evaluation);
        const automatic = await session.wait("automatic gate baseline", async () => {
          const definition = await session.get("evaluations", evaluation.metadata.name);
          if (!definition.status?.lastRunRef?.name) return false;
          const run = await session.get("evaluationruns", definition.status.lastRunRef.name);
          if (!["Ready", "Failed"].includes(run.status?.phase)) return false;
          assert.equal(run.status.phase, "Ready"); assert.equal(run.status.passed, true);
          session.report.automaticBaseline = run;
          return run;
        }, 360000);
        await session.inspectObserved("evaluationruns", automatic.metadata.name, ["Ready", "Case results"], "release-baseline");
      });
    await session.scene("R03", ["S30"], "Compile an explicit-target Solution and inspect its one-shot owned execution.",
      "This Solution declares the fictional support goal and explicitly selects the good Agent. We inspect the compiled AgentJob and its ownership. This is the existing-target path, not a demonstration of the generated multi-agent planner.", async () => {
        solution = await session.save(desired.solution);
        solution = await session.wait("initial active Solution revision", async () => {
          const current = await session.get("solutions", solution.metadata.name);
          return current.status?.phase === "Ready" && current.status.activeRevision === 1 ? current : false;
        });
        const job = await session.wait("original Solution AgentJob", async () => {
          const jobs = await session.list("agentjobs", `kognition.dev/solution=${solution.metadata.name}`);
          const owned = jobs.filter(item => item.metadata.ownerReferences?.some(owner => owner.uid === solution.metadata.uid));
          assert(owned.length <= 1);
          if (!owned.length || !["Ready", "Failed"].includes(owned[0].status?.phase)) return false;
          assert.equal(owned[0].status.phase, "Ready");
          assert.equal(owned[0].status.output.trim(), GOOD);
          return owned[0];
        }, 360000);
        session.report.originalExecution = job;
        session.report.activeBeforeGate = solution;
        await session.inspectObserved("solutions", solution.metadata.name, ["Ready", "activeRevision"], "solution-active-original");
        await session.inspectObserved("agentjobs", job.metadata.name, ["Ready", GOOD], "solution-original-execution");
      });
    } else {
      const prior = previous.report;
      assert.equal(prior.scenes.length, 4);
      assert(prior.scenes.slice(0, 3).every(scene => scene.passed === true));
      assert.equal(prior.scenes[3].id, "R04");
      assert.equal(prior.scenes[3].passed, false);
      for (const resource of [desired.good, desired.bad, ...desired.grants, desired.dataset, desired.evaluation]) {
        const identity = prior.created.find(item => item.kind === resource.kind && item.name === resource.metadata.name);
        assert(identity, "Adoption requires exact original creation evidence");
        const current = await session.get(PLURALS[resource.kind], resource.metadata.name);
        assert.equal(current.metadata.uid, identity.uid);
        verifyIntent(current, resource);
        if (resource.kind === "Agent") await ready(current);
      }
      dataset = await session.get("evaluationdatasets", desired.dataset.metadata.name);
      evaluation = await session.get("evaluations", desired.evaluation.metadata.name);
      solution = await session.get("solutions", desired.solution.metadata.name);
      const expected = structuredClone(desired.solution);
      expected.metadata.uid = prior.created.find(item => item.kind === "Solution").uid;
      expected.spec.target.ref.name = desired.bad.metadata.name;
      expected.spec.promotionGates = [{ evaluationRef: { name: evaluation.metadata.name } }];
      verifyIntent(solution, expected);
      assert.equal(solution.status.activeRevision, 1);
      assert.equal(solution.status.candidateRevision, 2);
      session.report.originalExecution = prior.originalExecution;
      session.report.automaticBaseline = prior.automaticBaseline;
      session.report.activeBeforeGate = prior.activeBeforeGate;
      session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
        sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false };
      session.persist();
    }
    const gate = async (revision, passed) => session.wait("exact candidate promotion result", async () => {
      const current = await session.get("solutions", solution.metadata.name);
      const runs = (await session.list("evaluationruns")).filter(run =>
        run.metadata.ownerReferences?.some(owner => owner.uid === solution.metadata.uid) &&
        run.spec.targetRef?.revision === revision);
      assert(runs.length <= 1, "Do not repeat or accept ambiguous candidate work");
      if (!runs.length || !["Ready", "Failed"].includes(runs[0].status?.phase)) return false;
      verifyGate(runs[0], solution, evaluation, revision, passed);
      if (passed && (current.status?.phase !== "Ready" || current.status.activeRevision !== revision)) return false;
      if (!passed && current.status?.candidateRevision !== revision) return false;
      assert.equal(current.metadata.uid, solution.metadata.uid);
      assert.equal(current.status.activeRevision, passed ? revision : 1);
      if (!passed) assert(current.status.conditions.some(item => item.reason === "PromotionBlocked"));
      session.report.evidence.push({ type: "candidate-bound-gate", solution: current, run: runs[0] });
      return { solution: current, run: runs[0] };
    }, 360000);
    await session.scene("R04", ["S31"], "Keep the original active revision when the exact bad candidate fails.",
      previous
        ? "The earlier take already created and evaluated this candidate. Its recorder rejected an omitted false Boolean in the Kubernetes response. We inspect that same settled failed result without replaying work. Candidate revision two remains blocked, and revision one remains active."
        : "We select the separate bad Agent and require the fixed Evaluation. The controller creates a run bound to candidate Solution revision two. The known-region case fails while the unknown-region case passes. The previously active revision remains active.", async () => {
        if (!previous) {
        const candidate = await session.get("solutions", solution.metadata.name);
        candidate.spec.target.ref.name = desired.bad.metadata.name;
        candidate.spec.promotionGates = [{ evaluationRef: { name: evaluation.metadata.name } }];
        await session.save(candidate, false);
        }
        const observed = await gate(2, false);
        session.report.blockedCandidate = observed;
        await session.inspectObserved("evaluationruns", observed.run.metadata.name, ["Case results", BAD], "candidate-failed-score");
        await session.inspectObserved("solutions", solution.metadata.name, ["PromotionBlocked", "activeRevision"], "active-preserved-on-failure");
      });
    await session.scene("R05", ["S31"], "Promote the repaired candidate using its own passing revision-bound run.",
      "We repair the Solution target rather than changing data or weakening thresholds. The controller evaluates candidate revision three, then promotes that exact candidate after its own run passes. The earlier failed run and original completed AgentJob remain unchanged.", async () => {
        const repaired = await session.get("solutions", solution.metadata.name);
        repaired.spec.target.ref.name = desired.good.metadata.name;
        await session.save(repaired, false);
        const observed = await gate(3, true);
        session.report.promotedCandidate = observed;
        await session.inspectObserved("evaluationruns", observed.run.metadata.name, ["Ready", "Case results"], "candidate-passing-score");
        await session.inspectObserved("solutions", solution.metadata.name, ["Ready", "activeRevision"], "candidate-promoted");
        const unchanged = await session.get("evaluationdatasets", dataset.metadata.name);
        assert.equal(unchanged.metadata.uid, dataset.metadata.uid); verifyIntent(unchanged, dataset);
        for (const desiredAgent of [desired.good, desired.bad]) verifyIntent(await session.get("agents", desiredAgent.metadata.name), desiredAgent);
        const original = await session.get("agentjobs", session.report.originalExecution.metadata.name);
        assert.equal(original.metadata.uid, session.report.originalExecution.metadata.uid);
        assert.equal(original.status.output, session.report.originalExecution.status.output);
        session.report.originalExecutionAfterPromotion = original;
      });
    session.report.quotaAfter = await session.get("cognitivequotas", "tutorial-budget");
    assert(session.report.quotaAfter.status.tokenUsage.totalTokens <= 100000);
  });
}

if (require.main === module) runRelease(parseContinuationOptions(process.argv.slice(2))).catch(error => {
  console.error(error.message); process.exitCode = 1;
});

module.exports = { POLICY, fixtures, verifyGate, runRelease };
