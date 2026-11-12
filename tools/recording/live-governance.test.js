// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { assertResourceScope, verifyStartedWorkflow } = require("./live-production-session");
const { CONTENT, POLICY, fixtures, verifyGrantOutcome, runGovernance } = require("./live-governance");

const runId = "rancher-gov-unit-a1";

test("governance fixtures affect only a fresh namespaced alias and exact narrow grants", () => {
  const objects = fixtures(runId);
  for (const object of Object.values(objects)) {
    assertResourceScope(object, { ...POLICY, runId, target: RANCHER_TARGET });
    assert.equal(object.metadata.namespace, "tutorial-support");
  }
  assert.equal(objects.tool.spec.endpoint, "http://story-integrations.tutorial-support.svc.cluster.local:8081/publish");
  assert.deepEqual(objects.grant.spec.agentRef, { name: objects.actor.metadata.name });
  assert.equal(objects.actor.spec.replicas, 0);
  assert.equal(objects.actor.spec.runtime.maxTokens, 128);
  assert.deepEqual(objects.grant.spec.toolRef, { name: objects.tool.metadata.name });
  assert.deepEqual(objects.grant.spec.verbs, ["invoke"]);
  assert.equal(objects.grant.spec.argumentConstraints.properties.content.const, CONTENT);
  assert.equal(objects.grant.spec.argumentConstraints.additionalProperties, false);
  assert.equal(objects.flow.spec.steps[0].retry.maxAttempts, 1);
  assert.equal(objects.quorum.spec.approvalGates[0].requiredApprovers, 2);
  assert.equal(objects.quorum.spec.approvalGates[0].timeout, "60s");
  assert.throws(() => fixtures("../escape"));
  assert.throws(() => fixtures("rancher-gov-" + "x".repeat(40)));
});

function runFixture(phase, output = "") {
  return {
    apiVersion: "kognition.dev/v1alpha1", kind: "WorkflowRun",
    metadata: { name: "fresh-run", namespace: "tutorial-support", uid: "run-uid" },
    status: { phase, stepStatuses: [{ name: "publish", attempts: 1, output }] },
  };
}

test("denial qualification requires a terminal failure, exact UID-scoped reason and zero receiver effects", () => {
  const run = runFixture("Failed");
  const events = [{ reason: "ToolStepFailed", involvedObject: { uid: run.metadata.uid },
    message: 'no ToolGrant permits agent "support" to invoke tool "isolated"' }];
  const receipt = { runID: "fresh-publication", publicationCount: 0 };
  verifyGrantOutcome(run, events, receipt, "ungranted", receipt.runID);
  assert.throws(() => verifyGrantOutcome(run, [], receipt, "ungranted", receipt.runID));
  assert.throws(() => verifyGrantOutcome(run, [{ ...events[0], involvedObject: { uid: "different-run" } }],
    receipt, "ungranted", receipt.runID));
  assert.throws(() => verifyGrantOutcome(run, events, { ...receipt, publicationCount: 1 }, "ungranted", receipt.runID));
  assert.throws(() => verifyGrantOutcome({ ...run, status: { ...run.status, phase: "Pending" } },
    events, receipt, "ungranted", receipt.runID));
  assert.throws(() => verifyGrantOutcome(run, events, receipt, "arguments", receipt.runID));
});

test("allowed execution requires the actual exact single publication, not just Ready metadata", () => {
  const receipt = { runID: "fresh-publication", publicationCount: 1, content: CONTENT };
  const run = runFixture("Ready", JSON.stringify(receipt));
  verifyGrantOutcome(run, [], receipt, "allowed", receipt.runID);
  assert.throws(() => verifyGrantOutcome(run, [], { ...receipt, content: "changed" }, "allowed", receipt.runID));
  assert.throws(() => verifyGrantOutcome(run, [], { ...receipt, publicationCount: 2 }, "allowed", receipt.runID));
  run.status.stepStatuses[0].attempts = 2;
  assert.throws(() => verifyGrantOutcome(run, [], receipt, "allowed", receipt.runID));
});

test("Studio-generated workflow invocation must match its exact source, input, namespace and fresh identity", () => {
  const workflow = fixtures(runId).flow;
  const input = "One bounded invocation.";
  const parameters = { publicationID: "fresh", content: CONTENT };
  const run = { apiVersion: "kognition.dev/v1alpha1", kind: "WorkflowRun",
    metadata: { name: `${workflow.metadata.name}-studio-12345`, namespace: "tutorial-support", uid: "fresh-run" },
    spec: { workflowRef: { name: workflow.metadata.name }, input, parameters } };
  verifyStartedWorkflow(run, workflow, input, parameters, "tutorial-support");
  assert.throws(() => verifyStartedWorkflow({ ...run, metadata: { ...run.metadata, uid: "" } },
    workflow, input, parameters, "tutorial-support"));
  assert.throws(() => verifyStartedWorkflow(run, workflow, "changed", parameters, "tutorial-support"));
  assert.throws(() => verifyStartedWorkflow(run, workflow, input, parameters, "default"));
  const empty = { ...run, spec: { workflowRef: { name: workflow.metadata.name }, input } };
  verifyStartedWorkflow(empty, workflow, input, {}, "tutorial-support");
  assert.throws(() => verifyStartedWorkflow(empty, workflow, input, { unexpected: "required" }, "tutorial-support"));
  assert.throws(() => verifyStartedWorkflow({ ...empty, spec: { ...empty.spec, approval: "bypass" } },
    workflow, input, {}, "tutorial-support"));
});

test("governance execution refuses missing live-write consent before preparing resources", async () => {
  await assert.rejects(runGovernance({ runtime: "runtime", output: "unused-governance",
    runId, target: RANCHER_TARGET }), /authorize-live-writes/);
});
