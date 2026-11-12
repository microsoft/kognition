// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const assert = require("node:assert/strict");
const { parseArgs, promisify } = require("node:util");
const { execFile } = require("node:child_process");
const { chromium, expect } = require("@playwright/test");
const { RANCHER_TARGET, BASELINE_V4_TARGET, recordingTarget, verifyRecordingCLI,
  verifyTarget, finalizeRecording } = require("./live-authoring");
const { startUI, stopChildren } = require("./live-pilot");
const execute = promisify(execFile);
const PLANES = ["tool", "memory", "model", "communication"];
const NARRATION = Object.freeze({
  TF01: "Trust Focus brings together evidence from Tool, Memory, Model and Communication boundaries. We select the support namespace, the assistant's identity and the example's time window. These filters define the evidence being inspected.",
  TF02: "The protection-plane counters and outcome summary describe the current results. We compare decisions that reached a target with decisions stopped before execution. An empty Communication counter means no matching evidence is present; it does not prove a communication request succeeded.",
  TF03: "The calculator session has its own evidence. We filter to that session and inspect a Tool decision, including its identity, request, roles, grant, approval and execution outcome.",
  TF04: "Memory and Model have separate protection boundaries. A successful model call is not permission to write Memory. We select each plane to inspect the matching decisions and their applicable authority.",
  TF05: "A bounded query can return a truncated result set. The warning means the displayed events are incomplete. We restore the wider result limit before interpreting the counters.",
  TF06: "A session filter with no matching events shows an empty view. Missing run correlation, run-proof reports or observability links remain unavailable. The dashboard does not substitute unrelated historical evidence.",
});

function verifyList(body, filter) {
  assert(Array.isArray(body.items));
  assert.equal(typeof body.truncated, "boolean");
  assert(body.items.length <= Number(filter.limit));
  const ids = new Set();
  for (const item of body.items) {
    assert(item.id && !ids.has(item.id)); ids.add(item.id);
    assert(PLANES.includes(item.plane));
    assert.equal(item.workspace, "tutorial-support");
    assert.equal(item.identity?.agentIdentity, filter.agentIdentity);
    if (filter.sessionId) assert.equal(item.session?.sessionId, filter.sessionId);
    if (filter.plane) assert.equal(item.plane, filter.plane);
    const time = new Date(item.timestamp).getTime();
    assert(Number.isFinite(time) && time >= new Date(filter.since).getTime() &&
      time <= new Date(filter.until).getTime());
  }
}

function countsFor(body) {
  const counts = Object.fromEntries(PLANES.map(plane => [plane, 0]));
  for (const decision of body.items) counts[decision.plane]++;
  return counts;
}

function minuteWindow(start, end) {
  const from = new Date(start).getTime(), to = new Date(end).getTime();
  assert(Number.isFinite(from) && Number.isFinite(to) && to > from);
  return { since: new Date(Math.floor(from / 60000) * 60000).toISOString(),
    until: new Date(Math.ceil(to / 60000) * 60000).toISOString() };
}

function recordingEvidence(source, targetName) {
  assert.equal(source.passed, true);
  if (targetName === BASELINE_V4_TARGET) {
    assert.equal(source.batch, "trust-evolution");
    assert.equal(source.mockedAPI, false);
    assert.equal(source.trustTransitionsVerified, true);
    assert.deepEqual(source.approvedTarget, recordingTarget(targetName));
    const agent = source.evidence.find(entry => entry.resource?.kind === "Agent")?.resource;
    assert(agent?.metadata.uid && source.controlledDeniedSession);
    assert(source.scenes.some(scene => scene.id === "TE04" && scene.passed));
    return { agentName: agent.metadata.name, agentUID: agent.metadata.uid,
      identityName: `agent-identity-${agent.metadata.name}`, toolSession: source.controlledDeniedSession,
      showMemory: false };
  }
  assert.equal(targetName, RANCHER_TARGET);
  return { agentName: source.supportAgent.metadata.name, agentUID: source.supportAgent.metadata.uid,
    identityName: "agent-identity-rehearsal-story-support", toolSession: source.calls[0].sessionID,
    workflowRunName: source.workflowRunName, showMemory: true };
}

async function runTrustFocus(options) {
  const target = recordingTarget(options.target);
  if (![RANCHER_TARGET, BASELINE_V4_TARGET].includes(options.target) || !options.runtime || !options.output ||
      path.dirname(path.resolve(options.output)).toLowerCase() !== path.resolve(options.runtime).toLowerCase() ||
      !path.basename(options.output).startsWith("trust-focus-") || fs.existsSync(options.output)) {
    throw new Error("Trust Focus requires a fresh trust-focus-* child of an exact reviewed runtime");
  }
  const viewport = { width: 1920, height: 1080 };
  const fixedTarget = options.target === BASELINE_V4_TARGET;
  const sourceFile = path.join(options.runtime, fixedTarget ? "trust-evolution-20261107-a1-cont1" : "connected-20261105-a1", "report.json");
  const source = JSON.parse(fs.readFileSync(sourceFile, "utf8"));
  const evidence = recordingEvidence(source, options.target);
  const narration = { ...NARRATION, ...(fixedTarget ? {
    TF01: "Trust Focus inspects the actual evidence for disposable Agent full-trust-actor. We select its verified identity and recorded time window. These filters keep this experiment separate from the support assistant's history.",
    TF03: "This calculator session contains the real argument-constraint denial. The grant check is denied and the target was not reached. An earned autonomy class does not override a ToolGrant's argument constraints.",
    TF04: "The Model and Tool boundaries record different decisions. The Model completed, while the disallowed Tool request was blocked. This disposable experiment did not exercise Memory or Communication; their empty counters remain explicit.",
  } : {}) };
  const report = { schemaVersion: 1, batch: "trust-focus-read-only", passed: false, readOnly: true,
    newProviderCalls: 0, decisionsSubmitted: 0, resourceMutations: 0, narration: false, viewport,
    originalReportSHA256: crypto.createHash("sha256").update(fs.readFileSync(sourceFile)).digest("hex"),
    scenes: [], queries: [], browserErrors: [], expectedInitialQueryFailures: [],
    fullFourPlaneExecutionVerified: false, trustTransitionsVerified: false };
  fs.mkdirSync(options.output);
  const persist = () => fs.writeFileSync(path.join(options.output, "report.json"), JSON.stringify(report, null, 2));
  persist();
  const children = [];
  let browser, context, page, video, failure;
  const cluster = JSON.parse(fs.readFileSync(path.join(options.runtime, "cluster.json"), "utf8"));
  const env = { ...process.env, KUBECONFIG: cluster.kubeconfig };
  const kube = async args => (await execute("kubectl", ["--kubeconfig", cluster.kubeconfig,
    "--context", target.context, "--request-timeout=20s", ...args], {
    env, windowsHide: true, encoding: "utf8", timeout: 30000, maxBuffer: 8 * 1024 * 1024,
  })).stdout;
  const get = async (kind, name, ns = target.namespace) => JSON.parse(await kube([
    "get", kind, name, ...(ns ? ["-n", ns] : []), "-o", "json"]));
  try {
    const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
    verifyRecordingCLI(crypto.createHash("sha256").update(fs.readFileSync(cli)).digest("hex"), target);
    const config = JSON.parse(await kube(["config", "view", "--minify", "-o", "json"]));
    verifyTarget(cluster, config, await get("namespace", "kube-system", ""),
      await get("namespace", target.namespace, ""), await get("deployment", target.controller, target.systemNamespace), target);
    const agent = await get("agent", evidence.agentName);
    assert.equal(agent.metadata.uid, evidence.agentUID);
    const identity = await get("agentidentity", evidence.identityName);
    assert(identity.spec.spiffeID);
    const filter = { agentIdentity: identity.spec.spiffeID, sessionId: "", plane: "", outcome: "",
      ...minuteWindow(source.startedAt, source.completedAt), limit: "100" };
    report.scope = { namespace: target.namespace, agentUID: agent.metadata.uid,
      agentIdentityUID: identity.metadata.uid, agentIdentity: identity.spec.spiffeID,
      since: filter.since, until: filter.until, originatingWorkflowRun: evidence.workflowRunName,
      runFilterQualification: "Recorded entries lack runId; scope uses exact identity, minute-bounded recorded window and individual recorded sessions." };
    const origin = await startUI("dashboard", { cli, mode: "kubernetes", namespace: target.namespace,
      "kube-context": target.context, systemNamespace: target.systemNamespace,
      outputDir: options.output, environment: { KUBECONFIG: cluster.kubeconfig,
        APPDATA: path.join(options.output, "dashboard-profile") } }, undefined, children);
    const meta = await fetch(`${origin}/api/meta`).then(response => {
      assert(response.ok); return response.json();
    });
    assert.equal(meta.mode, "kubernetes"); assert.equal(meta.namespace, target.namespace);
    report.dashboard = origin; report.auditNamespace = target.systemNamespace;
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport, serviceWorkers: "block", recordVideo: {
      dir: options.output, size: viewport, showActions: { duration: 600, fontSize: 16, cursor: "pointer" },
    } });
    await context.route("**/api/**", async route => {
      const request = route.request();
      if (new URL(request.url()).origin !== origin || !["GET", "HEAD"].includes(request.method())) {
        report.browserErrors.push(`Non-read-only API request refused: ${request.method()} ${new URL(request.url()).pathname}`);
        await route.abort("blockedbyclient"); return;
      }
      await route.continue();
    });
    page = await context.newPage(); video = page.video();
    const epoch = performance.now(); const seconds = () => (performance.now() - epoch) / 1000;
    page.on("pageerror", error => report.browserErrors.push(error.message));
    const scene = async (id, action) => {
      const entry = { id, caption: narration[id].split(". ")[0], narration: narration[id],
        startSeconds: seconds(), passed: false };
      report.scenes.push(entry); persist();
      await action();
      const remaining = 2 + narration[id].split(/\s+/).length * 60 / 150 - (seconds() - entry.startSeconds);
      if (remaining > 0) await page.waitForTimeout(remaining * 1000);
      entry.endSeconds = seconds(); entry.passed = true; persist();
    };
    const localTime = iso => {
      const date = new Date(iso);
      return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    };
    const apply = async changes => {
      Object.assign(filter, changes);
      const panel = page.locator("#trust-focus-filter-panel");
      if (await panel.getAttribute("open") === null) await panel.locator("summary").click();
      await page.locator("#trust-agent").fill(filter.agentIdentity);
      await page.locator("#trust-session").fill(filter.sessionId);
      await page.locator("#trust-plane").selectOption(filter.plane);
      await page.locator("#trust-outcome").selectOption(filter.outcome);
      await page.locator("#trust-since").fill(localTime(filter.since));
      await page.locator("#trust-until").fill(localTime(filter.until));
      await page.locator("#trust-limit").fill(filter.limit);
      const [response] = await Promise.all([
        page.waitForResponse(r => new URL(r.url()).pathname === "/api/trust-decisions" &&
          new URL(r.url()).searchParams.get("agentIdentity") === filter.agentIdentity &&
          (new URL(r.url()).searchParams.get("sessionId") || "") === filter.sessionId &&
          (new URL(r.url()).searchParams.get("plane") || "") === filter.plane &&
          new URL(r.url()).searchParams.get("limit") === filter.limit),
        page.getByRole("button", { name: "Apply filters", exact: true }).click(),
      ]);
      const body = await response.json();
      assert(response.ok(), `Actual Trust Focus query failed: ${JSON.stringify(body)}`);
      const exact = new URL(response.url()).searchParams;
      verifyList(body, { ...filter, since: exact.get("since"), until: exact.get("until") });
      report.queries.push({ seconds: seconds(), filter: Object.fromEntries(exact), body }); persist();
      await expect(page.locator("#trust-total")).toHaveText(String(body.items.length));
      await expect(page.locator("#trust-focus-stream-body button.trust-stream-event")).toHaveCount(body.items.length);
      for (const [plane, count] of Object.entries(countsFor(body))) {
        await expect(page.locator(`#trust-plane-strip [data-plane="${plane}"] strong`)).toHaveText(String(count));
      }
      await panel.locator("summary").click();
      return body;
    };
    const selectDecision = async (body, sessionId) => {
      const decision = body.items[0];
      assert(decision?.id);
      const event = page.locator("#trust-focus-stream-body button.trust-stream-event")
        .filter({ has: page.locator(".trust-stream-meta") });
      const target = event.locator(`xpath=self::*[@data-decision-id="${decision.id}"]`);
      await expect(target).toHaveCount(1);
      await target.click();
      await expect(page.locator("#trust-evidence-state")).toHaveText(decision.id);
      if (sessionId) await expect(page.locator("#trust-focus-evidence-body")).toContainText(sessionId);
      (report.selectedDecisions ||= []).push({ id: decision.id, plane: decision.plane,
        sessionId: decision.session?.sessionId, seconds: seconds(), exactSelectionVerified: true });
      persist();
    };
    await scene("TF01", async () => {
      const [initial] = await Promise.all([
        page.waitForResponse(r => new URL(r.url()).pathname === "/api/trust-decisions"),
        page.goto(`${origin}/?view=trust&focus=1&namespace=tutorial-support`),
      ]);
      await expect(page.locator("#trust-focus")).toBeVisible();
      if (!initial.ok()) report.expectedInitialQueryFailures.push(await initial.json());
      const body = await apply({});
      assert(body.items.length > 0 && !body.truncated);
      report.observedPlanes = countsFor(body);
      await page.waitForTimeout(3000);
    });
    await scene("TF02", async () => {
      await expect(page.locator('#trust-plane-strip [data-plane="communication"] small')).toContainText("No decisions");
      await page.waitForTimeout(5000);
    });
    await scene("TF03", async () => {
      const body = await apply({ sessionId: evidence.toolSession, plane: "tool" });
      assert(body.items.length > 0);
      await selectDecision(body, evidence.toolSession);
      await expect(page.locator("#trust-focus-evidence-body")).toContainText("Execution outcome");
      await expect(page.locator("#trust-focus-evidence-body")).toContainText("Approval evidence");
      await page.locator("#trust-focus-evidence-body").scrollIntoViewIfNeeded();
      await page.waitForTimeout(5000);
    });
    await scene("TF04", async () => {
      for (const plane of evidence.showMemory ? ["memory", "model"] : ["model"]) {
        const body = await apply({ sessionId: "", plane });
        assert(body.items.length > 0);
        await selectDecision(body);
        await expect(page.locator("#trust-focus-evidence-body")).toContainText("Execution outcome");
        await page.waitForTimeout(4000);
      }
    });
    await scene("TF05", async () => {
      const short = await apply({ plane: "", sessionId: "", limit: "1" });
      assert.equal(short.truncated, true);
      await expect(page.locator("#trust-overflow")).toBeVisible();
      await page.waitForTimeout(4000);
      await apply({ limit: "100" });
      await expect(page.locator("#trust-overflow")).toBeHidden();
    });
    await scene("TF06", async () => {
      const empty = await apply({ sessionId: "no-such-tutorial-session" });
      assert.equal(empty.items.length, 0);
      await expect(page.locator("#trust-focus-stream-body")).toContainText("No decisions");
      await page.waitForTimeout(4000);
      await apply({ sessionId: "" });
      report.runProofAvailable = await page.locator("#trust-run-proof").isVisible();
      report.traceLinkAvailable = await page.locator("#trust-open-trace").isVisible();
      report.metricsLinkAvailable = await page.locator("#trust-open-metrics").isVisible();
    });
    assert.equal(report.browserErrors.length, 0);
    report.passed = true;
  } catch (error) {
    failure = error; report.failure = error.message;
    if (page) await page.screenshot({ path: path.join(options.output, "failure.png") });
  } finally {
    if (context && browser && video) {
      report.recording = await finalizeRecording(context, browser, video,
        path.join(options.output, failure ? "trust-focus.failed.webm" : "trust-focus.recording.webm"));
    } else if (browser) await browser.close();
    await stopChildren(children);
    report.completedAt = new Date().toISOString(); persist();
  }
  if (failure) throw failure;
  return report;
}

if (require.main === module) {
  const { values } = parseArgs({ args: process.argv.slice(2), strict: true, options: {
    runtime: { type: "string" }, output: { type: "string" }, target: { type: "string" },
  } });
  runTrustFocus(values).catch(error => { console.error(error.message); process.exitCode = 1; });
}
module.exports = { PLANES, NARRATION, verifyList, countsFor, minuteWindow, recordingEvidence, runTrustFocus };
