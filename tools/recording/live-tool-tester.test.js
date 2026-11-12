// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET } = require("./live-authoring");
const { verifyToolFixture, verifySchemaEvidence, verifyToolResult, verifyToolFailure, isWelcomeDismissal, runToolTester } = require("./live-tool-tester");

test("programmatic execution requires actual boolean live-write consent before preparing evidence or calling a service", async () => {
  for (const consent of [undefined, false, "true", "false", 1]) {
    await assert.rejects(runToolTester({ runtime: "runtime", output: "unused-tool-recording-test",
      runId: "test-consent", authorizedLiveWrites: consent }), /authorize-live-writes/);
  }
});

test("the recording-only profile permits one exact welcome dismissal without permitting resource mutations", () => {
  const url = "http://127.0.0.1:18081/api/studio/tutorial-dismiss";
  assert.equal(isWelcomeDismissal("POST", url, {}, 0), true);
  assert.equal(isWelcomeDismissal("POST", url, {}, 1), false);
  assert.equal(isWelcomeDismissal("DELETE", url, {}, 0), false);
  assert.equal(isWelcomeDismissal("POST", url + "?namespace=default", {}, 0), false);
  assert.equal(isWelcomeDismissal("POST", url, { reset: true }, 0), false);
  assert.equal(isWelcomeDismissal("POST", "http://127.0.0.1:18081/api/resources/agents", {}, 0), false);
});

function fixture() {
  return { kind: "Tool", metadata: { name: "story-calculator", namespace: "tutorial-support",
    uid: "d34aea73-9d0d-47c6-8398-885adb75961e", labels: { "kognition.dev/story-fixture": "true" } },
  spec: { className: "story-http", protocol: "OpenAPI",
    endpoint: "http://story-integrations.tutorial-support.svc.cluster.local:8080/calculator",
    sandbox: { timeout: "20s" }, inputSchema: { type: "object",
      properties: { expression: { type: "string" } }, required: ["expression"] } },
  status: { phase: "Ready", observedDigest: "sha256:" + "a".repeat(64) } };
}

test("independent testing binds the exact existing calculator rather than adopting another Tool", () => {
  verifyToolFixture(fixture());
  for (const mutate of [
    tool => { tool.metadata.uid = "replacement"; },
    tool => { tool.metadata.namespace = "another-workspace"; },
    tool => { tool.metadata.deletionTimestamp = "2026-11-03T00:00:00Z"; },
    tool => { tool.spec.endpoint = "http://another-host/calculator"; },
    tool => { tool.spec.sandbox.timeout = "5m"; },
    tool => { tool.status.phase = "Pending"; },
  ]) {
    const tool = fixture();
    mutate(tool);
    assert.throws(() => verifyToolFixture(tool));
  }
});

test("Rancher calculator inspection requires its separately reviewed UID", () => {
  const rancher = fixture();
  rancher.metadata.uid = "b3aade98-6097-4153-9906-3f399bc2cac0";
  verifyToolFixture(rancher, RANCHER_TARGET);
  assert.throws(() => verifyToolFixture(rancher));
  assert.throws(() => verifyToolFixture(fixture(), RANCHER_TARGET));
  assert.throws(() => verifyToolFixture(rancher, "unreviewed"));
  rancher.metadata.uid = "replacement";
  assert.throws(() => verifyToolFixture(rancher, RANCHER_TARGET));
});

test("declared schema fallback retains honest provenance and cannot impersonate live introspection", () => {
  const tool = fixture();
  verifySchemaEvidence({ source: "declared", inputSchema: tool.spec.inputSchema, warning: "Endpoint has no live schema." }, tool);
  verifySchemaEvidence({ source: "live", inputSchema: tool.spec.inputSchema }, tool);
  assert.throws(() => verifySchemaEvidence({ inputSchema: tool.spec.inputSchema }, tool));
  assert.throws(() => verifySchemaEvidence({ source: "declared",
    inputSchema: { ...tool.spec.inputSchema, required: [] } }, tool));
});

test("operator result evidence requires exact echoed input, numeric391 and actual execution metadata", () => {
  const result = { output: { expression: "17 * 23", result: 391 }, protocol: "OpenAPI",
    executionMode: "sandboxed", durationMs: 2 };
  verifyToolResult(result, { expression: "17 * 23" });
  verifyToolResult({ ...result, output: JSON.stringify(result.output) }, { expression: "17 * 23" });
  for (const change of [
    { output: { expression: "17 * 23", result: 390 } },
    { output: { expression: "17 * 23", result: 391, error: "Calculation failed" } },
    { error: "Unavailable" }, { protocol: "shell" }, { executionMode: "" }, { durationMs: -1 },
  ]) assert.throws(() => verifyToolResult({ ...result, ...change }, { expression: "17 * 23" }));
  assert.throws(() => verifyToolResult(result, { expression: "17 + 23" }));
});

test("the fixture's explicit HTTP200 application error remains distinct from transport errors and success", () => {
  const result = { protocol: "OpenAPI", durationMs: 55, executionMode: "sandbox",
    output: { expression: "17 *", error: "SyntaxError: invalid syntax (<unknown>, line1)" } };
  const evidence = verifyToolFailure(200, result, { expression: "17 *" });
  assert.equal(evidence.kind, "application");
  assert.equal(evidence.httpStatus, 200);
  assert.equal(evidence.message, result.output.error);
  assert.equal(verifyToolFailure(502, { protocol: "OpenAPI", durationMs: 4, error: "Connection failed" },
    { expression: "17 *" }).kind, "transport");
  assert.throws(() => verifyToolFailure(200, { ...result, output: { ...result.output, result: 391 } }, { expression: "17 *" }));
  assert.throws(() => verifyToolFailure(200, { ...result, output: { expression: "17 *", response: "Failed" } }, { expression: "17 *" }));
  assert.throws(() => verifyToolFailure(200, result, { expression: "17 * 23" }));
});
