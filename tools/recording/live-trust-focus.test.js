// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { BASELINE_V4_TARGET, BASELINE_V4_APPROVED } = require("./live-authoring");
const { verifyList, countsFor, minuteWindow, recordingEvidence } = require("./live-trust-focus");
const filter = { agentIdentity: "spiffe://example/agent", sessionId: "same-session", plane: "",
  since: "2026-11-05T11:00:00Z", until: "2026-11-05T12:00:00Z", limit: "100" };
function list() {
  return { truncated: false, items: [{ id: "event-one", plane: "tool", workspace: "tutorial-support",
    identity: { agentIdentity: filter.agentIdentity }, session: { sessionId: filter.sessionId },
    timestamp: "2026-11-05T11:30:00Z" }] };
}
test("Trust Focus capture verifies real identity, session, time and bounded result scope", () => {
  verifyList(list(), filter);
  for (const mutate of [
    body => { body.items[0].identity.agentIdentity = "different"; },
    body => { body.items[0].session.sessionId = "different"; },
    body => { body.items[0].timestamp = "2026-11-05T13:00:00Z"; },
    body => { body.items[0].workspace = "another-namespace"; },
    body => { body.items.push(body.items[0]); },
  ]) {
    const body = list(); mutate(body); assert.throws(() => verifyList(body, filter));
  }
});

test("repaired Trust Focus must use the exact completed trust experiment rather than historical actors", () => {
  const source = { passed: true, batch: "trust-evolution", mockedAPI: false,
    trustTransitionsVerified: true, approvedTarget: BASELINE_V4_APPROVED,
    evidence: [{ resource: { kind: "Agent", metadata: { name: "full-trust-actor", uid: "same-actor" } } }],
    controlledDeniedSession: "denied-session", scenes: [{ id: "TE04", passed: true }] };
  assert.deepEqual(recordingEvidence(source, BASELINE_V4_TARGET), {
    agentName: "full-trust-actor", agentUID: "same-actor", identityName: "agent-identity-full-trust-actor",
    toolSession: "denied-session", showMemory: false,
  });
  for (const change of [{ passed: false }, { mockedAPI: true }, { trustTransitionsVerified: false },
    { approvedTarget: { ...BASELINE_V4_APPROVED, clusterUID: "other-cluster" } },
    { controlledDeniedSession: "" }, { scenes: [{ id: "TE04", passed: false }] }]) {
    assert.throws(() => recordingEvidence({ ...source, ...change }, BASELINE_V4_TARGET));
  }
});
test("an empty protection plane stays zero rather than acquiring fabricated coverage", () => {
  assert.deepEqual(countsFor(list()), { tool: 1, memory: 0, model: 0, communication: 0 });
  assert.deepEqual(countsFor({ items: [] }), { tool: 0, memory: 0, model: 0, communication: 0 });
});

test("recorded time bounds fit native minute-step datetime controls without dropping events", () => {
  assert.deepEqual(minuteWindow("2026-11-05T11:39:28.209Z", "2026-11-05T11:43:43.883Z"),
    { since: "2026-11-05T11:39:00.000Z", until: "2026-11-05T11:44:00.000Z" });
  assert.throws(() => minuteWindow("invalid", "2026-11-05T12:00:00Z"));
});
