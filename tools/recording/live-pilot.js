// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const https = require("node:https");
const crypto = require("node:crypto");
const { spawn, execFile } = require("node:child_process");
const { promisify, parseArgs } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const { READING_POLICY, holdReadableText } = require("./reading");

const execute = promisify(execFile);
const VIEWPORT = { width: 1920, height: 1080 };
const SCENE_SECONDS = 25;
const INPUT = "Help Cedar Labs with fictional incident INC-1042.";
const INSTRUCTIONS = [
  "You are a support assistant for the fictional Orbital Mark 7 product. " +
    "Reply with exactly this sentence and no other text: " +
    "SUPPORT V1: For INC-1042, collect the affected region and timeout rate.",
  "You are a support assistant for the fictional Orbital Mark 7 product. " +
    "Reply with exactly this sentence and no other text: " +
    "SUPPORT V2: For INC-1042, collect the region, timeout rate, and customer impact.",
];

function fixedOrigin(value, label) {
  const url = new URL(value);
  const loopback = ["127.0.0.1", "[::1]"].includes(url.hostname);
  if ((url.protocol !== "https:" && !(url.protocol === "http:" && loopback)) ||
      url.username || url.password || url.search || url.hash || url.pathname !== "/") {
    throw new Error(`${label} must be a fixed HTTPS origin or literal loopback HTTP origin`);
  }
  return url.origin;
}

function fieldLabel(value) {
  const escaped = value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`^${escaped}\\s*\\*?$`, "i");
}

function parseOptions(args) {
  const { values } = parseArgs({
    args,
    options: {
      cli: { type: "string" },
      mode: { type: "string" },
      server: { type: "string" },
      "kube-context": { type: "string" },
      namespace: { type: "string" },
      model: { type: "string" },
      "run-id": { type: "string" },
      "output-dir": { type: "string" },
      ffmpeg: { type: "string" },
      ffprobe: { type: "string" },
      "authorize-live-writes": { type: "boolean" },
      headed: { type: "boolean" },
    },
    strict: true,
    allowPositionals: false,
  });
  for (const name of ["cli", "mode", "namespace", "model", "run-id", "output-dir", "ffmpeg", "ffprobe"]) {
    if (!values[name]) throw new Error(`--${name} is required`);
  }
  if (!values["authorize-live-writes"]) throw new Error("--authorize-live-writes is required");
  if (!["standalone", "kubernetes"].includes(values.mode)) throw new Error("--mode must be explicit");
  for (const name of ["namespace", "model", "run-id"]) {
    if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(values[name])) {
      throw new Error(`--${name} must be a lowercase DNS label`);
    }
  }
  if (values["run-id"].length > 40 || values.namespace.length > 63 || values.model.length > 63) {
    throw new Error("resource names are too long");
  }
  if (values.mode === "standalone") {
    if (!values.server || values["kube-context"]) throw new Error("standalone requires --server and no kube context");
    values.server = fixedOrigin(values.server, "--server");
  } else if (!values["kube-context"] || values.server) {
    throw new Error("kubernetes requires --kube-context and no standalone server");
  }
  return {
    ...values,
    cli: path.resolve(values.cli),
    ffmpeg: path.resolve(values.ffmpeg),
    ffprobe: path.resolve(values.ffprobe),
    outputDir: path.resolve(values["output-dir"]),
    runId: values["run-id"],
    agentName: `pilot-${values["run-id"]}`,
    grantName: `pilot-${values["run-id"]}-model`,
  };
}

function verifyAgent(agent, options, instructions, minimumRevision, expectedUID) {
  if (agent.kind !== "Agent" || agent.metadata?.name !== options.agentName ||
      agent.metadata?.namespace !== options.namespace || !agent.metadata.uid ||
      (expectedUID && agent.metadata.uid !== expectedUID) ||
      agent.spec?.modelRef?.name !== options.model ||
      agent.spec?.instructions?.inline !== instructions ||
      agent.spec?.runtime?.maxTokens !== 256 || agent.spec?.replicas !== 0 ||
      agent.status?.phase !== "Ready" || !Number.isSafeInteger(agent.status.revisionCount) ||
      agent.status.revisionCount < minimumRevision) {
    throw new Error("persisted Agent does not match the authored revision, identity, bounds or readiness");
  }
}

function verifyKubernetesRuntime(agent, expected, deployment) {
  if (!expected.name || !expected.namespace || !expected.uid || !expected.deploymentUID ||
      !Number.isSafeInteger(expected.generation) || expected.generation < 1 ||
      !Number.isSafeInteger(expected.replicas) || expected.replicas < 1 ||
      !Number.isSafeInteger(expected.maxTokens) || expected.maxTokens < 1 ||
      agent.kind !== "Agent" || agent.metadata?.name !== expected.name ||
      agent.metadata?.namespace !== expected.namespace || agent.metadata.uid !== expected.uid ||
      agent.metadata.generation !== expected.generation || agent.metadata.deletionTimestamp ||
      agent.spec?.replicas !== expected.replicas || agent.spec?.runtime?.maxTokens !== expected.maxTokens ||
      agent.status?.phase !== "Ready" || agent.status.replicas !== expected.replicas ||
      agent.status.readyReplicas !== expected.replicas ||
      deployment?.kind !== "Deployment" || deployment.metadata?.name !== `agent-${expected.name}` ||
      deployment.metadata?.namespace !== expected.namespace || deployment.metadata.uid !== expected.deploymentUID ||
      deployment.metadata.deletionTimestamp || !Number.isSafeInteger(deployment.metadata.generation) ||
      deployment.metadata.generation < 1 || deployment.status?.observedGeneration !== deployment.metadata.generation ||
      deployment.spec?.replicas !== expected.replicas || deployment.status?.readyReplicas !== expected.replicas ||
      deployment.status?.availableReplicas !== expected.replicas || deployment.status?.updatedReplicas !== expected.replicas) {
    throw new Error("Kubernetes Agent runtime does not match the authored identity, generation, bounds or ready replica count");
  }
}

function verifyModel(model, mode, profile, expectedBounds = { defaultTokens: 256, limitTokens: 256 }) {
  if (model.status?.phase !== "Ready") throw new Error("the selected live model is not Ready");
  const reference = model.spec?.invocationProfileRef;
  if (reference) {
    if (!profile || profile.kind !== "ModelInvocationProfile" ||
        profile.metadata?.name !== reference.name || !profile.metadata.uid ||
        profile.metadata.namespace !== model.metadata?.namespace || profile.metadata.deletionTimestamp) {
      throw new Error("model invocation profile must be the current namespace-local resource");
    }
    if (!model.spec.invocationContract || !/^sha256:[a-f0-9]{64}$/.test(reference.specDigest || "")) {
      throw new Error("model invocation profile requires an explicit contract and reviewed digest");
    }
    if (!Number.isSafeInteger(expectedBounds.defaultTokens) || expectedBounds.defaultTokens < 1 ||
        !Number.isSafeInteger(expectedBounds.limitTokens) || expectedBounds.limitTokens < expectedBounds.defaultTokens ||
        profile.spec?.operation !== "chat" || profile.spec.defaults?.maxCompletionTokens !== expectedBounds.defaultTokens ||
        profile.spec.limits?.maxCompletionTokens !== expectedBounds.limitTokens) {
      throw new Error(`invocation profile must match chat default ${expectedBounds.defaultTokens} and limit ${expectedBounds.limitTokens} tokens`);
    }
    if (Object.keys(model.spec.parameters || {}).some(key => !["apiVersion", "api-version"].includes(key))) {
      throw new Error("generation parameters must be declared in the invocation profile");
    }
  } else if (mode === "standalone") {
    if (Number(model.spec?.parameters?.max_tokens) !== 256) {
      throw new Error("standalone pilot requires an explicit provider max_tokens ceiling of 256");
    }
  }
  if (model.spec.provider === "foundry" &&
      !(model.spec.parameters?.apiVersion || model.spec.parameters?.["api-version"])) {
    throw new Error("the Foundry model must configure its inference API version");
  }
}

function verifyChat(response, version) {
  const expected = version === 1
    ? "SUPPORT V1: For INC-1042, collect the affected region and timeout rate."
    : "SUPPORT V2: For INC-1042, collect the region, timeout rate, and customer impact.";
  const usage = response.usage;
  if (response.response?.trim() !== expected || !response.sessionID ||
      !usage || !Number.isSafeInteger(usage.promptTokens) || usage.promptTokens <= 0 ||
      !Number.isSafeInteger(usage.completionTokens) || usage.completionTokens <= 0 ||
      usage.completionTokens > 256 || !Number.isSafeInteger(usage.totalTokens) ||
      usage.totalTokens > 4096 || usage.totalTokens !== usage.promptTokens + usage.completionTokens) {
    throw new Error(`chat V${version} lacks the exact live result, session or consistent bounded usage`);
  }
}

function verifyRevisions({ revisions, before, after, diff }, first, second) {
  for (const resource of [revisions, before, after, diff]) {
    if (resource?.metadata?.name !== first.metadata.name ||
        resource.metadata.namespace !== first.metadata.namespace) {
      throw new Error("revision evidence does not belong to the recorded Agent");
    }
  }
  const from = first.status.revisionCount;
  const to = second.status.revisionCount;
  if (revisions.kind !== "RevisionList" || !Array.isArray(revisions.items) ||
      ![from, to].every(revision => revisions.items.some(item => item.revision === revision)) ||
      before.kind !== "Revision" || before.metadata.revision !== from ||
      after.kind !== "Revision" || after.metadata.revision !== to ||
      before.spec?.spec?.instructions?.inline !== first.spec.instructions.inline ||
      after.spec?.spec?.instructions?.inline !== second.spec.instructions.inline ||
      diff.kind !== "RevisionDiff" || diff.fromRevision !== from || diff.toRevision !== to ||
      !Array.isArray(diff.patch) || diff.patch.length !== 1 ||
      diff.patch[0].op !== "replace" || diff.patch[0].path !== "/spec/instructions/inline" ||
      diff.patch[0].value !== second.spec.instructions.inline) {
    throw new Error("immutable revision snapshots or specification diff do not match the UI edits");
  }
}

function verifyMedia(probe) {
  const video = probe.streams?.find(stream => stream.codec_type === "video");
  const duration = Number(probe.format?.duration);
  if (!video || video.width !== VIEWPORT.width || video.height !== VIEWPORT.height ||
      video.codec_name !== "h264" || !Number.isFinite(duration) || duration < 180 || duration > 300) {
    throw new Error("pilot video must be decodable H.264, 1920x1080, and three to five minutes long");
  }
  return { durationSeconds: duration, width: video.width, height: video.height, codec: video.codec_name };
}

function srtTime(seconds) {
  const milliseconds = Math.round(seconds * 1000);
  return `${String(Math.floor(milliseconds / 3600000)).padStart(2, "0")}:` +
    `${String(Math.floor(milliseconds / 60000) % 60).padStart(2, "0")}:` +
    `${String(Math.floor(milliseconds / 1000) % 60).padStart(2, "0")},` +
    String(milliseconds % 1000).padStart(3, "0");
}

function buildCaptions(scenes) {
  return scenes.map((scene, index) =>
    `${index + 1}\n${srtTime(scene.startSeconds)} --> ${srtTime(scene.endSeconds)}\n` +
    `AUTOMATED LIVE PILOT: ${scene.caption}\n`).join("\n");
}

function proxyWriteAllowed(method, pathname, body, options) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return false;
  const prefix = `/apis/kognition.dev/v1alpha1/namespaces/${options.namespace}`;
  if (method === "PUT" && pathname === `${prefix}/agents/${options.agentName}`) {
    return body.kind === "Agent" && body.metadata?.name === options.agentName &&
      body.metadata.namespace === options.namespace;
  }
  if (method !== "POST") return false;
  if (pathname === `${prefix}/agents/${options.agentName}/chat`) {
    return typeof body.message === "string" && body.message.length <= 4096;
  }
  const kind = pathname === `${prefix}/agents` ? "Agent" :
    pathname === `${prefix}/modelgrants` ? "ModelGrant" : "";
  const name = kind === "Agent" ? options.agentName : options.grantName;
  return Boolean(kind) && body.kind === kind && body.metadata?.name === name &&
    body.metadata.namespace === options.namespace &&
    (kind !== "ModelGrant" || (body.spec?.agentRef?.name === options.agentName &&
      body.spec?.modelRef?.name === options.model));
}

async function startAuthProxy(origin, token, options, onError) {
  fixedOrigin(origin, "API target");
  if (typeof token !== "string" || token.length < 32 || /[\r\n]/.test(token)) {
    throw new Error("KOGNITION_RECORDING_TOKEN must contain the approved operator token");
  }
  const transport = origin.startsWith("https:") ? https : http;
  let chatRequests = 0;
  const server = http.createServer(async (request, response) => {
    try {
      const target = new URL(request.url, origin);
      if (target.origin !== origin || !target.pathname.startsWith("/apis/kognition.dev/v1alpha1/")) {
        response.writeHead(403).end("pilot proxy target rejected");
        return;
      }
      let payload = Buffer.alloc(0);
      if (request.method !== "GET") {
        for await (const chunk of request) {
          payload = Buffer.concat([payload, chunk]);
          if (payload.length > 4 * 1024 * 1024) {
            response.writeHead(413).end("pilot request too large");
            return;
          }
        }
        let body;
        try {
          body = JSON.parse(payload.toString("utf8"));
        } catch {
          response.writeHead(400).end("pilot request must be JSON");
          return;
        }
        if (!proxyWriteAllowed(request.method, target.pathname, body, options)) {
          response.writeHead(403).end("pilot write outside approved resource scope");
          return;
        }
        if (target.pathname.endsWith("/chat") && ++chatRequests > 2) {
          response.writeHead(429).end("pilot permits at most two model invocations");
          return;
        }
      }
      const upstream = transport.request(target, {
        method: request.method,
        headers: {
          authorization: `Bearer ${token}`,
          "content-type": "application/json",
          ...(payload.length ? { "content-length": payload.length } : {}),
        },
      }, result => {
        if (result.statusCode >= 300 && result.statusCode < 400) {
          result.resume();
          onError("authenticated API redirect rejected");
          response.writeHead(502).end("authenticated API redirect rejected");
          return;
        }
        response.writeHead(result.statusCode, {
          "content-type": result.headers["content-type"] || "application/json",
        });
        result.on("error", () => {
          onError("upstream response stream failed");
          response.destroy();
        });
        result.pipe(response);
      });
      upstream.setTimeout(180000, () => upstream.destroy(new Error("API request timed out")));
      upstream.on("error", () => {
        onError("authenticated API forwarding failed");
        if (!response.headersSent) response.writeHead(502).end("authenticated API forwarding failed");
        else response.destroy();
      });
      request.on("aborted", () => upstream.destroy());
      upstream.end(payload);
    } catch {
      onError("pilot proxy request failed");
      if (!response.headersSent) response.writeHead(500).end("pilot proxy request failed");
      else response.destroy();
    }
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  return { server, origin: `http://127.0.0.1:${server.address().port}` };
}

function backendArgs(options, server) {
  return [
    "--mode", options.mode, "--namespace", options.namespace,
    ...(options.mode === "standalone" ? ["--server", server] : ["--kube-context", options["kube-context"]]),
  ];
}

async function startUI(kind, options, server, children) {
  const child = spawn(options.cli, [
    kind, ...backendArgs(options, server),
    ...(options.systemNamespace ? ["--kube-namespace", options.systemNamespace] : []),
    ...(options.systemNamespace && kind === "dashboard" ? ["--audit-namespace", options.systemNamespace] : []),
    "--bind", "127.0.0.1", "--port", "0",
  ], {
    windowsHide: true, stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, ...options.environment },
  });
  children.push(child);
  return new Promise((resolve, reject) => {
    let output = "";
    const timeout = setTimeout(() => reject(new Error(`${kind} startup timed out`)), 30000);
    child.on("error", error => { clearTimeout(timeout); reject(error); });
    child.on("exit", code => {
      clearTimeout(timeout);
      reject(new Error(`${kind} exited during startup (${code})`));
    });
    child.stderr.on("data", chunk => {
      fs.appendFileSync(path.join(options.outputDir, `${kind}.log`), chunk);
    });
    child.stdout.on("data", chunk => {
      output += chunk;
      const match = output.match(/listening on (http:\/\/127\.0\.0\.1:\d+)/);
      if (match) { clearTimeout(timeout); resolve(match[1]); }
    });
  });
}

async function cliJSON(options, server, args) {
  const result = await execute(options.cli, [...args, "-o", "json", ...backendArgs(options, server)], {
    timeout: 30000, maxBuffer: 4 * 1024 * 1024, windowsHide: true,
  });
  return JSON.parse(result.stdout);
}

async function getJSON(url, allowMissing = false) {
  const response = await fetch(url, { redirect: "error", signal: AbortSignal.timeout(20000) });
  if (allowMissing && response.status === 404) return null;
  if (!response.ok) throw new Error(`GET ${new URL(url).pathname} failed (${response.status})`);
  return response.json();
}

async function stopChildren(children) {
  for (const child of [...children].reverse()) {
    if (child.exitCode === null && child.signalCode === null && child.pid) {
      if (process.platform === "win32") {
        await execute("powershell.exe", ["-NoProfile", "-Command", `Stop-Process -Id ${child.pid} -ErrorAction Stop`],
          { windowsHide: true });
      } else {
        child.kill("SIGTERM");
      }
    }
  }
}

async function runPilot(options) {
  for (const name of ["cli", "ffmpeg", "ffprobe"]) {
    if (!fs.statSync(options[name]).isFile()) throw new Error(`${name} executable is unavailable`);
  }
  fs.mkdirSync(path.dirname(options.outputDir), { recursive: true });
  fs.mkdirSync(options.outputDir);
  const report = {
    schemaVersion: 1, runId: options.runId, mode: options.mode, namespace: options.namespace,
    agentName: options.agentName, modelName: options.model, grantName: options.grantName,
    startedAt: new Date().toISOString(), passed: false, pilotVerified: false,
    fullTutorialReady: false, speechSynthesisVerified: false, mockedAPI: false,
    viewport: VIEWPORT, assertions: [], scenes: [], requests: [], browserErrors: [],
    readingPolicy: READING_POLICY, readingHolds: [],
    cliSHA256: crypto.createHash("sha256").update(fs.readFileSync(options.cli)).digest("hex"),
  };
  const children = [];
  let proxy, browser, context, page, video, failure;
  let server = options.server;
  const assert = (name, evidence) => report.assertions.push({ name, passed: true, evidence });
  const resourcePath = (kind, name) => `/api/resources/${kind}/${encodeURIComponent(name)}?namespace=${options.namespace}`;
  try {
    if (options.mode === "standalone") {
      proxy = await startAuthProxy(server, process.env.KOGNITION_RECORDING_TOKEN, options,
        error => report.browserErrors.push(error));
      server = proxy.origin;
    }
    const studio = await startUI("studio", options, server, children);
    const dashboard = await startUI("dashboard", options, server, children);
    report.target = { studio, dashboard, ...(options.mode === "standalone" ?
      { api: options.server, authentication: "operator token through scoped loopback proxy" } :
      { kubeContext: options["kube-context"] }) };
    const metadata = await getJSON(`${studio}/api/meta`);
    if (metadata.mode !== options.mode || metadata.namespace !== options.namespace) {
      throw new Error("Studio backend does not match the explicit pilot mode and namespace");
    }
    for (const [kind, name] of [["agents", options.agentName], ["modelgrants", options.grantName]]) {
      if (await getJSON(studio + resourcePath(kind, name), true)) throw new Error(`refusing to overwrite existing ${kind}/${name}`);
    }
    const model = await getJSON(studio + resourcePath("models", options.model));
    const profile = model.spec?.invocationProfileRef
      ? await getJSON(studio + resourcePath("modelinvocationprofiles", model.spec.invocationProfileRef.name))
      : undefined;
    verifyModel(model, options.mode, profile);
    if (profile) {
      const profilePath = path.join(options.outputDir, "invocation-profile.json");
      fs.writeFileSync(profilePath, JSON.stringify(profile, null, 2), { flag: "wx" });
      const { stdout } = await execute(options.cli, ["invocation-profile-digest", "-f", profilePath],
        { timeout: 30000, windowsHide: true });
      if (stdout.trim() !== model.spec.invocationProfileRef.specDigest) {
        throw new Error("persisted invocation profile digest differs from the reviewed Model reference");
      }
    }
    assert("live-target-preflight", {
      mode: metadata.mode, modelUID: model.metadata.uid, phase: model.status.phase,
      invocationContract: model.spec.invocationContract, invocationProfileRef: model.spec.invocationProfileRef,
      invocationProfileUID: profile?.metadata.uid,
    });
    browser = await chromium.launch({ headless: !options.headed });
    context = await browser.newContext({
      viewport: VIEWPORT, colorScheme: "light",
      recordVideo: {
        dir: options.outputDir, size: VIEWPORT,
        showActions: { duration: 1000, fontSize: 18, cursor: "pointer" },
      },
    });
    const epoch = performance.now();
    page = await context.newPage();
    video = page.video();
    page.setDefaultTimeout(30000);
    page.on("pageerror", error => report.browserErrors.push(error.message));
    page.on("response", response => {
      const url = new URL(response.url());
      if (url.pathname.startsWith("/api/")) {
        report.requests.push({
          seconds: (performance.now() - epoch) / 1000,
          method: response.request().method(), path: url.pathname, status: response.status(),
        });
      }
    });
    const scene = async (id, caption, action) => {
      const entry = { id, caption, startSeconds: (performance.now() - epoch) / 1000 };
      report.scenes.push(entry);
      console.log(`${id}: ${caption}`);
      await action();
      entry.observedSeconds = (performance.now() - epoch) / 1000;
      const remaining = SCENE_SECONDS * 1000 - (performance.now() - epoch - entry.startSeconds * 1000);
      if (remaining > 0) await new Promise(resolve => setTimeout(resolve, remaining));
      entry.endSeconds = (performance.now() - epoch) / 1000;
      entry.passed = true;
    };
    const click = async locator => { await locator.scrollIntoViewIfNeeded(); await locator.click(); };
    const createResource = async selector => {
      await click(page.locator("#btn-create"));
      await click(page.locator(selector));
      await click(page.locator('[data-view="builder"]'));
      await expect(page.locator("#view-builder")).toBeVisible();
    };
    const type = async (locator, text) => {
      await click(locator);
      await locator.fill("");
      await locator.pressSequentially(text, { delay: 35 });
    };
    const label = text => page.getByLabel(fieldLabel(text));
    const section = name => page.locator("#builder-host details").filter({
      has: page.locator("summary").filter({ hasText: new RegExp(`^${name}(advanced)?$`) }),
    }).first();
    const openSection = async name => {
      const details = section(name);
      if (await details.getAttribute("open") === null) await click(details.locator("summary"));
    };
    const deployedAgent = async (version, uid) => {
      let result;
      await expect.poll(async () => {
        result = await getJSON(studio + resourcePath("agents", options.agentName));
        return result.status?.phase === "Ready" && result.status.revisionCount >= version;
      }, { timeout: 60000 }).toBe(true);
      verifyAgent(result, options, INSTRUCTIONS[version - 1], version, uid);
      return result;
    };
    let first, second;
    await scene("P01", "Inspect the real Studio and the prepared model.", async () => {
      await page.goto(studio, { waitUntil: "domcontentloaded" });
      await expect(page.locator("#status")).toHaveText("ready");
      await expect(page.locator("#meta-mode")).toContainText(options.mode);
      await expect(page.locator("#namespace-select")).toHaveValue(options.namespace);
      const modelItem = page.locator("#list-models").getByText(options.model, { exact: true });
      await click(modelItem.locator("xpath=ancestor::details[1]").locator("summary"));
      await click(modelItem);
      await click(page.locator('[data-view="yaml"]'));
      await expect(page.locator("#yaml-area")).toHaveValue(new RegExp(options.model));
    });
    await scene("P02", "Create a bounded support Agent in the visual builder.", async () => {
      await createResource("#btn-new-agent");
      await type(label("Name"), options.agentName);
      await label("Model").selectOption(options.model);
      await type(label("Inline instructions"), INSTRUCTIONS[0]);
      await openSection("Runtime");
      await type(label("Timeout"), "45s");
      await label("MaxTokens").fill("256");
      await openSection("Scaling");
      await label("Replicas").fill("0");
      await click(page.locator('[data-view="yaml"]'));
      await expect(page.locator("#yaml-area")).toHaveValue(/modelRef/);
      await click(page.locator("#btn-deploy"));
      await expect(page.locator("#status")).toHaveText(`deployed Agent ${options.agentName}`);
      first = await deployedAgent(1);
      assert("agent-created-through-ui", first);
      await page.screenshot({ path: path.join(options.outputDir, "created.png") });
    });
    await scene("P03", "Authorize this Agent to use the prepared model.", async () => {
      await createResource("#btn-new-modelgrant");
      await type(label("Name"), options.grantName);
      await label("Agent").selectOption(options.agentName);
      await label("Model").selectOption(options.model);
      await click(page.locator("#btn-deploy"));
      await expect(page.locator("#status")).toHaveText(`deployed ModelGrant ${options.grantName}`);
      const grant = await cliJSON(options, server, ["get", "modelgrant", options.grantName]);
      if (grant.spec?.agentRef?.name !== options.agentName || grant.spec?.modelRef?.name !== options.model) {
        throw new Error("persisted ModelGrant differs from the authored grant");
      }
      assert("model-grant-persisted", grant);
    });
    const chat = async version => {
      const pending = page.waitForResponse(response =>
        new URL(response.url()).pathname === `/api/invoke/agent/${options.agentName}` &&
        response.request().method() === "POST", { timeout: 120000 });
      await type(page.locator(".chat-input-row textarea"), INPUT);
      await click(page.getByRole("button", { name: "Send", exact: true }));
      const response = await pending;
      if (!response.ok()) throw new Error(`live chat failed (${response.status()})`);
      const body = await response.json();
      verifyChat(body, version);
      await expect(page.locator(".chat-message.assistant .chat-bubble").last()).toHaveText(body.response.trim());
      assert(`live-model-chat-v${version}`, body);
      const startSeconds = (performance.now() - epoch) / 1000;
      const hold = await holdReadableText(page, page.locator(".chat-message.assistant .chat-bubble").last());
      report.readingHolds.push({ target: "agent-answer", version, startSeconds,
        endSeconds: (performance.now() - epoch) / 1000, ...hold });
      return body;
    };
    await scene("P04", "Send a real model request and inspect its response and usage.", async () => {
      await click(page.locator("#list-agents").getByText(options.agentName, { exact: true }));
      await click(page.locator('[data-view="preview"]'));
      await chat(1);
      await page.screenshot({ path: path.join(options.outputDir, "chat-v1.png") });
    });
    await scene("P05", "Revise the instructions and deploy the changed definition.", async () => {
      await click(page.locator('[data-view="builder"]'));
      await type(label("Inline instructions"), INSTRUCTIONS[1]);
      await click(page.locator("#btn-deploy"));
      await expect(page.locator("#status")).toHaveText(`deployed Agent ${options.agentName}`);
      second = await deployedAgent(2, first.metadata.uid);
      assert("same-agent-new-revision", second);
    });
    await scene("P06", "Start a fresh conversation and verify the new behavior.", async () => {
      await click(page.locator('[data-view="preview"]'));
      await click(page.getByRole("button", { name: "Clear", exact: true }));
      const result = await chat(2);
      const original = report.assertions.find(item => item.name === "live-model-chat-v1").evidence;
      if (result.sessionID === original.sessionID) throw new Error("Clear did not create a new backend session");
      assert("fresh-session-new-instructions", { oldSession: original.sessionID, newSession: result.sessionID });
      await page.screenshot({ path: path.join(options.outputDir, "chat-v2.png") });
    });
    await scene("P07", "Inspect the persisted Agent in the operations Dashboard.", async () => {
      await page.goto(dashboard, { waitUntil: "domcontentloaded" });
      await expect(page.locator("#namespace")).toHaveValue(options.namespace);
      await click(page.locator('#sidebar [data-plural="agents"]'));
      await type(page.locator("#filter"), options.agentName);
      await click(page.locator("#table-wrap tbody tr").filter({ hasText: options.agentName }));
      await click(page.locator('[data-tab="json"]'));
      await expect(page.locator("#detail-body")).toContainText(INSTRUCTIONS[1]);
      const persisted = await cliJSON(options, server, ["get", "agent", options.agentName]);
      verifyAgent(persisted, options, INSTRUCTIONS[1], 2, first.metadata.uid);
      assert("independent-cli-readback", persisted);
      await page.screenshot({ path: path.join(options.outputDir, "dashboard.png") });
    });
    await scene("P08", "Verify immutable revisions and the instructions-only change.", async () => {
      const revisions = await cliJSON(options, server, ["revisions", "agent", options.agentName]);
      const before = await cliJSON(options, server, ["revision", "agent", options.agentName, String(first.status.revisionCount)]);
      const after = await cliJSON(options, server, ["revision", "agent", options.agentName, String(second.status.revisionCount)]);
      const diff = await cliJSON(options, server, [
        "revision-diff", "agent", options.agentName, String(first.status.revisionCount), String(second.status.revisionCount),
      ]);
      fs.writeFileSync(path.join(options.outputDir, "revisions.json"), JSON.stringify({ revisions, before, after, diff }, null, 2));
      verifyRevisions({ revisions, before, after, diff }, first, second);
      assert("immutable-revisions-and-diff", { revisions, before, after, diff });
      await click(page.locator('[data-tab="yaml"]'));
      await expect(page.locator("#detail-body")).toContainText("revisionCount");
    });
    if (report.browserErrors.length) throw new Error("browser or authenticated proxy reported errors");
    if (report.requests.some(request => request.status >= 400)) throw new Error("recorded UI received a failed API response");
    await context.close();
    context = null;
    await video.saveAs(path.join(options.outputDir, "pilot.raw.webm"));
    fs.writeFileSync(path.join(options.outputDir, "pilot.srt"), buildCaptions(report.scenes));
    await execute(options.ffmpeg, [
      "-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-i", "pilot.raw.webm",
      "-vf", "subtitles=pilot.srt:force_style='FontName=Segoe UI,FontSize=16,MarginV=14,BorderStyle=3'",
      "-r", "30", "-c:v", "libx264", "-preset", "fast", "-crf", "20",
      "-pix_fmt", "yuv420p", "-an", "-movflags", "+faststart", "pilot.mp4",
    ], { cwd: options.outputDir, timeout: 180000, maxBuffer: 2 * 1024 * 1024, windowsHide: true });
    const probed = await execute(options.ffprobe, [
      "-v", "error", "-show_streams", "-show_format", "-of", "json", "pilot.mp4",
    ], { cwd: options.outputDir, timeout: 30000, windowsHide: true });
    report.media = verifyMedia(JSON.parse(probed.stdout));
    await execute(options.ffmpeg, ["-v", "error", "-nostdin", "-i", "pilot.mp4", "-f", "null", "-"],
      { cwd: options.outputDir, timeout: 120000, maxBuffer: 2 * 1024 * 1024, windowsHide: true });
    assert("final-video-full-decode", report.media);
    report.media.video = "pilot.mp4";
    report.media.rawVideo = "pilot.raw.webm";
    report.media.captions = "pilot.srt";
    report.media.narration = false;
    report.passed = true;
    report.pilotVerified = true;
  } catch (error) {
    failure = error;
    report.failure = error.message.replaceAll(process.env.KOGNITION_RECORDING_TOKEN || "\0", "[REDACTED]");
    if (page && !page.isClosed()) {
      await page.screenshot({ path: path.join(options.outputDir, "failure.png") }).catch(error => {
        report.browserErrors.push(`failure screenshot unavailable: ${error.message}`);
      });
    }
  } finally {
    try {
      if (context) {
        await context.close();
        if (video) await video.saveAs(path.join(options.outputDir, "pilot.failed.webm"));
      }
      if (browser) await browser.close();
      await stopChildren(children);
      if (proxy) await new Promise((resolve, reject) => proxy.server.close(error => error ? reject(error) : resolve()));
    } catch (error) {
      report.cleanupFailure = error.message;
      report.passed = false;
      report.pilotVerified = false;
      failure = failure || error;
    }
    report.completedAt = new Date().toISOString();
    fs.writeFileSync(path.join(options.outputDir, "report.json"), JSON.stringify(report, null, 2), { flag: "wx" });
  }
  if (failure) throw new Error(`Pilot failed; inspect ${path.join(options.outputDir, "report.json")}`);
  console.log(`Verified live pilot: ${path.join(options.outputDir, "pilot.mp4")}`);
  return report;
}

if (require.main === module) {
  Promise.resolve().then(() => runPilot(parseOptions(process.argv.slice(2)))).catch(error => {
    console.error(error.message);
    process.exitCode = 1;
  });
}

module.exports = {
  parseOptions, fixedOrigin, fieldLabel, verifyAgent, verifyKubernetesRuntime, verifyModel, verifyChat, verifyRevisions, verifyMedia,
  buildCaptions, proxyWriteAllowed, startAuthProxy, runPilot, startUI, stopChildren,
};
