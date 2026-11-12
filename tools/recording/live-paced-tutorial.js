// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const assert = require("node:assert/strict");
const { expect } = require("@playwright/test");
const { RANCHER_TARGET, INSTRUCTIONS, ANSWERS, INPUT } = require("./live-authoring");
const { verifyKubernetesRuntime } = require("./live-pilot");
const { runProduction, parseContinuationOptions, readContinuation, observedResourceView, verifyIntent } = require("./live-production-session");
const { verifyEvaluationOutcome } = require("./live-evaluation");
const { verifyGate } = require("./live-release");

const POLICY = Object.freeze({ batch: "paced-current-ui", uiRevision: "create-picker-v1",
  manifestPreviewSeconds: 4, kinds: ["Agent", "ModelGrant"] });
const NARRATION = Object.freeze({
  N01: "The updated Create picker puts common resources first. Search and category filters keep the full supported catalog accessible. Selecting an item opens its editor; execution still requires the separate deployment or launch action.",
  N02: "We begin a fresh authoring demonstration in the actual support namespace. The prepared model already exists. Its YAML is readable block-style YAML, rather than a JSON document pasted into a YAML-labelled tab.",
  N03: "We create a new bounded Agent with an explicit model reference and inline instructions. The YAML is parsed by the real backend and checked against the intended specification. Saving a definition is separate from permission to call its model.",
  N04: "The exact ModelGrant authorizes this Agent to use the prepared model. We wait for the physical runtime, then verify its actual instruction and 256-token completion limit before making a call.",
  N05: "This is a fresh real provider-backed call. The first answer follows the authored response contract. Studio shows the completed answer and usage; we keep enough time to read that result.",
  N06: "Now we change only the inline instruction on the same Agent. After the updated runtime is ready, a separate real call follows the second contract. The two actual results are retained; they are not substituted responses.",
  F01: "We now inspect the existing Prompt from the earlier execution, without replaying it. It stores reusable instructions and parameter declarations. The current version and recorded snapshots establish stored configuration, not automatic parameter substitution or hot reload.",
  F02: "The frozen native dispatcher did not deliver the referenced Prompt text to its runtime. The original demonstration explicitly switched to inline instructions before inference. Here we inspect its already completed revision-pinned AgentJob and actual output. This is read-only inspection, not a new job.",
  G01: "These are the existing governance probe results. The missing-grant run failed before publication. The separately allowed run completed one exact fictional publication. We inspect the original run identities without submitting another publication.",
  G02: "The changed-argument run failed even though a grant existed, and the expired-grant run also failed. Their original receiver receipts recorded zero effects. The current inspection does not recreate an expired grant or repeat the requested action.",
  G03: "The rejected workflow remains failed. The two-vote gate received one authenticated scripted vote, stayed insufficient, then expired without publication. This does not demonstrate two independent human approvers. The frozen viewer can retain an approval banner after failure; actual terminal status and events determine the outcome.",
  E01: "The existing dataset has two stable fictional cases. The Evaluation declares the graders and required numeric thresholds in YAML. This is a controlled regression fixture, not a universal safety or factuality benchmark.",
  E02: "The original configuration run completed its answers but failed because required metric names did not match the emitted score names. Correcting those names preserved the expected data, required flags and numeric bounds. The corrected automatic run passed.",
  E03: "This distinct manual run also passed. We inspect its own identity and case results. We are not pressing Run evaluation again, and the inspection makes no new provider call.",
  E04: "The bad candidate failed the known-region case while the unknown-region case passed. Repairing the same target restored the expected answer and passed the unchanged checks. Both original results remain separate and inspectable.",
  E05: "A required metric that no configured grader emitted caused a genuine quality failure. A completed execution is not necessarily a passing evaluation. The frozen viewer's pending label can be misleading; the actual metrics and immutable result are the authority.",
  E06: "The malformed-output request actually returned valid judge JSON. The separate one-token experiment failed at runtime with an inactive-session 502 before grading. Neither result proves malformed-JSON grader rejection. The judge was restored only after every attempt settled.",
  S01: "This existing Solution explicitly targets an Agent. We inspect its YAML and the previously compiled owned AgentJob. It uses the existing-target path; we do not claim that a generated multi-agent planning variant executed.",
  S02: "Candidate Solution revision two has its own failed evaluation. The active revision stayed at one. Repaired candidate three passed its own revision-bound gate and became active. We inspect those exact results without replaying the original job or changing thresholds.",
  S03: "The Solution is now active at revision three. The gate pins a Solution revision, while the resolved Agent configuration is observed rather than immutably pinned. Original failure reports, outputs and the single original execution remain retained.",
});

function fixtures(runId) {
  if (!/^rancher-paced-[a-z0-9-]+$/.test(runId) || runId.length > 30) throw new Error("Use a fresh bounded rancher-paced-* ID");
  const resource = (kind, suffix, spec) => ({ apiVersion: "kognition.dev/v1alpha1", kind,
    metadata: { name: `full-${runId}-${suffix}`, namespace: "tutorial-support",
      labels: { "kognition.dev/recording-run": runId } }, spec });
  const agent = resource("Agent", "agent", { modelRef: { name: "tutorial-support-model" },
    instructions: { inline: INSTRUCTIONS[0] }, runtime: { class: "default", timeout: "120s", maxTokens: 256 }, replicas: 1 });
  const grant = resource("ModelGrant", "model", { agentRef: { name: agent.metadata.name },
    modelRef: { name: "tutorial-support-model" } });
  return { agent, grant };
}

function verifySnapshot(current, recorded) {
  assert.equal(current.kind, recorded.kind);
  assert.equal(current.metadata.uid, recorded.metadata.uid, "Historical inspection requires the original resource incarnation");
  assert.equal(current.metadata.name, recorded.metadata.name);
  assert.equal(current.metadata.namespace, "tutorial-support");
  assert.deepEqual(current.spec, recorded.spec);
  assert.equal(current.status.phase, recorded.status.phase);
}

async function runPaced(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  if (options.target !== RANCHER_TARGET || path.dirname(options.output).toLowerCase() !== options.runtime.toLowerCase() ||
      !path.basename(options.output).startsWith("paced-")) {
    throw new Error("Paced capture requires a fresh paced-* child on the exact Rancher target");
  }
  const desired = fixtures(options.runId);
  const previous = readContinuation(options, POLICY.batch, "paced-");
  const read = relative => {
    const file = path.join(options.runtime, relative);
    const bytes = fs.readFileSync(file);
    return { file, sha256: crypto.createHash("sha256").update(bytes).digest("hex"), report: JSON.parse(bytes) };
  };
  const foundation = read("foundations-20261105-a2\\report.json");
  const governance = read("governance-20261105-a3-continuation1\\report.json");
  const evaluation = read("evaluation-20261105-a3-continuation2\\report.json");
  const closure = read("evaluation-20261105-a3-failure-closure\\report.json");
  const release = read("release-20261105-a1-continuation1\\report.json");
  assert(foundation.report.passed && governance.report.passed && closure.report.passed && release.report.passed);
  return runProduction(options, previous ? { ...POLICY, kinds: [] } : POLICY, async session => {
    session.report.scope = "Fresh opening authoring and actual new calls; subsequent completed work inspected read-only through the updated UI. User-approved pacing correction.";
    session.report.lineage = [foundation, governance, evaluation, closure, release].map(({ file, sha256 }) => ({ file, sha256 }));
    session.report.yamlChecks = [];
    session.report.inspections = [];
    const scene = (id, caption, action) => session.scene(id, [], caption, NARRATION[id], action);
    const yaml = async (plural, name) => {
      await session.select(plural, name);
      await session.click(session.page.locator('[data-view="yaml"]'));
      const area = session.page.locator("#yaml-area");
      await area.press("Control+Home");
      const text = await area.inputValue();
      assert(/^apiVersion:\s/m.test(text) && /^kind:\s/m.test(text) && !/^\s*[{[]/.test(text),
        "The YAML tab must show readable native YAML, never a JSON-formatted document");
      session.report.yamlChecks.push({ plural, name, seconds: session.seconds(),
        readableBlockYAML: true, textSHA256: crypto.createHash("sha256").update(text).digest("hex") });
      await session.page.waitForTimeout(3000);
    };
    const inspect = async (plural, recorded) => {
      const current = await session.get(plural, recorded.metadata.name);
      verifySnapshot(current, recorded);
      session.report.inspections.push({ plural, uid: current.metadata.uid, name: current.metadata.name,
        historicalRunReadOnly: true, current });
      await session.select(plural, current.metadata.name);
      await session.click(session.page.locator('[data-view="builder"]'));
      const view = observedResourceView(session.page, plural);
      await expect(view).toBeVisible();
      if (["evaluationruns", "workflowruns"].includes(plural)) {
        for (const detail of await view.locator("details").all()) {
          if (await detail.getAttribute("open") === null) await detail.locator("summary").click();
        }
      }
      await view.scrollIntoViewIfNeeded();
      await session.page.waitForTimeout(3000);
      return current;
    };
    const ready = async (saved, instruction) => session.wait("fresh paced Agent physical runtime", async () => {
      const current = await session.get("agents", saved.metadata.name);
      const deployment = await session.get("deployments", `agent-${saved.metadata.name}`, session.namespace, true);
      if (!deployment || current.status?.phase !== "Ready" ||
          deployment.status.observedGeneration !== deployment.metadata.generation ||
          !["readyReplicas", "availableReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
      assert.equal(current.metadata.uid, saved.metadata.uid);
      const container = deployment.spec.template.spec.containers.find(item => item.name === "agent");
      if (container.env.find(item => item.name === "KOGNITION_SYSTEM_PROMPT")?.value !== instruction) return false;
      verifyKubernetesRuntime(current, { name: current.metadata.name, namespace: session.namespace, uid: current.metadata.uid,
        generation: current.metadata.generation, deploymentUID: deployment.metadata.uid, replicas: 1, maxTokens: 256 }, deployment);
      session.report.evidence.push({ type: "paced-runtime-verified", agent: current, deployment });
      return current;
    });
    const quota = await session.get("cognitivequotas", "tutorial-budget");
    assert.equal(quota.spec.tokenBudget.maxTotalTokens, 100000);
    assert(quota.status.tokenUsage.totalTokens < 95000);
    if (!previous) assert(quota.status.used.agents + 1 <= quota.spec.hard.agents);
    let agent;
    if (!previous) {
    await scene("N01", "Open and search the updated Create picker.", async () => {
      await session.click(session.page.locator("#btn-create"));
      await expect(session.page.getByRole("menuitem")).toHaveCount(43);
      await session.click(session.page.getByRole("button", { name: "Common", exact: true }));
      await expect(session.page.getByRole("menuitem")).toHaveCount(8);
      await session.page.waitForTimeout(4000);
      await session.click(session.page.getByRole("button", { name: "All", exact: true }));
      await session.page.getByRole("searchbox", { name: "Search resource types" }).fill("Agent");
      await session.page.waitForTimeout(2000);
      await session.page.keyboard.press("Escape");
    });
    await scene("N02", "Inspect the actual namespace and readable Model YAML.", async () => {
      await yaml("models", "tutorial-support-model");
    });
    await scene("N03", "Create a fresh Agent using actual readable YAML.", async () => {
      agent = await session.save(desired.agent);
      await yaml("agents", agent.metadata.name);
    });
    await scene("N04", "Save its exact ModelGrant and verify runtime readiness.", async () => {
      await session.save(desired.grant);
      agent = await ready(agent, INSTRUCTIONS[0]);
      await yaml("modelgrants", desired.grant.metadata.name);
    });
    await scene("N05", "Make and read the first real provider call.", async () => {
      await session.chat(agent.metadata.name, INPUT, body => {
        assert.equal(body.response.trim(), ANSWERS[0]); assert(body.usage.completionTokens <= 256);
      });
    });
    await scene("N06", "Revise the same Agent and verify a second real answer.", async () => {
      const revised = await session.get("agents", agent.metadata.name);
      revised.spec.instructions.inline = INSTRUCTIONS[1];
      agent = await ready(await session.save(revised, false), INSTRUCTIONS[1]);
      await session.chat(agent.metadata.name, INPUT, body => {
        assert.equal(body.response.trim(), ANSWERS[1]); assert(body.usage.completionTokens <= 256);
      }, true);
    });
    await scene("F01", "Inspect the saved Prompt's actual YAML and version.", async () => {
      const prompt = await session.get("prompts", foundation.report.promptV2.metadata.name);
      assert.equal(prompt.metadata.uid, foundation.report.promptV2.metadata.uid);
      verifyIntent(prompt, foundation.report.promptV2);
      await yaml("prompts", prompt.metadata.name);
    });
    await scene("F02", "Inspect the already completed pinned AgentJob, without replay.", async () => {
      const job = await inspect("agentjobs", foundation.report.job);
      assert(job.spec.agentRef.name && job.spec.agentRevision > 0);
      assert.equal(job.status.output, foundation.report.job.status.output);
      await yaml("agentjobs", job.metadata.name);
    });
    const probe = expected => governance.report.probes.find(item => item.expected === expected);
    await scene("G01", "Inspect the original missing-grant and exact allowed runs.", async () => {
      await inspect("workflowruns", probe("ungranted").run);
      await inspect("workflowruns", probe("allowed").run);
    });
    await scene("G02", "Inspect argument-constraint and expired-grant failures.", async () => {
      await inspect("workflowruns", probe("arguments").run);
      await inspect("workflowruns", probe("expired").run);
    });
    } else {
      assert.equal(previous.report.calls.length, 2);
      assert.equal(previous.report.scenes.at(-1).id, "G03");
      assert.equal(previous.report.scenes.at(-1).passed, false);
      assert(previous.report.scenes.slice(0, -1).every(item => item.passed === true));
      const current = await session.get("agents", desired.agent.metadata.name);
      const originalUID = previous.report.created.find(item => item.kind === "Agent").uid;
      assert.equal(current.metadata.uid, originalUID);
      assert.equal(current.spec.instructions.inline, INSTRUCTIONS[1]);
      session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
        sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false,
        readOnlyContinuation: true, originalFreshCallsRetained: 2 };
      session.persist();
    }
    await scene("G03", "Inspect rejection and insufficient-quorum expiry.", async () => {
      await inspect("workflowruns", governance.report.rejectedGate);
      await inspect("workflowruns", governance.report.expiredQuorum.run);
    });
    const run = variant => evaluation.report.runs.find(item => item.variant === variant).run;
    await scene("E01", "Inspect actual dataset and Evaluation YAML.", async () => {
      await yaml("evaluationdatasets", run("good").spec.datasetRef.name);
      await yaml("evaluations", run("good").spec.evaluationRef.name);
    });
    await scene("E02", "Inspect preserved configuration failure and corrected pass.", async () => {
      await inspect("evaluationruns", run("configuration"));
      const good = await inspect("evaluationruns", run("good"));
      verifyEvaluationOutcome(good, "good", good.spec.datasetRef.name);
    });
    await scene("E03", "Inspect the distinct existing manual EvaluationRun.", async () => {
      const manual = await inspect("evaluationruns", run("manual"));
      verifyEvaluationOutcome(manual, "manual", manual.spec.datasetRef.name);
    });
    await scene("E04", "Inspect the failed candidate and unchanged-check repair.", async () => {
      const bad = await inspect("evaluationruns", run("bad"));
      verifyEvaluationOutcome(bad, "bad", bad.spec.datasetRef.name);
      const fixed = await inspect("evaluationruns", run("fixed"));
      verifyEvaluationOutcome(fixed, "fixed", fixed.spec.datasetRef.name);
    });
    await scene("E05", "Inspect required missing-metric failure.", async () => {
      const missing = await inspect("evaluationruns", run("metric"));
      verifyEvaluationOutcome(missing, "metric", missing.spec.datasetRef.name);
    });
    await scene("E06", "Inspect actual judge outcomes and healthy restoration.", async () => {
      await inspect("evaluationruns", closure.report.unproducedMalformedOutput.run);
      await inspect("evaluationruns", closure.report.settledRuntimeFailure.run);
      const judge = await session.get("agents", closure.report.restoredJudge.metadata.name);
      assert.equal(judge.metadata.uid, closure.report.restoredJudge.metadata.uid);
      assert.equal(judge.spec.runtime.maxTokens, 256);
    });
    await scene("S01", "Inspect explicit-target Solution YAML and its original owned job.", async () => {
      await yaml("solutions", release.report.promotedCandidate.solution.metadata.name);
      await inspect("agentjobs", release.report.originalExecutionAfterPromotion);
    });
    await scene("S02", "Inspect exact blocked and passing Solution candidate runs.", async () => {
      const solution = await session.get("solutions", release.report.promotedCandidate.solution.metadata.name);
      const definition = await session.get("evaluations", release.report.promotedCandidate.run.spec.evaluationRef.name);
      const failed = await inspect("evaluationruns", release.report.blockedCandidate.run);
      verifyGate(failed, solution, definition, 2, false);
      const promoted = await inspect("evaluationruns", release.report.promotedCandidate.run);
      verifyGate(promoted, solution, definition, 3, true);
    });
    await scene("S03", "Verify current active revision and retained evidence.", async () => {
      const solution = await session.get("solutions", release.report.promotedCandidate.solution.metadata.name);
      assert.equal(solution.metadata.uid, release.report.promotedCandidate.solution.metadata.uid);
      assert.equal(solution.status.activeRevision, 3);
      await yaml("solutions", solution.metadata.name);
    });
    assert.equal(session.report.calls.length, previous ? 0 : 2);
    assert.equal(session.report.created.filter(item => item.kind === "Agent").length, previous ? 0 : 1);
    session.report.quotaAfter = await session.get("cognitivequotas", "tutorial-budget");
    assert(session.report.quotaAfter.status.tokenUsage.totalTokens < 100000);
  });
}

if (require.main === module) runPaced(parseContinuationOptions(process.argv.slice(2))).catch(error => {
  console.error(error.message); process.exitCode = 1;
});
module.exports = { POLICY, NARRATION, fixtures, verifySnapshot, runPaced };
