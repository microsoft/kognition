// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { execFileSync } = require("node:child_process");
const { parseArgs, isDeepStrictEqual } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const { startUI, stopChildren, fieldLabel, verifyKubernetesRuntime } = require("./live-pilot");
const { READING_POLICY, readingWindowSeconds, holdReadableText } = require("./reading");
const { CLI_SHA256, RANCHER_TARGET, recordingTarget, verifyClusterFile, verifyTarget, dismissWelcome } = require("./live-authoring");

const API = "kognition.dev/v1alpha1";
const NAMESPACE = "tutorial-support";
const FIXTURE = "story-integrations";
const TOOL_CLASS = "story-http";
const MODEL = "tutorial-support-model";
const APPROVAL_REASON = "Scripted demonstration approval of the reviewed fictional bulletin.";
const REPAIR = "docker.io/kognition-tutorial/k8s-controller@sha256:444e940bf22189b9947eae9c2b005c4c78788f366fc042774cffdb5538cc128c";
const RUNTIME_IMAGE = "docker.io/kognition-tutorial/agent-runtime@sha256:083ae72319c3aacdf7fd441775e2ffda3a6bdba0fc61feba63f77f4119e8aea2";
const CORPUS = [
  "Orbital Mark 7 supported regions at launch: westus3, eastus2, northeurope, and australiaeast. " +
    "The Brazil South region is targeted for the Mark 7.1 dot-release in November 2026 and is not bookable yet.",
  "Orbital Mark 7 known limitations on day one: no fine-tuning endpoint, no JSON-mode for streaming responses, " +
    "and a 32k-token context window. Function calling, vision, and embeddings are supported.",
];

function extractSandboxSource(text) {
  const marker = "  server.py: |\n";
  const normalized = text.replaceAll("\r\n", "\n");
  const offset = normalized.indexOf(marker);
  if (offset < 0) throw new Error("reviewed sandbox ConfigMap source is missing");
  const lines = [];
  for (const line of normalized.slice(offset + marker.length).split("\n")) {
    if (line && !line.startsWith("    ")) break;
    lines.push(line.slice(4));
  }
  const result = lines.join("\n");
  if (!result.includes("HTTPServer") || !result.includes("/calculator")) {
    throw new Error("reviewed sandbox source is incomplete");
  }
  return result;
}

function resource(kind, name, spec, namespace = NAMESPACE) {
  return {
    apiVersion: API, kind,
    metadata: {
      name, ...(namespace ? { namespace } : {}),
      labels: { "kognition.dev/recording-run": "full-tutorial-20261029-220040", "kognition.dev/story-fixture": "true" },
    },
    ...(spec === undefined ? {} : { spec }),
  };
}

function tool(name, endpoint, properties, required) {
  return resource("Tool", name, {
    className: TOOL_CLASS, protocol: "OpenAPI", endpoint,
    description: `Fictional support tutorial capability: ${name}.`,
    metadata: { allowPrivateNetwork: "true" },
    inputSchema: { type: "object", properties, required },
    sandbox: { timeout: "20s" },
  });
}

function fixtureObjects(repository, targetName) {
  recordingTarget(targetName);
  const labels = { "app.kubernetes.io/part-of": "kognition-tutorial-tools", "kognition.dev/story-fixture": "true" };
  const source = extractSandboxSource(fs.readFileSync(path.join(repository, "samples", "workflows", "tool-sandbox.yaml"), "utf8"));
  const publication = fs.readFileSync(path.join(repository, "samples", "hackathon-trustworthiness", "publication_server.py"), "utf8");
  const securityContext = { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] } };
  return [
    { apiVersion: "v1", kind: "ConfigMap", metadata: { name: FIXTURE, namespace: NAMESPACE, labels },
      data: { "sandbox.py": source, "publication.py": publication } },
    { apiVersion: "v1", kind: "PersistentVolumeClaim", metadata: { name: FIXTURE, namespace: NAMESPACE, labels },
      spec: { accessModes: ["ReadWriteOnce"], storageClassName: targetName === RANCHER_TARGET ? "local-path" : "standard",
        resources: { requests: { storage: "1Gi" } } } },
    { apiVersion: "apps/v1", kind: "Deployment", metadata: { name: FIXTURE, namespace: NAMESPACE, labels },
      spec: { replicas: 1, selector: { matchLabels: { app: FIXTURE } }, template: {
        metadata: { labels: { ...labels, app: FIXTURE } },
        spec: {
          automountServiceAccountToken: false,
          securityContext: { runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, seccompProfile: { type: "RuntimeDefault" } },
          containers: [
            { name: "sandbox", image: RUNTIME_IMAGE, imagePullPolicy: "Never",
              command: ["python", "-u", "/fixture/sandbox.py"], ports: [{ containerPort: 8080 }],
              securityContext, resources: { requests: { cpu: "20m", memory: "32Mi" }, limits: { cpu: "250m", memory: "128Mi" } },
              env: [{ name: "PYTHONDONTWRITEBYTECODE", value: "1" }],
              volumeMounts: [{ name: "source", mountPath: "/fixture", readOnly: true }],
              readinessProbe: { httpGet: { path: "/health", port: 8080 }, initialDelaySeconds: 2, periodSeconds: 5 } },
            { name: "publication", image: RUNTIME_IMAGE, imagePullPolicy: "Never",
              command: ["python", "-u", "/fixture/publication.py"], ports: [{ containerPort: 8081 }],
              securityContext, resources: { requests: { cpu: "20m", memory: "32Mi" }, limits: { cpu: "250m", memory: "128Mi" } },
              env: [
                { name: "PUBLICATION_HOST", value: "0.0.0.0" }, { name: "PUBLICATION_PORT", value: "8081" },
                { name: "PUBLICATION_DATABASE", value: "/data/publications.db" }, { name: "PYTHONDONTWRITEBYTECODE", value: "1" },
              ],
              volumeMounts: [{ name: "source", mountPath: "/fixture", readOnly: true }, { name: "data", mountPath: "/data" }],
              readinessProbe: { httpGet: { path: "/healthz", port: 8081 }, initialDelaySeconds: 2, periodSeconds: 5 } },
          ],
          volumes: [{ name: "source", configMap: { name: FIXTURE } }, { name: "data", persistentVolumeClaim: { claimName: FIXTURE } }],
        },
      } } },
    { apiVersion: "v1", kind: "Service", metadata: { name: FIXTURE, namespace: NAMESPACE, labels },
      spec: { selector: { app: FIXTURE }, ports: [{ name: "sandbox", port: 8080 }, { name: "publication", port: 8081 }] } },
    resource("ToolClass", TOOL_CLASS, { provisioner: "openapi", executionTier: "process", transport: "http" }, ""),
    tool("story-calculator", `http://${FIXTURE}.${NAMESPACE}.svc.cluster.local:8080/calculator`,
      { expression: { type: "string", description: "Pure arithmetic, such as 17 * 23." } }, ["expression"]),
    tool("story-publish", `http://${FIXTURE}.${NAMESPACE}.svc.cluster.local:8081/publish`,
      { runID: { type: "string" }, content: { type: "string", maxLength: 4096 } }, ["runID", "content"]),
    resource("KnowledgeBaseClass", "story-inline", { provisioner: "inline", indexMode: "inline", reclaimPolicy: "Retain" }, ""),
    resource("KnowledgeBase", "story-orbital-docs", {
      knowledgeBaseClassName: "story-inline", sources: CORPUS.map(data => ({ type: "inline", data })),
    }),
    resource("KnowledgeBaseClaim", "story-docs-reader", { knowledgeBaseName: "story-orbital-docs", accessMode: "ReadOnly", retrieval: { topK: 3 } }),
    resource("MemoryClaim", "story-customer-memory", {
      memoryClassName: "default", type: "episodic", capacity: "16Mi", accessModes: ["ReadWrite"],
    }),
    resource("AutonomyClass", "story-demonstration", {
      level: 90, requiresApproval: false, allowedTools: [TOOL_CLASS],
      approvalPolicy: { toolInvocation: "never", memoryWrite: "never", externalCommunication: "never" },
    }, ""),
  ];
}

function teamCommunicationPolicies(prefix) {
  return [["support", "writer"], ["writer", "reviewer"]].map(([from, to]) =>
    resource("AgentCommunicationPolicy", `${prefix}-${from}-to-${to}`, {
      defaultDeny: true, allowFrom: [{ name: `${prefix}-${from}` }], allowTo: [{ name: `${prefix}-${to}` }],
    }));
}

function teamObjects(prefix) {
  const support = `${prefix}-support`, writer = `${prefix}-writer`, reviewer = `${prefix}-reviewer`;
  const objects = [];
  for (const [name, instructions] of [
    [support, "You assist fictional Cedar Labs with Orbital Mark 7 incident INC-1042. " +
      "Answer only the latest user request; do not re-answer earlier turns. " +
      "Use this conversation's history for questions about earlier messages; never use persistent Memory tools for chat-history retrieval. " +
      "Always call knowledge_search before product-fact answers and quote its relevant source. " +
      "Use story_calculator for arithmetic. Use memory_store only when asked to remember; store the exact supplied note and tag. " +
      "After memory_store confirms Stored., confirm only the stored note and tag, without revisiting product questions. " +
      "If storage is unconfirmed, say so. " +
      "Use memory_recall before recalling customer preferences, with the supplied tag. Never invent retrieved results. " +
      "For product facts absent from the source say you do not have evidence. Keep replies under 120 words."],
    [writer, "Write a concise customer incident bulletin using only the supplied evidence. Include INC-1042, " +
      "Cedar Labs, the exact impact result, the documented Brazil South limitation, and the customer update interval. " +
      "Do not invent a resolution or claim that publication happened."],
    [reviewer, "Review the supplied incident bulletin. Preserve and return the complete bulletin, followed by " +
      "'REVIEW: APPROVED' only when it contains INC-1042, 391, Brazil South, and 30 minutes. Otherwise explain the missing fields."],
  ]) {
    const spec = { modelRef: { name: MODEL }, instructions: { inline: instructions }, replicas: 0,
      runtime: { class: "default", timeout: "120s", maxTokens: 512 } };
    if (name === support) {
      spec.tools = [{ ref: { name: "story-calculator" }, required: true }];
      spec.knowledgeBaseClaims = [{ name: "story-docs-reader", required: true }];
      spec.memory = { claims: [{ name: "story-customer-memory" }] };
    }
    objects.push(resource("Agent", name, spec));
    objects.push(resource("ModelGrant", `${name}-model`, { agentRef: { name }, modelRef: { name: MODEL } }));
  }
  const role = resource("AgentRole", `${prefix}-capabilities`);
  role.rules = [{ resources: ["Tool"], verbs: ["get", "invoke", "discover", "inspect"] },
    { resources: ["Memory"], verbs: ["get", "update"] }];
  objects.push(role, resource("AgentRoleBinding", `${prefix}-capabilities`, {
    roleRef: { name: role.metadata.name }, subjects: [{ kind: "Agent", name: support }],
  }));
  for (const name of ["story-calculator", "story-publish"]) {
    objects.push(resource("ToolGrant", `${prefix}-${name}`, {
      agentRef: { name: support }, toolRef: { name }, verbs: ["invoke", "discover", "inspect"],
      rateLimits: { callsPerMinute: 20 },
    }));
  }
  objects.push(resource("MemoryGrant", `${prefix}-memory`, {
    agentRef: { name: support }, memoryClaimRef: { name: "story-customer-memory" }, accessMode: "ReadWrite",
  }));
  objects.push(...teamCommunicationPolicies(prefix));
  objects.push(resource("Workflow", `${prefix}-incident`, {
    steps: [
      { name: "research", agentRef: { name: support },
        input: "For fictional INC-1042, use knowledge_search to establish whether Brazil South is bookable. " +
          "Then use memory_recall with tag {{ .Params.memoryTag }} to retrieve Cedar Labs' update interval. Quote both results.", timeout: "180s" },
      { name: "impact", type: "tool", agentRef: { name: support }, toolRef: { name: "story-calculator" }, input: '{"expression":"17 * 23"}' },
      { name: "write", agentRef: { name: writer },
        input: "Incident INC-1042 affects Cedar Labs. Product and preference evidence: {{ .Steps.research.Output }}\n" +
          "Exact impact calculation: {{ .Steps.impact.Output }}\nWrite the customer bulletin with citations.", timeout: "180s" },
      { name: "review", agentRef: { name: reviewer }, input: "{{ .Steps.write.Output }}", timeout: "180s" },
      { name: "publish", type: "tool", agentRef: { name: support }, toolRef: { name: "story-publish" },
        input: '{"runID":{{ printf "%q" .Params.publicationID }},"content":{{ printf "%q" .Steps.review.Output }}}' },
    ],
    edges: [
      { from: "research", to: "write" }, { from: "impact", to: "write" },
      { from: "write", to: "review" }, { from: "review", to: "publish" },
    ],
    maxParallelSteps: 2, maxConcurrentRuns: 1, historyLimit: 10,
    approvalGates: [{ from: "review", to: "publish", requiredApprovers: 1, timeout: "10m" }],
    budgets: { maxDuration: "15m", maxTokens: 15000 },
  }));
  return objects;
}

function parseOptions(args) {
  const { values } = parseArgs({ args, strict: true, allowPositionals: false, options: {
    runtime: { type: "string" }, output: { type: "string" }, "run-id": { type: "string" },
    target: { type: "string" },
    stage: { type: "string" }, "authorize-live-writes": { type: "boolean" },
  } });
  if (!values.runtime || !values.output || !/^[a-z0-9][a-z0-9-]{0,30}$/.test(values["run-id"] || "") ||
      !["prepare", "rehearse", "record"].includes(values.stage) || !values["authorize-live-writes"]) {
    throw new Error("Explicit --runtime, new --output, --run-id, --stage prepare|rehearse|record and --authorize-live-writes are required");
  }
  recordingTarget(values.target);
  return { ...values, runtime: path.resolve(values.runtime), output: path.resolve(values.output), runId: values["run-id"] };
}

function verifyStoryRuntime(agent, deployment) {
  verifyKubernetesRuntime(agent, {
    name: agent.metadata.name, namespace: NAMESPACE, uid: agent.metadata.uid,
    generation: agent.metadata.generation, deploymentUID: deployment?.metadata?.uid,
    replicas: 1, maxTokens: 512,
  }, deployment);
}

function verifyMemoryEvidence(body, operation, tag) {
  if (!["memory_store", "memory_recall"].includes(operation) || !Array.isArray(body.toolCallsDetail)) {
    throw new Error("Actual governed Memory tool evidence is required");
  }
  const calls = body.toolCallsDetail.filter(call => call.name === operation);
  if (calls.length !== 1) throw new Error(`Expected exactly one actual ${operation} call`);
  const call = calls[0];
  const args = JSON.parse(call.args);
  if (!args || args.tag !== tag) throw new Error(`${operation} did not use the exact fresh memory tag`);
  if (operation === "memory_store") {
    if (call.result !== "Stored." || typeof args.content !== "string" ||
        !args.content.includes("Cedar Labs") || !/\b30\s+minutes\b/.test(args.content)) {
      throw new Error("memory_store did not confirm the requested persistent note");
    }
    return { args };
  }
  const records = JSON.parse(call.result);
  if (!Array.isArray(records) || !records.length ||
      records.some(record => !record || record.tag !== tag || typeof record.content !== "string")) {
    throw new Error("memory_recall did not return actual records with the exact memory tag");
  }
  return { args, records };
}

function verifyGroundedToolEvidence(body, operation) {
  const calls = body.toolCallsDetail.filter(call => call.name === operation || call.name === operation.replaceAll("_", "-"));
  if (calls.length !== 1) throw new Error(`Expected exactly one actual ${operation} result`);
  const call = calls[0];
  const result = JSON.parse(call.result);
  if (operation === "story_calculator") {
    const args = JSON.parse(call.args);
    if (typeof args.expression !== "string" || args.expression.replace(/\s/g, "") !== "17*23" ||
        result?.expression !== args.expression || result.result !== 391) {
      throw new Error("The actual calculator result does not verify 17 * 23 = 391");
    }
  } else if (operation === "knowledge_search") {
    if (!Array.isArray(result) || !result.some(hit => hit.kb === "story-orbital-docs" &&
        hit.source_type === "inline" && typeof hit.chunk === "string" &&
        hit.chunk.includes("Brazil South") && hit.chunk.includes("not bookable yet"))) {
      throw new Error("Actual inline Knowledge retrieval lacks the documented Brazil South limitation");
    }
  } else {
    throw new Error("Unsupported grounded story tool");
  }
}

function publicationGateReady(state) {
  const status = state.status;
  const review = status?.stepStatuses?.find(step => step.name === "review");
  const publish = status?.stepStatuses?.find(step => step.name === "publish");
  return status?.phase === "Pending" && status.activeStep === "publish" &&
    review?.phase === "Ready" && (!publish || publish.phase === "Pending") &&
    status.conditions?.some(condition =>
      condition.type === "Ready" && condition.status === "False" && condition.reason === "WaitingApproval") === true;
}

function verifyReviewedBulletin(state) {
  const output = state.status?.stepStatuses?.find(step => step.name === "review")?.output;
  if (typeof output !== "string" ||
      !["INC-1042", "391", "Brazil South", "30"].every(text => output.includes(text)) ||
      !/(?:^|\n)\s*(?:\*\*)?REVIEW:\s*APPROVED(?:\*\*)?\s*$/.test(output)) {
    throw new Error("The actual reviewer did not approve a complete evidence-based bulletin");
  }
  return output;
}

function storyApprovalDecision(runId, identity) {
  const approver = identity?.status?.userInfo?.username;
  if (typeof approver !== "string" || !approver.trim()) {
    throw new Error("Approval requires the authenticated Kubernetes requester from auth whoami");
  }
  const decision = resource("ApprovalDecision", `${runId}-approval`, {
    workflowRunRef: { name: runId }, step: "publish", decision: "approve", approver,
    reason: APPROVAL_REASON,
  });
  decision.metadata.labels["kognition.dev/scripted-approval"] = "true";
  return decision;
}

function verifyStoryRun(run, workflowName, runId) {
  if (run.kind !== "WorkflowRun" || run.metadata?.namespace !== NAMESPACE || !run.metadata.uid ||
      !run.metadata.name || run.spec?.workflowRef?.name !== workflowName ||
      run.spec.parameters?.memoryTag !== runId || run.spec.parameters?.publicationID !== runId) {
    throw new Error("Created workflow run does not match the exact story, namespace and fresh parameters");
  }
}

function verifyStoryApproval(decision, runName, requester) {
  if (decision.kind !== "ApprovalDecision" || decision.metadata?.namespace !== NAMESPACE || !decision.metadata.uid ||
      decision.spec?.workflowRunRef?.name !== runName || decision.spec.step !== "publish" ||
      decision.spec.decision !== "approve" || decision.spec.approver !== requester ||
      decision.spec.reason !== APPROVAL_REASON) {
    throw new Error("Actual Studio approval does not match the authenticated scripted publication decision");
  }
}

async function startStoryWorkflowInStudio(page, workflowName, runId) {
  await page.locator("#list-workflows").getByText(workflowName, { exact: true }).click();
  await page.locator('[data-view="preview"]').click();
  const form = page.locator("#preview-host .run-form").first();
  await form.locator("textarea.run-input").fill("Prepare fictional INC-1042 bulletin for Cedar Labs.");
  for (const key of ["memoryTag", "publicationID"]) {
    await form.getByRole("button", { name: "+ add parameter", exact: true }).click();
    const row = form.locator(".run-param-row").last();
    await row.getByPlaceholder("key", { exact: true }).fill(key);
    await row.getByPlaceholder("value", { exact: true }).fill(runId);
  }
  const pending = page.waitForResponse(response =>
    new URL(response.url()).pathname === `/api/invoke/workflow/${workflowName}` &&
    response.request().method() === "POST", { timeout: 60000 });
  await form.getByRole("button", { name: "Run workflow", exact: true }).click();
  const response = await pending;
  const created = await response.json();
  if (!response.ok()) throw new Error(`Studio workflow start failed: HTTP ${response.status()}`);
  verifyStoryRun(created, workflowName, runId);
  return created;
}

async function showStoryStepOutput(page, name, requiredText) {
  const card = page.locator(".run-step-card:visible").filter({
    has: page.locator(".run-step-name").filter({ hasText: new RegExp(`^${name}$`) }),
  });
  await expect(card).toHaveCount(1, { timeout: 30000 });
  const details = card.locator(".run-step-output");
  await details.locator("summary").click();
  const output = details.locator("pre.run-output");
  await output.scrollIntoViewIfNeeded();
  await expect(output).toBeVisible();
  for (const text of requiredText) await expect(output).toContainText(text);
}

async function approveStoryWorkflowInStudio(page, runName, requester) {
  const gate = page.locator(".run-approval-gate");
  await expect(gate).toBeVisible({ timeout: 30000 });
  await expect(gate).toContainText("publish");
  await gate.getByPlaceholder("e.g. reviewed by me").fill(APPROVAL_REASON);
  // Keep the real pending gate visible while narration explains the scripted decision.
  await page.waitForTimeout(8000);
  const pending = page.waitForResponse(response =>
    new URL(response.url()).pathname === "/api/resources/approvaldecisions" &&
    response.request().method() === "POST", { timeout: 60000 });
  await gate.getByRole("button", { name: "Approve", exact: true }).click();
  const response = await pending;
  const created = await response.json();
  if (!response.ok()) throw new Error(`Studio approval failed: HTTP ${response.status()}`);
  verifyStoryApproval(created, runName, requester);
  return created;
}

async function verifyStoryChatView(page, previousCount, requiredText) {
  const messages = page.locator(".chat-message.assistant .chat-bubble:not(.typing)");
  await expect(messages).toHaveCount(previousCount + 1);
  const bubble = messages.last();
  await expect(bubble).toBeVisible();
  await expect(bubble).not.toHaveClass(/\berror\b/);
  for (const text of requiredText) await expect(bubble).toContainText(text);
  return bubble.innerText();
}

async function runStory(options) {
  fs.mkdirSync(options.output);
  const cluster = JSON.parse(fs.readFileSync(path.join(options.runtime, "cluster.json"), "utf8"));
  const approved = recordingTarget(options.target);
  verifyClusterFile(cluster, approved);
  const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
  if (crypto.createHash("sha256").update(fs.readFileSync(cli)).digest("hex") !== CLI_SHA256) {
    throw new Error("The frozen tutorial CLI digest differs");
  }
  const fixtureRun = options.target === RANCHER_TARGET ? RANCHER_TARGET : "full-tutorial-20261029-220040";
  const repository = path.resolve(__dirname, "..", "..");
  const common = ["--kubeconfig", cluster.kubeconfig, "--context", cluster.context, "--request-timeout=20s"];
  const env = { ...process.env, KUBECONFIG: cluster.kubeconfig };
  const report = { schemaVersion: 1, runId: options.runId, stage: options.stage, startedAt: new Date().toISOString(),
    passed: false, mockedAPI: false, fullTutorialRecorded: false, milestones: [], calls: [], created: [], scenes: [],
    readingHolds: [], readingPolicy: READING_POLICY, approvedTarget: approved, cliSHA256: CLI_SHA256,
    recorderSHA256: crypto.createHash("sha256").update(fs.readFileSync(__filename)).digest("hex") };
  const filming = options.stage === "record";
  const children = [];
  let browser, context, page, video, failure, videoEpoch;
  const cue = (id, caption) => {
    if (!filming) return;
    const seconds = (performance.now() - videoEpoch) / 1000;
    if (report.scenes.length) report.scenes.at(-1).endSeconds = seconds;
    report.scenes.push({ id, caption, startSeconds: seconds });
  };
  const raw = (args, input) => execFileSync("kubectl", [...common, ...args], {
    input, encoding: "utf8", timeout: 90000, maxBuffer: 8 * 1024 * 1024, windowsHide: true,
  });
  const get = (kind, name, namespace = NAMESPACE) => JSON.parse(raw([
    "get", kind, name, ...(namespace ? ["-n", namespace] : []), "-o", "json",
  ]));
  const collection = (kind, namespace = NAMESPACE) => JSON.parse(raw(["get", kind, ...(namespace ? ["-n", namespace] : []), "-o", "json"])).items;
  const wait = async (label, action, timeout = 180000) => {
    let result;
    await expect.poll(async () => { result = await action(); return Boolean(result); }, { timeout, message: label }).toBe(true);
    return result;
  };
  const retainCreated = created => {
    const namespace = created.metadata.namespace || "";
    if (namespace && namespace !== NAMESPACE) throw new Error("Story writes cannot escape the approved support namespace");
    report.created.push({ kind: created.kind, name: created.metadata.name, namespace, uid: created.metadata.uid });
    fs.writeFileSync(path.join(options.output, `${created.kind}-${created.metadata.name}.json`), JSON.stringify(created, null, 2), { flag: "wx" });
    return created;
  };
  const create = object => {
    if (object.metadata.namespace && object.metadata.namespace !== NAMESPACE) {
      throw new Error("Story writes cannot escape the approved support namespace");
    }
    if (object.metadata.labels?.["kognition.dev/recording-run"]) {
      object.metadata.labels["kognition.dev/recording-run"] = fixtureRun;
    }
    return retainCreated(JSON.parse(raw(["create", "-f", "-", "-o", "json"], JSON.stringify(object))));
  };
  const receipt = id => JSON.parse(raw(["exec", `deployment/${FIXTURE}`, "-n", NAMESPACE, "-c", "publication", "--",
    "python", "-c", "import urllib.request,sys; print(urllib.request.urlopen(sys.argv[1], timeout=10).read().decode())",
    `http://127.0.0.1:8081/publications/${id}`]));
  try {
    const root = get("namespace", "kube-system", "");
    const controller = get("deployment", "kognition-tutorial-k8s-controller", "tutorial-system");
    const config = JSON.parse(raw(["config", "view", "--minify", "-o", "json"]));
    verifyTarget(cluster, config, root, get("namespace", NAMESPACE, ""), controller, approved);
    const model = get("model", MODEL);
    if (model.status?.phase !== "Ready") throw new Error("The prepared real model is not Ready");
    report.clusterUID = root.metadata.uid;
    report.controller = { uid: controller.metadata.uid, image: REPAIR };
    if (options.stage === "prepare") {
      for (const object of fixtureObjects(repository, options.target)) create(object);
      raw(["rollout", "status", `deployment/${FIXTURE}`, "-n", NAMESPACE, "--timeout=180s"]);
      for (const [kind, name, phase] of [
        ["tool", "story-calculator", "Ready"], ["tool", "story-publish", "Ready"],
        ["knowledgebase", "story-orbital-docs", "Ready"], ["knowledgebaseclaim", "story-docs-reader", "Bound"],
        ["memoryclaim", "story-customer-memory", "Bound"],
      ]) await wait(`${kind} readiness`, () => get(kind, name).status?.phase === phase);
      report.fixtureDeployment = get("deployment", FIXTURE);
      report.passed = true;
      return report;
    }
    const prefix = "rehearsal-story";
    const approval = storyApprovalDecision(options.runId, JSON.parse(raw(["auth", "whoami", "-o", "json"])));
    report.approvalRequester = approval.spec.approver;
    const support = `${prefix}-support`;
    const existing = collection("agents").find(item => item.metadata.name === support);
    if (!existing) {
      for (const object of teamObjects(prefix)) create(object);
      const agent = get("agent", support);
      create(resource("TrustProfile", `${prefix}-trust`, {
        agentRef: { name: support }, agentIncarnationUID: agent.metadata.uid,
        initialScore: 0.9, initialClass: "story-demonstration", maxClass: "story-demonstration",
      }));
    } else if (existing.metadata.labels?.["kognition.dev/story-fixture"] !== "true" ||
        existing.metadata.labels?.["kognition.dev/recording-run"] !== fixtureRun) {
      throw new Error("Refusing to adopt an unrelated existing story Agent");
    }
    const communication = collection("agentcommunicationpolicies");
    for (const policy of teamCommunicationPolicies(prefix)) {
      const current = communication.find(item => item.metadata.name === policy.metadata.name);
      if (!current) create(policy);
      else if (current.metadata.labels?.["kognition.dev/story-fixture"] !== "true" ||
          current.metadata.labels?.["kognition.dev/recording-run"] !== fixtureRun ||
          !isDeepStrictEqual(current.spec, policy.spec)) {
        throw new Error("Refusing to adopt a different or unrelated team communication policy");
      }
    }
    const supportReady = await wait("support reconciled", () => {
      const item = get("agent", support); return item.status?.phase === "Ready" && item.status.revisionCount > 0 ? item : false;
    });
    const activeInstances = collection("agentinstances").filter(item =>
      [support, `${prefix}-writer`, `${prefix}-reviewer`].includes(item.spec.agentRef.name) &&
      item.status?.phase === "Running");
    if (activeInstances.length) throw new Error("Prior story invocations remain unsettled; no new work will be dispatched");
    for (const desired of teamObjects(prefix).filter(item => item.kind === "Agent")) {
      const name = desired.metadata.name;
      const current = get("agent", name);
      if (current.spec.instructions?.inline !== desired.spec.instructions.inline) {
        throw new Error(`The deployed story instructions differ from the authored fixture: ${name}`);
      }
      if (current.spec.replicas === 0) {
        current.spec.replicas = 1;
        const updated = JSON.parse(raw(["replace", "-f", "-", "-o", "json"], JSON.stringify(current)));
        report.milestones.push({ name: "runtime-requested", agent: name, generation: updated.metadata.generation });
      } else if (current.spec.replicas !== 1) {
        throw new Error("Unexpected Agent replica count; refusal to adopt another runtime configuration");
      }
      await wait(`physical runtime readiness: ${name}`, () => {
        const current = get("agent", name);
        const deployment = collection("deployments").find(item => item.metadata.name === `agent-${name}`);
        if (current.status?.phase !== "Ready" || current.status.readyReplicas !== 1 ||
            deployment?.status?.availableReplicas !== 1) return false;
        verifyStoryRuntime(current, deployment);
        return true;
      });
    }
    const before = receipt(options.runId);
    if (before.publicationCount !== 0 || collection("workflowruns").some(item =>
      item.metadata.name === options.runId || item.spec.parameters?.publicationID === options.runId)) {
      throw new Error("Story run ID was already used; no replay is permitted");
    }
    const memoryClaim = get("memoryclaim", "story-customer-memory");
    const memoryVolume = get("memoryvolume", memoryClaim.status.boundVolumeName);
    const readStoryMemory = () => JSON.parse(raw([
      "exec", "kognition-tutorial-postgres-0", "-n", "tutorial-system", "-i", "--",
      "psql", "-X", "-U", "kognition", "-d", "kognition", "-v", "ON_ERROR_STOP=1",
      "-v", `volume=${memoryVolume.spec.backendHandle}`, "-v", `tag=${options.runId}`, "-At",
    ], "SELECT COALESCE(json_agg(json_build_object('id', id, 'volumeHandle', volume_id, 'content', content, " +
      "'tag', metadata->>'tag', 'createdAt', created_at) ORDER BY id), '[]'::json) FROM memory_vectors " +
      "WHERE volume_id = :'volume' AND metadata->>'tag' = :'tag';\n"));
    if (readStoryMemory().length !== 0) throw new Error("Memory tag was already used; no duplicate mutation is permitted");
    const profileDirectory = path.join(options.output, "studio-profile");
    fs.mkdirSync(profileDirectory);
    const uiOptions = { cli, mode: "kubernetes",
      namespace: NAMESPACE, "kube-context": cluster.context, systemNamespace: "tutorial-system",
      outputDir: options.output, environment: { ...env, APPDATA: profileDirectory } };
    report.uiPreferenceProfile = profileDirectory;
    const studio = await startUI("studio", uiOptions, undefined, children);
    report.studio = studio;
    const metadata = await fetch(`${studio}/api/meta`, { signal: AbortSignal.timeout(30000) }).then(response => response.json());
    if (metadata.mode !== "kubernetes" || metadata.namespace !== NAMESPACE) throw new Error("Live Studio scope differs");
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport: { width: 1920, height: 1080 },
      recordVideo: { dir: options.output, size: { width: 1920, height: 1080 } } });
    videoEpoch = performance.now();
    report.videoStartedAt = new Date().toISOString();
    page = await context.newPage();
    video = page.video();
    const pageErrors = [];
    page.on("pageerror", error => pageErrors.push(error.message));
    await page.goto(studio, { waitUntil: "domcontentloaded" });
    await expect(page.locator("#status")).toHaveText("ready", { timeout: 60000 });
    if (await dismissWelcome(page)) {
      report.uiPreferenceChanges = [{ action: "dismiss-welcome", profileDirectory, workloadMutation: false }];
    }
    await page.locator("#list-agents").getByText(support, { exact: true }).click();
    await page.locator('[data-view="preview"]').click();
    const chat = async (message, requiredTools, requiredText, clear = false) => {
      if (clear) await page.getByRole("button", { name: "Clear", exact: true }).click();
      const previousCount = await page.locator(".chat-message.assistant .chat-bubble:not(.typing)").count();
      const pending = page.waitForResponse(response => new URL(response.url()).pathname === `/api/invoke/agent/${support}` &&
        response.request().method() === "POST", { timeout: 180000 });
      const input = page.locator(".chat-input-row textarea");
      await input.fill(filming ? "" : message);
      if (filming) await input.pressSequentially(message, { delay: 18 });
      await page.getByRole("button", { name: "Send", exact: true }).click();
      const response = await pending;
      const body = await response.json();
      const recordedCall = { message, httpStatus: response.status(), ...body };
      report.calls.push(recordedCall);
      if (!response.ok() || !body.response || !body.sessionID || body.usage?.totalTokens <= 0 ||
          body.usage.promptTokens + body.usage.completionTokens !== body.usage.totalTokens) {
        throw new Error(`Real story chat failed: HTTP ${response.status()}`);
      }
      const detail = body.toolCallsDetail || [];
      for (const toolName of requiredTools) {
        if (!detail.some(call => call.name === toolName || call.name === toolName.replaceAll("-", "_"))) {
          throw new Error(`Actual invocation evidence lacks ${toolName}`);
        }
        if (["memory_store", "memory_recall"].includes(toolName)) verifyMemoryEvidence(body, toolName, options.runId);
        else verifyGroundedToolEvidence(body, toolName);
      }
      for (const text of requiredText) if (!body.response.includes(text)) throw new Error(`Response lacks evidence signal ${text}`);
      recordedCall.renderedText = await verifyStoryChatView(page, previousCount, requiredText);
      report.milestones.push({ name: "verified-chat", sessionID: body.sessionID, at: new Date().toISOString() });
      if (filming) {
        const readingStarted = (performance.now() - videoEpoch) / 1000;
        const answer = page.locator(".chat-message.assistant .chat-bubble:not(.typing):visible").last();
        recordedCall.readingHold = await holdReadableText(page, answer);
        report.readingHolds.push({ target: "agent-answer", sessionID: body.sessionID,
          startSeconds: readingStarted, endSeconds: (performance.now() - videoEpoch) / 1000,
          ...recordedCall.readingHold });
        await page.locator(".chat-message.assistant").last().locator(".tool-call-badge").click();
        const modal = page.locator("#tool-calls-modal");
        await expect(modal).toBeVisible();
        for (const tool of requiredTools) await expect(modal).toContainText(tool);
        await page.waitForTimeout(2000);
        await modal.locator("button[data-modal-close]").first().click();
      }
      return body;
    };
    cue("calculator", "Invoke an authorized calculator Tool and inspect its actual arguments and result.");
    await chat("What is 17 * 23? Always use story_calculator and report its exact result.", ["story_calculator"], ["391"]);
    cue("knowledge", "Retrieve product evidence from the attached inline KnowledgeBase.");
    await chat("Is Orbital Mark 7 bookable in Brazil South according to the attached product documentation? Search first.",
      ["knowledge_search"], ["Brazil South"]);
    cue("memory-store", "Persist the customer preference through the governed Memory service.");
    const stored = await chat(`Remember using memory_store: Cedar Labs requests incident updates every 30 minutes. Tag this memory exactly ${options.runId}.`,
      ["memory_store"], ["30"]);
    const storeEvidence = verifyMemoryEvidence(stored, "memory_store", options.runId);
    const persisted = readStoryMemory();
    if (persisted.length !== 1 || !persisted[0].id || persisted[0].content !== storeEvidence.args.content ||
        persisted[0].volumeHandle !== memoryVolume.spec.backendHandle) {
      throw new Error("Independent readback did not confirm exactly one stored note in the bound MemoryVolume");
    }
    report.memory = { claimName: memoryClaim.metadata.name, claimUID: memoryClaim.metadata.uid,
      volumeName: memoryVolume.metadata.name, volumeUID: memoryVolume.metadata.uid,
      backendHandle: memoryVolume.spec.backendHandle, record: persisted[0], storeSessionID: stored.sessionID };
    cue("memory-recall", "Clear the conversation and retrieve the actual tagged persistent note.");
    const recalled = await chat(`Use memory_recall with tag ${options.runId} to retrieve Cedar Labs' incident update preference.`,
      ["memory_recall"], ["30"], true);
    const recallEvidence = verifyMemoryEvidence(recalled, "memory_recall", options.runId);
    if (stored.sessionID === recalled.sessionID ||
        !recallEvidence.records.some(record => record.content === persisted[0].content)) {
      throw new Error("Fresh-session recall did not retrieve the independently verified persistent note");
    }
    report.memory.recallSessionID = recalled.sessionID;
    report.memory.recalledRecords = recallEvidence.records;
    const workflow = get("workflow", `${prefix}-incident`);
    cue("workflow", "Start the team workflow through Studio and observe parallel work and the reviewed bulletin.");
    const run = filming
      ? retainCreated(await startStoryWorkflowInStudio(page, workflow.metadata.name, options.runId))
      : create(resource("WorkflowRun", options.runId, {
      workflowRef: { name: workflow.metadata.name }, input: "Prepare fictional INC-1042 bulletin for Cedar Labs.",
      parameters: { memoryTag: options.runId, publicationID: options.runId },
    }));
    verifyStoryRun(run, workflow.metadata.name, options.runId);
    report.workflowRunName = run.metadata.name;
    const pendingApproval = await wait("workflow reached publication gate", () => {
      const state = get("workflowrun", run.metadata.name);
      if (state.status?.phase === "Failed") {
        fs.writeFileSync(path.join(options.output, "workflow.failed.json"), JSON.stringify(state, null, 2));
        throw new Error("Workflow failed before its publication gate");
      }
      return publicationGateReady(state) ? state : false;
    }, 600000);
    report.pendingApproval = pendingApproval;
    const reviewedBulletin = verifyReviewedBulletin(pendingApproval);
    report.reviewApproved = true;
    if (filming) {
      await showStoryStepOutput(page, "review", ["REVIEW: APPROVED", "391", "Brazil South", "30"]);
      const output = page.locator(".run-step-card:visible").filter({
        has: page.locator(".run-step-name").filter({ hasText: /^review$/ }),
      }).locator("pre.run-output");
      const readingStarted = (performance.now() - videoEpoch) / 1000;
      const hold = await holdReadableText(page, output);
      report.readingHolds.push({ target: "reviewer-answer", startSeconds: readingStarted,
        endSeconds: (performance.now() - videoEpoch) / 1000, ...hold });
    }
    report.beforeApproval = receipt(options.runId);
    if (report.beforeApproval.publicationCount !== 0) throw new Error("Publication happened before workflow approval");
    cue("approval", "Record an explicitly automated demonstration approval as the actual authenticated requester.");
    if (filming) {
      retainCreated(await approveStoryWorkflowInStudio(page, run.metadata.name, report.approvalRequester));
      report.workflowObservedThroughStudio = true;
      report.approvalObservedThroughStudio = true;
    } else {
      create(approval);
    }
    report.workflow = await wait("workflow completed", () => {
      const state = get("workflowrun", run.metadata.name);
      if (state.status?.phase === "Failed") {
        fs.writeFileSync(path.join(options.output, "workflow.failed.json"), JSON.stringify(state, null, 2));
        throw new Error("Workflow failed after publication approval");
      }
      return ["Ready", "Succeeded"].includes(state.status?.phase) ? state : false;
    }, 180000);
    report.afterApproval = receipt(options.runId);
    if (report.afterApproval.publicationCount !== 1 ||
        report.afterApproval.content !== reviewedBulletin ||
        !["INC-1042", "391", "Brazil South", "30"].every(text => report.afterApproval.content?.includes(text))) {
      throw new Error("Durable publication receipt does not match the reviewed bulletin");
    }
    cue("publication", "Inspect the completed run and the exact durable publication of the reviewed bulletin.");
    if (filming) {
      await expect(page.locator(".run-phase-row .run-phase")).toHaveText(/^(Ready|Succeeded)$/, { timeout: 30000 });
      await page.getByRole("button", { name: "Open run in sidebar", exact: true }).click();
      await showStoryStepOutput(page, "publish", ["publicationCount", "INC-1042", "391"]);
      report.publicationObservedThroughStudio = true;
      await page.screenshot({ path: path.join(options.output, "publication-visible.png") });
      await page.waitForTimeout(14000);
    }
    const current = get("agent", support);
    const deployment = get("deployment", `agent-${support}`);
    report.supportAgent = current;
    report.supportDeployment = deployment;
    report.agentIdentityUnchanged = current.metadata.uid === supportReady.metadata.uid;
    if (!report.agentIdentityUnchanged) throw new Error("The story Agent identity changed during the rehearsal");
    verifyStoryRuntime(current, deployment);
    report.publicationApprovalAutomated = true;
    if (pageErrors.length) throw new Error("Browser errors occurred during connected rehearsal");
    report.passed = true;
    return report;
  } catch (error) {
    failure = error;
    report.failure = error.message;
    if (page && !page.isClosed()) await page.screenshot({ path: path.join(options.output, "failure.png") });
    throw error;
  } finally {
    if (context) {
      if (filming && report.scenes.length) report.scenes.at(-1).endSeconds = (performance.now() - videoEpoch) / 1000;
      await context.close();
      if (video) await video.saveAs(path.join(options.output,
        failure ? "story.failed.webm" : filming ? "story.recording.webm" : "story.rehearsal.webm"));
    }
    if (browser) await browser.close();
    await stopChildren(children);
    report.completedAt = new Date().toISOString();
    fs.writeFileSync(path.join(options.output, "report.json"), JSON.stringify(report, null, 2), { flag: "wx" });
  }
}

if (require.main === module) {
  Promise.resolve().then(() => runStory(parseOptions(process.argv.slice(2)))).then(result => {
    console.log(`Live story ${result.stage}: ${result.passed ? "passed" : "failed"}`);
  }).catch(error => { console.error(error.message); process.exitCode = 1; });
}

module.exports = {
  extractSandboxSource, resource, fixtureObjects, teamObjects, parseOptions, verifyStoryRuntime,
  verifyMemoryEvidence, verifyGroundedToolEvidence, publicationGateReady, verifyReviewedBulletin, storyApprovalDecision,
  verifyStoryRun, verifyStoryApproval, showStoryStepOutput, verifyStoryChatView, readingWindowSeconds, holdReadableText, runStory,
};
