// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { BASELINE_V3_TARGET, BASELINE_V4_TARGET } = require("./live-authoring");
const { assertResourceScope, recordingNamespaces } = require("./live-production-session");
const { verifyLessonQuota } = require("./live-instructional");
const { POLICY, fixtures, verifyTransition, verifyExpiredReview, verifyDeniedTrustDecision,
  verifyConfigurationUnchanged, runTrustEvolution } = require("./live-trust-evolution");

test("trust experiments use a disposable incarnation and narrowly bounded automatic policy", () => {
  const runId = "isolated-trust-unit";
  const desired = fixtures(runId, "actor-incarnation");
  for (const object of Object.values(desired)) assertResourceScope(object, { ...POLICY, runId, target: BASELINE_V3_TARGET });
  assert.equal(desired.agent.spec.replicas, 1);
  assert.equal(desired.agent.spec.runtime.maxTokens, 192);
  assert.equal(POLICY.observedStatusPreviewSeconds, 4);
  assert(!desired.agent.spec.memory);
  assert.equal(desired.profile.spec.agentIncarnationUID, "actor-incarnation");
  assert.equal(desired.profile.spec.initialScore, 10);
  assert.equal(desired.profile.spec.evaluationInterval, "1m");
  assert.equal(desired.profile.spec.maxClass, "assisted");
  assert.equal(desired.profile.spec.promotionPolicy.minActions, 1);
  assert.equal(desired.profile.spec.promotionPolicy.minScoreDuration, "1m");
  assert.equal(desired.profile.spec.promotionPolicy.thresholds[0].minScore, 50);
  assert.equal(desired.grant.spec.argumentConstraints.properties.expression.const, "17 * 23");
  assert.equal(desired.denialWorkflow.spec.steps[0].agentRef.name, desired.agent.metadata.name);
  assert.equal(desired.denialWorkflow.spec.steps[0].input, '{"expression":"17 * 24"}');
  assert.equal(desired.denialWorkflow.spec.steps[0].retry.maxAttempts, 1);
  assert(!POLICY.kinds.includes("RuntimeApproval"));
  assert(!POLICY.kinds.includes("AutonomyReview"));
  assert.throws(() => fixtures("rancher-trust-unit"));
});

test("status-only configuration retries reject concurrent intent, incarnation or ownership changes", () => {
  const before = { ...fixtures("isolated-trust-unit", "actor").profile,
    metadata: { name: "profile", namespace: "tutorial-support", uid: "same-profile", generation: 1, resourceVersion: "1" } };
  const live = structuredClone(before);
  live.metadata.resourceVersion = "2";
  live.status = { currentScore: 76 };
  verifyConfigurationUnchanged(live, before);
  for (const mutate of [
    item => { item.spec.initialScore = 100; },
    item => { item.metadata.uid = "replacement"; },
    item => { item.metadata.generation++; },
    item => { item.metadata.ownerReferences = [{ uid: "other-owner" }]; },
    item => { item.metadata.deletionTimestamp = "2026-11-07T00:00:00Z"; },
  ]) {
    const changed = structuredClone(live); mutate(changed);
    assert.throws(() => verifyConfigurationUnchanged(changed, before));
  }
  assert.equal(POLICY.manifestPreviewSeconds, 4);
});

test("trust transition proof requires actual audit history, unchanged identity and controller conditions", () => {
  const initial = { metadata: { uid: "profile" }, spec: fixtures("isolated-trust-unit", "actor").profile.spec };
  const promoted = { ...structuredClone(initial), status: { currentClass: "assisted", currentScore: 100,
    auditedActions: 1, totalEvaluations: 1,
    scoreHistory: [{ reason: "audit:2026-11-06T22:00:00Z", dimensions: { policyCompliance: 1 } }],
    conditions: [{ type: "Promoted", status: "True" }] } };
  verifyTransition(promoted, initial, "assisted", "Promoted");
  for (const change of [
    { metadata: { uid: "replacement" } },
    { spec: { ...initial.spec, agentIncarnationUID: "replacement" } },
    { spec: { ...initial.spec, initialScore: 100 } },
    { status: { ...promoted.status, auditedActions: 0 } },
    { status: { ...promoted.status, scoreHistory: [] } },
    { status: { ...promoted.status, conditions: [] } },
    { status: { ...promoted.status, currentScore: 101 } },
  ]) assert.throws(() => verifyTransition({ ...promoted, ...change }, initial, "assisted", "Promoted"));
});

test("review expiry proof cannot substitute approval, a different review or an early denial", () => {
  const profile = { metadata: { name: "profile" }, status: { currentClass: "supervised" } };
  const pending = { metadata: { uid: "review" }, spec: { trustProfileRef: "profile" },
    status: { decision: "Pending", expiresAt: "2026-11-06T22:05:00Z" } };
  const expired = { ...structuredClone(pending), status: { decision: "Denied", decidedBy: "system/ttl",
    reason: "TTL expired", decidedAt: "2026-11-06T22:05:01Z" } };
  verifyExpiredReview(expired, pending, profile);
  for (const change of [
    { metadata: { uid: "replacement" } },
    { status: { ...expired.status, decision: "Approved" } },
    { status: { ...expired.status, decidedBy: "scripted-user" } },
    { status: { ...expired.status, decidedAt: "2026-11-06T22:04:59Z" } },
  ]) assert.throws(() => verifyExpiredReview({ ...expired, ...change }, pending, profile));
  assert.throws(() => verifyExpiredReview(expired, pending, { ...profile, status: { currentClass: "assisted" } }));
});

test("trust recording refuses absent live-write consent before contacting Kubernetes", async () => {
  await assert.rejects(runTrustEvolution({ runtime: "runtime", output: "unused-trust-evolution",
    runId: "isolated-trust-unit", target: BASELINE_V3_TARGET }), /authorize-live-writes/);
});

test("repaired trust recording has its own namespaces and a conservative 4,000-token share", () => {
  assert.equal(recordingNamespaces(BASELINE_V4_TARGET)["tutorial-support"], "6f6d7a87-049f-4945-8464-bbcf1614980a");
  assert.notDeepEqual(recordingNamespaces(BASELINE_V4_TARGET), recordingNamespaces(BASELINE_V3_TARGET));
  const quota = { spec: { tokenBudget: { maxTotalTokens: 4000 } }, status: { phase: "Ready" } };
  assert.equal(verifyLessonQuota(quota, BASELINE_V4_TARGET), 4000);
  assert.throws(() => verifyLessonQuota(quota, BASELINE_V3_TARGET));
  assert.throws(() => verifyLessonQuota({ ...quota, status: { phase: "Ready", tokenUsage: { totalTokens: 3600 } } },
    BASELINE_V4_TARGET));
});

test("demotion evidence requires the real denied action, exact session, identity and unreached target", () => {
  const decision = { plane: "tool", outcome: "denied", reasonCode: "argument_constraints_denied",
    identity: { agentIdentity: "spiffe://kognition.dev/workspace/tutorial-support/agent/actor" },
    session: { sessionId: "same-session" }, request: { targetRef: { name: "calculator" } },
    authorization: { grant: "denied" }, execution: { targetReached: false } };
  verifyDeniedTrustDecision(decision, "actor", "calculator", "same-session");
  for (const change of [{ reasonCode: "unavailable" }, { session: { sessionId: "other" } },
    { identity: { agentIdentity: "forged" } }, { execution: { targetReached: true } },
    { request: { targetRef: { name: "other-tool" } } }]) {
    assert.throws(() => verifyDeniedTrustDecision({ ...decision, ...change }, "actor", "calculator", "same-session"));
  }
});
