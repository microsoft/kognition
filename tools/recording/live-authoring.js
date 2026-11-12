// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { execFile } = require("node:child_process");
const { parseArgs, promisify, isDeepStrictEqual } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const {
  startUI, stopChildren, fieldLabel, verifyKubernetesRuntime,
  verifyModel: verifyPilotModel, verifyRevisions,
} = require("./live-pilot");
const { READING_POLICY, holdReadableText } = require("./reading");

const execute = promisify(execFile);
const API = "kognition.dev/v1alpha1";
const VIEWPORT = Object.freeze({ width: 1920, height: 1080 });
const APPROVED = Object.freeze({
  context: "kind-kognition-tutorial-20261029-220040",
  clusterUID: "9f5bdd0d-281b-432e-8cb6-39d9b34c6d98",
  server: "https://127.0.0.1:50303",
  namespace: "tutorial-support",
  namespaceUID: "757c831a-9c8b-4e1d-aa02-4905795da1e8",
  systemNamespace: "tutorial-system",
  controller: "kognition-tutorial-k8s-controller",
  controllerUID: "029334a0-60a0-4180-8d91-850b8d898f16",
  controllerGeneration: 9,
  controllerImage: "docker.io/kognition-tutorial/k8s-controller@sha256:444e940bf22189b9947eae9c2b005c4c78788f366fc042774cffdb5538cc128c",
  runtimeImage: "docker.io/kognition-tutorial/agent-runtime@sha256:083ae72319c3aacdf7fd441775e2ffda3a6bdba0fc61feba63f77f4119e8aea2",
  model: "tutorial-support-model",
  profile: "tutorial-chat",
});
const CLI_SHA256 = "da20389ca3cf8ef313281af37b47a8f5957ff0d3764e062d8b2ff3cbcf45db27";
const RANCHER_TARGET = "rancher-tutorial-20261104";
const RANCHER_APPROVED = Object.freeze({
  ...APPROVED,
  context: "rancher-desktop",
  clusterUID: "361cd5ab-d49e-4a68-ad51-5e8b27dc1eaa",
  server: "https://127.0.0.1:6443",
  namespaceUID: "d85c34f7-69d3-45ad-919a-495d6d3a0bc4",
  controllerUID: "bc5f0102-45a0-4f12-94b0-72146587bb92",
  controllerGeneration: 2,
  activationTargetSHA256: "41efa4e44d3ea3eb08447019643f30953d982926d5a284982c760089e9660460",
});
const BASELINE_V3_TARGET = "isolated-tutorial-20261106-v3";
const BASELINE_V3_APPROVED = Object.freeze({
  ...APPROVED,
  context: "kind-kognition-video-v3",
  clusterUID: "e08de6bd-4e2b-4a16-b3a9-4783e27ca2f4",
  server: "https://127.0.0.1:16443",
  namespaceUID: "16dd8b57-eeb1-4cf9-a1db-d2847691719b",
  controllerUID: "be56cb04-3072-4a00-9749-ce8c019c3c87",
  controllerGeneration: 2,
  controllerImage: "docker.io/kognition-tutorial/k8s-controller@sha256:23f7b06644291fd8de4e7e339aa1c8c3c857431b7d0f1a34f93980a60f4b847f",
  activationTargetSHA256: "d61b0c698fdda3f1d98af6113fdf5b5e7cc229abde6320771e13993cd1d8572d",
  baselineSourceSHA256: "f633d15b0627875f38f1f28f3ddbf6d1dec5d1310515088aa6818ff17a117cef",
  cliSHA256: "c8c8648fcd1d0123a8ccb10a3f5a0e753e4abb5e734acb49b65896fce86eaf45",
  uiRevision: "create-picker-v1",
  tutorialTokenBudget: 50000,
});
const BASELINE_V4_TARGET = "isolated-tutorial-20261107-v4";
const BASELINE_V4_APPROVED = Object.freeze({
  ...BASELINE_V3_APPROVED,
  context: "kind-kognition-video-v4",
  clusterUID: "9eb21dc0-e813-4053-b246-e591ca03503a",
  server: "https://127.0.0.1:17443",
  namespaceUID: "6f6d7a87-049f-4945-8464-bbcf1614980a",
  controllerUID: "00630cf6-1bef-4212-ae44-7ea5117c584d",
  controllerImage: "docker.io/kognition-tutorial/k8s-controller@sha256:9def2a1ac56fe347aa56df15e3849d14cfd3614ea4cdbbc6df3d2e58db36b755",
  activationTargetSHA256: "55b16253d3df4ed2b21a3cd87569ab03cecfac9552519777fc4db289ada51d8c",
  baselineSourceSHA256: "2bf83ff7e4716eb8a366bcaf9ea50b4ca3c4e122c36a83848449466f9fa8a576",
  cliSourceSHA256: BASELINE_V3_APPROVED.baselineSourceSHA256,
  tutorialTokenBudget: 4000,
  toolGateway: "kognition-tutorial-tool-gateway",
  toolGatewayUID: "3361d55a-5f6a-4e96-a300-10e8dca69ac7",
  toolGatewayGeneration: 1,
  toolGatewayImage: "docker.io/kognition-tutorial/tool-gateway@sha256:2ad4e66fc171e7503e52c5d7d03a912b4660b7138445e6b78e23146ce68f75ff",
});

function recordingTarget(name) {
  if (name === undefined) return APPROVED;
  if (name === RANCHER_TARGET) return RANCHER_APPROVED;
  if (name === BASELINE_V3_TARGET) return BASELINE_V3_APPROVED;
  if (name === BASELINE_V4_TARGET) return BASELINE_V4_APPROVED;
  throw new Error("Unknown recording target; an exact reviewed target is required");
}

function verifyRecordingCLI(digest, approved = APPROVED) {
  if (digest !== (approved.cliSHA256 || CLI_SHA256)) throw new Error("The approved frozen CLI digest differs");
}

function isWelcomeDismissal(method, url, body, previousCount) {
  const target = new URL(url);
  return method === "POST" && target.pathname === "/api/studio/tutorial-dismiss" &&
    target.search === "" && previousCount === 0 && isDeepStrictEqual(body, {});
}

async function dismissWelcome(page) {
  const button = page.getByRole("button", { name: "Not now", exact: true });
  if (!await button.isVisible()) return false;
  const [response] = await Promise.all([
    page.waitForResponse(item => new URL(item.url()).pathname === "/api/studio/tutorial-dismiss"),
    button.press("Enter"),
  ]);
  if (!response.ok() || (await response.json()).dismissed !== true) {
    throw new Error("Recording-owned welcome preference was not saved");
  }
  return true;
}

const BOUNDS = Object.freeze({ class: "default", timeout: "120s", maxTokens: 256, replicas: 1 });
const ANSWERS = Object.freeze([
  "SUPPORT V1: For Cedar Labs INC-1042 on Orbital Mark 7, collect the region and timeout rate.",
  "SUPPORT V2: For Cedar Labs INC-1042 on Orbital Mark 7, collect the region, timeout rate, and customer impact.",
]);
const INSTRUCTIONS = Object.freeze(ANSWERS.map(answer =>
  "You assist Cedar Labs with fictional incident INC-1042 on the fictional Orbital Mark 7 product. " +
  "This is a bounded response-format demonstration, not an actual incident investigation. " +
  `Reply with exactly this sentence and no other text: ${answer}`));
const INPUT = "Help Cedar Labs triage fictional incident INC-1042 on Orbital Mark 7.";
const STEPS = Object.freeze(["agent-create", "grant-create", "chat-v1", "agent-revise", "chat-v2"]);
const PENDING = Object.freeze([
  { scene: "S02", actions: ["On-screen CLI version and standalone/Kubernetes comparison."] },
  { scene: "S03", actions: ["Workspace, AgentJob, class/ownership and full resource-model walkthrough."] },
  { scene: "S04", actions: [
    "Gallery bundle/prerequisites inspection and separately reviewed import/results.",
    "ModelClass capabilities and credential-reference inspection; remaining Agent capability sections.",
  ] },
  { scene: "S05", actions: [
    "Same-conversation follow-up and arbitrary conversation-history behavior.",
    "CLI chat, status, logs and events; broader execution inspection.",
  ] },
  { scene: "S06", actions: ["Prompt authoring, stored declarations/defaults, versions and execution-path qualification of references."] },
  { scene: "S07", actions: [
    "Apply YAML round-trip and disposable malformed-YAML draft.",
    "Prompt revision history; on-screen CLI get/describe/watch and optimistic-concurrency conflict.",
  ] },
]);

function parseOptions(args) {
  const { values, tokens } = parseArgs({ args, strict: true, allowPositionals: false, tokens: true, options: {
    runtime: { type: "string" }, output: { type: "string" }, "run-id": { type: "string" },
    target: { type: "string" },
    "authorize-live-writes": { type: "boolean" },
  } });
  const seen = new Set();
  for (const token of tokens.filter(token => token.kind === "option")) {
    if (seen.has(token.name)) throw new Error(`Duplicate option --${token.name}`);
    seen.add(token.name);
  }
  if (!values["authorize-live-writes"]) throw new Error("--authorize-live-writes is required");
  recordingTarget(values.target);
  for (const name of ["runtime", "output", "run-id"]) {
    if (!values[name]?.trim() || values[name].includes("\0")) throw new Error(`--${name} is required`);
  }
  const runId = values["run-id"];
  if (runId.length > 50 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(runId)) {
    throw new Error("--run-id must be a lowercase DNS label of at most 50 characters");
  }
  const runtime = path.resolve(values.runtime);
  const output = path.resolve(values.output);
  if (fs.existsSync(output)) throw new Error("Output already exists; preserve it and choose a new output directory");
  const relative = path.relative(output, runtime);
  if (!relative || (!relative.startsWith(`..${path.sep}`) && relative !== ".." && !path.isAbsolute(relative))) {
    throw new Error("Output must not contain or replace the frozen runtime directory");
  }
  return { runtime, output, runId, authorizedLiveWrites: true,
    ...(values.target === undefined ? {} : { target: values.target }),
    agentName: `author-${runId}`, grantName: `author-${runId}-model` };
}

function verifyClusterFile(cluster, approved = APPROVED) {
  if (cluster?.context !== approved.context || cluster.clusterUID !== approved.clusterUID ||
      (approved.activationTargetSHA256 && (cluster.activationTargetSHA256 !== approved.activationTargetSHA256 ||
        cluster.controllerActivated !== true)) ||
      (approved.baselineSourceSHA256 && cluster.baselineSourceSHA256 !== approved.baselineSourceSHA256) ||
      typeof cluster.kubeconfig !== "string" || !cluster.kubeconfig.trim() || cluster.kubeconfig.includes("\0")) {
    throw new Error("The runtime cluster.json differs from the exact approved tutorial target");
  }
}

function verifyController(controller, approved = APPROVED) {
  const metadata = controller?.metadata;
  const status = controller?.status;
  if (controller?.kind !== "Deployment" || metadata?.name !== approved.controller ||
      metadata.namespace !== approved.systemNamespace || metadata.uid !== approved.controllerUID ||
      metadata.generation !== approved.controllerGeneration || metadata.deletionTimestamp ||
      controller.spec?.replicas !== 1 || status?.observedGeneration !== approved.controllerGeneration ||
      !["replicas", "readyReplicas", "availableReplicas", "updatedReplicas"].every(key => status[key] === 1) ||
      (status.unavailableReplicas || 0) !== 0 ||
      controller.spec.template?.spec?.containers?.length !== 1 ||
      controller.spec.template.spec.containers[0].image !== approved.controllerImage) {
    throw new Error("The exact approved controller identity, generation, image or ready rollout differs");
  }
}

function verifyTarget(cluster, config, root, namespace, controller, approved = APPROVED) {
  verifyClusterFile(cluster, approved);
  const selected = config?.contexts;
  const clusters = config?.clusters;
  if (selected?.length !== 1 || clusters?.length !== 1 || selected[0].name !== approved.context ||
      selected[0].context?.cluster !== clusters[0].name || clusters[0].cluster?.server !== approved.server ||
      clusters[0].cluster["insecure-skip-tls-verify"] === true ||
      root?.kind !== "Namespace" || root.metadata?.name !== "kube-system" ||
      root.metadata.uid !== approved.clusterUID || root.metadata.deletionTimestamp ||
      namespace?.kind !== "Namespace" || namespace.metadata?.name !== approved.namespace ||
      namespace.metadata.uid !== approved.namespaceUID || namespace.metadata.deletionTimestamp ||
      namespace.status?.phase !== "Active") {
    throw new Error("The approved context, API endpoint, cluster UID or namespace UID differs");
  }
  verifyController(controller, approved);
}

function verifyModel(model, profile) {
  if (model?.apiVersion !== API || model.kind !== "Model" ||
      model.metadata?.name !== APPROVED.model || model.metadata.namespace !== APPROVED.namespace ||
      !model.metadata.uid || model.metadata.deletionTimestamp ||
      model.spec?.invocationProfileRef?.name !== APPROVED.profile ||
      profile?.metadata?.name !== APPROVED.profile) {
    throw new Error("The prepared Model and exact tutorial-chat invocation profile are required");
  }
  verifyPilotModel(model, "kubernetes", profile, { defaultTokens: 512, limitTokens: 1024 });
}

function instructionsOnly(before, after) {
  const withoutInstructions = spec => {
    const copy = structuredClone(spec);
    delete copy.instructions;
    return copy;
  };
  return isDeepStrictEqual(withoutInstructions(before), withoutInstructions(after));
}

function comparableAgentSpec(spec) {
  const result = structuredClone(spec);
  if (isDeepStrictEqual(result?.probes, {})) delete result.probes;
  if (isDeepStrictEqual(result?.restartLadder, {}) ||
      isDeepStrictEqual(result?.restartLadder, { steps: null }) ||
      isDeepStrictEqual(result?.restartLadder, { steps: [] })) delete result.restartLadder;
  return result;
}

function verifyAgent(agent, options, version, identity, ready = false) {
  const metadata = agent?.metadata;
  if (![1, 2].includes(version) || agent?.apiVersion !== API || agent.kind !== "Agent" ||
      metadata?.name !== options.agentName || metadata.namespace !== APPROVED.namespace ||
      !metadata.uid || !Number.isSafeInteger(metadata.generation) || metadata.generation < 1 ||
      metadata.deletionTimestamp || metadata.labels?.["kognition.dev/recording-run"] !== options.runId ||
      (identity && (metadata.uid !== identity.uid || metadata.generation !== identity.generation)) ||
      agent.spec?.modelRef?.name !== APPROVED.model ||
      (agent.spec.modelRef.namespace && agent.spec.modelRef.namespace !== APPROVED.namespace) ||
      !isDeepStrictEqual(agent.spec.instructions, { inline: INSTRUCTIONS[version - 1] }) ||
      agent.spec.replicas !== BOUNDS.replicas || agent.spec.runtime?.class !== BOUNDS.class ||
      agent.spec.runtime.timeout !== BOUNDS.timeout || agent.spec.runtime.maxTokens !== BOUNDS.maxTokens ||
      (ready && (agent.status?.phase !== "Ready" || !Number.isSafeInteger(agent.status.revisionCount) ||
        agent.status.revisionCount < 1))) {
    throw new Error("Authored Agent identity, instructions, generation, runtime bounds or readiness differs");
  }
}

function verifyGrant(grant, options, expectedUID) {
  if (grant?.apiVersion !== API || grant.kind !== "ModelGrant" ||
      grant.metadata?.name !== options.grantName || grant.metadata.namespace !== APPROVED.namespace ||
      !grant.metadata.uid || grant.metadata.deletionTimestamp ||
      (expectedUID && grant.metadata.uid !== expectedUID) ||
      grant.metadata.labels?.["kognition.dev/recording-run"] !== options.runId ||
      !isDeepStrictEqual(grant.spec, {
        agentRef: { name: options.agentName }, modelRef: { name: APPROVED.model },
      })) {
    throw new Error("Persisted ModelGrant identity or exact Agent/Model authorization differs");
  }
}

function runtimeContainer(deployment) {
  return deployment?.spec?.template?.spec?.containers?.find(container => container.name === "agent");
}

function runtimeValues(deployment) {
  return Object.fromEntries((runtimeContainer(deployment)?.env || []).map(entry => [entry.name, entry.value]));
}

function verifyDeploymentIdentity(deployment, options, uid) {
  if (deployment?.kind !== "Deployment" || deployment.metadata?.name !== `agent-${options.agentName}` ||
      deployment.metadata.namespace !== APPROVED.namespace || !deployment.metadata.uid ||
      (uid && deployment.metadata.uid !== uid) || deployment.metadata.deletionTimestamp ||
      !Number.isSafeInteger(deployment.metadata.generation) || deployment.metadata.generation < 1 ||
      deployment.metadata.annotations?.["kognition.dev/owner-agent"] !== `${APPROVED.namespace}/${options.agentName}` ||
      deployment.spec?.replicas !== BOUNDS.replicas || runtimeContainer(deployment)?.image !== APPROVED.runtimeImage) {
    throw new Error("Physical runtime Deployment identity, ownership, image or replica bound differs");
  }
}

function verifyRuntime(agent, deployment, options, expected, version, previous) {
  verifyAgent(agent, options, version, expected, true);
  verifyDeploymentIdentity(deployment, options, expected.deploymentUID);
  verifyKubernetesRuntime(agent, {
    name: options.agentName, namespace: APPROVED.namespace, uid: expected.uid,
    generation: expected.generation, deploymentUID: expected.deploymentUID,
    replicas: BOUNDS.replicas, maxTokens: BOUNDS.maxTokens,
  }, deployment);
  const env = runtimeValues(deployment);
  if (!expected.deploymentUID || deployment.metadata.generation !== expected.deploymentGeneration ||
      deployment.status.replicas !== 1 || (deployment.status.unavailableReplicas || 0) !== 0 ||
      env.KOGNITION_SYSTEM_PROMPT !== INSTRUCTIONS[version - 1] ||
      env.KOGNITION_AGENT_NAME !== options.agentName || env.KOGNITION_NAMESPACE !== APPROVED.namespace ||
      env.KOGNITION_MAX_TOKENS !== String(BOUNDS.maxTokens) || env.KOGNITION_TIMEOUT_SECONDS !== BOUNDS.timeout ||
      (previous && (previous.metadata.uid !== deployment.metadata.uid ||
        deployment.metadata.generation <= previous.metadata.generation))) {
    throw new Error("Runtime must serve the exact authored instructions and bounds on the observed new rollout");
  }
}

function verifyChat(body, version, submitted, previous) {
  const usage = body?.usage;
  if (![1, 2].includes(version) || body?.error || body?.errorCode || body?.error_code ||
      body?.response?.trim() !== ANSWERS[version - 1] ||
      typeof body.sessionID !== "string" || !body.sessionID.trim() ||
      typeof body.conversationID !== "string" || !body.conversationID.trim() ||
      submitted?.message !== INPUT || submitted.conversationID !== body.conversationID || submitted.sessionID ||
      !["promptTokens", "completionTokens", "totalTokens"].every(key => Number.isSafeInteger(usage?.[key]) && usage[key] > 0) ||
      usage.completionTokens > BOUNDS.maxTokens || usage.totalTokens !== usage.promptTokens + usage.completionTokens ||
      (body.toolCallsDetail && (!Array.isArray(body.toolCallsDetail) || body.toolCallsDetail.length)) ||
      (previous && (previous.sessionID === body.sessionID || previous.conversationID === body.conversationID))) {
    throw new Error("Live answer lacks the exact contract, fresh correlated conversation/session or actual bounded token usage");
  }
}

function verifyInvocation(instance, agent, submitted, body) {
  if (instance?.kind !== "AgentInstance" || !instance.metadata?.uid ||
      instance.metadata.namespace !== APPROVED.namespace || instance.metadata.deletionTimestamp ||
      instance.metadata.labels?.["kognition.dev/agent"] !== agent.metadata.name ||
      instance.metadata.labels?.["kognition.dev/created-by"] !== "studio" ||
      instance.spec?.agentRef?.name !== agent.metadata.name || instance.spec.revision !== agent.status.revisionCount ||
      instance.spec.sessionContext?.input !== submitted.message ||
      instance.spec.sessionContext.conversation_id !== submitted.conversationID ||
      instance.status?.phase !== "Ready" || instance.status.sessionID !== body.sessionID ||
      instance.status.output?.trim() !== body.response.trim() ||
      !isDeepStrictEqual(instance.status.tokenUsage, body.usage)) {
    throw new Error("Independent AgentInstance does not correlate the submitted input, revision, conversation, output and usage");
  }
}

function revisionParentKey(name) {
  return crypto.createHash("sha256").update(`Agent\0${name}`).digest("hex").slice(0, 24);
}

function verifyRevisionEvidence(evidence, first, second, configMaps) {
  if (!first?.metadata?.uid || first.metadata.uid !== second?.metadata?.uid ||
      first.metadata.generation + 1 !== second.metadata.generation ||
      first.status?.revisionCount + 1 !== second.status?.revisionCount ||
      !instructionsOnly(first.spec, second.spec)) {
    throw new Error("Revision must be an instructions-only change on the same Agent UID and next generation/revision");
  }
  verifyRevisions(evidence, first, second);
  for (const [envelope, agent] of [[evidence.before, first], [evidence.after, second]]) {
    const snapshotSpec = { ...envelope.spec, spec: comparableAgentSpec(envelope.spec?.spec) };
    if (envelope.resource !== "Agent" || !isDeepStrictEqual(snapshotSpec, { spec: comparableAgentSpec(agent.spec) })) {
      throw new Error("Revision envelope does not contain the exact independently persisted Agent specification");
    }
    const matches = configMaps?.items?.filter(item =>
      item.metadata?.annotations?.["kognition.dev/revision"] === String(envelope.metadata.revision));
    const snapshot = matches?.[0];
    if (matches?.length !== 1 || snapshot.kind !== "ConfigMap" || !snapshot.metadata.uid ||
        snapshot.metadata.namespace !== first.metadata.namespace || snapshot.metadata.deletionTimestamp ||
        snapshot.immutable !== true ||
        snapshot.metadata.labels?.["kognition.dev/revision-parent"] !== revisionParentKey(first.metadata.name) ||
        snapshot.metadata.labels?.["kognition.dev/revision-kind"] !== "agent" ||
        !snapshot.metadata.ownerReferences?.some(owner => owner.apiVersion === API &&
          owner.kind === "Agent" && owner.name === first.metadata.name && owner.uid === first.metadata.uid) ||
        typeof snapshot.binaryData?.spec !== "string" ||
        !isDeepStrictEqual(JSON.parse(Buffer.from(snapshot.binaryData.spec, "base64").toString("utf8")), envelope.spec)) {
      throw new Error("Immutable revision ConfigMap lacks the exact parent UID, revision or specification snapshot");
    }
  }
}

function createWriteGuard(options) {
  let next = 0, armed;
  const submissions = [];
  return {
    submissions,
    arm(step, expectedAgent) {
      if (armed || STEPS[next] !== step) throw new Error("Authoring writes cannot be repeated or reordered");
      armed = { step, expectedAgent };
    },
    consume(method, url, body) {
      if (!armed || !body || typeof body !== "object" || Array.isArray(body)) {
        throw new Error("Unarmed or non-JSON authoring mutation refused");
      }
      const { step, expectedAgent } = armed;
      const version = step === "agent-revise" || step === "chat-v2" ? 2 : 1;
      const target = new URL(url);
      if (target.searchParams.get("namespace") !== APPROVED.namespace ||
          [...target.searchParams.keys()].some(key => key !== "namespace") ||
          target.searchParams.getAll("namespace").length !== 1) throw new Error("Write namespace differs");
      const chat = step.startsWith("chat-");
      const grant = step === "grant-create";
      const expectedPath = chat ? `/api/invoke/agent/${options.agentName}` :
        `/api/resources/${grant ? "modelgrants" : "agents"}${step === "agent-revise" ? `/${options.agentName}` : ""}`;
      if (target.pathname !== expectedPath || method !== (step === "agent-revise" ? "PUT" : "POST")) {
        throw new Error("Write method or resource exceeds the bounded authoring batch");
      }
      if (chat) {
        if (body.message !== INPUT || body.sessionID || typeof body.conversationID !== "string" ||
            !body.conversationID.trim() || Object.keys(body).some(key => !["message", "sessionID", "conversationID"].includes(key))) {
          throw new Error("Only one exact input per fresh conversation is authorized");
        }
        const prior = submissions.find(item => item.step === "chat-v1");
        if (prior?.body.conversationID === body.conversationID) throw new Error("Clear must create a fresh conversation");
      } else {
        const expectedSpec = grant ? { agentRef: { name: options.agentName }, modelRef: { name: APPROVED.model } } :
          { modelRef: { name: APPROVED.model }, instructions: { inline: INSTRUCTIONS[version - 1] },
            runtime: { class: BOUNDS.class, timeout: BOUNDS.timeout, maxTokens: BOUNDS.maxTokens }, replicas: BOUNDS.replicas };
        if (body.apiVersion !== API || body.kind !== (grant ? "ModelGrant" : "Agent") ||
            body.metadata?.name !== (grant ? options.grantName : options.agentName) ||
            body.metadata.namespace !== APPROVED.namespace ||
            !isDeepStrictEqual(body.metadata.labels, { "kognition.dev/recording-run": options.runId }) ||
            body.metadata.deletionTimestamp || body.metadata.ownerReferences?.length ||
            Object.keys(body.metadata.annotations || {}).length ||
            !isDeepStrictEqual(comparableAgentSpec(body.spec), comparableAgentSpec(step === "agent-revise"
              ? { ...expectedAgent?.spec, instructions: expectedSpec.instructions } : expectedSpec))) {
          throw new Error("Submitted resource differs from the exact authored identity, instructions or bounds");
        }
        if (step === "agent-revise") {
          if (!expectedAgent?.metadata?.uid || body.metadata.uid !== expectedAgent.metadata.uid ||
              body.metadata.generation !== expectedAgent.metadata.generation || !body.metadata.resourceVersion) {
            throw new Error("An instructions revision requires the original UID/generation and optimistic resourceVersion");
          }
        } else if (body.metadata.uid || body.metadata.resourceVersion || body.metadata.generation || body.status) {
          throw new Error("Create must never adopt or overwrite an existing resource");
        }
      }
      const submission = { step, method, path: target.pathname, body: structuredClone(body), at: new Date().toISOString() };
      submissions.push(submission);
      next++;
      armed = undefined;
      return submission;
    },
    verifyComplete() {
      if (armed || next !== STEPS.length) throw new Error("The bounded authoring sequence is incomplete");
    },
  };
}

async function verifyCompletedAnswer(page, previousCount, body, timeout = 30000) {
  const messages = page.locator("#preview-host .chat-message.assistant .chat-bubble:not(.typing)");
  await expect(messages).toHaveCount(previousCount + 1, { timeout });
  const bubble = messages.last();
  await expect(bubble).toBeVisible({ timeout });
  await expect(bubble).not.toHaveClass(/\berror\b/, { timeout });
  await expect(bubble).toHaveText(body.response.trim(), { timeout });
  await expect(page.locator("#preview-host .chat-bubble.typing")).toHaveCount(0, { timeout });
  await expect(page.locator("#preview-host .chat-message.assistant").last().locator(".chat-meta"))
    .toHaveText(`${body.usage.totalTokens} tokens`, { timeout });
  return bubble;
}

function runtimeEvidence(deployment) {
  const env = runtimeValues(deployment);
  return {
    kind: deployment.kind, metadata: { name: deployment.metadata.name, namespace: deployment.metadata.namespace,
      uid: deployment.metadata.uid, generation: deployment.metadata.generation },
    replicas: deployment.spec.replicas, status: deployment.status, image: runtimeContainer(deployment).image,
    instructions: env.KOGNITION_SYSTEM_PROMPT, maxTokens: env.KOGNITION_MAX_TOKENS,
    timeout: env.KOGNITION_TIMEOUT_SECONDS,
  };
}

async function finalizeRecording(context, browser, video, destination) {
  const errors = [];
  let bytes = 0;
  await context.close().catch(error => errors.push(`Context close: ${error.message}`));
  try {
    await video.saveAs(destination);
    bytes = fs.statSync(destination).size;
    if (!bytes) throw new Error("Recorded video is empty");
  } catch (error) {
    errors.push(`Video persistence: ${error.message}`);
  }
  await browser.close().catch(error => errors.push(`Browser close: ${error.message}`));
  if (errors.length) throw new Error(errors.join("; "));
  return { path: destination, bytes };
}

async function runAuthoring(input) {
  const options = parseOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  const APPROVED = recordingTarget(options.target);
  fs.mkdirSync(path.dirname(options.output), { recursive: true });
  fs.mkdirSync(options.output);
  const reportPath = path.join(options.output, "report.json");
  const report = {
    schemaVersion: 1, batch: "first-authoring", runId: options.runId, startedAt: new Date().toISOString(),
    passed: false, mockedAPI: false, fullTutorialRecorded: false, viewport: VIEWPORT,
    approvedTarget: APPROVED, recorderSHA256: crypto.createHash("sha256").update(fs.readFileSync(__filename)).digest("hex"),
    bounds: BOUNDS, readingPolicy: READING_POLICY, readingHolds: [],
    resources: { agentName: options.agentName, grantName: options.grantName },
    coverage: { entireScenesCompleted: [], coveredSubActions: [], pendingObligations: PENDING,
      conversationHistoryProven: false, deterministicResponseContractOnly: true },
    scenes: [], checkpoints: [], calls: [], submissions: [], apiResponses: [], browserErrors: [], uiPreferenceChanges: [],
    safety: { noChatRetries: true, noResourceDeletion: true, frozenCLIOnly: true },
  };
  const persist = () => fs.writeFileSync(reportPath, JSON.stringify(report, null, 2));
  persist();
  const children = [], captures = new Set();
  let browser, context, page, video, epoch, failure, get, targetVerified = false;
  let first, second, firstDeployment;
  const seconds = () => epoch === undefined ? null : (performance.now() - epoch) / 1000;
  const checkpoint = (name, evidence) => {
    report.checkpoints.push({ name, at: new Date().toISOString(), seconds: seconds(), evidence });
    persist();
  };
  const cover = (scene, action) => report.coverage.coveredSubActions.push({ scene, action, at: new Date().toISOString() });
  const scene = async (id, description, action) => {
    const entry = { id, description, startedAt: new Date().toISOString(), startSeconds: seconds(), passed: false };
    report.scenes.push(entry);
    persist();
    console.log(`${id}: ${description}`);
    try { await action(); entry.passed = true; }
    finally { entry.endedAt = new Date().toISOString(); entry.endSeconds = seconds(); persist(); }
  };
  const checkAPIs = () => {
    if (report.browserErrors.length || report.apiResponses.some(item => item.status >= 400 || item.failure)) {
      throw new Error("Recorded UI reported a browser error or failed API response; no next action is permitted");
    }
  };
  try {
    const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
    if (!fs.statSync(cli).isFile()) throw new Error("The frozen runtime CLI is unavailable");
    report.cli = { path: cli, sha256: crypto.createHash("sha256").update(fs.readFileSync(cli)).digest("hex") };
    const cluster = JSON.parse(fs.readFileSync(path.join(options.runtime, "cluster.json"), "utf8"));
    verifyClusterFile(cluster, APPROVED);
    verifyRecordingCLI(report.cli.sha256, APPROVED);
    const kubeconfig = path.resolve(options.runtime, cluster.kubeconfig);
    if (!fs.statSync(kubeconfig).isFile()) throw new Error("The runtime kubeconfig is unavailable");
    const environment = { ...process.env, KUBECONFIG: kubeconfig };
    const common = ["--kubeconfig", kubeconfig, "--context", APPROVED.context, "--request-timeout=20s"];
    const run = async (executable, args) => (await execute(executable, args, {
      env: environment, encoding: "utf8", windowsHide: true, timeout: 30000, maxBuffer: 16 * 1024 * 1024,
    })).stdout;
    const kubectl = async args => run("kubectl", [...common, ...args]);
    get = async (kind, name, namespace = APPROVED.namespace, missing = false) => {
      const text = await kubectl(["get", kind, name, ...(namespace ? ["-n", namespace] : []),
        ...(missing ? ["--ignore-not-found=true"] : []), "-o", "json"]);
      return text.trim() ? JSON.parse(text) : null;
    };
    const list = async (kind, selector) => JSON.parse(await kubectl([
      "get", kind, "-n", APPROVED.namespace, ...(selector ? ["-l", selector] : []), "-o", "json",
    ]));
    const cliJSON = async args => JSON.parse(await run(cli, [...args, "-o", "json", "--mode", "kubernetes",
      "--kube-context", APPROVED.context, "--namespace", APPROVED.namespace]));
    const config = JSON.parse(await kubectl(["config", "view", "--minify", "-o", "json"]));
    const root = await get("namespace", "kube-system", "");
    const namespace = await get("namespace", APPROVED.namespace, "");
    const controller = await get("deployment", APPROVED.controller, APPROVED.systemNamespace);
    verifyTarget(cluster, config, root, namespace, controller, APPROVED);
    targetVerified = true;
    checkpoint("exact-approved-target", { context: APPROVED.context, server: APPROVED.server,
      clusterUID: root.metadata.uid, namespaceUID: namespace.metadata.uid,
      controllerUID: controller.metadata.uid, generation: controller.metadata.generation, status: controller.status,
      controllerImage: APPROVED.controllerImage, kubeconfig });
    const revisionSelector = `kognition.dev/revision-parent=${revisionParentKey(options.agentName)},kognition.dev/revision-kind=agent`;
    for (const [kind, name] of [["agent", options.agentName], ["modelgrant", options.grantName],
      ["deployment", `agent-${options.agentName}`]]) {
      if (await get(kind, name, APPROVED.namespace, true)) throw new Error(`Refusing existing ${kind}/${name}; choose a fresh run ID`);
    }
    if ((await list("configmaps", revisionSelector)).items.length ||
        (await list("agentinstances", `kognition.dev/agent=${options.agentName}`)).items.length) {
      throw new Error("Prior immutable revisions or invocations use this Agent name; choose a fresh run ID");
    }
    const model = await get("model", APPROVED.model);
    const profile = await get("modelinvocationprofile", APPROVED.profile);
    verifyModel(model, profile);
    const profilePath = path.join(options.output, "invocation-profile.json");
    fs.writeFileSync(profilePath, JSON.stringify(profile, null, 2), { flag: "wx" });
    const digest = (await run(cli, ["invocation-profile-digest", "-f", profilePath])).trim();
    if (digest !== model.spec.invocationProfileRef.specDigest) throw new Error("The actual invocation profile digest differs from the Model pin");
    checkpoint("prepared-model-and-profile", { model, profile, digest });
    const profileDirectory = path.join(options.output, "studio-profile");
    fs.mkdirSync(profileDirectory);
    const uiOptions = { cli, mode: "kubernetes", namespace: APPROVED.namespace, "kube-context": APPROVED.context,
      systemNamespace: APPROVED.systemNamespace, environment: { KUBECONFIG: kubeconfig, APPDATA: profileDirectory },
      outputDir: options.output };
    report.uiPreferenceProfile = profileDirectory;
    const studio = await startUI("studio", uiOptions, undefined, children);
    const dashboard = await startUI("dashboard", uiOptions, undefined, children);
    report.ui = { studio, dashboard, childPIDs: children.map(child => child.pid) };
    for (const [kind, origin] of [["studio", studio], ["dashboard", dashboard]]) {
      const response = await fetch(`${origin}/api/meta`, { redirect: "error", signal: AbortSignal.timeout(30000) });
      const text = await response.text();
      report.apiResponses.push({ source: kind, method: "GET", path: "/api/meta", status: response.status, text });
      const metadata = JSON.parse(text);
      if (!response.ok || metadata.mode !== "kubernetes" || metadata.namespace !== APPROVED.namespace) {
        throw new Error(`${kind} is not responsive on the exact Kubernetes workload scope`);
      }
    }
    const guard = createWriteGuard(options);
    report.submissions = guard.submissions;
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport: VIEWPORT, colorScheme: "light", serviceWorkers: "block",
      recordVideo: { dir: options.output, size: VIEWPORT,
        showActions: { duration: 1000, fontSize: 18, cursor: "pointer" } } });
    await context.route("**/api/**", async route => {
      const request = route.request();
      try {
        if (![studio, dashboard].includes(new URL(request.url()).origin)) throw new Error("Non-local UI API target refused");
        if (!["GET", "HEAD"].includes(request.method())) {
          if (new URL(request.url()).origin === studio &&
              isWelcomeDismissal(request.method(), request.url(), request.postDataJSON(), report.uiPreferenceChanges.length)) {
            report.uiPreferenceChanges.push({ action: "dismiss-welcome", profileDirectory, workloadMutation: false });
          } else {
            guard.consume(request.method(), request.url(), request.postDataJSON());
          }
          persist();
        }
        await route.continue();
      } catch (error) {
        report.browserErrors.push(error.message);
        persist();
        await route.abort("blockedbyclient");
      }
    });
    epoch = performance.now();
    report.videoStartedAt = new Date().toISOString();
    page = await context.newPage();
    video = page.video();
    report.recording = { path: await video.path(), size: VIEWPORT, cursorActions: true, narration: false };
    page.setDefaultTimeout(30000);
    page.on("pageerror", error => report.browserErrors.push(error.message));
    page.on("response", response => {
      const url = new URL(response.url());
      if (!url.pathname.startsWith("/api/")) return;
      const entry = { method: response.request().method(), path: url.pathname, query: url.search,
        status: response.status(), seconds: seconds(), at: new Date().toISOString() };
      report.apiResponses.push(entry);
      const capture = response.text().then(text => { entry.text = text; },
        error => { entry.failure = error.message; }).finally(() => { captures.delete(capture); persist(); });
      captures.add(capture);
    });
    page.on("requestfailed", request => {
      if (new URL(request.url()).pathname.startsWith("/api/")) {
        report.browserErrors.push(`API transport failed: ${new URL(request.url()).pathname}: ${request.failure()?.errorText}`);
      }
    });
    page.on("dialog", async dialog => {
      // Only a deploy confirmation for this exact resource is authorized, never an unsolicited discard.
      if (dialog.type() === "confirm" && /^Apply (Agent|ModelGrant) /.test(dialog.message()) &&
          [options.agentName, options.grantName].some(name => dialog.message().includes(` ${name}?`))) await dialog.accept();
      else { report.browserErrors.push(`Unexpected ${dialog.type()} dialog`); await dialog.dismiss(); }
    });
    const click = async locator => { await locator.scrollIntoViewIfNeeded(); await locator.click(); };
    const type = async (locator, text) => { await click(locator); await locator.fill(""); await locator.pressSequentially(text, { delay: 18 }); };
    const label = name => page.locator("#builder-host").getByLabel(fieldLabel(name));
    const section = name => page.locator("#builder-host details").filter({
      has: page.locator("summary").filter({ hasText: new RegExp(`^${name}(advanced)?$`) }),
    }).first();
    const open = async name => {
      const details = section(name);
      if (await details.getAttribute("open") === null) await click(details.locator("summary"));
    };
    const addRunLabel = async () => {
      const labels = section("Identity").locator(".field").filter({ has: page.locator("label").filter({ hasText: fieldLabel("Labels") }) });
      await click(labels.getByRole("button", { name: "+ add entry", exact: true }));
      await type(labels.getByPlaceholder("key", { exact: true }).last(), "kognition.dev/recording-run");
      await type(labels.getByPlaceholder("value", { exact: true }).last(), options.runId);
    };
    const create = async selector => {
      checkAPIs();
      await click(page.locator("#btn-create"));
      await click(page.locator(selector));
      await click(page.locator('[data-view="builder"]'));
      await expect(page.locator("#view-builder")).toBeVisible();
    };
    const deploy = async (step, plural, expectedAgent) => {
      guard.arm(step, expectedAgent);
      const pending = page.waitForResponse(response => response.request().method() === (expectedAgent ? "PUT" : "POST") &&
        new URL(response.url()).pathname === `/api/resources/${plural}${expectedAgent ? `/${options.agentName}` : ""}`,
      { timeout: 45000 });
      await click(page.locator("#btn-deploy"));
      const response = await pending;
      const text = await response.text();
      report.lastDeployResponse = { step, httpStatus: response.status(), text };
      persist();
      if (!response.ok()) throw new Error(`Live authoring ${step} failed (HTTP ${response.status()}); it will not be retried`);
      const body = JSON.parse(text);
      await expect(page.locator("#status")).toHaveText(`deployed ${body.kind} ${body.metadata.name}`);
      checkAPIs();
      return body;
    };
    const waitRuntime = async (created, version, previous) => {
      const identity = { uid: created.metadata.uid, generation: created.metadata.generation };
      const deadline = performance.now() + 180000;
      let deploymentUID = previous?.metadata.uid;
      while (performance.now() < deadline) {
        const agent = await get("agent", options.agentName);
        verifyAgent(agent, options, version, identity);
        const deployment = await get("deployment", `agent-${options.agentName}`, APPROVED.namespace, true);
        if (deployment) {
          verifyDeploymentIdentity(deployment, options, deploymentUID);
          deploymentUID ||= deployment.metadata.uid;
          report.resources.deploymentUID = deploymentUID;
          report.lastRuntimeObservation = { at: new Date().toISOString(), agent, deployment: runtimeEvidence(deployment) };
          persist();
          const current = runtimeValues(deployment).KOGNITION_SYSTEM_PROMPT === INSTRUCTIONS[version - 1];
          const expectedRevision = version === 1 ? 1 : first.status.revisionCount + 1;
          if (agent.status?.phase === "Failed" || agent.status?.revisionCount > expectedRevision) {
            throw new Error("Agent reconciliation failed or an unexpected revision appeared");
          }
          if (current && agent.status?.phase === "Ready" && agent.status.revisionCount === expectedRevision &&
              agent.status.readyReplicas === 1 && deployment.status?.observedGeneration === deployment.metadata.generation &&
              ["replicas", "readyReplicas", "availableReplicas", "updatedReplicas"].every(key => deployment.status[key] === 1) &&
              (!previous || deployment.metadata.generation > previous.metadata.generation)) {
            verifyRuntime(agent, deployment, options, { ...identity, deploymentUID,
              deploymentGeneration: deployment.metadata.generation }, version, previous);
            verifyController(await get("deployment", APPROVED.controller, APPROVED.systemNamespace), APPROVED);
            checkpoint(`physical-runtime-v${version}`, { agent, deployment: runtimeEvidence(deployment) });
            return { agent, deployment };
          }
        }
        await new Promise(resolve => setTimeout(resolve, 1000));
      }
      throw new Error(`Physical runtime V${version} did not become ready within 180 seconds; no chat was dispatched`);
    };
    await scene("A01", "Inspect real Studio scope and the prepared model YAML.", async () => {
      await page.goto(studio, { waitUntil: "domcontentloaded" });
      await expect(page.locator("#status")).toHaveText("ready", { timeout: 60000 });
      await dismissWelcome(page);
      await expect(page.locator("#meta-mode")).toContainText("kubernetes");
      await expect(page.locator("#namespace-select")).toHaveValue(APPROVED.namespace);
      const item = page.locator("#list-models").getByText(APPROVED.model, { exact: true });
      const group = item.locator("xpath=ancestor::details[1]");
      if (await group.getAttribute("open") === null) await click(group.locator("summary"));
      await click(item);
      await click(page.locator('[data-view="yaml"]'));
      await expect(page.locator("#yaml-area")).toHaveValue(new RegExp(APPROVED.profile));
      await page.waitForTimeout(3000);
      cover("S02", "Frozen CLI, exact target/controller/profile preflight and responsive loopback Studio/Dashboard.");
      cover("S03", "Studio Explorer, namespace and real Model resource YAML.");
    });
    await scene("A02", "Visually author a one-replica bounded Agent, inspect YAML, and save its pending definition.", async () => {
      await create("#btn-new-agent");
      await type(label("Name"), options.agentName);
      await expect(label("Namespace")).toHaveValue(APPROVED.namespace);
      await addRunLabel();
      await label("Model").selectOption(APPROVED.model);
      await type(label("Inline instructions"), INSTRUCTIONS[0]);
      await open("Runtime");
      await type(label("Class"), BOUNDS.class);
      await type(label("Timeout"), BOUNDS.timeout);
      await label("MaxTokens").fill(String(BOUNDS.maxTokens));
      await open("Scaling");
      await label("Replicas").fill(String(BOUNDS.replicas));
      await click(page.locator('[data-view="yaml"]'));
      const yaml = await page.locator("#yaml-area").inputValue();
      const parsed = await page.request.post(`${studio}/api/parse-yaml`, {
        headers: { "Content-Type": "application/yaml" }, data: yaml,
      });
      if (!parsed.ok()) throw new Error(`Visible draft YAML failed parsing (HTTP ${parsed.status()})`);
      const draft = await parsed.json();
      const draftValidation = createWriteGuard(options);
      draftValidation.arm("agent-create");
      draftValidation.consume("POST", `${studio}/api/resources/agents?namespace=${APPROVED.namespace}`, draft);
      checkpoint("visible-agent-draft", { yaml, parsed: draft });
      await page.waitForTimeout(4000);
      const created = await deploy("agent-create", "agents");
      verifyAgent(created, options, 1);
      if (created.metadata.generation !== 1) throw new Error("A newly created Agent must start at generation one");
      report.resources.agentUID = created.metadata.uid;
      const persisted = await get("agent", options.agentName);
      verifyAgent(persisted, options, 1, created.metadata);
      checkpoint("agent-created-before-grant", { submitted: guard.submissions.at(-1), created, persisted });
      first = created;
      cover("S04", "Visual Agent identity/labels/model/inline instructions/runtime controls and YAML inspection.");
      cover("S05", "Definition deployment is recorded separately from post-grant physical readiness.");
    });
    await scene("A03", "Visually author the exact ModelGrant, then wait for the physical runtime.", async () => {
      await create("#btn-new-modelgrant");
      await type(label("Name"), options.grantName);
      await addRunLabel();
      await label("Agent").selectOption(options.agentName);
      await label("Model").selectOption(APPROVED.model);
      const grant = await deploy("grant-create", "modelgrants");
      verifyGrant(grant, options);
      report.resources.grantUID = grant.metadata.uid;
      const persisted = await get("modelgrant", options.grantName);
      verifyGrant(persisted, options, grant.metadata.uid);
      checkpoint("modelgrant-created", { submitted: guard.submissions.at(-1), grant, persisted });
      const ready = await waitRuntime(first, 1);
      first = ready.agent;
      firstDeployment = ready.deployment;
      cover("S04", "Actual visual ModelGrant creation and independent authorization readback.");
      cover("S05", "Exact Agent/Deployment identities, generations and one ready/available/updated physical replica.");
    });
    const chat = async (version, agent, previous) => {
      checkAPIs();
      const previousCount = await page.locator("#preview-host .chat-message.assistant .chat-bubble:not(.typing)").count();
      await type(page.locator("#preview-host .chat-input-row textarea"), INPUT);
      guard.arm(`chat-v${version}`);
      const pending = page.waitForResponse(response => response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/invoke/agent/${options.agentName}`, { timeout: 180000 });
      const call = { version, agentUID: agent.metadata.uid, generation: agent.metadata.generation,
        revision: agent.status.revisionCount, startedAt: new Date().toISOString(), startSeconds: seconds(),
        outcome: "ambiguous-until-verified" };
      report.calls.push(call);
      persist();
      await click(page.getByRole("button", { name: "Send", exact: true }));
      const response = await pending;
      call.httpStatus = response.status();
      call.responseText = await response.text();
      call.responseReceivedAt = new Date().toISOString();
      call.responseReceivedSeconds = seconds();
      persist();
      if (!response.ok()) throw new Error(`Live chat V${version} failed (HTTP ${response.status()}); no retry is permitted`);
      const body = JSON.parse(call.responseText);
      call.body = body;
      const submitted = guard.submissions.at(-1).body;
      verifyChat(body, version, submitted, previous);
      const bubble = await verifyCompletedAnswer(page, previousCount, body);
      call.renderedText = await bubble.innerText();
      const hold = { target: "completed-agent-answer", version, sessionID: body.sessionID,
        conversationID: body.conversationID, startedAt: new Date().toISOString(), startSeconds: seconds(), policy: READING_POLICY };
      report.readingHolds.push(hold);
      persist();
      Object.assign(hold, await holdReadableText(page, bubble), { endedAt: new Date().toISOString(), endSeconds: seconds() });
      checkpoint(`readable-completed-answer-v${version}`, { sessionID: body.sessionID, conversationID: body.conversationID, hold });
      const instances = await list("agentinstances", `kognition.dev/agent=${options.agentName}`);
      const matches = instances.items.filter(item => item.status?.sessionID === body.sessionID);
      if (matches.length !== 1) throw new Error("The actual session does not identify exactly one persisted AgentInstance");
      verifyInvocation(matches[0], agent, submitted, body);
      call.instance = matches[0];
      call.outcome = "verified";
      checkpoint(`independent-invocation-v${version}`, { agentUID: agent.metadata.uid, instance: matches[0] });
      await page.screenshot({ path: path.join(options.output, `answer-v${version}.png`) });
      checkAPIs();
      return body;
    };
    let responseV1;
    await scene("A04", "Invoke the real model and hold the completed V1 answer and reported usage for reading.", async () => {
      await click(page.locator("#list-agents").getByText(options.agentName, { exact: true }));
      await click(page.locator('[data-view="preview"]'));
      responseV1 = await chat(1, first);
      cover("S05", "Real authenticated model invocation, visible completed answer/actual tokens and correlated AgentInstance.");
      await click(page.getByRole("button", { name: "Clear", exact: true }));
      await expect(page.locator("#preview-host .chat-message")).toHaveCount(0);
      checkpoint("conversation-cleared", { previousSessionID: responseV1.sessionID, previousConversationID: responseV1.conversationID });
    });
    await scene("A05", "Change only inline instructions and wait for the same runtime's newly observed rollout.", async () => {
      checkAPIs();
      await click(page.locator('[data-view="builder"]'));
      await type(label("Inline instructions"), INSTRUCTIONS[1]);
      const revised = await deploy("agent-revise", "agents", first);
      verifyAgent(revised, options, 2, { uid: first.metadata.uid, generation: first.metadata.generation + 1 });
      if (!instructionsOnly(first.spec, revised.spec)) throw new Error("The UI changed more than instructions");
      checkpoint("instructions-only-ui-revision", { submitted: guard.submissions.at(-1), revised });
      second = (await waitRuntime(revised, 2, firstDeployment)).agent;
      cover("S07", "Instructions-only visual edit; unchanged Agent/Deployment UIDs and physically observed new rollout.");
    });
    await scene("A06", "Verify the V2 contract in a new real conversation, then hold its completed answer.", async () => {
      await click(page.locator('[data-view="preview"]'));
      await expect(page.locator("#preview-host .chat-message")).toHaveCount(0);
      const responseV2 = await chat(2, second, responseV1);
      checkpoint("fresh-conversation", { before: { sessionID: responseV1.sessionID, conversationID: responseV1.conversationID },
        after: { sessionID: responseV2.sessionID, conversationID: responseV2.conversationID },
        qualification: "Fresh identifiers and deterministic instruction revisions only; not arbitrary history behavior." });
      cover("S05", "Clear produces a different backend session and conversation; V2 is a genuine new response.");
    });
    await scene("A07", "Independently verify immutable Agent revision snapshots and their instructions-only diff.", async () => {
      const evidence = {
        revisions: await cliJSON(["revisions", "agent", options.agentName]),
        before: await cliJSON(["revision", "agent", options.agentName, String(first.status.revisionCount)]),
        after: await cliJSON(["revision", "agent", options.agentName, String(second.status.revisionCount)]),
        diff: await cliJSON(["revision-diff", "agent", options.agentName,
          String(first.status.revisionCount), String(second.status.revisionCount)]),
      };
      const configMaps = await list("configmaps", revisionSelector);
      fs.writeFileSync(path.join(options.output, "revisions.json"), JSON.stringify({ ...evidence, configMaps }, null, 2), { flag: "wx" });
      verifyRevisionEvidence(evidence, first, second, configMaps);
      const persisted = await cliJSON(["get", "agent", options.agentName]);
      verifyAgent(persisted, options, 2, second.metadata, true);
      if (!isDeepStrictEqual(persisted.spec, second.spec) || persisted.status.revisionCount !== second.status.revisionCount) {
        throw new Error("The independently read Agent changed after the recorded revision");
      }
      checkpoint("immutable-revisions-and-cli-readback", { evidence, configMaps, persisted });
      cover("S07", "Separate frozen-CLI revision list/snapshots/spec diff and immutable ConfigMap parent-UID readback (off-screen evidence).");
    });
    await scene("A08", "Inspect the actual persisted Agent through Dashboard JSON and YAML.", async () => {
      checkAPIs();
      await page.goto(dashboard, { waitUntil: "domcontentloaded" });
      await expect(page.locator("#namespace")).toHaveValue(APPROVED.namespace);
      const agents = page.locator('#sidebar [data-plural="agents"]');
      if (await agents.count() !== 1) throw new Error("Frozen Dashboard does not expose the expected Agent inspection controls");
      await click(agents);
      await type(page.locator("#filter"), options.agentName);
      const row = page.locator("#table-wrap tbody tr").filter({ has: page.getByText(options.agentName, { exact: true }) });
      await click(row);
      await click(page.locator('[data-tab="json"]'));
      await expect(page.locator("#detail-body")).toContainText(INSTRUCTIONS[1]);
      await expect(page.locator("#detail-body")).toContainText(second.metadata.uid);
      await page.waitForTimeout(4000);
      await click(page.locator('[data-tab="yaml"]'));
      await expect(page.locator("#detail-body")).toContainText("revisionCount");
      await page.screenshot({ path: path.join(options.output, "dashboard.png") });
      await page.waitForTimeout(4000);
      cover("S03", "Actual Dashboard Agent envelope, spec/status, UID and revision inspection in JSON/YAML.");
    });
    guard.verifyComplete();
    await Promise.all([...captures]);
    checkAPIs();
    verifyController(await get("deployment", APPROVED.controller, APPROVED.systemNamespace), APPROVED);
    verifyGrant(await get("modelgrant", options.grantName), options, report.resources.grantUID);
    const finalAgent = await get("agent", options.agentName);
    const finalDeployment = await get("deployment", `agent-${options.agentName}`);
    verifyRuntime(finalAgent, finalDeployment, options, { uid: second.metadata.uid, generation: second.metadata.generation,
      deploymentUID: report.resources.deploymentUID, deploymentGeneration: report.lastRuntimeObservation.deployment.metadata.generation },
    2, firstDeployment);
    if (crypto.createHash("sha256").update(fs.readFileSync(cli)).digest("hex") !== report.cli.sha256) {
      throw new Error("Frozen CLI bytes changed during the recording");
    }
    checkpoint("all-authoring-checks-passed", { finalAgent, deployment: runtimeEvidence(finalDeployment) });
  } catch (error) {
    failure = error;
    report.failure = error.message;
    if (page && !page.isClosed()) await page.screenshot({ path: path.join(options.output, "failure.png") })
      .catch(screenshotError => report.browserErrors.push(`Failure screenshot: ${screenshotError.message}`));
    if (targetVerified && get) {
      report.retainedResources = [];
      for (const [kind, name] of [["agent", options.agentName], ["modelgrant", options.grantName]]) {
        try {
          const resource = await get(kind, name, APPROVED.namespace, true);
          if (resource) report.retainedResources.push(resource);
        } catch (readbackError) { report.browserErrors.push(`Failure readback ${kind}: ${readbackError.message}`); }
      }
    }
  } finally {
    const cleanupErrors = [];
    if (context && browser && video) {
      const destination = path.join(options.output, failure ? "authoring.failed.webm" : "authoring.recording.webm");
      try {
        Object.assign(report.recording, await finalizeRecording(context, browser, video, destination));
      } catch (error) { cleanupErrors.push(error.message); }
    } else {
      if (context) await context.close().catch(error => cleanupErrors.push(`Context close: ${error.message}`));
      if (browser) await browser.close().catch(error => cleanupErrors.push(`Browser close: ${error.message}`));
    }
    await Promise.all([...captures]);
    for (const child of [...children].reverse()) {
      await stopChildren([child]).catch(error => cleanupErrors.push(`UI process ${child.pid}: ${error.message}`));
    }
    if (cleanupErrors.length) report.cleanupFailures = cleanupErrors;
    if (!failure && (cleanupErrors.length || !report.recording?.bytes)) {
      failure = new Error("Recording persistence or exact UI process cleanup failed");
      report.failure = failure.message;
    }
    report.passed = !failure;
    report.completedAt = new Date().toISOString();
    persist();
  }
  if (failure) throw new Error(`Authoring batch failed; retain resources and inspect ${reportPath}: ${report.failure}`);
  console.log(`Verified first authoring batch: ${report.recording.path}`);
  return report;
}

if (require.main === module) {
  Promise.resolve().then(() => runAuthoring(parseOptions(process.argv.slice(2)))).catch(error => {
    console.error(error.message);
    process.exitCode = 1;
  });
}

module.exports = {
  APPROVED, RANCHER_TARGET, RANCHER_APPROVED, BASELINE_V3_TARGET, BASELINE_V3_APPROVED,
  BASELINE_V4_TARGET, BASELINE_V4_APPROVED,
  CLI_SHA256, recordingTarget, verifyRecordingCLI, isWelcomeDismissal, dismissWelcome,
  BOUNDS, ANSWERS, INSTRUCTIONS, INPUT, VIEWPORT, PENDING, parseOptions, verifyClusterFile,
  verifyController, verifyTarget, verifyModel, verifyAgent, verifyGrant, verifyDeploymentIdentity, verifyRuntime,
  verifyChat, verifyInvocation, verifyRevisionEvidence, createWriteGuard, verifyCompletedAnswer, runAuthoring,
  finalizeRecording,
};
