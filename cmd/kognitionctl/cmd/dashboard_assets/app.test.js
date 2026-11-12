// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

class TestElement {
  constructor(tagName) {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.className = "";
    this.listeners = {};
    this._text = "";
  }

  appendChild(child) {
    this.children.push(child);
    return child;
  }

  addEventListener(name, listener) {
    this.listeners[name] = listener;
  }

  set innerHTML(value) {
    assert.equal(value, "");
    this.children = [];
    this._text = "";
  }

  set textContent(value) {
    this.children = [];
    this._text = String(value);
  }

  get textContent() {
    return this._text + this.children.map((child) => child.textContent).join("");
  }
}

const detailBody = new TestElement("div");
global.document = {
  addEventListener: () => {},
  createElement: (tagName) => new TestElement(tagName),
  createTextNode: (text) => {
    const node = new TestElement("#text");
    node.textContent = text;
    return node;
  },
  getElementById: (id) => id === "detail-body" ? detailBody : null,
};

const explorer = require("./app.js");

function decision(id, timestamp, sessionId, invocationId) {
  return {
    id,
    timestamp,
    plane: "tool",
    outcome: "allowed",
    reasonCode: "authorized",
    session: sessionId ? { sessionId } : undefined,
    request: { operation: "invoke", invocationId, targetRef: { kind: "Tool", name: "weather" } },
    authorization: { identity: "allowed", role: "allowed", grant: "allowed", policy: "allowed" },
    execution: { status: "completed", targetReached: true },
  };
}

function descendants(element, tagName) {
  const matches = element.tagName === tagName.toUpperCase() ? [element] : [];
  return matches.concat(element.children.flatMap((child) => descendants(child, tagName)));
}

test("sorts the decision list newest first with a stable ID tie-break", () => {
  const items = [
    decision("missing", undefined, "session-a", "invoke-a"),
    decision("a", "2026-08-27T10:00:00Z", "session-a", "invoke-a"),
    decision("invalid", "not-a-time", "session-a", "invoke-a"),
    decision("b", "2026-08-27T11:00:00Z", "session-a", "invoke-a"),
    decision("c", "2026-08-27T11:00:00Z", "session-a", "invoke-a"),
  ];
  assert.deepEqual(explorer.sortTrustDecisionsNewestFirst(items).map((item) => item.id), ["c", "b", "a", "missing", "invalid"]);
  assert.deepEqual(items.map((item) => item.id), ["missing", "a", "invalid", "b", "c"], "sorting must not mutate API state");
});

test("groups by session and invocation then renders the group chronologically", () => {
  const selected = decision("new", "2026-08-27T11:00:00Z", "session-a", "invoke-a");
  const related = explorer.relatedTrustDecisions(selected, [
    decision("missing", undefined, "session-a", "invoke-a"),
    selected,
    decision("other-invocation", "2026-08-27T09:00:00Z", "session-a", "invoke-b"),
    decision("old", "2026-08-27T10:00:00Z", "session-a", "invoke-a"),
    decision("other-session", "2026-08-27T08:00:00Z", "session-b", "invoke-a"),
  ]);
  assert.deepEqual(related.map((item) => item.id), ["old", "new", "missing"]);
});

test("does not group unrelated decisions when both grouping IDs are absent", () => {
  const first = decision("first", "2026-08-27T10:00:00Z");
  const second = decision("second", "2026-08-27T11:00:00Z");
  assert.deepEqual(explorer.relatedTrustDecisions(first, [first, second]).map((item) => item.id), ["first"]);
});

test("renders fixed evidence labels, unavailable fields, safe text, and resource links", () => {
  const fixture = decision("missing", "2026-08-27T10:00:00Z", "session-a", "invoke-a");
  fixture.reasonCode = "<img src=x onerror=alert(1)>";
  fixture.timestamp = "not-a-time";
  fixture.identity = undefined;
  fixture.approval = undefined;
  fixture.trace = { traceId: "trace-a" };
  fixture.authorization.policy = "";
  fixture.authorization.grantRef = { kind: "Grant", namespace: "team-a", name: "grant-a" };
  explorer.renderTrustDecisionTimeline([fixture]);

  const text = detailBody.textContent;
  for (const label of ["Identity", "Session", "Capability ID", "Request", "Authorization · identity", "Authorization · role", "Authorization · grant", "Authorization · policy", "Approval evidence", "Execution outcome", "Trace correlation"]) {
    assert.match(text, new RegExp(label));
  }
  assert.match(text, /Evidence unavailable/);
  assert.doesNotMatch(text, /Invalid Date/);
  assert.match(text, /<img src=x onerror=alert\(1\)>/, "untrusted details remain text content");
  const links = descendants(detailBody, "button");
  assert.equal(links.length, 2);
  assert.deepEqual(links.map((link) => link.textContent), ["Tool/weather", "Grant/grant-a"]);
  assert.ok(links.every((link) => typeof link.listeners.click === "function"));
});

test("builds the complete server-side filter query with RFC3339 times", () => {
  const query = new URLSearchParams(explorer.buildTrustDecisionQuery("team a", {
    runId: "demo-a",
    agentIdentity: "agent-a",
    sessionId: "session-a",
    plane: "tool",
    outcome: "denied",
    since: "2026-08-27T10:00",
    until: "2026-08-27T11:00",
    limit: "25",
  }));
  assert.equal(query.get("namespace"), "team a");
  assert.equal(query.get("runId"), "demo-a");
  assert.equal(query.get("agentIdentity"), "agent-a");
  assert.equal(query.get("sessionId"), "session-a");
  assert.equal(query.get("plane"), "tool");
  assert.equal(query.get("outcome"), "denied");
  assert.match(query.get("since"), /^2026-08-27T\d{2}:00:00\.000Z$/);
  assert.match(query.get("until"), /^2026-08-27T\d{2}:00:00\.000Z$/);
  assert.equal(query.get("limit"), "25");
});

test("parses run-scoped and all-runs Trust Focus routes", () => {
  assert.deepEqual(explorer.parseDashboardRoute("?view=trust&runId=demo-a&focus=1"), { focus: true, runId: "demo-a", namespace: null });
  assert.deepEqual(explorer.parseDashboardRoute("?view=trust&runId=%20demo-a%20&focus=1"), { focus: true, runId: "demo-a", namespace: null });
  assert.deepEqual(explorer.parseDashboardRoute("?view=trust&namespace=%20team-a%20&focus=1"), { focus: true, runId: "", namespace: "team-a" });
  assert.deepEqual(explorer.parseDashboardRoute("?view=trust&focus=1"), { focus: true, runId: "", namespace: null });
  assert.deepEqual(explorer.parseDashboardRoute(""), { focus: false, runId: "", namespace: null });
  for (const search of ["?view=trust&runId=demo-a", "?view=resources&runId=demo-a&focus=1"]) {
    assert.deepEqual(explorer.parseDashboardRoute(search), { focus: false, runId: "demo-a", namespace: null });
  }
});

test("reconciles Trust Focus selection by ID and retains stale evidence", () => {
  const selected = decision("selected", "2026-08-27T10:00:00Z", "session-a", "invoke-a");
  const replacement = { ...selected, outcome: "denied" };
  const newest = decision("newest", "2026-08-27T12:00:00Z", "session-b", "invoke-b");

  assert.deepEqual(explorer.reconcileTrustDecisionSelection(selected, [newest, replacement]), { selected: replacement, stale: false });
  assert.deepEqual(explorer.reconcileTrustDecisionSelection(selected, [newest]), { selected, stale: true });
  assert.deepEqual(explorer.reconcileTrustDecisionSelection(null, [selected, newest]), { selected: newest, stale: false });
  assert.deepEqual(explorer.reconcileTrustDecisionSelection(null, []), { selected: null, stale: false });
});

test("builds an exact encoded Jaeger trace URL only when configured", () => {
  assert.equal(explorer.buildTraceURL("https://observe.example.test/jaeger/", "trace/a b"), "https://observe.example.test/jaeger/trace/trace%2Fa%20b");
  assert.equal(explorer.buildTraceURL("", "trace-a"), "");
  assert.equal(explorer.buildTraceURL("https://observe.example.test", ""), "");
});

test("summarizes only observable execution and approval facts", () => {
  const reached = decision("reached", "2026-08-27T10:00:00Z", "session-a", "invoke-a");
  const stopped = { ...decision("stopped", "2026-08-27T10:01:00Z", "session-b", "invoke-b"), execution: { status: "denied", targetReached: false } };
  const awaiting = { ...decision("awaiting", "2026-08-27T10:02:00Z", "session-c", "invoke-c"), approval: { status: "pending" }, execution: undefined };
  const unknown = { ...decision("unknown", "2026-08-27T10:03:00Z", "session-d", "invoke-d"), execution: undefined };

  assert.deepEqual(explorer.trustExecutionSummary([reached, stopped, awaiting, unknown]), {
    events: 4,
    reachedTarget: 1,
    stoppedBeforeTarget: 1,
    awaitingApproval: 1,
  });
});

test("classifies every canonical outcome intentionally", () => {
  const classes = Object.fromEntries([
    "allowed", "denied", "revoked", "replayed", "pending", "approved", "rejected", "completed", "failed",
  ].map((outcome) => [outcome, explorer.decisionOutcomeClass(outcome)]));

  assert.deepEqual(classes, {
    allowed: "allowed",
    denied: "blocked",
    revoked: "blocked",
    replayed: "blocked",
    pending: "pending",
    approved: "allowed",
    rejected: "blocked",
    completed: "allowed",
    failed: "blocked",
  });
});

test("accepts only run-matched and internally consistent Run Proof payloads", () => {
  const proof = {
    schemaVersion: 1,
    runId: "demo-a",
    scenario: "Delegated research",
    protectedAction: "Publish result",
    mode: "standalone-fallback",
    passed: true,
    assertions: { total: 2, passed: 2, failed: 0 },
    reportRef: "run-report.json",
  };

  assert.deepEqual(explorer.normalizeRunProof(proof, "demo-a"), proof);
  assert.equal(explorer.normalizeRunProof(proof, "demo-b"), null);
  assert.equal(explorer.normalizeRunProof({ ...proof, assertions: { total: 3, passed: 2, failed: 0 } }, "demo-a"), null);
  assert.equal(explorer.normalizeRunProof({ ...proof, scenario: "" }, "demo-a"), null);
  assert.equal(explorer.normalizeRunProof({ ...proof, reportRef: "C:\\private\\report.json" }, "demo-a").reportRef, "");
});