// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { execFileSync } = require("node:child_process");
const { isDeepStrictEqual } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const { APPROVED, RANCHER_TARGET, recordingTarget, parseOptions: sharedOptions, verifyTarget, finalizeRecording } = require("./live-authoring");
const { startUI, stopChildren } = require("./live-pilot");
const { verifyToolFixture, isWelcomeDismissal } = require("./live-tool-tester");
const { READING_POLICY, holdReadableText } = require("./reading");

const RUNTIME = "C:\\Users\\thiagosalles\\Videos\\Kognition\\full-tutorial-20261029-220040.runtime";
const PRODUCTION = path.join(RUNTIME, "full-master-20261103-203444");
const RANCHER_RUNTIME = "C:\\Users\\thiagosalles\\Videos\\Kognition\\rancher-tutorial-20261104.runtime";
const RANCHER_PRODUCTION = path.join(RANCHER_RUNTIME, "full-production-20261105");
const CLI_SHA256 = "da20389ca3cf8ef313281af37b47a8f5957ff0d3764e062d8b2ff3cbcf45db27";
const PRINCIPAL = "rehearsal-story-support";
const PRINCIPAL_UID = "0e08e151-ca83-4f51-80f5-b76488a35c52";
const GRANT = "rehearsal-story-story-calculator";
const GRANT_UID = "fd9de9ec-38b2-47e2-8c42-c9d31bb97110";
const VIEWPORT = Object.freeze({ width: 1920, height: 1080 });
const PENDING = Object.freeze([
  "S20-S23 are not claimed complete: this batch covers only the observed Tool-only subactions.",
  "True overlapping parallel execution is not demonstrated: the frozen Tool-step dispatch loop invokes Tools synchronously.",
  "No transient HTTP failure followed by recovery: the only existing grant names story-calculator, not a disposable unknown-route Tool. No grant or principal is created or broadened.",
  "No maxDuration/maxTokens enforcement claim: those WorkflowBudget declarations are not checked by this frozen execution path.",
  "The cycle token-budget example counts whitespace-delimited input words in the frozen reconciler, not model tokenizer usage or incurred model cost.",
  "No model-backed steps, new ToolClass, resilience Lease, publication, approval, authority changes, or emergent planner are exercised.",
]);

function parseOptions(args) {
  const options = sharedOptions(args);
  const runtime = options.target === RANCHER_TARGET ? RANCHER_RUNTIME : RUNTIME;
  const production = options.target === RANCHER_TARGET ? RANCHER_PRODUCTION : PRODUCTION;
  if (path.resolve(options.runtime).toLowerCase() !== path.resolve(runtime).toLowerCase()) {
    throw new Error("Only the exact frozen tutorial runtime is approved");
  }
  if (path.dirname(options.output).toLowerCase() !== production.toLowerCase() ||
      !/^workflow-variants-a[1-9]\d*$/.test(path.basename(options.output))) {
    throw new Error("Output must be a fresh workflow-variants-aN directory directly beneath the final-production root");
  }
  if (!options.runId.startsWith("full-wv-")) throw new Error("--run-id must start full-wv-");
  return options;
}

function labels(runId, targetName) {
  recordingTarget(targetName);
  return { "kognition.dev/recording-run": targetName === RANCHER_TARGET ? "full-20261105" : "full-20261103",
    "kognition.dev/recording-take": runId };
}

function calculatorStep(name, input, extra = {}) {
  return { name, type: "tool", toolRef: { name: "story-calculator" },
    agentRef: { name: PRINCIPAL }, input: JSON.stringify({ expression: input }), timeout: "5s", ...extra };
}

function variants(runId, targetName) {
  const workflow = (suffix, spec) => ({
    apiVersion: "kognition.dev/v1alpha1", kind: "Workflow",
    metadata: { name: `${runId}-${suffix}`, namespace: APPROVED.namespace, labels: labels(runId, targetName) },
    spec: { maxConcurrentRuns: 1, ...spec },
  });
  const seedOutput = '{"expression":"17 * 23","result":391}';
  const leftOutput = '{"expression":"17 + 23","result":40}';
  const rightOutput = '{"expression":"17 - 23","result":-6}';
  return [
    { id: "dataflow", scenes: ["S20", "S21", "S22"], input: "17 * 23",
      parameters: { left: "17 + 23", right: "17 - 23", extra: "no", take: runId },
      workflow: workflow("flow", {
        maxParallelSteps: 2,
        steps: [
          calculatorStep("seed", "{{ .Input }}"),
          calculatorStep("left", "", {
            input: `{"expression":{{ if eq .Steps.seed.Output ${JSON.stringify(seedOutput)} }}{{ printf "%q" .Params.left }}{{ else }}"0"{{ end }}}`,
          }),
          calculatorStep("right", "{{ .Params.right }}", { condition: 'params.extra == "no"' }),
          calculatorStep("joined", "", {
            input: `{"expression":{{ if and (eq .Steps.left.Output ${JSON.stringify(leftOutput)}) (eq .Steps.right.Output ${JSON.stringify(rightOutput)}) }}"40 + (-6)"{{ else }}"0"{{ end }}}`,
          }),
          calculatorStep("skip-edge", "1 + 1"),
          calculatorStep("skip-step", "1 + 1", { condition: 'params.extra == "yes"' }),
        ],
        edges: [
          { from: "seed", to: "left", condition: 'steps.seed.output.contains("391")' },
          { from: "seed", to: "right" },
          { from: "left", to: "joined" }, { from: "right", to: "joined" },
          { from: "seed", to: "skip-edge", condition: 'params.extra == "yes"' },
          { from: "seed", to: "skip-step" },
        ],
      }) },
    { id: "cycle", scenes: ["S22", "S23"], input: "Two bounded calculator iterations",
      parameters: { expression: "17 * 23", take: runId },
      workflow: workflow("cycle", {
        entryPoint: "loop", budgets: { maxIterations: 2 },
        steps: [calculatorStep("loop", "{{ .Params.expression }}"), calculatorStep("confirm", "391 + 1")],
        edges: [{ from: "loop", to: "confirm" }, { from: "confirm", to: "loop" }],
      }) },
    { id: "retry-deadline", scenes: ["S23"], input: "Deliberately expire the direct Tool step deadline",
      parameters: { expression: "17 * 23", take: runId },
      workflow: workflow("retry", {
        steps: [calculatorStep("expired", "{{ .Params.expression }}", {
          timeout: "1ns", retry: { maxAttempts: 2, backoff: "1s", backoffMultiplier: 1, maxBackoff: "1s" },
        })],
      }) },
    { id: "cycle-budget", scenes: ["S23"], input: "Reject an estimated input-word budget before Tool dispatch",
      parameters: { expression: "17 * 23", take: runId },
      workflow: workflow("budget", {
        entryPoint: "limited", budgets: { maxIterations: 2, maxTokensPerCycle: 1 },
        steps: [calculatorStep("limited", "{{ .Params.expression }}"), calculatorStep("unreached", "1 + 1")],
        edges: [{ from: "limited", to: "unreached" }, { from: "unreached", to: "limited" }],
      }) },
  ];
}

function verifyPrincipal(agent, grant, targetName) {
  recordingTarget(targetName);
  const principalUID = targetName === RANCHER_TARGET ? "be5f84a0-8356-4fc5-86c9-bce9c71d351e" : PRINCIPAL_UID;
  const grantUID = targetName === RANCHER_TARGET ? "e52e469a-9c34-490f-a627-1743549b0ced" : GRANT_UID;
  if (agent?.kind !== "Agent" || agent.metadata?.name !== PRINCIPAL ||
      agent.metadata.namespace !== APPROVED.namespace || agent.metadata.uid !== principalUID ||
      agent.metadata.deletionTimestamp || agent.status?.phase !== "Ready" ||
      grant?.kind !== "ToolGrant" || grant.metadata?.name !== GRANT || grant.metadata.namespace !== APPROVED.namespace ||
      grant.metadata.uid !== grantUID || grant.metadata.deletionTimestamp ||
      !isDeepStrictEqual(grant.spec, { agentRef: { name: PRINCIPAL },
        toolRef: { name: "story-calculator" }, verbs: ["invoke", "discover", "inspect"], rateLimits: { callsPerMinute: 20 } })) {
    throw new Error("The existing exact calculator principal/grant differs; no authority may be created or repaired");
  }
}

function verifyCreatedRun(run, variant) {
  const workflowName = variant.workflow.metadata.name;
  if (run?.apiVersion !== "kognition.dev/v1alpha1" || run.kind !== "WorkflowRun" ||
      run.metadata?.namespace !== APPROVED.namespace || !run.metadata.uid ||
      !new RegExp(`^${workflowName}-studio-\\d+$`).test(run.metadata.name || "") ||
      run.metadata.deletionTimestamp ||
      !isDeepStrictEqual(run.spec, { workflowRef: { name: workflowName },
        input: variant.input, parameters: variant.parameters })) {
    throw new Error("Studio did not create the exact fresh, correlated, namespaced WorkflowRun");
  }
}

function verifyResult(run, variant) {
  verifyCreatedRun(run, variant);
  const status = run.status || {};
  const actual = new Map((status.stepStatuses || []).map(step => [step.name, step]));
  const { parameters: snapshotParameters, ...snapshotBody } = status.snapshotSpec || {};
  const { parameters: templateParameters, ...templateBody } = variant.workflow.spec;
  const mergedParameters = { ...templateParameters, ...variant.parameters };
  if (!status.startTime || !status.completionTime ||
      !isDeepStrictEqual(snapshotBody, templateBody) ||
      (!isDeepStrictEqual(snapshotParameters, templateParameters) && !isDeepStrictEqual(snapshotParameters, mergedParameters))) {
    throw new Error("Run lacks a terminal, independently readable frozen spec and timestamps");
  }
  const output = (name, expression, result) => {
    const step = actual.get(name);
    if (step?.phase !== "Ready" || step.attempts !== 1 ||
        !isDeepStrictEqual(JSON.parse(step.output), { expression, result })) {
      throw new Error(`Unexpected actual calculator output/attempts for ${name}`);
    }
  };
  if (variant.id === "dataflow") {
    if (status.phase !== "Ready" || actual.size !== 6) throw new Error("Dataflow did not finish its six exact nodes");
    output("seed", "17 * 23", 391); output("left", "17 + 23", 40);
    output("right", "17 - 23", -6); output("joined", "40 + (-6)", 34);
    for (const name of ["skip-edge", "skip-step"]) {
      const step = actual.get(name);
      if (step?.phase !== "Skipped" || step.attempts || step.output) throw new Error(`${name} was not skipped before dispatch`);
    }
  } else if (variant.id === "cycle") {
    output("loop", "17 * 23", 391);
    output("confirm", "391 + 1", 392);
    if (status.phase !== "Ready" || actual.size !== 2 ||
        !isDeepStrictEqual([...status.completedSteps].sort(), ["confirm@1", "confirm@2", "loop@1", "loop@2"])) {
      throw new Error("Cycle did not stop after exactly two independently recorded iterations");
    }
  } else if (variant.id === "retry-deadline") {
    if (status.phase !== "Failed" || actual.size !== 1 || actual.get("expired")?.phase !== "Failed" ||
        actual.get("expired").attempts !== 2 || actual.get("expired").output ||
        !status.conditions?.some(condition => condition.reason === "StepFailed" && condition.status === "False")) {
      throw new Error("The deliberate deadline did not exhaust exactly two real attempts");
    }
  } else if (variant.id === "cycle-budget") {
    if (status.phase !== "Failed" || actual.size !== 0 || (status.completedSteps || []).length ||
        !status.conditions?.some(condition => condition.reason === "BudgetExceeded" &&
          condition.status === "False" && condition.message.includes("maxTokensPerCycle"))) {
      throw new Error("The input-word estimate budget did not stop the cycle before dispatch");
    }
  } else throw new Error("Unknown workflow variant");
}

function requestAllowed({ method, url, body }, state) {
  const target = new URL(url);
  if (target.origin !== state.origin || target.username || target.password) return false;
  if (["GET", "HEAD"].includes(method)) {
    if (["/api/meta", "/api/namespaces"].includes(target.pathname)) return target.search === "";
    const match = target.pathname.match(/^\/api\/resources\/([a-z]+)(?:\/([a-z0-9-]+))?$/);
    if (!match) return false;
    const descriptor = state.catalog.find(item => item.plural === match[1] && item.treatment !== "unavailable");
    if (!descriptor || target.searchParams.size !== 1 || !target.searchParams.has("namespace") ||
        target.searchParams.get("namespace") !== (descriptor.namespaced === false ? "" : APPROVED.namespace)) return false;
    if (!match[2]) return true; // Studio requires its catalog-declared sidebar lists.
    if (match[1] === "workflows") return state.workflowNames.includes(match[2]);
    if (match[1] !== "workflowruns") return false;
    if (state.runNames.includes(match[2])) return true;
    const activeName = state.expected?.workflow.metadata.name;
    return !!activeName && state.started.has(activeName) && !state.pendingRunName &&
      new RegExp(`^${activeName}-studio-\\d+$`).test(match[2]);
  }
  if (isWelcomeDismissal(method, url, body, state.dismissals)) return true;
  const expected = state.expected;
  return method === "POST" && !!expected && !state.started.has(expected.workflow.metadata.name) &&
    target.pathname === `/api/invoke/workflow/${expected.workflow.metadata.name}` &&
    target.searchParams.size === 1 && target.searchParams.get("namespace") === APPROVED.namespace &&
    isDeepStrictEqual(body, { input: expected.input, parameters: expected.parameters });
}

function verifyMedia(metadata, lastSceneEnd) {
  const streams = metadata.streams || [];
  const video = streams.find(stream => stream.codec_type === "video");
  const duration = Number(metadata.format?.duration);
  if (streams.length !== 1 || !video || video.width !== 1920 || video.height !== 1080 ||
      !["vp8", "vp9"].includes(video.codec_name) || video.avg_frame_rate !== "25/1" ||
      !Number.isFinite(duration) || duration < 20 || duration + 2 < lastSceneEnd) {
    throw new Error("Saved source is not continuous 1920x1080 25fps WebM covering all recorded scenes");
  }
}

function narrationFor(variant, run) {
  const name = run.metadata.name;
  switch (variant.id) {
    case "dataflow": return `Studio created ${name}. The direct calculator seed returned 391. ` +
      "The parameterized branches returned 40 and minus 6; the join checked both prior outputs and returned 34. " +
      "The false edge and false step condition were skipped without attempts. This is a fan-out/join graph, not proof of overlapping Tool calls.";
    case "cycle": return `Studio created ${name}. The actual run completed loop and confirm at iteration one and iteration two, ` +
      "then stopped at the configured two-iteration budget. The final calculator results were 391 and 392.";
    case "retry-deadline": return `Studio created ${name}. A deliberately tiny one-nanosecond Tool deadline exhausted ` +
      "two configured attempts. The real run ended Failed; it did not recover. This is deadline exhaustion, not the calculator's HTTP200 application error.";
    case "cycle-budget": return `Studio created ${name}. The controller reported BudgetExceeded before any Tool step ` +
      "was dispatched. On this frozen baseline maxTokensPerCycle uses a whitespace-delimited input-word estimate, not model token accounting.";
    default: throw new Error("No narration without an observed supported variant");
  }
}

async function runWorkflowVariants(input) {
  const options = parseOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  const APPROVED = recordingTarget(options.target);
  fs.mkdirSync(path.dirname(options.output), { recursive: true });
  fs.mkdirSync(options.output);
  const reportPath = path.join(options.output, "report.json");
  const report = { schemaVersion: 1, batch: "workflow-variants", runId: options.runId,
    startedAt: new Date().toISOString(), passed: false, mockedAPI: false, fullTutorialRecorded: false,
    narration: false, viewport: VIEWPORT, expectedFPS: 25, readingPolicy: READING_POLICY,
    readingHolds: [], scenes: [], runs: [], createdResources: [], browserErrors: [], requests: [],
    uiPreferenceChanges: [], offCameraOperations: [
      "Create only these labeled namespaced Workflow fixtures with explicit kubeconfig/context.",
      "Studio generates fresh run names; add recording labels to those exact new UID-bound WorkflowRuns, metadata only, after terminal settlement.",
      "Read run status, outputs and exact UID-scoped Kubernetes events independently; do not patch status, finalizers or runtime authority.",
      "Tool steps reference the unchanged existing calculator-authorized Agent solely as authorization principal. They do not invoke its model, memory or runtime.",
    ], coverage: { entireScenesCompleted: [], coveredSubActions: [], pending: [...PENDING] } };
  const persist = () => fs.writeFileSync(reportPath, JSON.stringify(report, null, 2));
  persist();
  const children = [];
  let browser, context, page, video, epoch, failure;
  const seconds = () => epoch === undefined ? 0 : (performance.now() - epoch) / 1000;
  const hold = async (locator, target) => {
    const startSeconds = seconds();
    const startAt = new Date().toISOString();
    const exposure = await holdReadableText(page, locator);
    report.readingHolds.push({ target, startAt, endAt: new Date().toISOString(),
      startSeconds, endSeconds: seconds(), ...exposure });
    persist();
  };
  const scene = async (id, description, action) => {
    const entry = { id, description, startSeconds: seconds(), startAt: new Date().toISOString(), passed: false };
    report.scenes.push(entry); persist(); console.log(`${id}: ${description}`);
    try {
      await action();
      if (report.browserErrors.length) throw new Error("Browser error or refused request; do not continue dispatch");
      entry.passed = true;
    } finally { entry.endSeconds = seconds(); entry.endAt = new Date().toISOString(); persist(); }
  };
  try {
    const cluster = JSON.parse(fs.readFileSync(path.join(options.runtime, "cluster.json"), "utf8"));
    if (path.resolve(cluster.kubeconfig).toLowerCase() !== path.join(options.runtime, "kubeconfig").toLowerCase()) {
      throw new Error("The approved runtime kubeconfig path differs");
    }
    const common = ["--kubeconfig", cluster.kubeconfig, "--context", APPROVED.context, "--request-timeout=20s"];
    const kubectl = args => JSON.parse(execFileSync("kubectl", [...common, ...args], {
      encoding: "utf8", windowsHide: true, timeout: 30000, maxBuffer: 16 * 1024 * 1024,
    }));
    const get = (kind, name, ns = APPROVED.namespace) =>
      kubectl(["get", kind, name, ...(ns ? ["-n", ns] : []), "-o", "json"]);
    const collection = kind => kubectl(["get", kind, "-n", APPROVED.namespace, "-o", "json"]).items || [];
    verifyTarget(cluster, kubectl(["config", "view", "--minify", "-o", "json"]),
      get("namespace", "kube-system", ""), get("namespace", APPROVED.namespace, ""),
      get("deployment", APPROVED.controller, APPROVED.systemNamespace), APPROVED);
    const tool = get("tool", "story-calculator");
    verifyToolFixture(tool, options.target);
    const principal = get("agent", PRINCIPAL), grant = get("toolgrant", GRANT);
    verifyPrincipal(principal, grant, options.target);
    report.approvedTarget = APPROVED;
    report.toolBefore = tool;
    report.principalBefore = { metadata: principal.metadata, spec: principal.spec };
    report.grantBefore = grant;
    const beforeJobs = new Set(collection("agentjobs").map(item => item.metadata.uid));
    const beforeInstances = new Set(collection("agentinstances").map(item => item.metadata.uid));
    const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
    report.cliSHA256 = crypto.createHash("sha256").update(fs.readFileSync(cli)).digest("hex");
    if (report.cliSHA256 !== CLI_SHA256) throw new Error("Frozen CLI hash differs");
    const cases = variants(options.runId, options.target);
    for (const variant of cases) {
      const manifest = path.join(options.output, `${variant.id}.workflow.json`);
      fs.writeFileSync(manifest, JSON.stringify(variant.workflow, null, 2), { flag: "wx" });
      const created = kubectl(["create", "-n", APPROVED.namespace, "-f", manifest, "-o", "json"]);
      if (!created.metadata?.uid || created.metadata.name !== variant.workflow.metadata.name ||
          !isDeepStrictEqual(created.spec, variant.workflow.spec) ||
          !isDeepStrictEqual(created.metadata.labels, labels(options.runId, options.target))) {
        throw new Error("Created workflow does not match the reviewed Tool-only fixture");
      }
      report.createdResources.push({ kind: "Workflow", name: created.metadata.name,
        namespace: created.metadata.namespace, uid: created.metadata.uid, manifest });
      persist();
    }
    const profile = path.join(options.output, "studio-profile");
    fs.mkdirSync(profile);
    const origin = await startUI("studio", { cli, mode: "kubernetes", namespace: APPROVED.namespace,
      "kube-context": APPROVED.context, systemNamespace: APPROVED.systemNamespace, outputDir: options.output,
      environment: { KUBECONFIG: cluster.kubeconfig, APPDATA: profile } }, undefined, children);
    report.studio = { origin, profile, processIDs: children.map(child => child.pid),
      explicitKubeconfigEnvironment: cluster.kubeconfig, explicitContextFlag: APPROVED.context };
    persist();
    const metaResponse = await fetch(`${origin}/api/meta`, { signal: AbortSignal.timeout(30000) });
    const meta = await metaResponse.json();
    if (!metaResponse.ok || meta.mode !== "kubernetes" || meta.namespace !== APPROVED.namespace ||
        meta.environment === "practice") throw new Error("Studio is not responsive on the exact live namespace");
    const catalogResponse = await fetch(`${origin}/resource-catalog.json`, { signal: AbortSignal.timeout(30000) });
    if (!catalogResponse.ok) throw new Error("Cannot determine the real Studio sidebar read contract");
    const catalog = await catalogResponse.json();
    if (!Array.isArray(catalog) || !catalog.some(item => item.plural === "workflows") ||
        !catalog.some(item => item.plural === "workflowruns")) throw new Error("Unexpected resource catalog");
    const guard = { origin, catalog, workflowNames: cases.map(item => item.workflow.metadata.name),
      runNames: [], dismissals: 0, expected: null, started: new Set() };
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport: VIEWPORT,
      recordVideo: { dir: options.output, size: VIEWPORT, showActions: { duration: 1000, fontSize: 18, cursor: "pointer" } } });
    await context.route("**/*", async route => {
      const request = route.request(), url = new URL(request.url());
      const isAPI = url.pathname.startsWith("/api/");
      let body;
      try { if (!["GET", "HEAD"].includes(request.method())) body = request.postDataJSON(); }
      catch { /* Invalid JSON is refused by the exact-body guard. */ }
      const allowed = isAPI ? requestAllowed({ method: request.method(), url: request.url(), body }, guard) :
        url.origin === origin && request.method() === "GET" && !url.search &&
        (url.pathname === "/" || /^\/[a-z0-9._-]+\.(?:js|css|json|svg|ico|woff2?)$/.test(url.pathname));
      if (!allowed) {
        report.browserErrors.push(`Refused ${request.method()} ${url.origin}${url.pathname}${url.search}`);
        persist(); await route.abort(); return;
      }
      if (request.method() === "POST") {
        if (url.pathname === "/api/studio/tutorial-dismiss") {
          guard.dismissals++;
          report.uiPreferenceChanges.push({ action: "dismiss-welcome", profile, workloadMutation: false });
        } else {
          guard.started.add(guard.expected.workflow.metadata.name);
          report.requests.push({ method: request.method(), path: url.pathname, body, seconds: seconds() });
        }
        persist();
      }
      if (request.method() === "GET" && url.pathname.startsWith("/api/resources/workflowruns/")) {
        const runName = url.pathname.split("/").at(-1);
        if (!guard.runNames.includes(runName)) {
          guard.pendingRunName = runName;
          guard.runNames.push(runName);
        }
      }
      await route.continue();
    });
    page = await context.newPage(); video = page.video(); epoch = performance.now();
    page.setDefaultTimeout(45000);
    page.on("pageerror", error => { report.browserErrors.push(error.message); persist(); });
    page.on("response", response => {
      const url = new URL(response.url());
      if (url.pathname.startsWith("/api/")) report.requests.push({
        method: response.request().method(), path: url.pathname, status: response.status(), seconds: seconds(),
      });
    });
    const click = async locator => { await locator.scrollIntoViewIfNeeded(); await locator.click(); };
    const type = async (locator, value) => { await locator.fill(""); await locator.pressSequentially(value, { delay: 18 }); };
    await scene("WV00", "Enter actual Studio and dismiss only the recording-owned welcome preference.", async () => {
      await page.goto(origin, { waitUntil: "domcontentloaded" });
      await expect(page.locator("#status")).toHaveText("ready", { timeout: 60000 });
      const welcome = page.locator(".tutorial-welcome");
      await expect(welcome).toBeVisible();
      const [response] = await Promise.all([
        page.waitForResponse(response => response.request().method() === "POST" &&
          new URL(response.url()).pathname === "/api/studio/tutorial-dismiss"),
        welcome.getByRole("button", { name: "Not now", exact: true }).press("Enter"),
      ]);
      if (!response.ok() || (await response.json()).dismissed !== true) throw new Error("Welcome dismissal was not persisted");
      await expect(welcome).toHaveCount(0);
    });
    for (const variant of cases) {
      const evidence = { id: variant.id, sceneContracts: variant.scenes, workflow: variant.workflow, passed: false };
      report.runs.push(evidence); persist();
      await scene(`WV-${variant.id}-graph`, `Inspect the real ${variant.id} Tool-only workflow graph.`, async () => {
        const item = page.locator("#list-workflows").getByText(variant.workflow.metadata.name, { exact: true });
        const group = item.locator("xpath=ancestor::details[1]");
        if (await group.count() && await group.getAttribute("open") === null) await click(group.locator("summary"));
        await click(item);
        await click(page.locator('[data-view="builder"]'));
        await expect(page.locator("#wf-canvas")).toBeVisible();
        await hold(page.locator("#wf-canvas"), `${variant.id}-graph`);
      });
      let created;
      await scene(`WV-${variant.id}-start`, `Type fresh ${variant.id} inputs and start its actual WorkflowRun on screen.`, async () => {
        await click(page.locator('[data-view="preview"]'));
        const form = page.locator("#preview-host .run-form").first();
        await type(form.locator("textarea.run-input"), variant.input);
        for (const [key, value] of Object.entries(variant.parameters)) {
          await click(form.getByRole("button", { name: "+ add parameter", exact: true }));
          const row = form.locator(".run-param-row").last();
          await type(row.getByPlaceholder("key", { exact: true }), key);
          await type(row.getByPlaceholder("value", { exact: true }), value);
        }
        await hold(form.locator(".run-section-title"), `${variant.id}-inputs-completed`);
        guard.expected = variant;
        guard.pendingRunName = null;
        const [response] = await Promise.all([
          page.waitForResponse(response => response.request().method() === "POST" &&
            new URL(response.url()).pathname === `/api/invoke/workflow/${variant.workflow.metadata.name}`, { timeout: 60000 }),
          click(form.getByRole("button", { name: "Run workflow", exact: true })),
        ]);
        if (!response.ok()) throw new Error(`Studio workflow start HTTP${response.status()}: ${await response.text()}`);
        created = await response.json(); verifyCreatedRun(created, variant);
        if (guard.pendingRunName && guard.pendingRunName !== created.metadata.name) {
          throw new Error("The generated run poll did not correlate with the actual Studio response");
        }
        if (!guard.runNames.includes(created.metadata.name)) guard.runNames.push(created.metadata.name);
        evidence.created = created;
        report.createdResources.push({ kind: "WorkflowRun", name: created.metadata.name,
          namespace: created.metadata.namespace, uid: created.metadata.uid, startedThroughStudio: true });
        persist();
      });
      await scene(`WV-${variant.id}-result`, `Read the actual ${variant.id} run, steps and terminal evidence.`, async () => {
        const terminal = variant.id === "dataflow" || variant.id === "cycle" ? "Ready" : "Failed";
        await expect(page.locator("#preview-host .run-status > div > .run-phase-row > .run-phase"))
          .toHaveText(terminal, { timeout: 90000 });
        const run = get("workflowrun", created.metadata.name);
        if (run.metadata.uid !== created.metadata.uid) throw new Error("Generated run UID changed");
        evidence.independentReadback = run;
        fs.writeFileSync(path.join(options.output, `${variant.id}.run.json`), JSON.stringify(run, null, 2), { flag: "wx" });
        persist();
        const labeled = kubectl(["label", "workflowrun", run.metadata.name, "-n", APPROVED.namespace,
          ...Object.entries(labels(options.runId, options.target)).map(([key, value]) => `${key}=${value}`),
          `--resource-version=${run.metadata.resourceVersion}`, "-o", "json"]);
        if (labeled.metadata.uid !== created.metadata.uid ||
            !Object.entries(labels(options.runId, options.target)).every(([key, value]) => labeled.metadata.labels?.[key] === value)) {
          throw new Error("The exact generated run lacks its recording labels");
        }
        evidence.independentReadback = labeled;
        persist();
        verifyResult(run, variant);
        const events = kubectl(["get", "events", "-n", APPROVED.namespace, "--field-selector",
          `involvedObject.kind=WorkflowRun,involvedObject.name=${run.metadata.name},involvedObject.uid=${run.metadata.uid}`, "-o", "json"]);
        evidence.events = events.items;
        if (variant.id === "retry-deadline" &&
            (!events.items.some(event => event.reason === "StepRetrying") ||
              !events.items.some(event => event.reason === "ToolStepFailed" && /context deadline exceeded|deadline/i.test(event.message)))) {
          throw new Error("Real Kubernetes events do not prove retry plus deadline exhaustion");
        }
        fs.writeFileSync(path.join(options.output, `${variant.id}.events.json`), JSON.stringify(events, null, 2), { flag: "wx" });
        await hold(page.locator("#preview-host .run-status").locator(".run-phase-row").first(), `${variant.id}-terminal`);
        const statuses = run.status.stepStatuses || [];
        for (const step of statuses) {
          const card = page.locator("#preview-host .run-step-card").filter({
            has: page.locator(".run-step-name").filter({ hasText: new RegExp(`^${step.name}$`) }),
          });
          await expect(card).toHaveCount(1);
          await expect(card.locator(".run-step-card-head .run-phase")).toHaveText(step.phase);
          if (step.attempts > 1) await expect(card).toContainText(`attempt ${step.attempts}`);
          if (step.output) {
            const details = card.locator(".run-step-output");
            if (await details.getAttribute("open") === null) await click(details.locator("summary"));
            await expect(details.locator("pre")).toHaveText(step.output);
            await hold(details.locator("pre"), `${variant.id}-${step.name}-output`);
          } else await hold(card, `${variant.id}-${step.name}-${step.phase}`);
        }
        if (terminal === "Failed") {
          const error = page.locator("#preview-host .run-status .preview-banner.bad");
          await expect(error).toContainText(variant.id === "cycle-budget" ? "BudgetExceeded" : "StepFailed");
          await hold(error, `${variant.id}-actual-error-condition`);
        }
        await click(page.getByRole("button", { name: "Open run in sidebar", exact: true }));
        await expect(page.locator("#builder-host")).toContainText(run.metadata.name);
        await hold(page.locator("#builder-host .run-phase-row").first(), `${variant.id}-correlated-run-viewer`);
        evidence.narrationDraft = narrationFor(variant, run);
        evidence.passed = true;
        report.coverage.coveredSubActions.push(`${variant.scenes.join("/")} ${variant.id}: independently verified ${run.status.phase}; ${run.metadata.name}`);
        await page.screenshot({ path: path.join(options.output, `${variant.id}.png`) });
      });
    }
    const toolAfter = get("tool", "story-calculator"), principalAfter = get("agent", PRINCIPAL), grantAfter = get("toolgrant", GRANT);
    verifyToolFixture(toolAfter, options.target); verifyPrincipal(principalAfter, grantAfter, options.target);
    for (const [before, after] of [[tool, toolAfter], [principal, principalAfter], [grant, grantAfter]]) {
      if (before.metadata.uid !== after.metadata.uid || before.metadata.generation !== after.metadata.generation ||
          !isDeepStrictEqual(before.spec, after.spec)) throw new Error("An existing Tool, principal or grant changed during capture");
    }
    const newJobs = collection("agentjobs").filter(item => !beforeJobs.has(item.metadata.uid));
    const newInstances = collection("agentinstances").filter(item => !beforeInstances.has(item.metadata.uid));
    if (newJobs.length || newInstances.length) throw new Error("Unexpected AgentJob/AgentInstance creation during the zero-model batch");
    report.zeroModelEvidence = { allStepsExplicitlyTool: true, directModelOrAgentUIRequests: 0,
      newAgentJobs: 0, newAgentInstances: 0, existingPrincipalAndGrantUnchanged: true };
    report.dispatchSettledAt = new Date().toISOString();
    report.allOwnedRunsTerminal = true;
    persist();
    console.log(`DISPATCH SETTLED: ${report.dispatchSettledAt}; all ${cases.length} owned WorkflowRuns terminal; zero AgentJobs/AgentInstances.`);
  } catch (error) {
    failure = error; report.failure = error.message; persist();
    if (page && !page.isClosed()) await page.screenshot({ path: path.join(options.output, "failure.png") })
      .catch(captureError => { report.browserErrors.push(`Failure screenshot: ${captureError.message}`); });
  } finally {
    if (context && browser && video) {
      try {
        report.recording = await finalizeRecording(context, browser, video,
          path.join(options.output, failure ? "workflows.failed.webm" : "workflows.recording.webm"));
      } catch (error) { report.finalizationFailure = error.message; failure ||= error; }
    } else {
      if (context) await context.close().catch(error => { failure ||= error; report.finalizationFailure = error.message; });
      if (browser) await browser.close().catch(error => { failure ||= error; report.finalizationFailure = error.message; });
    }
    try { await stopChildren(children); report.uiChildrenStopped = true; }
    catch (error) { report.processCleanupFailure = error.message; failure ||= error; }
    if (!failure && report.recording) {
      try {
        const mediaRoot = path.resolve(__dirname, "..", "..", ".demo-preparation", "video-production", "node_modules");
        const probe = path.join(mediaRoot, "ffprobe-static", "bin", "win32", "x64", "ffprobe.exe");
        const ffmpeg = path.join(mediaRoot, "ffmpeg-static", "ffmpeg.exe");
        const metadata = JSON.parse(execFileSync(probe,
          ["-v", "error", "-show_streams", "-show_format", "-of", "json", report.recording.path],
          { encoding: "utf8", windowsHide: true, timeout: 30000 }));
        verifyMedia(metadata, report.scenes.at(-1)?.endSeconds || 0);
        const decode = execFileSync(ffmpeg,
          ["-nostdin", "-v", "error", "-i", report.recording.path, "-map", "0:v:0", "-f", "null", "-"],
          { encoding: "utf8", windowsHide: true, timeout: 300000, stdio: ["ignore", "pipe", "pipe"] });
        report.media = { probe: metadata, fullDecode: true, decodeStdout: decode,
          sourceSHA256: crypto.createHash("sha256").update(fs.readFileSync(report.recording.path)).digest("hex") };
      } catch (error) { report.mediaVerificationFailure = error.message; failure ||= error; }
    }
    report.passed = !failure && report.allOwnedRunsTerminal === true && report.runs.length === 4 &&
      report.runs.every(run => run.passed) && report.media?.fullDecode === true && report.uiChildrenStopped === true;
    report.completedAt = new Date().toISOString(); persist();
  }
  if (failure || !report.passed) throw new Error(`Workflow variants capture failed: ${failure?.message || "incomplete evidence"}; report: ${reportPath}`);
  console.log(`Verified source: ${report.recording.path}\nReport: ${reportPath}`);
  return report;
}

if (require.main === module) runWorkflowVariants(parseOptions(process.argv.slice(2))).catch(error => {
  console.error(error.message); process.exitCode = 1;
});

module.exports = { RUNTIME, PRODUCTION, RANCHER_RUNTIME, RANCHER_PRODUCTION, CLI_SHA256, parseOptions, variants, labels, verifyPrincipal,
  verifyCreatedRun, verifyResult, requestAllowed, verifyMedia, narrationFor, runWorkflowVariants };
