// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { assertResourceScope } = require("./live-production-session");
const { POLICY, fixtures, verifySnapshot, runPaced } = require("./live-paced-tutorial");

test("paced opening creates only one fresh bounded Agent and its exact model grant", () => {
  const runId = "rancher-paced-unit";
  const desired = fixtures(runId);
  for (const item of Object.values(desired)) assertResourceScope(item, { ...POLICY, runId, target: RANCHER_TARGET });
  assert.equal(desired.agent.spec.runtime.maxTokens, 256);
  assert.equal(desired.agent.spec.replicas, 1);
  assert.equal(desired.grant.spec.agentRef.name, desired.agent.metadata.name);
  assert.deepEqual(POLICY.kinds, ["Agent", "ModelGrant"]);
  assert.throws(() => fixtures("../escape"));
});

test("historical inspection requires exact UID, spec, namespace and terminal phase", () => {
  const recorded = { kind: "EvaluationRun", metadata: { name: "recorded", uid: "original", namespace: "tutorial-support" },
    spec: { targetRef: { revision: 3 } }, status: { phase: "Ready" } };
  verifySnapshot(structuredClone(recorded), recorded);
  for (const change of [
    item => { item.metadata.uid = "replacement"; },
    item => { item.metadata.namespace = "different"; },
    item => { item.spec.targetRef.revision = 2; },
    item => { item.status.phase = "Running"; },
  ]) {
    const current = structuredClone(recorded); change(current);
    assert.throws(() => verifySnapshot(current, recorded));
  }
});

test("paced recording refuses missing consent before interacting with the backend", async () => {
  await assert.rejects(runPaced({ runtime: "runtime", output: "unused-paced", runId: "rancher-paced-unit",
    target: RANCHER_TARGET }), /authorize-live-writes/);
});
