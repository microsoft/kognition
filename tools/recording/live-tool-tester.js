// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { execFileSync } = require("node:child_process");
const { isDeepStrictEqual } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const { APPROVED, CLI_SHA256, RANCHER_TARGET, recordingTarget, parseOptions,
  verifyTarget, finalizeRecording, isWelcomeDismissal } = require("./live-authoring");
const { startUI, stopChildren, fieldLabel } = require("./live-pilot");
const { READING_POLICY, holdReadableText } = require("./reading");

const TOOL = "story-calculator";
const TOOL_UID = "d34aea73-9d0d-47c6-8398-885adb75961e";
const RANCHER_TOOL_UID = "b3aade98-6097-4153-9906-3f399bc2cac0";
const EXPRESSION = "17 * 23";
const VIEWPORT = { width: 1920, height: 1080 };

function verifyToolFixture(tool, targetName) {
  recordingTarget(targetName);
  const expectedUID = targetName === RANCHER_TARGET ? RANCHER_TOOL_UID : TOOL_UID;
  if (tool?.kind !== "Tool" || tool.metadata?.name !== TOOL || tool.metadata.namespace !== APPROVED.namespace ||
      tool.metadata.uid !== expectedUID || tool.metadata.deletionTimestamp ||
      tool.metadata.labels?.["kognition.dev/story-fixture"] !== "true" ||
      tool.spec?.className !== "story-http" || tool.spec.protocol !== "OpenAPI" ||
      tool.spec.endpoint !== "http://story-integrations.tutorial-support.svc.cluster.local:8080/calculator" ||
      tool.spec.sandbox?.timeout !== "20s" || tool.status?.phase !== "Ready" ||
      !/^sha256:[a-f0-9]{64}$/.test(tool.status.observedDigest || "")) {
    throw new Error("The exact existing calculator identity, endpoint, timeout or readiness differs");
  }
}

function verifySchemaEvidence(body, tool) {
  if (!["live", "declared"].includes(body?.source) || body.inputSchema?.type !== "object" ||
      body.inputSchema.properties?.expression?.type !== "string" ||
      !body.inputSchema.required?.includes("expression") ||
      (body.source === "declared" && !isDeepStrictEqual(body.inputSchema, tool.spec.inputSchema))) {
    throw new Error("Schema evidence lacks its actual provenance or calculator argument contract");
  }
}

function verifyToolResult(body, input) {
  const output = typeof body?.output === "string" ? JSON.parse(body.output) : body?.output;
  if (body?.error || body?.protocol !== "OpenAPI" || typeof body.executionMode !== "string" || !body.executionMode ||
      !Number.isFinite(body.durationMs) || body.durationMs < 0 ||
      input?.expression !== EXPRESSION || output?.expression !== EXPRESSION || output.result !== 391 || output.error) {
    throw new Error("Independent invocation lacks the actual echoed expression, numeric391 result or execution metadata");
  }
}

function verifyToolFailure(httpStatus, body, input) {
  if (body?.protocol !== "OpenAPI" || !Number.isFinite(body.durationMs) || body.durationMs < 0 ||
      input?.expression !== "17 *") throw new Error("Missing actual failed-invocation metadata");
  if (httpStatus >= 400 && typeof body.error === "string" && body.error.trim()) {
    return { kind: "transport", message: body.error, httpStatus, code: body.code, retryable: body.retryable };
  }
  const output = typeof body.output === "string" ? JSON.parse(body.output) : body.output;
  if (httpStatus >= 200 && httpStatus < 300 && typeof output?.error === "string" && output.error.trim() &&
      output.expression === input.expression && output.result === undefined) {
    return { kind: "application", message: output.error, httpStatus,
      qualification: "The demo integration returns an explicit application error inside an HTTP200 response; this is not a transport failure." };
  }
  throw new Error("Invalid input has neither an explicit transport error nor the fixture's structured application error");
}

async function runToolTester(input) {
  const options = parseOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  const APPROVED = recordingTarget(options.target);
  fs.mkdirSync(options.output);
  const report = { schemaVersion: 1, runId: options.runId, startedAt: new Date().toISOString(),
    passed: false, mockedAPI: false, fullTutorialRecorded: false, narration: false,
    readingPolicy: READING_POLICY, readingHolds: [], scenes: [], schemas: [], calls: [],
    submittedInputs: [], uiPreferenceChanges: [], browserErrors: [], requests: [], createdResources: [],
    coverage: { entireScenesCompleted: [], coveredSubActions: [],
      pending: ["S08 visual new-Tool authoring and MCP variants", "S09 unavailable-endpoint and class-owned stdio variants"],
      operatorPreviewIsNotAgentAuthorization: true } };
  const children = [];
  let context, browser, page, video, epoch, failure;
  const seconds = () => (performance.now() - epoch) / 1000;
  const persist = () => fs.writeFileSync(path.join(options.output, "report.json"), JSON.stringify(report, null, 2));
  persist();
  const scene = async (id, description, action) => {
    const entry = { id, description, startSeconds: seconds(), passed: false };
    report.scenes.push(entry);
    persist();
    console.log(`${id}: ${description}`);
    await action();
    if (report.browserErrors.length) throw new Error("An unexpected browser error or out-of-scope write occurred");
    entry.endSeconds = seconds();
    entry.passed = true;
    persist();
  };
  try {
    const cluster = JSON.parse(fs.readFileSync(path.join(options.runtime, "cluster.json"), "utf8"));
    const common = ["--kubeconfig", cluster.kubeconfig, "--context", APPROVED.context, "--request-timeout=20s"];
    const kubectl = args => JSON.parse(execFileSync("kubectl", [...common, ...args], {
      encoding: "utf8", windowsHide: true, timeout: 30000, maxBuffer: 8 * 1024 * 1024,
    }));
    const get = (kind, name, namespace) => kubectl(["get", kind, name, ...(namespace ? ["-n", namespace] : []), "-o", "json"]);
    verifyTarget(cluster, kubectl(["config", "view", "--minify", "-o", "json"]),
      get("namespace", "kube-system"), get("namespace", APPROVED.namespace),
      get("deployment", APPROVED.controller, APPROVED.systemNamespace), APPROVED);
    const tool = get("tool", TOOL, APPROVED.namespace);
    verifyToolFixture(tool, options.target);
    const toolClass = get("toolclass", "story-http");
    if (toolClass.spec.provisioner !== "openapi" || toolClass.spec.transport !== "http" ||
        toolClass.spec.executionTier !== "process") throw new Error("Prepared ToolClass differs from the baseline HTTP fixture");
    report.toolBefore = tool;
    report.toolClass = toolClass;
    const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
    report.cliSHA256 = crypto.createHash("sha256").update(fs.readFileSync(cli)).digest("hex");
    if (report.cliSHA256 !== CLI_SHA256) throw new Error("Frozen CLI hash differs");
    report.approvedTarget = APPROVED;
    const profile = path.join(options.output, "studio-profile");
    fs.mkdirSync(profile);
    report.uiPreferenceProfile = profile;
    const origin = await startUI("studio", { cli, mode: "kubernetes", namespace: APPROVED.namespace,
      "kube-context": APPROVED.context, systemNamespace: APPROVED.systemNamespace,
      outputDir: options.output, environment: { KUBECONFIG: cluster.kubeconfig, APPDATA: profile } }, undefined, children);
    const meta = await fetch(`${origin}/api/meta`, { signal: AbortSignal.timeout(30000) });
    if (!meta.ok) throw new Error("Studio did not become responsive");
    const metadata = await meta.json();
    if (metadata.mode !== "kubernetes" || metadata.namespace !== APPROVED.namespace) throw new Error("Studio scope differs");
    report.studio = origin;
    report.uiProcessIDs = children.map(child => child.pid);
    persist();
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport: VIEWPORT, recordVideo: { dir: options.output, size: VIEWPORT,
      showActions: { duration: 1000, fontSize: 18, cursor: "pointer" } } });
    await context.route("**/api/**", async route => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.origin !== origin) {
        report.browserErrors.push("Non-local API request refused");
        await route.abort();
        return;
      }
      if (!["GET", "HEAD"].includes(request.method())) {
        const body = request.postDataJSON();
        if (isWelcomeDismissal(request.method(), request.url(), body, report.uiPreferenceChanges.length)) {
          report.uiPreferenceChanges.push({ action: "dismiss-welcome", profile, workloadMutation: false });
          await route.continue();
          return;
        }
        const expected = report.submittedInputs.length < 2 ? EXPRESSION : "17 *";
        if (request.method() !== "POST" || url.pathname !== `/api/invoke/tool/${TOOL}` ||
            url.searchParams.get("namespace") !== APPROVED.namespace || report.submittedInputs.length >= 3 ||
            !isDeepStrictEqual(body, { input: { expression: expected } })) {
          report.browserErrors.push("Out-of-scope Tool tester write refused");
          await route.abort();
          return;
        }
        report.submittedInputs.push(body.input);
      }
      await route.continue();
    });
    epoch = performance.now();
    page = await context.newPage();
    video = page.video();
    page.on("pageerror", error => report.browserErrors.push(error.message));
    page.on("response", response => {
      if (new URL(response.url()).pathname.startsWith("/api/")) report.requests.push({
        path: new URL(response.url()).pathname, method: response.request().method(), status: response.status(), seconds: seconds(),
      });
    });
    const click = async locator => { await locator.scrollIntoViewIfNeeded(); await locator.click(); };
    const type = async (locator, value) => { await locator.fill(""); await locator.pressSequentially(value, { delay: 30 }); };
    const hold = async (locator, target) => {
      const startSeconds = seconds();
      const exposure = await holdReadableText(page, locator);
      report.readingHolds.push({ target, startSeconds, endSeconds: seconds(), ...exposure });
    };
    const schemaResponse = () => page.waitForResponse(response => response.request().method() === "GET" &&
      new URL(response.url()).pathname === `/api/tools/${TOOL}/schema`, { timeout: 45000 });
    const inspectSchema = async response => {
      if (!response.ok()) throw new Error(`Schema request failed (HTTP${response.status()})`);
      const body = await response.json();
      verifySchemaEvidence(body, tool);
      report.schemas.push(body);
      await expect(page.locator(".tutorial-welcome")).toHaveCount(0);
      await expect(page.locator(".tool-tester")).toContainText(body.source === "live" ? "live from endpoint" : "declared on CR");
      if (body.warning) await expect(page.locator(".tool-tester")).toContainText(body.warning);
      return body;
    };
    const invoke = async expectedSuccess => {
      const [response] = await Promise.all([
        page.waitForResponse(response => response.request().method() === "POST" &&
          new URL(response.url()).pathname === `/api/invoke/tool/${TOOL}`, { timeout: 45000 }),
        click(page.getByRole("button", { name: "Invoke", exact: true })),
      ]);
      const body = await response.json();
      report.calls.push({ httpStatus: response.status(), input: report.submittedInputs.at(-1), body });
      if (expectedSuccess) {
        if (!response.ok()) throw new Error(`Calculator invocation failed (HTTP${response.status()}); no retry`);
        verifyToolResult(body, report.submittedInputs.at(-1));
        const output = page.locator(".tool-tester-output pre");
        await expect(output).toContainText("391");
        await hold(output, "independent-tool-result");
      } else {
        const evidence = verifyToolFailure(response.status(), body, report.submittedInputs.at(-1));
        report.calls.at(-1).failureEvidence = evidence;
        const error = evidence.kind === "transport"
          ? page.locator(".tool-tester .preview-banner.bad") : page.locator(".tool-tester-output pre");
        await expect(error).toContainText(evidence.kind === "transport" ? "Invocation failed" : evidence.message);
        if (evidence.kind === "transport" && body.code) await expect(error).toContainText(body.code);
        if (evidence.kind === "transport" && typeof body.retryable === "boolean") await expect(error).toContainText(`retryable: ${body.retryable}`);
        await hold(error, `actual-tool-${evidence.kind}-error`);
      }
    };
    await scene("T01", "Inspect the real ToolClass and calculator definition before independent execution.", async () => {
      await page.goto(origin, { waitUntil: "domcontentloaded" });
      await expect(page.locator("#status")).toHaveText("ready", { timeout: 60000 });
      const welcome = page.locator(".tutorial-welcome");
      await expect(welcome).toBeVisible();
      const [response] = await Promise.all([
        page.waitForResponse(response => response.request().method() === "POST" &&
          new URL(response.url()).pathname === "/api/studio/tutorial-dismiss"),
        welcome.getByRole("button", { name: "Not now", exact: true }).press("Enter"),
      ]);
      if (!response.ok() || (await response.json()).dismissed !== true) throw new Error("Recording-owned welcome preference was not saved");
      await expect(welcome).toHaveCount(0);
      const item = page.locator("#list-toolclasses").getByText("story-http", { exact: true });
      await click(item.locator("xpath=ancestor::details[1]").locator("summary"));
      await click(item);
      await click(page.locator('[data-view="yaml"]'));
      await expect(page.locator("#yaml-area")).toHaveValue(/executionTier:\s*process/);
      await page.waitForTimeout(5000);
      await click(page.locator("#list-tools").getByText(TOOL, { exact: true }));
      await expect(page.locator("#yaml-area")).toHaveValue(/protocol:\s*OpenAPI/);
      await page.waitForTimeout(5000);
    });
    await scene("T02", "Open Preview, inspect actual schema provenance and explicitly refresh it.", async () => {
      const [response] = await Promise.all([schemaResponse(), click(page.locator('[data-view="preview"]'))]);
      await inspectSchema(response);
      await page.waitForTimeout(5000);
      const [refreshed] = await Promise.all([
        schemaResponse(), click(page.getByRole("button", { name: "Refresh schema", exact: true })),
      ]);
      await inspectSchema(refreshed);
      await page.waitForTimeout(3000);
    });
    await scene("T03", "Invoke 17 times23 from Form and read the actual391 result.", async () => {
      await click(page.getByRole("button", { name: "Form", exact: true }));
      await type(page.locator(".tool-tester").getByLabel(fieldLabel("expression")), EXPRESSION);
      await invoke(true);
    });
    await scene("T04", "Round-trip the same argument to Raw JSON and invoke the independent calculator again.", async () => {
      await click(page.getByRole("button", { name: "Raw JSON", exact: true }));
      const raw = page.locator(".tool-tester textarea.run-input");
      if (!isDeepStrictEqual(JSON.parse(await raw.inputValue()), { expression: EXPRESSION })) throw new Error("Form/Raw round-trip changed arguments");
      await page.waitForTimeout(3000);
      await invoke(true);
    });
    await scene("T05", "Show malformed JSON rejected locally before any Tool request.", async () => {
      await type(page.locator(".tool-tester textarea.run-input"), '{"expression":');
      const before = report.submittedInputs.length;
      await click(page.getByRole("button", { name: "Invoke", exact: true }));
      const error = page.locator(".tool-tester .preview-banner.bad");
      await expect(error).toContainText("Input JSON parse error:");
      await hold(error, "local-json-validation-error");
      if (report.submittedInputs.length !== before) throw new Error("Malformed JSON reached Tool execution");
    });
    await scene("T06", "Show the calculator's actual application-error response for an invalid expression.", async () => {
      await type(page.locator(".tool-tester textarea.run-input"), '{"expression":"17 *"}');
      await invoke(false);
    });
    if (report.calls.length !== 3 || report.submittedInputs.length !== 3) throw new Error("Unexpected invocation count");
    const after = get("tool", TOOL, APPROVED.namespace);
    verifyToolFixture(after, options.target);
    if (!isDeepStrictEqual(after.spec, tool.spec) || after.metadata.generation !== tool.metadata.generation ||
        after.status.observedDigest !== tool.status.observedDigest) throw new Error("The calculator definition changed during testing");
    report.toolAfter = after;
    report.coverage.coveredSubActions = ["S08 actual ToolClass/Tool configuration inspection",
      "S09 actual schema provenance and Refresh", "S09 Form/Raw JSON round-trip and exact391 outputs",
      "S09 local malformed JSON and the integration's explicit application error (HTTP200, not a transport failure)"];
  } catch (error) {
    failure = error;
    report.failure = error.message;
    if (page && !page.isClosed()) await page.screenshot({ path: path.join(options.output, "failure.png") })
      .catch(captureError => report.browserErrors.push(`Failure screenshot: ${captureError.message}`));
  } finally {
    if (context && browser && video) {
      try { report.recording = await finalizeRecording(context, browser, video,
        path.join(options.output, failure ? "tools.failed.webm" : "tools.recording.webm")); }
      catch (error) { report.finalizationFailure = error.message; failure ||= error; }
    } else {
      if (context) await context.close().catch(error => { failure ||= error; report.finalizationFailure = error.message; });
      if (browser) await browser.close().catch(error => { failure ||= error; report.finalizationFailure = error.message; });
    }
    try { await stopChildren(children); }
    catch (error) { report.processCleanupFailure = error.message; failure ||= error; }
    report.passed = !failure;
    report.completedAt = new Date().toISOString();
    persist();
  }
  if (failure) throw new Error(`Independent Tool capture failed: ${failure.message}`);
  console.log(`Verified independent Tool tester footage: ${report.recording.path}`);
  return report;
}

if (require.main === module) runToolTester(parseOptions(process.argv.slice(2))).catch(error => {
  console.error(error.message); process.exitCode = 1;
});

module.exports = { verifyToolFixture, verifySchemaEvidence, verifyToolResult, verifyToolFailure, isWelcomeDismissal, runToolTester };
