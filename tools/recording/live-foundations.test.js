// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET, RANCHER_APPROVED } = require("./live-authoring");
const { assertResourceScope, recordingNamespaces } = require("./live-production-session");
const { ANSWER, POLICY, fixtures, verifyPromptInvocation, runtimeInstructions, verifyInlineDelivery, runFoundations } = require("./live-foundations");

test("foundation resources remain fresh, labeled, namespaced, bounded and separately authorized", () => {
  const runId = "rancher-foundation-unit-a1";
  const objects = fixtures(runId);
  for (const object of Object.values(objects)) {
    assertResourceScope(object, { ...POLICY, runId, target: RANCHER_TARGET });
    assert.equal(object.metadata.namespace, "tutorial-support");
    assert(object.metadata.name.startsWith("full-rancher-foundation-"));
  }
  assert.deepEqual(objects.agent.spec.instructions, { ref: { name: objects.prompt.metadata.name } });
  assert.equal(objects.agent.spec.runtime.maxTokens, 256);
  assert.equal(objects.agent.spec.replicas, 1);
  assert.equal(objects.grant.spec.agentRef.name, objects.agent.metadata.name);
  assert.match(objects.prompt.spec.parameters[0].description, /does not claim parameter rendering/);
  assert.throws(() => fixtures("../escape"));
  assert.throws(() => assertResourceScope(objects.prompt, { ...POLICY, runId: "another-take", target: RANCHER_TARGET }));
});

test("production namespace selection preserves exact Rancher identities and rejects unknown targets", () => {
  assert.equal(recordingNamespaces(RANCHER_TARGET)["tutorial-support"], RANCHER_APPROVED.namespaceUID);
  assert.notEqual(recordingNamespaces()["tutorial-support"], RANCHER_APPROVED.namespaceUID);
  assert.throws(() => recordingNamespaces("unreviewed"));
  const object = fixtures("rancher-foundation-unit-a1").prompt;
  assert.throws(() => assertResourceScope(object, { ...POLICY, runId: "rancher-foundation-unit-a1", namespace: "default" }));
});

test("Prompt delivery evidence requires actual exact output and bounded completion usage", () => {
  verifyPromptInvocation({ response: ANSWER, usage: { completionTokens: 23 }, toolCallsDetail: [] });
  assert.throws(() => verifyPromptInvocation({ response: "different", usage: { completionTokens: 23 } }));
  assert.throws(() => verifyPromptInvocation({ response: ANSWER, usage: { completionTokens: 257 } }));
  assert.throws(() => verifyPromptInvocation({ response: ANSWER, usage: { completionTokens: 23 }, toolCallsDetail: [{}] }));
});

test("a persisted Prompt reference cannot stand in for actual inline instruction delivery", () => {
  const deployment = { spec: { template: { spec: { containers: [{ name: "agent", env: [] }] } } } };
  assert.equal(runtimeInstructions(deployment), undefined);
  assert.throws(() => verifyInlineDelivery(deployment, "literal instructions"), /explicit inline instructions/);
  deployment.spec.template.spec.containers[0].env.push({ name: "KOGNITION_SYSTEM_PROMPT", value: "literal instructions" });
  verifyInlineDelivery(deployment, "literal instructions");
  assert.throws(() => verifyInlineDelivery(deployment, "different instructions"));
});

test("foundation execution refuses missing live-write consent before writing or contacting a backend", async () => {
  await assert.rejects(runFoundations({ runtime: "runtime", output: "unused-foundations",
    runId: "rancher-foundation-unit-a1", target: RANCHER_TARGET }), /authorize-live-writes/);
});
