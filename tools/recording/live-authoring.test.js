// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { chromium } = require("@playwright/test");
const { READING_POLICY, holdReadableText, readingWindowSeconds } = require("./reading");
const {
  APPROVED, RANCHER_TARGET, RANCHER_APPROVED, BASELINE_V3_TARGET, BASELINE_V3_APPROVED,
  BASELINE_V4_TARGET, BASELINE_V4_APPROVED,
  CLI_SHA256, recordingTarget, verifyRecordingCLI,
  BOUNDS, ANSWERS, INSTRUCTIONS, INPUT, VIEWPORT, PENDING, parseOptions, verifyClusterFile,
  verifyController, verifyTarget, verifyModel, verifyAgent, verifyGrant, verifyDeploymentIdentity,
  verifyRuntime, verifyChat, verifyInvocation, verifyRevisionEvidence, createWriteGuard,
  verifyCompletedAnswer, runAuthoring,
  finalizeRecording,
} = require("./live-authoring");

const options = { runId: "unit-new", agentName: "author-unit-new", grantName: "author-unit-new-model" };
const args = ["--runtime", "approved-runtime", "--output", `authoring-unused-${crypto.randomUUID()}`,
  "--run-id", "unit-new", "--authorize-live-writes"];
const copy = value => structuredClone(value);
const labels = () => ({ "kognition.dev/recording-run": options.runId });
const clusterFile = () => ({ context: APPROVED.context, clusterUID: APPROVED.clusterUID, kubeconfig: "kubeconfig.yaml" });

test("Rancher recording requires an explicit exact target without changing the historical default", () => {
  assert.equal(recordingTarget(), APPROVED);
  assert.equal(recordingTarget(RANCHER_TARGET), RANCHER_APPROVED);
  assert.equal(Object.isFrozen(RANCHER_APPROVED), true);
  assert.throws(() => recordingTarget("rancher-desktop"), /exact reviewed target/);
  assert.throws(() => recordingTarget(""), /exact reviewed target/);
  assert.equal(parseOptions([...args, "--target", RANCHER_TARGET]).target, RANCHER_TARGET);
  assert.throws(() => parseOptions([...args, "--target", "unreviewed"]), /exact reviewed target/);
  assert.throws(() => parseOptions([...args, "--target", RANCHER_TARGET, "--target", RANCHER_TARGET]), /Duplicate/);
  for (const field of ["namespace", "systemNamespace", "controller", "controllerImage", "runtimeImage", "model", "profile"]) {
    assert.equal(RANCHER_APPROVED[field], APPROVED[field], `Shared bounded authoring field changed: ${field}`);
  }
});

test("isolated baseline requires explicit selection and its own exact frozen CLI", () => {
  assert.equal(recordingTarget(), APPROVED);
  assert.equal(recordingTarget(RANCHER_TARGET), RANCHER_APPROVED);
  assert.equal(recordingTarget(BASELINE_V3_TARGET), BASELINE_V3_APPROVED);
  assert.equal(Object.isFrozen(BASELINE_V3_APPROVED), true);
  assert.equal(parseOptions([...args, "--target", BASELINE_V3_TARGET]).target, BASELINE_V3_TARGET);
  verifyRecordingCLI(CLI_SHA256);
  verifyRecordingCLI(CLI_SHA256, RANCHER_APPROVED);
  verifyRecordingCLI(BASELINE_V3_APPROVED.cliSHA256, BASELINE_V3_APPROVED);
  assert.throws(() => verifyRecordingCLI(CLI_SHA256, BASELINE_V3_APPROVED));
  assert.throws(() => verifyRecordingCLI(BASELINE_V3_APPROVED.cliSHA256, RANCHER_APPROVED));
  assert.throws(() => verifyRecordingCLI("0".repeat(64), BASELINE_V3_APPROVED));
});

test("the repaired trust target preserves the old profiles and separately pins both backend boundaries", () => {
  assert.equal(recordingTarget(BASELINE_V4_TARGET), BASELINE_V4_APPROVED);
  assert.equal(recordingTarget(BASELINE_V3_TARGET), BASELINE_V3_APPROVED);
  assert.equal(BASELINE_V4_APPROVED.cliSHA256, BASELINE_V3_APPROVED.cliSHA256);
  assert.equal(BASELINE_V4_APPROVED.cliSourceSHA256, BASELINE_V3_APPROVED.baselineSourceSHA256);
  assert.notEqual(BASELINE_V4_APPROVED.baselineSourceSHA256, BASELINE_V3_APPROVED.baselineSourceSHA256);
  assert.notEqual(BASELINE_V4_APPROVED.controllerImage, BASELINE_V3_APPROVED.controllerImage);
  assert.equal(BASELINE_V4_APPROVED.toolGatewayGeneration, 1);
  assert.match(BASELINE_V4_APPROVED.toolGatewayImage, /@sha256:2ad4e66f/);
  assert.equal(BASELINE_V4_APPROVED.tutorialTokenBudget, 4000);
});

for (const target of [RANCHER_APPROVED, BASELINE_V3_APPROVED, BASELINE_V4_APPROVED]) {
test(`${target.context} preflight rejects identity, endpoint, activation and physical rollout drift`, () => {
  const fixture = () => ({
    cluster: { context: target.context, clusterUID: target.clusterUID, kubeconfig: "reviewed-kubeconfig",
      controllerActivated: true, activationTargetSHA256: target.activationTargetSHA256,
      ...(target.baselineSourceSHA256 ? { baselineSourceSHA256: target.baselineSourceSHA256 } : {}) },
    config: { contexts: [{ name: target.context, context: { cluster: target.context } }],
      clusters: [{ name: target.context, cluster: { server: target.server } }] },
    root: { kind: "Namespace", metadata: { name: "kube-system", uid: target.clusterUID } },
    namespace: { kind: "Namespace", metadata: { name: target.namespace, uid: target.namespaceUID }, status: { phase: "Active" } },
    controller: {
      kind: "Deployment", metadata: { name: target.controller, namespace: target.systemNamespace,
        uid: target.controllerUID, generation: target.controllerGeneration },
      spec: { replicas: 1, template: { spec: { containers: [{ image: target.controllerImage }] } } },
      status: { observedGeneration: target.controllerGeneration, replicas: 1, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 1 },
    },
  });
  const verify = value => verifyTarget(value.cluster, value.config, value.root, value.namespace, value.controller, target);
  verify(fixture());
  assert.throws(() => verifyClusterFile(fixture().cluster));
  assert.throws(() => verifyController(fixture().controller));
  for (const mutate of [
    value => { value.cluster.activationTargetSHA256 = "0".repeat(64); },
    value => { value.cluster.controllerActivated = false; },
    value => { value.config.contexts[0].name = APPROVED.context; },
    value => { value.config.clusters[0].cluster.server = APPROVED.server; },
    value => { value.config.clusters[0].cluster["insecure-skip-tls-verify"] = true; },
    value => { value.root.metadata.uid = "replacement"; },
    value => { value.namespace.metadata.uid = "replacement"; },
    value => { value.controller.metadata.uid = "replacement"; },
    value => { value.controller.metadata.generation++; },
    value => { value.controller.status.observedGeneration--; },
    value => { value.controller.status.readyReplicas = 0; },
    value => { value.controller.status.availableReplicas = 0; },
    value => { value.controller.status.updatedReplicas = 0; },
    value => { value.controller.spec.template.spec.containers[0].image = "unreviewed"; },
  ]) {
    const changed = fixture();
    mutate(changed);
    assert.throws(() => verify(changed));
  }
  if (target.baselineSourceSHA256) {
    const changed = fixture();
    delete changed.cluster.baselineSourceSHA256;
    assert.throws(() => verify(changed));
  }
});
}

function controllerFixture() {
  return {
    kind: "Deployment",
    metadata: { name: APPROVED.controller, namespace: APPROVED.systemNamespace,
      uid: APPROVED.controllerUID, generation: APPROVED.controllerGeneration },
    spec: { replicas: 1, template: { spec: { containers: [{ name: "controller", image: APPROVED.controllerImage }] } } },
    status: { observedGeneration: 9, replicas: 1, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 1 },
  };
}

function targetFixture() {
  return {
    cluster: clusterFile(),
    config: { contexts: [{ name: APPROVED.context, context: { cluster: "approved-cluster" } }],
      clusters: [{ name: "approved-cluster", cluster: { server: APPROVED.server } }] },
    root: { kind: "Namespace", metadata: { name: "kube-system", uid: APPROVED.clusterUID } },
    namespace: { kind: "Namespace", metadata: { name: APPROVED.namespace, uid: APPROVED.namespaceUID }, status: { phase: "Active" } },
    controller: controllerFixture(),
  };
}

function agentFixture(version = 1) {
  return {
    apiVersion: "kognition.dev/v1alpha1", kind: "Agent",
    metadata: { name: options.agentName, namespace: APPROVED.namespace, uid: "agent-uid", generation: version,
      resourceVersion: String(100 + version), labels: labels() },
    spec: { modelRef: { name: APPROVED.model }, instructions: { inline: INSTRUCTIONS[version - 1] },
      runtime: { class: BOUNDS.class, timeout: BOUNDS.timeout, maxTokens: BOUNDS.maxTokens }, replicas: BOUNDS.replicas },
    status: { phase: "Ready", revisionCount: version, replicas: 1, readyReplicas: 1 },
  };
}

function grantFixture() {
  return {
    apiVersion: "kognition.dev/v1alpha1", kind: "ModelGrant",
    metadata: { name: options.grantName, namespace: APPROVED.namespace, uid: "grant-uid", labels: labels() },
    spec: { agentRef: { name: options.agentName }, modelRef: { name: APPROVED.model } },
  };
}

function deploymentFixture(version = 1) {
  return {
    kind: "Deployment",
    metadata: { name: `agent-${options.agentName}`, namespace: APPROVED.namespace, uid: "runtime-uid", generation: version + 2,
      annotations: { "kognition.dev/owner-agent": `${APPROVED.namespace}/${options.agentName}` } },
    spec: { replicas: 1, template: { spec: { containers: [{
      name: "agent", image: APPROVED.runtimeImage, env: [
        { name: "KOGNITION_SYSTEM_PROMPT", value: INSTRUCTIONS[version - 1] },
        { name: "KOGNITION_AGENT_NAME", value: options.agentName },
        { name: "KOGNITION_NAMESPACE", value: APPROVED.namespace },
        { name: "KOGNITION_MAX_TOKENS", value: String(BOUNDS.maxTokens) },
        { name: "KOGNITION_TIMEOUT_SECONDS", value: BOUNDS.timeout },
      ],
    }] } } },
    status: { observedGeneration: version + 2, replicas: 1, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 1 },
  };
}

function expectedRuntime(version = 1) {
  return { uid: "agent-uid", generation: version, deploymentUID: "runtime-uid", deploymentGeneration: version + 2 };
}

function modelFixture() {
  return {
    model: {
      apiVersion: "kognition.dev/v1alpha1", kind: "Model",
      metadata: { name: APPROVED.model, namespace: APPROVED.namespace, uid: "model-uid" }, status: { phase: "Ready" },
      spec: { provider: "foundry", parameters: { apiVersion: "2024-12-01-preview" },
        invocationContract: "openai-tool-chat-v1",
        invocationProfileRef: { name: APPROVED.profile, specDigest: "sha256:" + "a".repeat(64) } },
    },
    profile: {
      kind: "ModelInvocationProfile", metadata: { name: APPROVED.profile, namespace: APPROVED.namespace, uid: "profile-uid" },
      spec: { operation: "chat", defaults: { maxCompletionTokens: 512 }, limits: { maxCompletionTokens: 1024 } },
    },
  };
}

function chatFixture(version = 1) {
  const submitted = { message: INPUT, conversationID: `conversation-${version}` };
  const body = { response: ANSWERS[version - 1], sessionID: `session-${version}`, conversationID: submitted.conversationID,
    usage: { promptTokens: 87, completionTokens: 26, totalTokens: 113 }, toolCallsDetail: [] };
  const instance = {
    kind: "AgentInstance", metadata: { name: `invocation-${version}`, uid: `instance-${version}`, namespace: APPROVED.namespace,
      labels: { "kognition.dev/agent": options.agentName, "kognition.dev/created-by": "studio" } },
    spec: { agentRef: { name: options.agentName }, revision: version,
      sessionContext: { input: INPUT, conversation_id: submitted.conversationID } },
    status: { phase: "Ready", sessionID: body.sessionID, output: body.response, tokenUsage: copy(body.usage) },
  };
  return { submitted, body, instance };
}

function revisionFixture() {
  const first = agentFixture(1), second = agentFixture(2);
  const metadata = { name: options.agentName, namespace: APPROVED.namespace };
  const snapshot = agent => ({ kind: "Revision", resource: "Agent",
    metadata: { ...metadata, revision: agent.status.revisionCount }, spec: { spec: copy(agent.spec) } });
  const evidence = {
    revisions: { kind: "RevisionList", metadata, items: [{ revision: 1 }, { revision: 2 }] },
    before: snapshot(first), after: snapshot(second),
    diff: { kind: "RevisionDiff", metadata, fromRevision: 1, toRevision: 2,
      patch: [{ op: "replace", path: "/spec/instructions/inline", value: INSTRUCTIONS[1] }] },
  };
  const parentKey = crypto.createHash("sha256").update(`Agent\0${options.agentName}`).digest("hex").slice(0, 24);
  const configMaps = { kind: "ConfigMapList", items: [first, second].map(agent => ({
    kind: "ConfigMap", immutable: true,
    metadata: { name: `revision-${agent.status.revisionCount}`, uid: `snapshot-${agent.status.revisionCount}`, namespace: APPROVED.namespace,
      annotations: { "kognition.dev/revision": String(agent.status.revisionCount) },
      labels: { "kognition.dev/revision-parent": parentKey, "kognition.dev/revision-kind": "agent" },
      ownerReferences: [{ apiVersion: "kognition.dev/v1alpha1", kind: "Agent", name: options.agentName, uid: first.metadata.uid }] },
    binaryData: { spec: Buffer.from(JSON.stringify({ spec: agent.spec })).toString("base64") },
  })) };
  return { first, second, evidence, configMaps };
}

function newAgentBody() {
  const body = agentFixture();
  for (const key of ["uid", "generation", "resourceVersion"]) delete body.metadata[key];
  delete body.status;
  return body;
}

function newGrantBody() {
  const body = grantFixture();
  delete body.metadata.uid;
  return body;
}

const requestURL = pathname => `http://127.0.0.1:18081${pathname}?namespace=${APPROVED.namespace}`;
function throughGrant() {
  const guard = createWriteGuard(options);
  guard.arm("agent-create");
  guard.consume("POST", requestURL("/api/resources/agents"), newAgentBody());
  guard.arm("grant-create");
  guard.consume("POST", requestURL("/api/resources/modelgrants"), newGrantBody());
  return guard;
}

function ownedDirectory(t) {
  const directory = path.resolve(__dirname, "..", "..", "test-results", `authoring-safeguards-${crypto.randomUUID()}`);
  fs.mkdirSync(directory, { recursive: true });
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  return directory;
}

test("strict authoring options require explicit consent, singular arguments, DNS-safe names and new evidence", t => {
  assert.throws(() => parseOptions([]), /authorize-live-writes/);
  assert.throws(() => parseOptions(args.slice(0, -1)), /authorize-live-writes/);
  const parsed = parseOptions(args);
  assert.equal(parsed.agentName, options.agentName);
  assert.equal(parsed.grantName, options.grantName);
  assert.equal(parsed.runtime, path.resolve("approved-runtime"));
  for (const bad of ["../escape", "a\\b", "a/b", "a.b", "Upper", "-bad", "bad-", "a".repeat(51), "id with spaces"]) {
    assert.throws(() => parseOptions(args.map(value => value === "unit-new" ? bad : value)), /DNS label|ambiguous/);
  }
  const longest = parseOptions(args.map(value => value === "unit-new" ? "a".repeat(50) : value));
  assert.equal(longest.grantName.length, 63);
  assert.throws(() => parseOptions([...args, "--unknown"]), /Unknown option/);
  assert.throws(() => parseOptions([...args, "positional"]), /Unexpected argument/);
  assert.throws(() => parseOptions([...args, "--output", "another"]), /Duplicate option/);
  assert.throws(() => parseOptions([...args, "--authorize-live-writes"]), /Duplicate option/);
  assert.throws(() => parseOptions(args.map(value => value === "--authorize-live-writes" ? "--authorize-live-writes=false" : value)));
  const directory = ownedDirectory(t);
  const marker = path.join(directory, "existing-evidence.txt");
  fs.writeFileSync(marker, "preserve");
  assert.throws(() => parseOptions(args.map(value => value === args[3] ? directory : value)), /already exists/);
  assert.equal(fs.readFileSync(marker, "utf8"), "preserve");
  const ancestor = path.join(directory, "new");
  assert.throws(() => parseOptions(["--runtime", path.join(ancestor, "frozen"), "--output", ancestor,
    "--run-id", "unit-new", "--authorize-live-writes"]), /frozen runtime/);
});

test("preflight binds all supplied and observed target identities, endpoint and namespace without changing context", () => {
  const verify = f => verifyTarget(f.cluster, f.config, f.root, f.namespace, f.controller);
  verify(targetFixture());
  for (const mutate of [
    f => { f.cluster.context = "kind-another"; },
    f => { f.cluster.clusterUID = "another-cluster"; },
    f => { f.cluster.kubeconfig = ""; },
    f => { f.config.contexts[0].name = "kind-another"; },
    f => { f.config.contexts[0].context.cluster = "unselected"; },
    f => { f.config.clusters[0].cluster.server = "https://127.0.0.1:50304"; },
    f => { f.config.clusters[0].cluster.server = "http://127.0.0.1:50303"; },
    f => { f.config.clusters[0].cluster["insecure-skip-tls-verify"] = true; },
    f => { f.config.clusters.push(copy(f.config.clusters[0])); },
    f => { f.root.metadata.uid = "recreated-cluster"; },
    f => { f.namespace.metadata.uid = "recreated-namespace"; },
    f => { f.namespace.metadata.name = "default"; },
    f => { f.namespace.status.phase = "Terminating"; },
    f => { f.namespace.metadata.deletionTimestamp = "2026-11-03T00:00:00Z"; },
  ]) {
    const f = targetFixture();
    mutate(f);
    assert.throws(() => verify(f));
  }
  assert.throws(() => verifyClusterFile({ ...clusterFile(), kubeconfig: "\0" }));
});

test("approved controller requires the fixed UID, generation nine, pinned image and completely ready one-replica rollout", () => {
  verifyController(controllerFixture());
  for (const mutate of [
    c => { c.metadata.uid = "replacement"; },
    c => { c.metadata.generation = 10; },
    c => { c.metadata.namespace = "default"; },
    c => { c.metadata.deletionTimestamp = "2026-11-03T00:00:00Z"; },
    c => { c.spec.replicas = 0; },
    c => { c.status.observedGeneration = 8; },
    c => { c.status.replicas = 2; },
    c => { c.status.readyReplicas = 0; },
    c => { c.status.availableReplicas = 0; },
    c => { c.status.updatedReplicas = 0; },
    c => { c.status.unavailableReplicas = 1; },
    c => { c.spec.template.spec.containers[0].image = "controller:latest"; },
  ]) {
    const controller = controllerFixture();
    mutate(controller);
    assert.throws(() => verifyController(controller), /controller/);
  }
});

test("authoring accepts the prepared 512-default 1024-limit profile while retaining its narrower 256-token runtime", () => {
  const { model, profile } = modelFixture();
  verifyModel(model, profile);
  assert.throws(() => verifyModel(model), /profile/);
  for (const mutate of [
    f => { f.model.metadata.name = "another-model"; },
    f => { f.model.metadata.uid = ""; },
    f => { f.model.status.phase = "Pending"; },
    f => { f.model.spec.invocationProfileRef.name = "unreviewed"; },
    f => { f.model.spec.invocationProfileRef.specDigest = ""; },
    f => { f.model.spec.invocationContract = ""; },
    f => { f.model.spec.parameters.max_tokens = "4096"; },
    f => { f.profile.metadata.uid = ""; },
    f => { f.profile.metadata.namespace = "another-workspace"; },
    f => { f.profile.spec.operation = "embedding"; },
    f => { f.profile.spec.defaults.maxCompletionTokens = 4096; },
    f => { f.profile.spec.limits.maxCompletionTokens = 128; },
  ]) {
    const f = modelFixture();
    mutate(f);
    assert.throws(() => verifyModel(f.model, f.profile));
  }
});

test("new Agent creation accepts Pending before grant but always requires the exact identity, instructions and explicit runtime", () => {
  const agent = agentFixture();
  agent.status = { phase: "Pending" };
  verifyAgent(agent, options, 1, agent.metadata);
  assert.throws(() => verifyAgent(agent, options, 1, agent.metadata, true), /readiness/);
  for (const mutate of [
    a => { a.metadata.uid = "replacement"; },
    a => { a.metadata.generation = 2; },
    a => { a.metadata.namespace = "default"; },
    a => { a.metadata.labels = {}; },
    a => { a.spec.modelRef.name = "another-model"; },
    a => { a.spec.instructions.inline = "not the submitted instructions"; },
    a => { a.spec.runtime.timeout = "45s"; },
    a => { a.spec.runtime.maxTokens = 4096; },
    a => { a.spec.runtime.class = "autogen"; },
    a => { a.spec.replicas = 0; },
  ]) {
    const f = copy(agent);
    mutate(f);
    assert.throws(() => verifyAgent(f, options, 1, agent.metadata));
  }
  verifyGrant(grantFixture(), options, "grant-uid");
  assert.throws(() => verifyGrant(grantFixture(), options, "replacement"));
  const wrong = grantFixture();
  wrong.spec.agentRef.name = "another-agent";
  assert.throws(() => verifyGrant(wrong, options), /authorization/);
});

test("runtime readiness rejects zero-replica Ready metadata, replacement identities, stale generations and old instructions", () => {
  const verify = (a, d, expected = expectedRuntime(2)) =>
    verifyRuntime(a, d, options, expected, 2, deploymentFixture(1));
  verify(agentFixture(2), deploymentFixture(2));
  assert.throws(() => verify(agentFixture(2), undefined));
  assert.throws(() => verifyDeploymentIdentity(deploymentFixture(2), options, "different-runtime"));
  for (const mutate of [
    f => { f.agent.metadata.uid = "replacement"; },
    f => { f.agent.metadata.generation = 3; },
    f => { f.agent.spec.replicas = 0; f.agent.status.readyReplicas = 0; },
    f => { f.agent.spec.runtime.timeout = "240s"; },
    f => { f.agent.status.phase = "Pending"; },
    f => { f.agent.status.readyReplicas = 0; },
    f => { f.deployment.metadata.uid = "replacement"; },
    f => { f.deployment.metadata.generation = 3; },
    f => { f.deployment.metadata.annotations["kognition.dev/owner-agent"] = "default/other"; },
    f => { f.deployment.spec.replicas = 0; },
    f => { f.deployment.spec.template.spec.containers[0].image = "agent:latest"; },
    f => { f.deployment.status.observedGeneration = 3; },
    f => { f.deployment.status.replicas = 2; },
    f => { f.deployment.status.availableReplicas = 0; },
    f => { f.deployment.status.readyReplicas = 0; },
    f => { f.deployment.status.updatedReplicas = 0; },
    f => { f.deployment.status.unavailableReplicas = 1; },
    f => { f.deployment.spec.template.spec.containers[0].env[0].value = INSTRUCTIONS[0]; },
    f => { f.deployment.spec.template.spec.containers[0].env[3].value = "512"; },
    f => { f.deployment.spec.template.spec.containers[0].env[4].value = "60s"; },
  ]) {
    const f = { agent: agentFixture(2), deployment: deploymentFixture(2) };
    mutate(f);
    assert.throws(() => verify(f.agent, f.deployment));
  }
  const sameRollout = deploymentFixture(1);
  sameRollout.spec.template.spec.containers[0].env[0].value = INSTRUCTIONS[1];
  assert.throws(() => verify(agentFixture(2), sameRollout, { ...expectedRuntime(2), deploymentGeneration: 3 }), /new rollout/);
});

test("mutation guard permits exactly one create, grant, V1 invocation, instructions-only update and fresh V2 invocation", () => {
  const guard = throughGrant();
  const v1 = chatFixture(1), v2 = chatFixture(2);
  guard.arm("chat-v1");
  guard.consume("POST", requestURL(`/api/invoke/agent/${options.agentName}`), v1.submitted);
  assert.throws(() => guard.consume("POST", requestURL(`/api/invoke/agent/${options.agentName}`), v1.submitted), /Unarmed/);
  assert.throws(() => guard.arm("chat-v1"), /repeated or reordered/);
  guard.arm("agent-revise", agentFixture(1));
  const revised = agentFixture(2);
  revised.metadata.generation = 1;
  guard.consume("PUT", requestURL(`/api/resources/agents/${options.agentName}`), revised);
  guard.arm("chat-v2");
  guard.consume("POST", requestURL(`/api/invoke/agent/${options.agentName}`), v2.submitted);
  guard.verifyComplete();
  assert.equal(guard.submissions.length, 5);
  assert.equal(guard.submissions[0].body.spec.replicas, 1);
  assert.equal(guard.submissions[3].body.spec.instructions.inline, INSTRUCTIONS[1]);
  assert.throws(() => guard.arm("agent-create"), /repeated or reordered/);
});

test("the unchanged Studio builder may serialize empty probe sections but cannot add a configured execution hook", () => {
  const body = newAgentBody();
  body.spec.probes = {};
  body.spec.restartLadder = {};
  const guard = createWriteGuard(options);
  guard.arm("agent-create");
  guard.consume("POST", requestURL("/api/resources/agents"), body);
  const invalid = createWriteGuard(options);
  invalid.arm("agent-create");
  body.spec.probes = { liveness: { type: "http", path: "/changed" } };
  assert.throws(() => invalid.consume("POST", requestURL("/api/resources/agents"), body));
});

test("mutation guard refuses out-of-scope writes, upserts, hidden capabilities, excessive bounds and invalid input", () => {
  for (const [method, pathname, mutate] of [
    ["DELETE", "/api/resources/agents", () => {}],
    ["POST", "/api/resources/modelgrants", () => {}],
    ["POST", "/api/resources/agents/another", () => {}],
    ["POST", "/api/resources/agents", b => { b.metadata.uid = "existing"; }],
    ["POST", "/api/resources/agents", b => { b.metadata.resourceVersion = "1"; }],
    ["POST", "/api/resources/agents", b => { b.metadata.namespace = "default"; }],
    ["POST", "/api/resources/agents", b => { b.metadata.labels = {}; }],
    ["POST", "/api/resources/agents", b => { b.spec.runtime.maxTokens = 512; }],
    ["POST", "/api/resources/agents", b => { b.spec.replicas = 0; }],
    ["POST", "/api/resources/agents", b => { b.spec.tools = [{ name: "unapproved" }]; }],
    ["POST", "/api/resources/agents", b => { b.spec.instructions.inline = INSTRUCTIONS[1]; }],
  ]) {
    const guard = createWriteGuard(options), body = newAgentBody();
    guard.arm("agent-create");
    mutate(body);
    assert.throws(() => guard.consume(method, requestURL(pathname), body));
    assert.equal(guard.submissions.length, 0);
  }
  const wrongNamespace = createWriteGuard(options);
  wrongNamespace.arm("agent-create");
  assert.throws(() => wrongNamespace.consume("POST", requestURL("/api/resources/agents") + "&namespace=default", newAgentBody()));
  const guard = throughGrant();
  guard.arm("chat-v1");
  assert.throws(() => guard.consume("POST", requestURL(`/api/invoke/agent/${options.agentName}`),
    { ...chatFixture().submitted, sessionID: "previous-session" }), /fresh conversation/);
  assert.throws(() => guard.consume("POST", requestURL(`/api/invoke/agent/${options.agentName}`),
    { ...chatFixture().submitted, message: "do something else" }));
  assert.throws(() => guard.verifyComplete(), /incomplete/);
});

test("revision mutation requires original UID, generation, resourceVersion and unchanged non-instruction specification", () => {
  for (const mutate of [
    b => { b.metadata.uid = "replacement"; },
    b => { b.metadata.generation = 2; },
    b => { delete b.metadata.resourceVersion; },
    b => { b.spec.runtime.timeout = "240s"; },
    b => { b.spec.replicas = 0; },
    b => { b.spec.instructions.inline = INSTRUCTIONS[0]; },
  ]) {
    const guard = throughGrant();
    guard.arm("chat-v1");
    guard.consume("POST", requestURL(`/api/invoke/agent/${options.agentName}`), chatFixture().submitted);
    guard.arm("agent-revise", agentFixture(1));
    const body = agentFixture(2);
    body.metadata.generation = 1;
    mutate(body);
    assert.throws(() => guard.consume("PUT", requestURL(`/api/resources/agents/${options.agentName}`), body));
  }
});

test("live response contracts reject positive prose with errors, invented/over-budget usage and reused conversations", () => {
  const { submitted, body } = chatFixture();
  verifyChat(body, 1, submitted);
  verifyChat(chatFixture(2).body, 2, chatFixture(2).submitted, body);
  for (const mutate of [
    f => { f.body.response = "I successfully fixed the incident."; },
    f => { f.body.error = "ModelAccessDenied"; },
    f => { f.body.error_code = "Unavailable"; },
    f => { f.body.sessionID = ""; },
    f => { f.body.conversationID = "unrelated"; },
    f => { f.submitted.sessionID = "previous-session"; },
    f => { f.body.usage.totalTokens = 999; },
    f => { f.body.usage.promptTokens = 0; },
    f => { f.body.usage.completionTokens = 0; },
    f => { f.body.usage.completionTokens = 257; f.body.usage.totalTokens = f.body.usage.promptTokens + 257; },
    f => { f.body.usage.totalTokens = "113"; },
    f => { f.body.toolCallsDetail = [{ name: "unapproved" }]; },
  ]) {
    const f = chatFixture();
    mutate(f);
    assert.throws(() => verifyChat(f.body, 1, f.submitted), /Live answer/);
  }
  const next = chatFixture(2);
  assert.throws(() => verifyChat({ ...next.body, sessionID: body.sessionID }, 2, next.submitted, body), /fresh/);
  assert.throws(() => verifyChat({ ...next.body, conversationID: body.conversationID }, 2,
    { ...next.submitted, conversationID: body.conversationID }, body), /fresh/);
});

test("persisted invocation readback binds actual input, current Agent revision, session, conversation, completed output and usage", () => {
  const { submitted, body, instance } = chatFixture(2);
  verifyInvocation(instance, agentFixture(2), submitted, body);
  for (const mutate of [
    i => { i.metadata.uid = ""; },
    i => { i.metadata.namespace = "default"; },
    i => { i.metadata.labels["kognition.dev/created-by"] = "cli"; },
    i => { i.spec.agentRef.name = "other"; },
    i => { i.spec.revision = 1; },
    i => { i.spec.sessionContext.input = "different input"; },
    i => { i.spec.sessionContext.conversation_id = "another"; },
    i => { i.status.sessionID = "another"; },
    i => { i.status.phase = "Running"; },
    i => { i.status.output = "unrelated positive prose"; },
    i => { i.status.tokenUsage.totalTokens++; },
  ]) {
    const f = copy(instance);
    mutate(f);
    assert.throws(() => verifyInvocation(f, agentFixture(2), submitted, body), /Independent AgentInstance/);
  }
});

test("revision evidence requires next-generation same-UID instruction changes and actual immutable parent-owned snapshots", () => {
  const verify = f => verifyRevisionEvidence(f.evidence, f.first, f.second, f.configMaps);
  verify(revisionFixture());
  for (const mutate of [
    f => { f.second.metadata.uid = "recreated"; },
    f => { f.second.metadata.generation = 3; },
    f => { f.second.status.revisionCount = 3; },
    f => { f.second.spec.runtime.maxTokens = 512; },
    f => { f.evidence.diff.patch.push({ op: "replace", path: "/spec/replicas", value: 0 }); },
    f => { f.evidence.revisions.items = [{ revision: 2 }]; },
    f => { f.evidence.before.metadata.namespace = "default"; },
    f => { f.evidence.after.spec = f.evidence.before.spec; },
    f => { f.evidence.after.spec.spec.extra = "not persisted"; },
    f => { f.evidence.after.resource = "Prompt"; },
    f => { f.configMaps.items[0].immutable = false; },
    f => { f.configMaps.items[0].metadata.ownerReferences[0].uid = "prior-agent"; },
    f => { f.configMaps.items[0].metadata.namespace = "default"; },
    f => { f.configMaps.items[0].metadata.labels["kognition.dev/revision-parent"] = "different"; },
    f => { f.configMaps.items[0].binaryData.spec = Buffer.from('{"spec":{}}').toString("base64"); },
    f => { f.configMaps.items.push(copy(f.configMaps.items[0])); },
    f => { f.configMaps.items = []; },
  ]) {
    const f = revisionFixture();
    mutate(f);
    assert.throws(() => verify(f));
  }
});

test("immutable internal snapshots preserve empty restart semantics when Go serializes missing steps as null", () => {
  const f = revisionFixture();
  for (const [agent, envelope, snapshot] of [
    [f.first, f.evidence.before, f.configMaps.items[0]],
    [f.second, f.evidence.after, f.configMaps.items[1]],
  ]) {
    agent.spec.probes = {};
    agent.spec.restartLadder = {};
    envelope.spec.spec.probes = {};
    envelope.spec.spec.restartLadder = { steps: null };
    snapshot.binaryData.spec = Buffer.from(JSON.stringify(envelope.spec)).toString("base64");
  }
  verifyRevisionEvidence(f.evidence, f.first, f.second, f.configMaps);
  f.evidence.after.spec.spec.restartLadder.steps = [{ action: "restart" }];
  assert.throws(() => verifyRevisionEvidence(f.evidence, f.first, f.second, f.configMaps));
});

test("finalization persists a real browser recording before releasing its browser connection", async t => {
  const directory = ownedDirectory(t);
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext({ viewport: { width: 640, height: 360 },
      recordVideo: { dir: directory, size: { width: 640, height: 360 } } });
    const page = await context.newPage();
    await page.setContent("<h1>Recording finalization regression</h1>");
    await page.waitForTimeout(200);
    const destination = path.join(directory, "finalized.webm");
    const result = await finalizeRecording(context, browser, page.video(), destination);
    assert.equal(result.path, destination);
    assert.ok(result.bytes > 256);
    assert.equal(fs.readFileSync(destination).subarray(0, 4).toString("hex"), "1a45dfa3");
    assert.equal(browser.isConnected(), false);
  } finally {
    if (browser.isConnected()) await browser.close();
  }
});

test("failed preflight persists a failed report without launching UI, retrying chat, deleting resources or reusing its output", async t => {
  const directory = ownedDirectory(t);
  const runtime = path.join(directory, "runtime");
  fs.mkdirSync(path.join(runtime, "windows"), { recursive: true });
  fs.writeFileSync(path.join(runtime, "windows", "kognitionctl.exe"), "unit fixture - never executed");
  fs.writeFileSync(path.join(runtime, "cluster.json"), JSON.stringify({ ...clusterFile(), context: "not-approved" }));
  const output = path.join(directory, "failure-take");
  const parsed = parseOptions(["--runtime", runtime, "--output", output, "--run-id", "unit-new", "--authorize-live-writes"]);
  await assert.rejects(runAuthoring(parsed), /exact approved tutorial target/);
  const report = JSON.parse(fs.readFileSync(path.join(output, "report.json"), "utf8"));
  assert.equal(report.passed, false);
  assert.equal(report.mockedAPI, false);
  assert.equal(report.fullTutorialRecorded, false);
  assert.equal(report.coverage.conversationHistoryProven, false);
  assert.equal(report.safety.noChatRetries, true);
  assert.equal(report.safety.noResourceDeletion, true);
  assert.deepEqual(report.submissions, []);
  assert.deepEqual(report.calls, []);
  assert.equal(report.recording, undefined);
  const originalReport = fs.readFileSync(path.join(output, "report.json"), "utf8");
  await assert.rejects(runAuthoring(parsed), /already exists/);
  assert.equal(fs.readFileSync(path.join(output, "report.json"), "utf8"), originalReport);
  assert.equal(report.coverage.entireScenesCompleted.length, 0);
  for (const id of ["S04", "S05", "S06", "S07"]) assert.ok(PENDING.some(item => item.scene === id));
});

test("completed answer verification excludes typing/errors and holds the rendered answer after completion, not model latency", async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: VIEWPORT });
    const body = chatFixture().body;
    await page.setContent('<div id="preview-host"><div class="chat-message assistant">' +
      '<div class="chat-bubble typing">Pending</div></div></div>');
    await page.evaluate(({ response, totalTokens }) => {
      setTimeout(() => {
        const row = document.querySelector(".chat-message");
        row.innerHTML = '<div class="chat-bubble"></div><div class="chat-meta"></div>';
        row.querySelector(".chat-bubble").textContent = response;
        row.querySelector(".chat-meta").textContent = `${totalTokens} tokens`;
      }, 250);
    }, { response: body.response, totalTokens: body.usage.totalTokens });
    const bubble = await verifyCompletedAnswer(page, 0, body);
    const completedAt = performance.now();
    const hold = await holdReadableText(page, bubble);
    assert.ok(hold.visibleSeconds >= readingWindowSeconds(body.response));
    assert.ok((performance.now() - completedAt) / 1000 >= hold.requiredSeconds);
    assert.equal(hold.wordCount, body.response.split(/\s+/).length);
    assert.equal(hold.textSHA256, crypto.createHash("sha256").update(body.response).digest("hex"));
    assert.deepEqual(READING_POLICY, { wordsPerMinute: 150, orientationSeconds: 2, minimumSeconds: 5, capped: false });
    await page.locator(".chat-bubble").evaluate(element => element.classList.add("error"));
    await assert.rejects(verifyCompletedAnswer(page, 0, body, 150));
    await page.locator(".chat-bubble").evaluate(element => { element.className = "chat-bubble typing"; });
    await assert.rejects(verifyCompletedAnswer(page, 0, body, 150));
  } finally {
    await browser.close();
  }
});
