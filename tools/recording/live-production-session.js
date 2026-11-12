// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { execFile } = require("node:child_process");
const { promisify, isDeepStrictEqual } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const { APPROVED, RANCHER_TARGET, BASELINE_V3_TARGET, BASELINE_V4_TARGET, recordingTarget, verifyRecordingCLI,
  parseOptions, verifyTarget, verifyController, finalizeRecording } = require("./live-authoring");
const { startUI, stopChildren } = require("./live-pilot");
const { READING_POLICY, holdReadableText } = require("./reading");

const execute = promisify(execFile);
const API = "kognition.dev/v1alpha1";
const CLI_SHA256 = "da20389ca3cf8ef313281af37b47a8f5957ff0d3764e062d8b2ff3cbcf45db27";
const STUDIO_UI_V1 = Object.freeze({
  revision: "create-picker-v1",
  cliSHA256: "c35cedf3ab2f6181214fe04f7ea8d2a93ac5bed579797fda8a95ec9531da4dd0",
  qualificationSHA256: "de4b6a88f930175758ad7c79f7f8703bf7aa8034246a7c12d9a2ba93dffa1e60",
});
const WORKFLOW_UI_V1 = Object.freeze({
  revision: "workflow-memory-v1",
  directory: "studio-ui-workflow-20261106-v1",
  cliSHA256: "7bb9dd1f0ea8d168b30609f695fb3bf30b8d6739b1cf1e0f5062b835c16d0445",
  qualificationSHA256: "58a1db53b58a4530626e3c9393310d4bfc7583aebb5901c53c19c9f7f7b7a4f8",
  originalAssetSHA256: "2b0328233557b0ff2c03595cc3a9c37a7200fe11242a79bad988f87e75a94b19",
  assetSHA256: "8811a059d4a15acd123f8f5c98bab5f4ecc93129dd640fd43d5652e9b9503e6a",
});
const VIEWPORT = { width: 1920, height: 1080 };
const NAMESPACES = {
  [APPROVED.namespace]: APPROVED.namespaceUID,
  "tutorial-engineering": "7b571eaa-2329-42f1-84f5-46a6d93aea70",
};
const RANCHER_NAMESPACES = Object.freeze({
  "tutorial-support": "d85c34f7-69d3-45ad-919a-495d6d3a0bc4",
  "tutorial-engineering": "b224d60f-d101-4100-89bc-d2c8a286b1e0",
});
const BASELINE_V3_NAMESPACES = Object.freeze({
  "tutorial-support": "16dd8b57-eeb1-4cf9-a1db-d2847691719b",
  "tutorial-engineering": "e24dc792-f76f-42cc-926e-40ec680e055d",
});
const BASELINE_V4_NAMESPACES = Object.freeze({
  "tutorial-support": "6f6d7a87-049f-4945-8464-bbcf1614980a",
  "tutorial-engineering": "6daccb66-5757-4d42-ba4b-0e178159c257",
});

function verifyStudioUI(proof, cliSHA256, qualificationSHA256, target) {
  if (target !== RANCHER_TARGET || proof.passed !== true || proof.backendSourceChanges !== 0 ||
      proof.clusterImageChanges !== 0 || proof.frozenCLISHA256 !== CLI_SHA256 ||
      cliSHA256 !== STUDIO_UI_V1.cliSHA256 || proof.cliSHA256 !== cliSHA256 ||
      qualificationSHA256 !== STUDIO_UI_V1.qualificationSHA256 ||
      !Array.isArray(proof.changedFiles) || proof.changedFiles.length !== 4 ||
      !["app.js", "index.html", "resources.js", "style.css"].every(name =>
        proof.changedFiles.filter(file => path.basename(file.relative) === name).length === 1)) {
    throw new Error("Recording UI must be the exact qualified asset-only Rancher derivative");
  }
}

function verifyWorkflowStudioUI(proof, cliSHA256, qualificationSHA256, target) {
  const baseline = recordingTarget(BASELINE_V3_TARGET);
  const asset = proof.changedFiles?.[0];
  const live = proof.nativeLiveAuthoring;
  if (target !== BASELINE_V3_TARGET || proof.target !== target || proof.passed !== true ||
      proof.revision !== WORKFLOW_UI_V1.revision || proof.backendSourceChanges !== 0 ||
      proof.clusterImageChanges !== 0 || proof.frozenCLISHA256 !== baseline.cliSHA256 ||
      proof.baselineSourceSHA256 !== baseline.baselineSourceSHA256 ||
      cliSHA256 !== WORKFLOW_UI_V1.cliSHA256 || proof.cliSHA256 !== cliSHA256 ||
      qualificationSHA256 !== WORKFLOW_UI_V1.qualificationSHA256 ||
      !Array.isArray(proof.changedFiles) || proof.changedFiles.length !== 1 ||
      asset.relative !== "cmd\\kognitionctl\\cmd\\studio_assets\\app.js" ||
      asset.originalSHA256 !== WORKFLOW_UI_V1.originalAssetSHA256 ||
      asset.replacementSHA256 !== WORKFLOW_UI_V1.assetSHA256 ||
      proof.browser?.passed !== 77 || proof.browser.skipped !== 1 ||
      live?.passed !== true || live.mockedAPI !== false || live.providerInvocations !== 0 ||
      !live.createdWorkflowUID || !Number.isSafeInteger(live.projectedTokensBefore) ||
      live.projectedTokensBefore < 0 || live.projectedTokensBefore !== live.projectedTokensAfter) {
    throw new Error("Recording UI must be the exact qualified single-asset Workflow derivative");
  }
}

function recordingNamespaces(targetName) {
  recordingTarget(targetName);
  if (targetName === BASELINE_V3_TARGET) return BASELINE_V3_NAMESPACES;
  if (targetName === BASELINE_V4_TARGET) return BASELINE_V4_NAMESPACES;
  return targetName === RANCHER_TARGET ? RANCHER_NAMESPACES : NAMESPACES;
}
const PLURALS = {
  Agent: "agents", AgentRole: "agentroles", AgentRoleBinding: "agentrolebindings",
  Tool: "tools", ToolGrant: "toolgrants", ModelGrant: "modelgrants", MemoryGrant: "memorygrants",
  Prompt: "prompts", KnowledgeBase: "knowledgebases", KnowledgeBaseClaim: "knowledgebaseclaims",
  KnowledgeBaseIndexRun: "knowledgebaseindexruns", KnowledgeBaseIndexSchedule: "knowledgebaseindexschedules",
  Memory: "memories", MemoryClaim: "memoryclaims", MemoryRecord: "memoryrecords",
  TrustProfile: "trustprofiles", AutonomyReview: "autonomyreviews",
  AgentCommunicationPolicy: "agentcommunicationpolicies", Workflow: "workflows", WorkflowRun: "workflowruns",
  ApprovalDecision: "approvaldecisions", Evaluation: "evaluations", EvaluationDataset: "evaluationdatasets",
  EvaluationRun: "evaluationruns", Solution: "solutions", AgentService: "agentservices",
  IntentGateway: "intentgateways", AgentJob: "agentjobs", CognitiveQuota: "cognitivequotas",
  CognitiveLimitRange: "cognitivelimitranges", AgentPolicy: "agentpolicies",
};

function normalizedSpec(spec) {
  const result = structuredClone(spec);
  if (isDeepStrictEqual(result?.probes, {})) delete result.probes;
  for (const empty of [{}, { steps: null }, { steps: [] }]) {
    if (isDeepStrictEqual(result?.restartLadder, empty)) delete result.restartLadder;
  }
  return result;
}

function verifyIntent(actual, expected) {
  if (actual?.apiVersion !== API || actual.kind !== expected.kind ||
      actual.metadata?.name !== expected.metadata.name || actual.metadata.namespace !== expected.metadata.namespace ||
      (expected.metadata.uid && actual.metadata.uid !== expected.metadata.uid) ||
      !isDeepStrictEqual(normalizedSpec(actual.spec), normalizedSpec(expected.spec)) ||
      !isDeepStrictEqual(actual.rules, expected.rules)) {
    throw new Error("The actual resource differs from the intended kind, identity or specification");
  }
}

function assertResourceScope(object, policy) {
  const namespaces = recordingNamespaces(policy.target);
  if (!PLURALS[object?.kind] || !policy.kinds.includes(object.kind) ||
      object.apiVersion !== API || !namespaces[policy.namespace || APPROVED.namespace] ||
      object.metadata?.namespace !== (policy.namespace || APPROVED.namespace) ||
      !/^[a-z0-9][a-z0-9-]{0,62}$/.test(object.metadata.name || "") ||
      object.metadata.deletionTimestamp) throw new Error("Resource exceeds the explicit recording scope");
  const owned = object.metadata.name.startsWith("full-") &&
    object.metadata.labels?.["kognition.dev/recording-run"] === policy.runId;
  const pinned = policy.existing?.[`${object.kind}/${object.metadata.name}`];
  if (!owned && (!pinned || object.metadata.uid !== pinned)) {
    throw new Error("Only fresh full-prefixed resources or exact approved existing UIDs may be written");
  }
}

function verifyStartedWorkflow(run, workflow, input, parameters, namespace) {
  const expectedSpec = { workflowRef: { name: workflow.metadata.name },
    ...(input === "" ? {} : { input }), ...(Object.keys(parameters).length ? { parameters } : {}) };
  if (run?.apiVersion !== API || run.kind !== "WorkflowRun" ||
      run.metadata?.namespace !== namespace || !run.metadata.uid ||
      !new RegExp(`^${workflow.metadata.name}-studio-[0-9]+$`).test(run.metadata.name || "") ||
      run.metadata.deletionTimestamp ||
      !isDeepStrictEqual(run.spec, expectedSpec)) {
    throw new Error("Studio did not create the exact fresh workflow invocation");
  }
}

async function prepareReviewedEditor(page, kind, create) {
  if (create) {
    await page.locator("#btn-create").click();
    const action = page.locator(`#btn-new-${kind.toLowerCase()}`);
    await expect(action).toHaveCount(1);
    await action.click();
  }
  await page.locator('[data-view="builder"]').click();
  const allFields = page.getByRole("button", { name: "All API fields", exact: true });
  if (await allFields.isVisible()) await allFields.click();
}

async function recordingYAML(page, object) {
  const text = await page.evaluate(value => toYaml(value), object);
  if (typeof text !== "string" || !/^apiVersion:\s/m.test(text) || !/^kind:\s/m.test(text) ||
      /^[\s]*[{[]/.test(text)) {
    throw new Error("Recording manifests must use Studio's readable block-style YAML");
  }
  return text;
}

async function selectRecordedResource(page, plural, name) {
  const explorer = page.locator("#btn-explorer");
  if (await explorer.isVisible() && await explorer.getAttribute("aria-expanded") !== "true") {
    await explorer.click();
  }
  const list = page.locator(`#list-${plural}`);
  const details = list.locator("xpath=ancestor::details[1]");
  if (await details.count() && await details.getAttribute("open") === null) {
    await details.locator("summary").click();
  }
  const [refreshed] = await Promise.all([
    page.waitForResponse(item => item.request().method() === "GET" &&
      new URL(item.url()).pathname === `/api/resources/${plural}`),
    list.locator("..").locator("button.refresh").click(),
  ]);
  if (!refreshed.ok()) throw new Error(`Actual ${plural} sidebar refresh failed (HTTP${refreshed.status()})`);
  const [selected] = await Promise.all([
    page.waitForResponse(item => item.request().method() === "GET" &&
      new URL(item.url()).pathname === `/api/resources/${plural}/${name}`),
    list.getByText(name, { exact: true }).click(),
  ]);
  if (!selected.ok()) throw new Error(`Actual ${plural}/${name} selection failed (HTTP${selected.status()})`);
}

function observedResourceView(page, plural) {
  if (["workflowruns", "evaluationruns"].includes(plural)) {
    return page.locator("#builder-host .run-panel");
  }
  return page.locator("#builder-host details").filter({
    has: page.locator("summary").filter({ hasText: /^Observed status/ }),
  }).locator("pre");
}

function parseContinuationOptions(args) {
  const index = args.indexOf("--continue-from");
  if (index === -1) return parseOptions(args);
  if (args.lastIndexOf("--continue-from") !== index || !args[index + 1] || args[index + 1].startsWith("--")) {
    throw new Error("--continue-from requires one preserved failed report");
  }
  const options = parseOptions([...args.slice(0, index), ...args.slice(index + 2)]);
  options.continuationPath = path.resolve(args[index + 1]);
  return options;
}

function readContinuation(options, batch, prefix) {
  if (!options.continuationPath) return undefined;
  const file = path.resolve(options.continuationPath);
  const relative = path.relative(options.runtime, file);
  if (!relative || relative.startsWith(`..${path.sep}`) || relative === ".." || path.isAbsolute(relative) ||
      path.basename(file) !== "report.json" || !path.basename(path.dirname(file)).startsWith(prefix)) {
    throw new Error("Continuation must be a preserved recording report inside the exact runtime");
  }
  const content = fs.readFileSync(file);
  const report = JSON.parse(content);
  if (report.passed !== false || !report.failure || report.batch !== batch || report.runId !== options.runId ||
      !isDeepStrictEqual(report.approvedTarget, recordingTarget(options.target)) ||
      !report.recording?.bytes || path.dirname(path.resolve(report.recording.path)) !== path.dirname(file) ||
      fs.statSync(report.recording.path).size !== report.recording.bytes) {
    throw new Error("Continuation report identity, failed state or original recording differs");
  }
  return { report, reportPath: file, reportSHA256: crypto.createHash("sha256").update(content).digest("hex"),
    sourceVideoSHA256: crypto.createHash("sha256").update(fs.readFileSync(report.recording.path)).digest("hex") };
}

class ProductionSession {
  constructor(options, policy) {
    this.options = options;
    this.target = recordingTarget(options.target);
    this.policy = { ...policy, target: options.target, runId: options.runId };
    this.namespace = policy.namespace || APPROVED.namespace;
    this.children = [];
    this.serial = 0;
    this.report = { schemaVersion: 1, runId: options.runId, batch: policy.batch,
      startedAt: new Date().toISOString(), passed: false, mockedAPI: false, fullTutorialRecorded: false,
      narration: false, viewport: VIEWPORT, readingPolicy: READING_POLICY, scenes: [], readingHolds: [],
      created: [], writes: [], calls: [], evidence: [], browserErrors: [], requests: [], uiPreferenceChanges: [] };
  }

  persist() {
    fs.writeFileSync(path.join(this.options.output, "report.json"), JSON.stringify(this.report, null, 2));
  }

  seconds() { return this.epoch === undefined ? 0 : (performance.now() - this.epoch) / 1000; }

  async command(executable, args) {
    const result = await execute(executable, args, { env: this.environment, encoding: "utf8",
      windowsHide: true, timeout: 30000, maxBuffer: 8 * 1024 * 1024 });
    return result.stdout;
  }

  async kube(args) { return this.command("kubectl", [...this.common, ...args]); }

  async get(kind, name, namespace = this.namespace, allowMissing = false) {
    const text = await this.kube(["get", kind, name, ...(namespace ? ["-n", namespace] : []),
      ...(allowMissing ? ["--ignore-not-found=true"] : []), "-o", "json"]);
    return text.trim() ? JSON.parse(text) : null;
  }

  async list(kind, selector, namespace = this.namespace) {
    return JSON.parse(await this.kube(["get", kind, "-n", namespace,
      ...(selector ? ["-l", selector] : []), "-o", "json"])).items;
  }

  async wait(label, action, timeout = 180000) {
    const deadline = performance.now() + timeout;
    while (performance.now() < deadline) {
      const result = await action();
      if (result) return result;
      await new Promise(resolve => setTimeout(resolve, 1200));
    }
    throw new Error(`Timed out waiting for ${label}`);
  }

  async initialize() {
    const APPROVED = this.target;
    const namespaces = recordingNamespaces(this.options.target);
    fs.mkdirSync(path.dirname(this.options.output), { recursive: true });
    fs.mkdirSync(this.options.output);
    this.persist();
    const cluster = JSON.parse(fs.readFileSync(path.join(this.options.runtime, "cluster.json"), "utf8"));
    this.common = ["--kubeconfig", cluster.kubeconfig, "--context", APPROVED.context, "--request-timeout=20s"];
    this.environment = { ...process.env, KUBECONFIG: cluster.kubeconfig };
    verifyTarget(cluster, JSON.parse(await this.kube(["config", "view", "--minify", "-o", "json"])),
      await this.get("namespace", "kube-system", ""), await this.get("namespace", APPROVED.namespace, ""),
      await this.get("deployment", APPROVED.controller, APPROVED.systemNamespace), APPROVED);
    if (!namespaces[this.namespace] ||
        (await this.get("namespace", this.namespace, "")).metadata.uid !== namespaces[this.namespace]) {
      throw new Error("Recording namespace is not the exact approved workload namespace");
    }
    this.report.namespace = this.namespace;
    this.report.namespaceUID = namespaces[this.namespace];
    this.report.approvedTarget = APPROVED;
    if (APPROVED.toolGateway) {
      const gateway = await this.get("deployment", APPROVED.toolGateway, APPROVED.systemNamespace);
      verifyController(gateway, { ...APPROVED, controller: APPROVED.toolGateway,
        controllerUID: APPROVED.toolGatewayUID, controllerGeneration: APPROVED.toolGatewayGeneration,
        controllerImage: APPROVED.toolGatewayImage });
      this.report.verifiedToolGateway = gateway;
    }
    this.cli = path.join(this.options.runtime, "windows", "kognitionctl.exe");
    const digest = crypto.createHash("sha256").update(fs.readFileSync(this.cli)).digest("hex");
    verifyRecordingCLI(digest, APPROVED);
    this.report.cliSHA256 = digest;
    let studioCLI = this.cli;
    if (this.policy.uiRevision !== undefined) {
      if (this.policy.uiRevision === WORKFLOW_UI_V1.revision) {
        const directory = path.join(this.options.runtime, WORKFLOW_UI_V1.directory);
        const proofBytes = fs.readFileSync(path.join(directory, "qualification.json"));
        studioCLI = path.join(directory, "kognitionctl.exe");
        const uiDigest = crypto.createHash("sha256").update(fs.readFileSync(studioCLI)).digest("hex");
        const proofDigest = crypto.createHash("sha256").update(proofBytes).digest("hex");
        verifyWorkflowStudioUI(JSON.parse(proofBytes.toString("utf8").replace(/^\uFEFF/, "")),
          uiDigest, proofDigest, this.options.target);
        this.report.studioUI = { ...WORKFLOW_UI_V1, path: studioCLI,
          baselineSourceSHA256: APPROVED.baselineSourceSHA256, originalFrozenCLIPreserved: true,
          assetOnlyDerivative: true };
      } else if (this.policy.uiRevision !== STUDIO_UI_V1.revision) {
        throw new Error("Unknown recording UI revision");
      } else if ([BASELINE_V3_TARGET, BASELINE_V4_TARGET].includes(this.options.target)) {
        if (this.policy.uiRevision !== APPROVED.uiRevision) throw new Error("UI revision differs from the frozen baseline");
        this.report.studioUI = { revision: APPROVED.uiRevision, cliSHA256: digest,
          baselineSourceSHA256: APPROVED.cliSourceSHA256 || APPROVED.baselineSourceSHA256, assetOnlyDerivative: false };
      } else {
        const directory = path.join(this.options.runtime, "studio-ui-20261105-v1");
        const proofBytes = fs.readFileSync(path.join(directory, "qualification.json"));
        studioCLI = path.join(directory, "kognitionctl.exe");
        const uiDigest = crypto.createHash("sha256").update(fs.readFileSync(studioCLI)).digest("hex");
        const proofDigest = crypto.createHash("sha256").update(proofBytes).digest("hex");
        verifyStudioUI(JSON.parse(proofBytes.toString("utf8").replace(/^\uFEFF/, "")),
          uiDigest, proofDigest, this.options.target);
        this.report.studioUI = { ...STUDIO_UI_V1, path: studioCLI, originalFrozenCLIPreserved: true };
      }
    }
    const profile = path.join(this.options.output, "studio-profile");
    fs.mkdirSync(profile);
    this.origin = await startUI("studio", { cli: studioCLI, mode: "kubernetes", namespace: this.namespace,
      "kube-context": APPROVED.context, systemNamespace: APPROVED.systemNamespace,
      outputDir: this.options.output, environment: { KUBECONFIG: cluster.kubeconfig, APPDATA: profile } },
    undefined, this.children);
    const metadata = await fetch(`${this.origin}/api/meta`, { signal: AbortSignal.timeout(30000) });
    if (!metadata.ok) throw new Error("Live Studio did not become responsive");
    const meta = await metadata.json();
    if (meta.mode !== "kubernetes" || meta.namespace !== this.namespace) throw new Error("Studio scope differs");
    this.report.studio = this.origin;
    this.report.uiProcessIDs = this.children.map(child => child.pid);
    this.report.uiPreferenceProfile = profile;
    this.persist();
    this.browser = await chromium.launch({ headless: true });
    this.context = await this.browser.newContext({ viewport: VIEWPORT, serviceWorkers: "block",
      recordVideo: { dir: this.options.output, size: VIEWPORT,
        showActions: { duration: 1000, fontSize: 18, cursor: "pointer" } } });
    await this.context.route("**/api/**", async route => {
      const request = route.request();
      const url = new URL(request.url());
      try {
        if (url.origin !== this.origin) throw new Error("Non-local UI API target refused");
        if (!["GET", "HEAD"].includes(request.method())) {
          if (url.pathname === "/api/studio/tutorial-dismiss" && request.method() === "POST" &&
              !url.search && this.report.uiPreferenceChanges.length === 0 &&
              isDeepStrictEqual(request.postDataJSON(), {})) {
            this.report.uiPreferenceChanges.push({ action: "dismiss-welcome", profile, workloadMutation: false });
          } else {
            const permit = this.permit;
            if (!permit || permit.method !== request.method() || permit.path !== url.pathname) {
              throw new Error(`Unarmed UI operation refused: ${request.method()} ${url.pathname}`);
            }
            if (permit.raw !== undefined) {
              if (request.postData() !== permit.raw) throw new Error("YAML parse body differs from the visible editor");
            } else {
              const body = request.postDataJSON();
              if (permit.resource) {
                if (url.searchParams.get("namespace") !== this.namespace) throw new Error("Write namespace differs");
                verifyIntent(body, permit.resource);
              } else if (permit.conversationInput !== undefined) {
                if (url.searchParams.get("namespace") !== this.namespace ||
                    body.message !== permit.conversationInput || typeof body.conversationID !== "string" ||
                    !body.conversationID.trim() || (body.sessionID !== undefined && typeof body.sessionID !== "string") ||
                    Object.keys(body).some(key => !["message", "sessionID", "conversationID"].includes(key))) {
                  throw new Error("Agent input or Studio conversation identifier differs");
                }
              } else if (permit.validateBody) {
                if (url.searchParams.get("namespace") !== this.namespace) throw new Error("Write namespace differs");
                permit.validateBody(body);
              } else if (!isDeepStrictEqual(body, permit.body)) throw new Error("Invocation input differs");
            }
            this.report.writes.push({ method: request.method(), path: url.pathname,
              resource: permit.resource, body: permit.raw === undefined ? request.postDataJSON() : undefined,
              at: new Date().toISOString(), seconds: this.seconds() });
            this.permit = undefined;
          }
        }
        await route.continue();
      } catch (error) {
        this.report.browserErrors.push(error.message);
        this.persist();
        await route.abort().catch(abortError => this.report.browserErrors.push(abortError.message));
      }
    });
    this.page = await this.context.newPage();
    this.video = this.page.video();
    this.epoch = performance.now();
    this.page.on("pageerror", error => this.report.browserErrors.push(error.message));
    this.page.on("dialog", async dialog => {
      const applying = this.permit?.resource;
      if (dialog.type() === "confirm" && applying &&
          dialog.message().includes(`${applying.kind} ${applying.metadata.name}?`)) await dialog.accept();
      else if (dialog.type() === "confirm" && /^Discard unsaved changes/.test(dialog.message())) await dialog.accept();
      else { this.report.browserErrors.push(`Unexpected dialog: ${dialog.message()}`); await dialog.dismiss(); }
    });
    this.page.on("response", response => {
      const url = new URL(response.url());
      if (url.pathname.startsWith("/api/")) this.report.requests.push({ method: response.request().method(),
        path: url.pathname, status: response.status(), seconds: this.seconds() });
    });
    await this.page.goto(this.origin, { waitUntil: "domcontentloaded" });
    await expect(this.page.locator("#status")).toHaveText("ready", { timeout: 60000 });
    const welcome = this.page.locator(".tutorial-welcome");
    await expect(welcome).toBeVisible();
    const [dismissed] = await Promise.all([
      this.page.waitForResponse(response => response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/studio/tutorial-dismiss"),
      welcome.getByRole("button", { name: "Not now", exact: true }).press("Enter"),
    ]);
    if (!dismissed.ok() || (await dismissed.json()).dismissed !== true) throw new Error("Welcome dismissal failed");
    await expect(welcome).toHaveCount(0);
    this.persist();
  }

  arm(permit) {
    if (this.permit) throw new Error("Previous operation is not settled; no next operation may be armed");
    this.permit = permit;
  }

  async click(locator) { await locator.scrollIntoViewIfNeeded(); await locator.click(); }

  async hold(locator, target) {
    const startSeconds = this.seconds();
    const hold = await holdReadableText(this.page, locator);
    this.report.readingHolds.push({ target, startSeconds, endSeconds: this.seconds(), ...hold });
    return hold;
  }

  async scene(id, scenes, caption, narration, action) {
    const entry = { id, scenes, caption, narration, startSeconds: this.seconds(), passed: false };
    this.report.scenes.push(entry);
    this.persist();
    console.log(`${id}: ${caption}`);
    try {
      await action();
      if (this.report.browserErrors.length) throw new Error("Unexpected browser/API guard error occurred");
      const minimum = 2 + Math.ceil(narration.trim().split(/\s+/).length * 60 / 150);
      const remaining = minimum - (this.seconds() - entry.startSeconds);
      if (remaining > 0) await this.page.waitForTimeout(remaining * 1000);
      entry.passed = true;
    } finally {
      entry.endSeconds = this.seconds();
      this.persist();
    }
  }

  resource(kind, name, spec) {
    return { apiVersion: API, kind, metadata: { name, namespace: this.namespace,
      labels: { "kognition.dev/recording-run": this.options.runId } }, ...(spec ? { spec } : {}) };
  }

  async select(plural, name) {
    await selectRecordedResource(this.page, plural, name);
    this.current = `${plural}/${name}`;
  }

  async applyYAML(object, review = true) {
    const text = await recordingYAML(this.page, object);
    await this.click(this.page.locator('[data-view="yaml"]'));
    await this.page.locator("#yaml-area").fill(text);
    if (review) {
      if (this.policy.manifestPreviewSeconds === undefined) {
        await this.hold(this.page.locator("#yaml-area"), `manifest-${object.kind}`);
      } else {
        const seconds = this.policy.manifestPreviewSeconds;
        if (!Number.isFinite(seconds) || seconds < 3 || seconds > 8) {
          throw new Error("A paced configuration preview must explicitly allow three to eight seconds");
        }
        const startSeconds = this.seconds();
        await this.page.waitForTimeout(seconds * 1000);
        (this.report.manifestPreviews ||= []).push({ kind: object.kind, startSeconds,
          endSeconds: this.seconds(), readableYAML: true, configurationPreviewOnly: true });
      }
    }
    this.arm({ method: "POST", path: "/api/parse-yaml", raw: text });
    const [parsed] = await Promise.all([
      this.page.waitForResponse(response => response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/parse-yaml"),
      this.click(this.page.getByRole("button", { name: "Apply YAML", exact: true })),
    ]);
    if (!parsed.ok()) throw new Error(`Actual YAML parsing failed (HTTP${parsed.status()})`);
    verifyIntent(await parsed.json(), object);
    await expect(this.page.locator("#status")).toHaveText("YAML applied to builder");
  }

  async save(object, create = true, review = true) {
    assertResourceScope(object, this.policy);
    const plural = PLURALS[object.kind];
    if (create) {
      if (object.metadata.uid || object.metadata.resourceVersion || object.status) throw new Error("New resources cannot carry server identity or status");
      if (await this.get(plural, object.metadata.name, this.namespace, true)) throw new Error("Refusing to overwrite an existing resource");
      this.current = undefined;
    } else {
      if (!object.metadata.uid || !object.metadata.resourceVersion) throw new Error("Updates require original UID and optimistic resourceVersion");
      const before = await this.get(plural, object.metadata.name);
      if (before.metadata.uid !== object.metadata.uid) throw new Error("The resource incarnation changed before editing");
      if (this.current !== `${plural}/${object.metadata.name}`) await this.select(plural, object.metadata.name);
    }
    await prepareReviewedEditor(this.page, object.kind, create);
    await this.applyYAML(object, review);
    if (create && await this.get(plural, object.metadata.name, this.namespace, true)) throw new Error("Apply YAML unexpectedly persisted a resource");
    return this.persistResource(object, create);
  }

  async deployBuiltResource(object, create = true) {
    assertResourceScope(object, this.policy);
    if (create && await this.get(PLURALS[object.kind], object.metadata.name, this.namespace, true)) {
      throw new Error("Refusing to replace an existing resource from the Builder");
    }
    if (!create) {
      const current = await this.get(PLURALS[object.kind], object.metadata.name);
      if (!object.metadata.uid || current.metadata.uid !== object.metadata.uid ||
          current.metadata.resourceVersion !== object.metadata.resourceVersion) {
        throw new Error("Builder updates require the exact current resource incarnation and version");
      }
    }
    const text = await this.page.locator("#yaml-area").inputValue();
    if (!/^apiVersion:\s/m.test(text) || /^\s*[{[]/.test(text)) throw new Error("Builder review must display native YAML");
    this.arm({ method: "POST", path: "/api/parse-yaml", raw: text });
    const [parsed] = await Promise.all([
      this.page.waitForResponse(response => response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/parse-yaml"),
      this.click(this.page.getByRole("button", { name: "Apply YAML", exact: true })),
    ]);
    if (!parsed.ok()) throw new Error(`Builder YAML validation failed (HTTP${parsed.status()})`);
    verifyIntent(await parsed.json(), object);
    return this.persistResource(object, create);
  }

  async persistResource(object, create) {
    const plural = PLURALS[object.kind];
    this.arm({ method: create ? "POST" : "PUT",
      path: `/api/resources/${plural}${create ? "" : `/${object.metadata.name}`}`, resource: object });
    const [saved] = await Promise.all([
      this.page.waitForResponse(response => response.request().method() === (create ? "POST" : "PUT") &&
        new URL(response.url()).pathname === `/api/resources/${plural}${create ? "" : `/${object.metadata.name}`}`),
      this.click(this.page.locator("#btn-deploy")),
    ]);
    const body = await saved.json();
    if (!saved.ok()) throw new Error(`Actual ${object.kind} save failed (HTTP${saved.status()}): ${JSON.stringify(body)}`);
    const current = await this.get(plural, object.metadata.name);
    verifyIntent(current, object);
    if (!current.metadata.uid) throw new Error("Persisted resource has no UID");
    this.current = `${plural}/${object.metadata.name}`;
    if (create) this.report.created.push({ kind: object.kind, name: current.metadata.name, uid: current.metadata.uid });
    fs.writeFileSync(path.join(this.options.output, `${++this.serial}-${object.kind}-${object.metadata.name}.json`),
      JSON.stringify(current, null, 2), { flag: "wx" });
    this.report.evidence.push({ type: create ? "created" : "updated", resource: current, seconds: this.seconds() });
    this.persist();
    return current;
  }

  async startWorkflow(workflow, input, parameters) {
    assertResourceScope(workflow, this.policy);
    const current = await this.get("workflows", workflow.metadata.name);
    if (current.metadata.uid !== workflow.metadata.uid) throw new Error("Workflow identity changed before invocation");
    verifyIntent(current, workflow);
    await this.select("workflows", workflow.metadata.name);
    await this.click(this.page.locator('[data-view="preview"]'));
    const form = this.page.locator("#preview-host .run-form").first();
    await form.locator("textarea.run-input").fill(input);
    while (await form.locator(".run-param-row").count()) {
      await this.click(form.locator(".run-param-row").first().getByRole("button", { name: "×", exact: true }));
    }
    for (const [key, value] of Object.entries(parameters)) {
      await this.click(form.getByRole("button", { name: "+ add parameter", exact: true }));
      const row = form.locator(".run-param-row").last();
      await row.getByPlaceholder("key", { exact: true }).fill(key);
      await row.getByPlaceholder("value", { exact: true }).fill(value);
    }
    const endpoint = `/api/invoke/workflow/${workflow.metadata.name}`;
    this.arm({ method: "POST", path: endpoint, body: { input, parameters } });
    const [response] = await Promise.all([
      this.page.waitForResponse(item => item.request().method() === "POST" &&
        new URL(item.url()).pathname === endpoint, { timeout: 60000 }),
      this.click(form.getByRole("button", { name: "Run workflow", exact: true })),
    ]);
    const run = await response.json();
    (this.report.workflowStarts ||= []).push({ httpStatus: response.status(), run, seconds: this.seconds() });
    this.persist();
    if (!response.ok()) throw new Error(`Actual workflow start failed (HTTP${response.status()}): ${JSON.stringify(run)}`);
    verifyStartedWorkflow(run, workflow, input, parameters, this.namespace);
    this.report.created.push({ kind: "WorkflowRun", name: run.metadata.name, uid: run.metadata.uid });
    this.persist();
    return run;
  }

  async inspectObserved(plural, name, requiredText, target) {
    await this.select(plural, name);
    await this.click(this.page.locator('[data-view="builder"]'));
    const status = observedResourceView(this.page, plural);
    await expect(status).toHaveCount(1, { timeout: 30000 });
    if (["workflowruns", "evaluationruns"].includes(plural)) {
      for (const details of await status.locator("details").all()) {
        if (await details.getAttribute("open") === null) await details.locator("summary").click();
      }
    }
    for (const text of requiredText) {
      await expect(status).toContainText(new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i"));
    }
    if (this.policy.observedStatusPreviewSeconds !== undefined) {
      const seconds = this.policy.observedStatusPreviewSeconds;
      if (!Number.isFinite(seconds) || seconds < 2 || seconds > 10) throw new Error("Status preview must be bounded to 2..10 seconds");
      const startSeconds = this.seconds();
      await status.scrollIntoViewIfNeeded();
      await this.page.waitForTimeout(seconds * 1000);
      (this.report.statusPreviews ||= []).push({ target, startSeconds, endSeconds: this.seconds(),
        requiredText, fullTextReadingClaimed: false });
      this.persist();
    } else {
      await this.hold(status, target);
    }
  }

  async chat(agent, message, verify, clear = false) {
    await this.select("agents", agent);
    await this.click(this.page.locator('[data-view="preview"]'));
    if (clear) await this.click(this.page.getByRole("button", { name: "Clear", exact: true }));
    const count = await this.page.locator(".chat-message.assistant .chat-bubble:not(.typing)").count();
    await this.page.locator(".chat-input-row textarea").fill("");
    const typingStart = this.seconds();
    await this.page.locator(".chat-input-row textarea").pressSequentially(message, {
      delay: this.policy.naturalTyping === true ? 45 : 16 });
    if (this.policy.naturalTyping === true) {
      (this.report.typingIntervals ||= []).push({ kind: "message", startSeconds: typingStart,
        endSeconds: this.seconds(), characters: message.length, playbackSpeed: 1 });
    }
    this.arm({ method: "POST", path: `/api/invoke/agent/${agent}`, conversationInput: message });
    const pending = this.page.waitForRequest(request => request.method() === "POST" &&
      new URL(request.url()).pathname === `/api/invoke/agent/${agent}`);
    const [response, submitted] = await Promise.all([
      this.page.waitForResponse(response => response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/invoke/agent/${agent}`, { timeout: 180000 }),
      pending,
      this.click(this.page.getByRole("button", { name: "Send", exact: true })),
    ]);
    const body = await response.json();
    const request = submitted.postDataJSON();
    this.report.calls.push({ message, request, httpStatus: response.status(), ...body });
    if (!response.ok() || !body.response || !body.sessionID || body.usage?.totalTokens <= 0 ||
        body.usage.totalTokens !== body.usage.promptTokens + body.usage.completionTokens) {
      throw new Error("Real Agent invocation did not return consistent actual output/session/usage");
    }
    if (request.message !== message || !request.conversationID || body.conversationID !== request.conversationID) {
      throw new Error("Studio invocation input or returned conversation differs");
    }
    await verify(body);
    const answers = this.page.locator(".chat-message.assistant .chat-bubble:not(.typing)");
    await expect(answers).toHaveCount(count + 1);
    await expect(answers.last()).not.toHaveClass(/\berror\b/);
    await this.hold(answers.last(), `agent-answer-${body.sessionID}`);
    const instances = await this.list("agentinstances", `kognition.dev/agent=${agent}`);
    const instance = instances.find(item => item.status?.sessionID === body.sessionID);
    if (!instance || instance.status.phase !== "Ready" || instance.status.output !== body.response ||
        instance.spec.sessionContext?.conversation_id !== request.conversationID) {
      throw new Error("Independent invocation readback does not match the actual session and conversation");
    }
    this.report.calls.at(-1).instance = instance;
    this.persist();
    return body;
  }

  async finish(failure) {
    if (this.context && this.browser && this.video) {
      try { this.report.recording = await finalizeRecording(this.context, this.browser, this.video,
        path.join(this.options.output, failure ? "production.failed.webm" : "production.recording.webm")); }
      catch (error) { this.report.finalizationFailure = error.message; failure ||= error; }
    } else {
      if (this.context) await this.context.close().catch(error => { failure ||= error; this.report.finalizationFailure = error.message; });
      if (this.browser) await this.browser.close().catch(error => { failure ||= error; this.report.finalizationFailure = error.message; });
    }
    try { await stopChildren(this.children); }
    catch (error) { this.report.processCleanupFailure = error.message; failure ||= error; }
    this.report.passed = !failure;
    this.report.completedAt = new Date().toISOString();
    if (failure) this.report.failure = failure.message;
    this.persist();
    return failure;
  }
}

async function runProduction(input, policy, action) {
  const options = parseOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  const session = new ProductionSession(options, policy);
  let failure;
  try { await session.initialize(); await action(session); }
  catch (error) {
    failure = error;
    if (session.page && !session.page.isClosed()) await session.page.screenshot({ path: path.join(options.output, "failure.png") })
      .catch(captureError => session.report.browserErrors.push(`Failure screenshot: ${captureError.message}`));
  } finally {
    if (fs.existsSync(options.output)) failure = await session.finish(failure);
  }
  if (failure) throw new Error(`${policy.batch} failed: ${failure.message}`);
  return session.report;
}

module.exports = { API, PLURALS, CLI_SHA256, STUDIO_UI_V1, WORKFLOW_UI_V1, verifyStudioUI,
  verifyWorkflowStudioUI, NAMESPACES, recordingNamespaces,
  normalizedSpec, verifyIntent, assertResourceScope, verifyStartedWorkflow, prepareReviewedEditor, recordingYAML,
  selectRecordedResource, observedResourceView, parseContinuationOptions, readContinuation, ProductionSession, runProduction };
