// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const { expect } = require("@playwright/test");
const { parseOptions, RANCHER_TARGET, recordingTarget } = require("./live-authoring");
const { runProduction } = require("./live-production-session");

const ANSWER = "PROMPT V1: Ask Cedar Labs for the region and timeout rate.";
const POLICY = Object.freeze({
  batch: "prompt-and-job-foundations",
  kinds: ["Prompt", "Agent", "ModelGrant", "AgentJob"],
});

function fixtures(runId) {
  if (!/^rancher-foundation-[a-z0-9-]+$/.test(runId) || runId.length > 35) {
    throw new Error("Foundation takes require a fresh bounded rancher-foundation-* run ID");
  }
  const prefix = `full-${runId}`;
  const envelope = (kind, suffix, spec) => ({
    apiVersion: "kognition.dev/v1alpha1", kind,
    metadata: { name: `${prefix}-${suffix}`, namespace: "tutorial-support",
      labels: { "kognition.dev/recording-run": runId } },
    spec,
  });
  const prompt = envelope("Prompt", "prompt", {
    content: `This is a fictional support response-format exercise. Reply with exactly this sentence: ${ANSWER}`,
    parameters: [{ name: "region", description: "Stored declaration; this take does not claim parameter rendering.",
      required: false, default: "westus3" }],
    metadata: { purpose: "tutorial-literal-prompt-reference" },
  });
  const agent = envelope("Agent", "agent", {
    modelRef: { name: "tutorial-support-model" }, instructions: { ref: { name: prompt.metadata.name } },
    runtime: { class: "default", timeout: "120s", maxTokens: 256 }, replicas: 1,
  });
  const grant = envelope("ModelGrant", "model", {
    agentRef: { name: agent.metadata.name }, modelRef: { name: "tutorial-support-model" },
  });
  return { prompt, agent, grant };
}

function verifyPromptInvocation(body) {
  assert.equal(body.response.trim(), ANSWER);
  assert(body.usage.completionTokens <= 256);
  assert(!body.toolCallsDetail?.length);
}

function runtimeInstructions(deployment) {
  const container = deployment.spec.template.spec.containers.find(item => item.name === "agent");
  assert(container, "The exact Agent runtime container is required");
  return container.env.find(item => item.name === "KOGNITION_SYSTEM_PROMPT")?.value;
}

function verifyInlineDelivery(deployment, expected) {
  assert.equal(runtimeInstructions(deployment), expected, "The runtime did not receive the explicit inline instructions");
}

async function runFoundations(input) {
  const options = parseOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  if (options.target !== RANCHER_TARGET) throw new Error("Foundation production is approved only for the exact Rancher target");
  recordingTarget(options.target);
  const desired = fixtures(options.runId);
  return runProduction(options, POLICY, async session => {
    session.report.coverage = {
      entireScenesCompleted: [], scenes: ["S03", "S06", "S07", "S19"],
      qualifications: [
        "Resources are created through actual YAML parsing and Studio deployment, with independent Kubernetes readback.",
        "The installed frozen Kubernetes dispatcher stores Prompt references but does not inject their content into the native runtime.",
        "Prompt parameters are persisted declarations; no substitution, runtime parameter validation or automatic hot reload is claimed.",
        "The following Agent invocation and AgentJob use an explicit visible switch to inline instructions, not a successful Prompt-reference invocation.",
        "The one-shot AgentJob references an existing Agent and its exact immutable revision; no inline-Agent variant is exercised.",
      ],
    };
    let prompt, agent, promptV1, job;
    await session.scene("F01", ["S06", "S07"], "Author a reusable Prompt and inspect its stored parameter declaration.",
      "A Prompt stores reusable instructions and parameter metadata. We save the actual content through Studio. This scene verifies stored configuration; runtime delivery is checked separately, without assuming template substitution.", async () => {
        prompt = await session.save(desired.prompt);
        promptV1 = await session.wait("Prompt version and hash", async () => {
          const current = await session.get("prompts", prompt.metadata.name);
          return current.status?.version > 0 && current.status.hash ? current : false;
        });
        session.report.promptV1 = promptV1;
      });
    await session.scene("F02", ["S03", "S06"], "Bind the ModelGrant and create an Agent that references the saved Prompt.",
      "The Agent saves a Prompt reference and a separate model permission. This frozen Kubernetes runtime does not receive the referenced instructions. A Ready definition alone does not prove Prompt delivery.", async () => {
        await session.save(desired.grant);
        agent = await session.save(desired.agent);
        await session.wait("physical Prompt-reference runtime", async () => {
          const current = await session.get("agents", agent.metadata.name);
          const deployment = await session.get("deployments", `agent-${agent.metadata.name}`, session.namespace, true);
          if (current.status?.phase !== "Ready" || !deployment ||
              deployment.status?.observedGeneration !== deployment.metadata.generation ||
              !["replicas", "availableReplicas", "readyReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
          assert.equal(runtimeInstructions(deployment), undefined, "The frozen Prompt-reference behavior differs; review before claiming delivery");
          session.report.promptRuntime = { agent: current, deployment };
          session.report.promptReferenceRuntimeDelivery = false;
          agent = current;
          return true;
        });
      });
    await session.scene("F03", ["S05", "S06"], "Explicitly switch to inline instructions before real runtime execution.",
      "We visibly replace the reference with the Prompt's literal content as inline instructions. After verifying the updated runtime, we make one real provider call. This response proves the explicit inline path, not reference delivery.", async () => {
        const inline = await session.get("agents", agent.metadata.name);
        assert.equal(inline.metadata.uid, agent.metadata.uid);
        inline.spec.instructions = { inline: desired.prompt.spec.content };
        const updated = await session.save(inline, false);
        agent = await session.wait("explicit inline runtime revision", async () => {
          const current = await session.get("agents", agent.metadata.name);
          const deployment = await session.get("deployments", `agent-${agent.metadata.name}`);
          if (current.status?.phase !== "Ready" || current.status.revisionCount <= agent.status.revisionCount ||
              deployment.status?.observedGeneration !== deployment.metadata.generation ||
              !["replicas", "availableReplicas", "readyReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
          verifyInlineDelivery(deployment, desired.prompt.spec.content);
          assert.equal(current.metadata.uid, updated.metadata.uid);
          session.report.inlineRuntime = { agent: current, deployment };
          return current;
        });
        await session.chat(agent.metadata.name, "Help Cedar Labs begin fictional incident triage.", verifyPromptInvocation);
      });
    await session.scene("F04", ["S19"], "Launch a one-shot AgentJob pinned to the existing Agent revision.",
      "An AgentJob represents one bounded piece of work. This job keeps the Agent's authorization identity and pins its immutable revision. We inspect its actual terminal output and token usage.", async () => {
        const definition = session.resource("AgentJob", `full-${options.runId}-job`, {
          agentRef: { name: agent.metadata.name }, agentRevision: agent.status.revisionCount,
          completionPolicy: "Success", backoffLimit: 0, input: "Help Cedar Labs begin fictional incident triage.",
        });
        const created = await session.save(definition);
        job = await session.wait("one-shot AgentJob terminal result", async () => {
          const current = await session.get("agentjobs", created.metadata.name);
          if (current.status?.phase === "Failed") throw new Error("The fresh AgentJob failed; it must not be replayed");
          return current.status?.phase === "Ready" ? current : false;
        });
        assert.equal(job.status.output.trim(), ANSWER);
        assert.equal(job.status.succeeded, 1);
        assert(job.status.tokenUsage.totalTokens > 0);
        session.report.job = job;
        await session.select("agentjobs", job.metadata.name);
        await session.click(session.page.locator('[data-view="builder"]'));
        const observed = session.page.locator("#builder-host details").filter({
          has: session.page.locator("summary").filter({ hasText: /^Observed status/ }),
        }).locator("pre");
        await expect(observed).toContainText(ANSWER);
        await session.hold(observed, "actual-AgentJob-terminal-output");
      });
    await session.scene("F05", ["S06", "S07"], "Revise the Prompt without claiming an automatic Agent hot reload.",
      "Changing Prompt content preserves its identity and advances its version and hash. We inspect the independent immutable snapshots. This take does not claim that existing runtimes automatically reload the new content.", async () => {
        const revised = await session.get("prompts", prompt.metadata.name);
        revised.spec.content = revised.spec.content.replace("PROMPT V1:", "PROMPT V2:");
        await session.save(revised, false);
        const current = await session.wait("Prompt next immutable version", async () => {
          const item = await session.get("prompts", prompt.metadata.name);
          return item.status?.version === promptV1.status.version + 1 ? item : false;
        });
        assert.equal(current.metadata.uid, promptV1.metadata.uid);
        assert.notEqual(current.status.hash, promptV1.status.hash);
        const parent = crypto.createHash("sha256").update(`Prompt\0${prompt.metadata.name}`).digest("hex").slice(0, 24);
        const snapshots = await session.list("configmaps", `kognition.dev/revision-parent=${parent},kognition.dev/revision-kind=prompt`);
        assert.equal(snapshots.length, 2);
        assert(snapshots.every(item => item.immutable && item.metadata.ownerReferences?.some(owner => owner.uid === prompt.metadata.uid)));
        session.report.promptV2 = current;
        session.report.promptSnapshots = snapshots;
        await session.select("prompts", prompt.metadata.name);
        await session.click(session.page.locator('[data-view="yaml"]'));
        await expect(session.page.locator("#yaml-area")).toHaveValue(/PROMPT V2:/);
        await session.hold(session.page.locator("#yaml-area"), "revised-Prompt-YAML");
      });
    });
}

if (require.main === module) runFoundations(parseOptions(process.argv.slice(2))).catch(error => {
  console.error(error.message); process.exitCode = 1;
});

module.exports = { ANSWER, POLICY, fixtures, verifyPromptInvocation, runtimeInstructions, verifyInlineDelivery, runFoundations };
