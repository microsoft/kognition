// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { expect } = require("@playwright/test");
const { BASELINE_V3_TARGET, BASELINE_V4_TARGET } = require("./live-authoring");
const { runProduction, verifyIntent, WORKFLOW_UI_V1, PLURALS,
  parseContinuationOptions, readContinuation } = require("./live-production-session");
const { expandBuilderObjects, builderTypingOptions, verifyLessonQuota,
  fixtures: lessonFixtures } = require("./live-instructional");
const { verifyKubernetesRuntime } = require("./live-pilot");

const POLICY = Object.freeze({ batch: "trust-evolution", uiRevision: WORKFLOW_UI_V1.revision,
  naturalTyping: true, manifestPreviewSeconds: 4, observedStatusPreviewSeconds: 4,
  kinds: ["Agent", "ModelGrant", "AgentRole", "AgentRoleBinding", "ToolGrant", "TrustProfile", "Workflow"] });
const INPUT = "Reply exactly Ready. Do not call tools.";
const DENIED_INPUT = "Call full_lesson_calculator exactly once with expression 17 * 24. If access is denied, report the denial and do not retry.";
const NARRATION = Object.freeze({
  AU01: "AutonomyClass supervised requires approval for Tool use, Memory writes and external communication. Grants authorize access to a resource; RuntimeApproval authorizes one bound action. Workflow approval gates control a step transition. These are separate decisions.",
  TE01: "We create disposable Agent full-trust-actor for this trust experiment. ModelGrant full-trust-model permits its Model. AgentRole and AgentRoleBinding full-trust-capabilities grant Tool verbs. ToolGrant full-trust-calculator permits only the expression seventeen times twenty-three.",
  TE02: "TrustProfile full-trust-profile binds this exact Agent incarnation. Its initial score is configured, not earned. We use one-minute audit evaluation, at least one observed action and a one-minute stabilization period. The assisted threshold is fifty, matching the built-in score boundary. Automatic promotion is capped at assisted for this disposable example.",
  TE03: "A successful Model response provides real audit evidence. The controller computes the observed scoring dimensions and records score history. After the configured stabilization period, the profile promotes from supervised to assisted. Permission grants still apply.",
  TE04: "Workflow full-trust-denied-action submits a direct Tool step as the disposable Agent, requesting seventeen times twenty-four. Its ToolGrant does not allow that expression. The request fails before calculator execution. The controller consumes the recorded policy violation and demotes the profile to supervised.",
  TE05: "We now require review for subsequent promotion. New successful evidence leads to a pending AutonomyReview, while the Agent remains supervised. This Kubernetes build has no authenticated review-decision endpoint, so this tutorial does not claim an approved review.",
  TE06: "With no reviewer decision, the five-minute review deadline expires. The controller records Denied, decided by system slash TTL, and keeps the Agent supervised. The score history and incarnation binding remain inspectable. We stop this disposable experiment without changing the support assistant.",
});

function fixtures(runId, agentUID) {
  assert(/^isolated-trust-[a-z0-9-]+$/.test(runId) && runId.length <= 30, "Use a bounded isolated-trust-* ID");
  const resource = (kind, suffix, spec) => ({ apiVersion: "kognition.dev/v1alpha1", kind,
    metadata: { name: `full-trust-${suffix}`, namespace: "tutorial-support",
      labels: { "kognition.dev/recording-run": runId } }, ...(spec ? { spec } : {}) });
  const agent = resource("Agent", "actor", {
    modelRef: { name: "tutorial-support-model" },
    instructions: { inline: "This disposable trust experiment handles only fictional data. Follow the current request. Call full_lesson_calculator only when explicitly requested, exactly once. Never retry a denied request. Report actual results or access denial without inventing success." },
    runtime: { class: "default", timeout: "120s", maxTokens: 192 }, replicas: 1,
    tools: [{ ref: { name: "full-lesson-calculator" }, required: true }],
  });
  const modelGrant = resource("ModelGrant", "model", { agentRef: { name: agent.metadata.name },
    modelRef: { name: "tutorial-support-model" } });
  const role = resource("AgentRole", "capabilities");
  role.rules = [{ resources: ["Tool"], verbs: ["get", "invoke", "discover", "inspect"] }];
  const binding = resource("AgentRoleBinding", "capabilities", { roleRef: { name: role.metadata.name },
    subjects: [{ kind: "Agent", name: agent.metadata.name }] });
  const grant = resource("ToolGrant", "calculator", { agentRef: { name: agent.metadata.name },
    toolRef: { name: "full-lesson-calculator" }, verbs: ["invoke", "discover", "inspect"],
    argumentConstraints: { type: "object", properties: { expression: { const: "17 * 23" } },
      required: ["expression"], additionalProperties: false } });
  const profile = resource("TrustProfile", "profile", { agentRef: { name: agent.metadata.name },
    agentIncarnationUID: agentUID, initialScore: 10, initialClass: "supervised", maxClass: "assisted",
    evaluationInterval: "1m",
    promotionPolicy: { minActions: 1, minScoreDuration: "1m", thresholds: [{ targetClass: "assisted", minScore: 50 }] },
    demotionPolicy: { mode: "immediate", fallbackClass: "supervised" } });
  const denialWorkflow = resource("Workflow", "denied-action", {
    steps: [{ name: "blocked-calculation", type: "tool", agentRef: { name: agent.metadata.name },
      toolRef: { name: "full-lesson-calculator" }, input: '{"expression":"17 * 24"}',
      timeout: "20s", retry: { maxAttempts: 1 } }], maxConcurrentRuns: 1, historyLimit: 5,
  });
  return { agent, modelGrant, role, binding, grant, profile, denialWorkflow };
}

function verifyTransition(profile, initial, targetClass, condition) {
  assert.equal(profile.metadata.uid, initial.metadata.uid);
  assert.equal(profile.spec.agentIncarnationUID, initial.spec.agentIncarnationUID);
  assert.equal(profile.spec.initialScore, 10);
  assert.equal(profile.status.currentClass, targetClass);
  assert(Number.isFinite(profile.status.currentScore) && profile.status.currentScore >= 0 && profile.status.currentScore <= 100);
  assert(profile.status.auditedActions >= 1);
  assert(profile.status.totalEvaluations >= 1);
  assert(profile.status.scoreHistory.some(entry => entry.reason?.startsWith("audit:") && entry.dimensions));
  assert(profile.status.conditions.some(entry => entry.type === condition && entry.status === "True"));
}

function verifyExpiredReview(review, pending, profile) {
  assert.equal(review.metadata.uid, pending.metadata.uid);
  assert.deepEqual(review.spec, pending.spec);
  assert.equal(review.spec.trustProfileRef, profile.metadata.name);
  assert.equal(review.status.decision, "Denied");
  assert.equal(review.status.decidedBy, "system/ttl");
  assert.equal(review.status.reason, "TTL expired");
  assert(new Date(review.status.decidedAt) >= new Date(pending.status.expiresAt));
  assert.equal(profile.status.currentClass, "supervised");
}

function verifyDeniedTrustDecision(decision, agent, tool, sessionID) {
  assert.equal(decision.plane, "tool");
  assert.equal(decision.outcome, "denied");
  assert.equal(decision.reasonCode, "argument_constraints_denied");
  assert.equal(decision.identity?.agentIdentity, `spiffe://kognition.dev/workspace/tutorial-support/agent/${agent}`);
  assert.equal(decision.session?.sessionId, sessionID);
  assert.equal(decision.request.targetRef.name, tool);
  assert.equal(decision.authorization.grant, "denied");
  assert.equal(decision.execution.targetReached, false);
}

function verifyConfigurationUnchanged(live, previous) {
  verifyIntent(live, previous);
  for (const field of ["generation", "labels", "annotations", "ownerReferences", "finalizers", "deletionTimestamp"]) {
    assert.deepEqual(live.metadata[field], previous.metadata[field], `Concurrent configuration changed metadata.${field}`);
  }
}

async function runTrustEvolution(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    "--target", input.target, ...(input.authorizedLiveWrites ? ["--authorize-live-writes"] : []),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : [])]);
  assert([BASELINE_V3_TARGET, BASELINE_V4_TARGET].includes(options.target));
  const fixedTarget = options.target === BASELINE_V4_TARGET;
  assert.equal(path.dirname(options.output).toLowerCase(), options.runtime.toLowerCase());
  assert(path.basename(options.output).startsWith("trust-evolution-"));
  const desired = fixtures(options.runId);
  const previous = readContinuation(options, POLICY.batch, "trust-evolution-");
  const policy = { ...POLICY, uiRevision: fixedTarget ? "create-picker-v1" : POLICY.uiRevision,
    kinds: fixedTarget ? [...POLICY.kinds, "Tool"] : POLICY.kinds };
  return runProduction(options, policy, async session => {
    const page = session.page;
    session.report.qualifications = {
      disposableAgentOnly: true, mainAssistantAutonomyChanged: false,
      patchedTrustStatus: false, submittedReviewDecisions: 0,
      kubernetesReviewDecisionEndpointAvailable: false,
      approvedReviewDemonstrated: false, independentHumanReviewDemonstrated: false,
    };
    const quota = await session.get("cognitivequotas", "tutorial-budget");
    verifyLessonQuota(quota, options.target, true);
    session.report.quotaBefore = quota;
    const restored = {};
    let recoveredPromotion = false;
    if (previous) {
      const chain = [previous];
      if (fixedTarget) {
        assert.equal(previous.report.continuation, undefined, "Repeated repaired-target recovery requires review");
        assert.deepEqual(previous.report.scenes.filter(scene => scene.passed).map(scene => scene.id),
          ["AU01", "TE01", "TE02", "TE03"]);
        assert.equal(previous.report.calls.length, 1);
        assert.equal(previous.report.calls[0].request.message, INPUT);
        assert.match(previous.report.failure, /object has been modified/);
        recoveredPromotion = true;
      } else if (previous.report.continuation) {
        const parent = readContinuation({ ...options, continuationPath: previous.report.continuation.reportPath },
          POLICY.batch, "trust-evolution-");
        assert.equal(parent.reportSHA256, previous.report.continuation.reportSHA256);
        assert.equal(parent.report.continuation, undefined, "Only the two settled trust attempts may be continued");
        chain.unshift(parent);
        assert.deepEqual(previous.report.scenes.filter(scene => scene.passed).map(scene => scene.id), ["TE02", "TE03"]);
        assert.equal(previous.report.calls.length, 1);
        assert.equal(previous.report.calls[0].request.message, DENIED_INPUT);
        assert.match(previous.report.calls[0].toolCallsDetail[0].result, /403 Forbidden/);
        recoveredPromotion = true;
      } else {
        assert.deepEqual(previous.report.scenes.filter(scene => scene.passed).map(scene => scene.id), ["AU01", "TE01", "TE02"]);
        assert.equal(previous.report.calls.length, 1);
        assert.equal(previous.report.calls[0].request.message, INPUT);
        assert(!previous.report.calls[0].toolCallsDetail?.length);
      }
      for (const [key, object] of Object.entries(desired)) {
        const archived = chain.flatMap(entry => entry.report.evidence).filter(entry =>
          entry.resource?.kind === object.kind && entry.resource.metadata.name === object.metadata.name).at(-1)?.resource;
        if (key === "denialWorkflow" && !archived) {
          assert.equal(await session.get("workflows", object.metadata.name, session.namespace, true), null);
          continue;
        }
        assert(archived?.metadata.uid, "Every restored resource requires original creation evidence");
        const live = await session.get(PLURALS[object.kind], object.metadata.name);
        verifyIntent(live, archived);
        restored[key] = live;
      }
      session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
        sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false,
        completedScenes: recoveredPromotion ? ["AU01", "TE01", "TE02", "TE03"] : ["AU01", "TE01"],
        configurationRevision: "Built-in assisted score boundary 50",
        priorCapabilityDenialIsNotAuditedDemotionEvidence: recoveredPromotion };
    } else {
      for (const object of Object.values(desired)) {
        assert.equal(await session.get(PLURALS[object.kind], object.metadata.name, session.namespace, true),
          null, "Trust capture must use fresh resources");
      }
    }
    let tool;
    if (fixedTarget) {
      if (previous) {
        const archived = previous.report.evidence.find(entry => entry.resource?.kind === "Tool")?.resource;
        assert(archived?.metadata.uid);
        tool = await session.get("tools", archived.metadata.name);
        verifyIntent(tool, archived);
      } else {
        tool = lessonFixtures(options.runId.replace("isolated-trust-", "isolated-lesson-")).tool;
        tool.metadata.labels["kognition.dev/recording-run"] = options.runId;
        assert.equal(await session.get("tools", tool.metadata.name, session.namespace, true), null);
      }
    } else {
      const coreFile = path.join(options.runtime, "instructional-v3-20261106-a1", "report.json");
      const coreBytes = fs.readFileSync(coreFile);
      const core = JSON.parse(coreBytes);
      assert(core.scenes.some(scene => scene.id === "T01" && scene.passed));
      tool = core.evidence.find(entry => entry.resource?.kind === "Tool" &&
        entry.resource.metadata.name === "full-lesson-calculator")?.resource;
      assert(tool?.metadata.uid);
      verifyIntent(await session.get("tools", tool.metadata.name), tool);
      session.report.existingCalculator = { uid: tool.metadata.uid, reportSHA256: crypto.createHash("sha256").update(coreBytes).digest("hex") };
    }
    const type = async (control, value) => {
      const startSeconds = session.seconds();
      await control.scrollIntoViewIfNeeded();
      await control.fill("");
      await control.pressSequentially(String(value), builderTypingOptions(value));
      (session.report.typingIntervals ||= []).push({ kind: "builder", startSeconds, endSeconds: session.seconds(),
        characters: String(value).length, playbackSpeed: 1 });
    };
    const set = async (key, value) => {
      await expandBuilderObjects(page);
      const control = page.locator(`#builder-host [data-path="${key}"]`);
      await expect(control).toHaveCount(1);
      if (await control.evaluate(element => element.tagName) === "SELECT") await control.selectOption(String(value));
      else await type(control, value);
    };
    const build = async (object, fields, arrays = []) => {
      await session.click(page.locator("#btn-create"));
      await session.click(page.locator(`#btn-new-${object.kind.toLowerCase()}`));
      await session.click(page.locator('[data-view="builder"]'));
      const all = page.getByRole("button", { name: "All API fields", exact: true });
      if (await all.isVisible()) await all.click();
      await set("metadata.name", object.metadata.name);
      await set("metadata.namespace", session.namespace);
      const labels = page.locator("#builder-host details.schema-object").filter({
        has: page.locator("summary").filter({ hasText: /^Labels$/ }) }).first();
      await type(labels.getByRole("textbox", { name: "Labels entry key", exact: true }), "kognition.dev/recording-run");
      await labels.getByRole("button", { name: "Add entry", exact: true }).click();
      await set("metadata.labels.kognition.dev/recording-run", options.runId);
      for (const [key, value] of Object.entries(fields)) await set(key, value);
      for (const [label, items] of arrays) {
        for (const fields of items) {
          await expandBuilderObjects(page);
          await page.getByRole("button", { name: `Add ${label} item`, exact: true }).click();
          for (const [key, value] of Object.entries(fields)) await set(key, value);
        }
      }
      await session.click(page.locator('[data-view="yaml"]'));
      await page.waitForTimeout(4000);
      return session.deployBuiltResource(object);
    };
    const narration = { ...NARRATION, ...(fixedTarget ? {
      TE01: "We configure a calculator Tool for disposable Agent full-trust-actor. ModelGrant full-trust-model permits its Model. AgentRole and AgentRoleBinding full-trust-capabilities grant Tool verbs. ToolGrant full-trust-calculator permits only the expression seventeen times twenty-three.",
      TE04: "Subsequent promotion now requires review. The Agent requests seventeen times twenty-four, which its ToolGrant does not allow. The gateway records a denied decision without reaching the calculator. The controller consumes this real policy violation and demotes the profile to supervised.",
    } : {}) };
    const scene = (id, action) => previous && session.report.continuation.completedScenes.includes(id) ? Promise.resolve() :
      session.scene(id, [], id, narration[id], action);
    let actor = restored.agent, profile = restored.profile, pending;
    const snapshot = async label => {
      const current = await session.get("trustprofiles", desired.profile.metadata.name);
      (session.report.trustSnapshots ||= []).push({ label, seconds: session.seconds(), resource: current });
      session.persist();
      return current;
    };
    const updateConfiguration = async (plural, name, mutate) => {
      for (let attempt = 0; attempt < 2; attempt++) {
        const before = await session.get(plural, name);
        const desired = structuredClone(before);
        mutate(desired.spec);
        try { return await session.save(desired, false); }
        catch (error) {
          if (attempt !== 0 || !/object has been modified/.test(error.message)) throw error;
          const current = await session.get(plural, name);
          verifyConfigurationUnchanged(current, before);
          (session.report.configurationWriteConflicts ||= []).push({ kind: before.kind, name,
            previousVersion: before.metadata.resourceVersion, currentVersion: current.metadata.resourceVersion,
            configurationUnchanged: true, actionsReplayed: false, error: error.message, seconds: session.seconds() });
          session.persist();
        }
      }
      throw new Error("Bounded configuration update exhausted");
    };
    const wait = async (label, action, timeout) => {
      const startSeconds = session.seconds();
      const result = await session.wait(label, action, timeout);
      (session.report.waitIntervals ||= []).push({ label, startSeconds, endSeconds: session.seconds(),
        editQualification: "Elapsed controller time; no message typing or provider output may be cut" });
      return result;
    };
    await scene("AU01", async () => {
      for (const name of ["supervised", "assisted"]) {
        const resource = await session.get("autonomyclasses", name, "");
        (session.report.autonomyClasses ||= []).push(resource);
        await session.select("autonomyclasses", name);
        await session.click(page.locator('[data-view="yaml"]'));
        await page.waitForTimeout(6000);
      }
    });
    await scene("TE01", async () => {
      if (fixedTarget) tool = await session.save(tool);
      await build(desired.modelGrant, { "spec.agentRef.name": desired.agent.metadata.name,
        "spec.modelRef.name": "tutorial-support-model" });
      await session.save(desired.role);
      await build(desired.binding, { "spec.roleRef.name": desired.role.metadata.name },
        [["Subjects", [{ "spec.subjects.0.kind": "Agent", "spec.subjects.0.name": desired.agent.metadata.name }]]]);
      await session.save(desired.grant);
      actor = await build(desired.agent, { "spec.modelRef.name": "tutorial-support-model",
        "spec.instructions.inline": desired.agent.spec.instructions.inline, "spec.runtime.class": "default",
        "spec.runtime.timeout": "120s", "spec.runtime.maxTokens": 192, "spec.replicas": 1 },
      [["Tools", [{ "spec.tools.0.ref.name": tool.metadata.name, "spec.tools.0.required": true }]]]);
      await session.wait("disposable Agent rollout", async () => {
        const live = await session.get("agents", actor.metadata.name);
        const deployment = await session.get("deployments", `agent-${actor.metadata.name}`, session.namespace, true);
        if (!deployment || live.status?.phase !== "Ready" || deployment.status.observedGeneration !== deployment.metadata.generation ||
            !["readyReplicas", "availableReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1)) return false;
        verifyKubernetesRuntime(live, { name: actor.metadata.name, namespace: session.namespace, uid: actor.metadata.uid,
          generation: actor.metadata.generation, deploymentUID: deployment.metadata.uid, replicas: 1, maxTokens: 192 }, deployment);
        actor = live; return true;
      });
    });
    await scene("TE02", async () => {
      desired.profile.spec.agentIncarnationUID = actor.metadata.uid;
      if (previous) {
        await session.select("trustprofiles", profile.metadata.name);
        await session.click(page.locator('[data-view="builder"]'));
        const all = page.getByRole("button", { name: "All API fields", exact: true });
        if (await all.isVisible()) await all.click();
        await set("spec.promotionPolicy.thresholds.0.minScore", 50);
        profile.spec.promotionPolicy.thresholds[0].minScore = 50;
        await session.click(page.locator('[data-view="yaml"]'));
        await page.waitForTimeout(6000);
        profile = await session.deployBuiltResource(profile, false);
        await snapshot("configured-built-in-threshold");
        return;
      }
      profile = await build(desired.profile, {
        "spec.agentRef.name": actor.metadata.name, "spec.agentIncarnationUID": actor.metadata.uid,
        "spec.initialScore": 10, "spec.initialClass": "supervised", "spec.maxClass": "assisted",
        "spec.evaluationInterval": "1m", "spec.promotionPolicy.minActions": 1,
        "spec.promotionPolicy.minScoreDuration": "1m", "spec.demotionPolicy.mode": "immediate",
        "spec.demotionPolicy.fallbackClass": "supervised",
      }, [["Thresholds", [{ "spec.promotionPolicy.thresholds.0.targetClass": "assisted",
        "spec.promotionPolicy.thresholds.0.minScore": 50 }]]]);
      await session.wait("initial trust reconciliation", async () => {
        const live = await session.get("trustprofiles", profile.metadata.name);
        return live.status?.currentClass === "supervised" && live.status.currentScore === 10 ? live : false;
      });
      profile = await snapshot("initial");
      assert.equal(profile.status.currentClass, "supervised");
      assert.equal(profile.status.currentScore, 10);
    });
    await scene("TE03", async () => {
      if (previous) {
        session.report.originalReadinessCall = previous.report.calls[0];
        await session.inspectObserved("trustprofiles", profile.metadata.name, ["audit:"], "observed-readiness-score");
      } else {
        await session.chat(actor.metadata.name, INPUT, body => assert.match(body.response, /Ready/i));
      }
      const promoted = await wait("audit scoring and automatic promotion", async () => {
        const live = await session.get("trustprofiles", profile.metadata.name);
        return live.status?.currentClass === "assisted" ? live : false;
      }, 240000);
      verifyTransition(promoted, profile, "assisted", "Promoted");
      await snapshot("promoted");
      await session.inspectObserved("trustprofiles", profile.metadata.name, ["assisted", "audit:"], "earned-promotion");
    });
    await scene("TE04", async () => {
      assert.equal((await snapshot("before-direct-denial")).status.currentClass, "assisted");
      if (fixedTarget) {
        await updateConfiguration("trustprofiles", profile.metadata.name, spec => {
          spec.promotionPolicy.thresholds[0].requireReview = true;
        });
        const denied = await session.chat(actor.metadata.name, DENIED_INPUT, body => {
          const calls = body.toolCallsDetail?.filter(call => call.name === "full_lesson_calculator");
          assert.equal(calls?.length, 1);
          assert.equal(JSON.parse(calls[0].args).expression, "17 * 24");
          assert.match(calls[0].result, /403 Forbidden/);
        }, true);
        const decisions = await session.wait("persisted attributed denial", async () => {
          const maps = await session.list("configmaps", "kognition.dev/cognitive-audit=true", session.target.systemNamespace);
          const entries = maps.flatMap(map => map.binaryData?.["entries.json"] ?
            JSON.parse(Buffer.from(map.binaryData["entries.json"], "base64").toString("utf8")) : []);
          const matches = entries.map(entry => entry.details).filter(decision => decision?.plane === "tool" &&
            decision.session?.sessionId === denied.sessionID && decision.reasonCode === "argument_constraints_denied");
          return matches.length ? matches : false;
        });
        assert.equal(decisions.length, 1);
        verifyDeniedTrustDecision(decisions[0], actor.metadata.name, tool.metadata.name, denied.sessionID);
        session.report.controlledDeniedSession = denied.sessionID;
        session.report.deniedTrustDecision = decisions[0];
        session.persist();
      } else {
      const flow = await session.save(desired.denialWorkflow);
      const run = await session.startWorkflow(flow, "", {});
      const denied = await wait("one bounded Tool step is denied", async () => {
        const live = await session.get("workflowruns", run.metadata.name);
        return ["Ready", "Failed"].includes(live.status?.phase) ? live : false;
      }, 120000);
      assert.equal(denied.status.phase, "Failed");
      assert.equal(denied.status.stepStatuses[0].attempts, 1);
      const events = (await session.list("events")).filter(event => event.involvedObject?.uid === run.metadata.uid);
      assert(events.some(event => /argument constraints denied/i.test(event.message)));
      session.report.controlledDeniedRun = denied;
      session.report.controlledDeniedEvents = events;
      session.persist();
      await session.inspectObserved("workflowruns", denied.metadata.name, ["Failed"], "denied-tool-step");
      }
      const demoted = await wait("actual policy-violation demotion", async () => {
        const live = await session.get("trustprofiles", profile.metadata.name);
        return live.status?.currentClass === "supervised" && live.status.conditions?.some(c => c.type === "Demoted") ? live : false;
      }, 180000);
      verifyTransition(demoted, profile, "supervised", "Demoted");
      assert(demoted.status.conditions.some(c => c.type === "Demoted" && /critical policy violation/.test(c.reason)));
      await snapshot("demoted");
      await session.inspectObserved("trustprofiles", profile.metadata.name, ["supervised", "Demoted"], "policy-demotion");
    });
    await scene("TE05", async () => {
      const reviewed = await session.get("trustprofiles", profile.metadata.name);
      if (!reviewed.spec.promotionPolicy.thresholds[0].requireReview) {
        await updateConfiguration("trustprofiles", profile.metadata.name, spec => {
          spec.promotionPolicy.thresholds[0].requireReview = true;
        });
      }
      await session.chat(actor.metadata.name, INPUT, body => assert.match(body.response, /Ready/i), true);
      pending = await wait("generated pending promotion review", async () => {
        const reviews = await session.list("autonomyreviews");
        return reviews.find(review => review.spec.trustProfileRef === profile.metadata.name &&
          review.spec.agentRef.name === actor.metadata.name && review.status?.decision === "Pending");
      }, 180000);
      assert.equal(pending.spec.requestedClass, "assisted");
      assert(pending.spec.evidence.startsWith("audit:") && pending.status.expiresAt);
      session.report.pendingReview = pending;
      const live = await snapshot("review-pending");
      assert.equal(live.status.currentClass, "supervised");
      await session.inspectObserved("autonomyreviews", pending.metadata.name, ["Pending", "expiresAt"], "pending-autonomy-review");
    });
    await scene("TE06", async () => {
      const expired = await wait("real five-minute review TTL", async () => {
        const live = await session.get("autonomyreviews", pending.metadata.name);
        return live.status?.decision === "Denied" ? live : false;
      }, 360000);
      const live = await snapshot("review-expired");
      verifyExpiredReview(expired, pending, live);
      session.report.expiredReview = expired;
      await session.inspectObserved("autonomyreviews", expired.metadata.name, ["Denied", "system/ttl", "TTL expired"], "expired-autonomy-review");
      await updateConfiguration("agents", actor.metadata.name, spec => { spec.replicas = 0; });
      await updateConfiguration("trustprofiles", profile.metadata.name, spec => {
        delete spec.evaluationInterval;
        delete spec.promotionPolicy;
        spec.maxClass = "supervised";
      });
      await session.wait("disposable Agent stopped", async () => {
        const deployment = await session.get("deployments", `agent-${actor.metadata.name}`);
        return deployment.spec.replicas === 0 && !deployment.status.replicas ? deployment : false;
      });
      session.report.experimentStopped = true;
    });
    session.report.quotaAfter = await session.get("cognitivequotas", "tutorial-budget");
    assert(session.report.quotaAfter.status.tokenUsage.totalTokens < session.target.tutorialTokenBudget);
    session.report.trustTransitionsVerified = true;
    session.persist();
  });
}

if (require.main === module) runTrustEvolution(parseContinuationOptions(process.argv.slice(2))).catch(error => {
  console.error(error.stack); process.exitCode = 1;
});
module.exports = { POLICY, INPUT, DENIED_INPUT, NARRATION, fixtures, verifyTransition, verifyExpiredReview,
  verifyDeniedTrustDecision, verifyConfigurationUnchanged, runTrustEvolution };
