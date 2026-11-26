// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const http = require("node:http");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const {
  parseOptions, fixedOrigin, fieldLabel, verifyAgent, verifyKubernetesRuntime, verifyModel, verifyChat, verifyRevisions, verifyMedia, buildCaptions, proxyWriteAllowed, startAuthProxy, runPilot,
} = require("./live-pilot");

const options = { namespace: "default", agentName: "pilot-test", grantName: "pilot-test-model", model: "model" };
const args = [
  "--cli", "cli.exe", "--mode", "standalone", "--server", "http://127.0.0.1:18080",
  "--namespace", "default", "--model", "model", "--run-id", "test", "--output-dir", "result",
  "--ffmpeg", "ffmpeg.exe", "--ffprobe", "ffprobe.exe", "--authorize-live-writes",
];

test("pilot requires explicit write authorization and rejects ambiguous backend selection", () => {
  assert.throws(() => parseOptions(args.slice(0, -1)), /authorize-live-writes/);
  assert.throws(() => parseOptions([...args, "--kube-context", "shared"]), /no kube context/);
  assert.equal(parseOptions(args).agentName, "pilot-test");
  assert.throws(() => parseOptions([...args, "--unknown"]), /Unknown option/);
});

test("authenticated target refuses remote plaintext, credentials and token-bearing URLs", () => {
  assert.equal(fixedOrigin("http://127.0.0.1:8080", "target"), "http://127.0.0.1:8080");
  for (const url of ["http://localhost:8080", "http://remote.test", "https://user:secret@remote.test",
    "https://remote.test?token=value", "https://remote.test/path"]) {
    assert.throws(() => fixedOrigin(url, "target"));
  }
});

test("field selection accepts required markers without confusing Name with Namespace", () => {
  assert.equal(fieldLabel("Name").test("Name"), true);
  assert.equal(fieldLabel("Name").test("Name*"), true);
  assert.equal(fieldLabel("Name").test("Namespace"), false);
  assert.equal(fieldLabel("Model").test("Model*"), true);
  assert.equal(fieldLabel("Model").test("Model grant"), false);
});

test("proxy permits only scoped pilot Agent and ModelGrant writes and bounded chat", () => {
  const prefix = "/apis/kognition.dev/v1alpha1/namespaces/default";
  const agent = { kind: "Agent", metadata: { name: options.agentName, namespace: options.namespace } };
  assert.equal(proxyWriteAllowed("POST", `${prefix}/agents`, agent, options), true);
  assert.equal(proxyWriteAllowed("PUT", `${prefix}/agents/pilot-test`, agent, options), true);
  assert.equal(proxyWriteAllowed("DELETE", `${prefix}/agents/pilot-test`, agent, options), false);
  assert.equal(proxyWriteAllowed("POST", `${prefix}/agents`, {
    ...agent, metadata: { name: "someone-elses-agent", namespace: "default" },
  }, options), false);
  assert.equal(proxyWriteAllowed("POST", `${prefix}/agents/pilot-test/chat`, { message: "hello" }, options), true);
  assert.equal(proxyWriteAllowed("POST", `${prefix}/agents/pilot-test/chat`, { message: "x".repeat(4097) }, options), false);
  assert.equal(proxyWriteAllowed("POST", `${prefix}/secretrefs`, agent, options), false);
  assert.equal(proxyWriteAllowed("POST", `${prefix}/agents`, null, options), false);
  const grant = {
    kind: "ModelGrant", metadata: { name: options.grantName, namespace: options.namespace },
    spec: { agentRef: { name: options.agentName }, modelRef: { name: options.model } },
  };
  assert.equal(proxyWriteAllowed("POST", `${prefix}/modelgrants`, grant, options), true);
  assert.equal(proxyWriteAllowed("POST", `${prefix}/modelgrants`, {
    ...grant, spec: { ...grant.spec, agentRef: { name: "other-agent" } },
  }, options), false);
});

test("persisted Agent evidence requires matching identity, instructions, revision and runtime bounds", () => {
  const agent = {
    kind: "Agent", metadata: { name: options.agentName, namespace: "default", uid: "original-agent" },
    spec: { modelRef: { name: "model" }, instructions: { inline: "instruction" }, runtime: { maxTokens: 256 }, replicas: 0 },
    status: { phase: "Ready", revisionCount: 2 },
  };
  verifyAgent(agent, options, "instruction", 2, "original-agent");
  assert.throws(() => verifyAgent(agent, options, "instruction", 2, "replacement-agent"));
  assert.throws(() => verifyAgent(agent, options, "different", 2));
  assert.throws(() => verifyAgent({ ...agent, status: { phase: "Pending", revisionCount: 2 } }, options, "instruction", 2));
  assert.throws(() => verifyAgent({ ...agent, status: { phase: "Ready" } }, options, "instruction", 2));
  assert.throws(() => verifyAgent({ ...agent, spec: { ...agent.spec, runtime: { maxTokens: 4096 } } }, options, "instruction", 2));
});

test("revision verification accepts the API envelope and requires only the authored instruction change", () => {
  const first = {
    metadata: { name: options.agentName, namespace: options.namespace },
    spec: { instructions: { inline: "before" } }, status: { revisionCount: 1 },
  };
  const second = {
    ...first, spec: { instructions: { inline: "after" } }, status: { revisionCount: 2 },
  };
  const evidence = {
    revisions: { kind: "RevisionList", metadata: first.metadata, items: [{ revision: 1 }, { revision: 2 }] },
    before: { kind: "Revision", metadata: { ...first.metadata, revision: 1 }, spec: { spec: first.spec } },
    after: { kind: "Revision", metadata: { ...first.metadata, revision: 2 }, spec: { spec: second.spec } },
    diff: {
      kind: "RevisionDiff", metadata: first.metadata, fromRevision: 1, toRevision: 2,
      patch: [{ op: "replace", path: "/spec/instructions/inline", value: "after" }],
    },
  };
  verifyRevisions(evidence, first, second);
  assert.throws(() => verifyRevisions({ ...evidence, diff: evidence.diff.patch }, first, second));
  assert.throws(() => verifyRevisions({
    ...evidence, diff: { ...evidence.diff, patch: [...evidence.diff.patch, { op: "replace", path: "/replicas", value: 1 }] },
  }, first, second));
  assert.throws(() => verifyRevisions({
    ...evidence, after: { ...evidence.after, spec: { spec: first.spec } },
  }, first, second));
  assert.throws(() => verifyRevisions({
    ...evidence, diff: { ...evidence.diff, toRevision: 3 },
  }, first, second));
  assert.throws(() => verifyRevisions({
    ...evidence, revisions: { ...evidence.revisions, items: [{ revision: 1 }] },
  }, first, second));
  assert.throws(() => verifyRevisions({
    ...evidence, before: { ...evidence.before, metadata: { ...evidence.before.metadata, namespace: "another" } },
  }, first, second));
});

test("Kubernetes readiness requires current Agent identity and an available physical runtime rollout", () => {
  const expected = { name: "support", namespace: "tutorial", uid: "agent-uid", deploymentUID: "deployment-uid", generation: 2, replicas: 1, maxTokens: 256 };
  const agent = {
    kind: "Agent", metadata: { name: expected.name, namespace: expected.namespace, uid: expected.uid, generation: 2 },
    spec: { replicas: 1, runtime: { maxTokens: 256 } },
    status: { phase: "Ready", replicas: 1, readyReplicas: 1 },
  };
  const deployment = {
    kind: "Deployment", metadata: { name: "agent-support", namespace: "tutorial", uid: "deployment-uid", generation: 2 },
    spec: { replicas: 1 },
    status: { observedGeneration: 2, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 1 },
  };
  verifyKubernetesRuntime(agent, expected, deployment);
  for (const status of [
    { phase: "Ready" },
    { phase: "Ready", replicas: 0, readyReplicas: 0 },
    { phase: "Ready", replicas: 1, readyReplicas: 0 },
    { phase: "Pending", replicas: 1, readyReplicas: 1 },
  ]) {
    assert.throws(() => verifyKubernetesRuntime({ ...agent, status }, expected, deployment), /ready replica count/);
  }
  for (const metadata of [
    { ...agent.metadata, uid: "replacement" },
    { ...agent.metadata, generation: 3 },
    { ...agent.metadata, namespace: "another" },
    { ...agent.metadata, deletionTimestamp: "2026-10-31T00:00:00Z" },
  ]) {
    assert.throws(() => verifyKubernetesRuntime({ ...agent, metadata }, expected, deployment));
  }
  assert.throws(() => verifyKubernetesRuntime({ ...agent, spec: { ...agent.spec, replicas: 0 } }, expected, deployment));
  assert.throws(() => verifyKubernetesRuntime({ ...agent, spec: { ...agent.spec, runtime: { maxTokens: 1024 } } }, expected, deployment));
  assert.throws(() => verifyKubernetesRuntime(agent, { ...expected, replicas: 0 }, deployment));
  assert.throws(() => verifyKubernetesRuntime(agent, expected));
  assert.throws(() => verifyKubernetesRuntime(agent, expected, { ...deployment, metadata: { ...deployment.metadata, uid: "replacement" } }));
  assert.throws(() => verifyKubernetesRuntime(agent, expected, { ...deployment, metadata: { ...deployment.metadata, deletionTimestamp: "2026-10-31T00:00:00Z" } }));
  for (const status of [
    { observedGeneration: 2 },
    { ...deployment.status, observedGeneration: 1 },
    { ...deployment.status, readyReplicas: 0 },
    { ...deployment.status, availableReplicas: 0 },
    { ...deployment.status, updatedReplicas: 0 },
  ]) {
    assert.throws(() => verifyKubernetesRuntime(agent, expected, { ...deployment, status }));
  }
});

test("runner refuses to overwrite existing evidence before starting browsers or writers", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "kognition-pilot-test-"));
  try {
    const marker = path.join(directory, "original.txt");
    fs.writeFileSync(marker, "existing evidence");
    await assert.rejects(runPilot({
      ...options, cli: __filename, ffmpeg: __filename, ffprobe: __filename, outputDir: directory,
    }), { code: "EEXIST" });
    assert.equal(fs.readFileSync(marker, "utf8"), "existing evidence");
    assert.deepEqual(fs.readdirSync(directory), ["original.txt"]);
  } finally {
    fs.rmSync(directory, { recursive: true });
  }
});

test("chat verification requires actual expected behavior and consistent positive token usage", () => {
  const response = {
    response: "SUPPORT V1: For INC-1042, collect the affected region and timeout rate.",
    sessionID: "session-1", usage: { promptTokens: 30, completionTokens: 15, totalTokens: 45 },
  };
  verifyChat(response, 1);
  assert.throws(() => verifyChat({ ...response, response: "loading" }, 1));
  assert.throws(() => verifyChat({ ...response, usage: { promptTokens: 0, completionTokens: 0, totalTokens: 0 } }, 1));
  assert.throws(() => verifyChat({ ...response, usage: { promptTokens: 30, completionTokens: 15, totalTokens: 99 } }, 1));
  assert.throws(() => verifyChat({ ...response, sessionID: "" }, 1));
});

test("standalone model preflight requires an actual provider token ceiling and Foundry API version", () => {
  const model = {
    status: { phase: "Ready" }, spec: { provider: "foundry", parameters: { max_tokens: "256", apiVersion: "2024-10-21" } },
  };
  verifyModel(model, "standalone");
  assert.throws(() => verifyModel({ ...model, spec: { ...model.spec, parameters: {} } }, "standalone"), /ceiling/);
  assert.throws(() => verifyModel({
    ...model, spec: { ...model.spec, parameters: { ...model.spec.parameters, max_tokens: "4096" } },
  }, "standalone"), /ceiling/);
  assert.throws(() => verifyModel({
    ...model, spec: { ...model.spec, parameters: { max_tokens: "256" } },
  }, "standalone"), /API version/);
  assert.throws(() => verifyModel({ ...model, status: { phase: "Failed" } }, "standalone"), /not Ready/);
});

test("profile-backed model preflight verifies a namespace-local bounded chat profile", () => {
  const profile = {
    kind: "ModelInvocationProfile",
    metadata: { name: "pilot-chat", namespace: "default", uid: "profile-uid" },
    spec: { operation: "chat", defaults: { maxCompletionTokens: 256 }, limits: { maxCompletionTokens: 256 } },
  };
  const model = {
    metadata: { namespace: "default" }, status: { phase: "Ready" },
    spec: {
      provider: "foundry", parameters: { apiVersion: "2024-12-01-preview" },
      invocationContract: "openai-tool-chat-v1",
      invocationProfileRef: { name: "pilot-chat", specDigest: "sha256:" + "a".repeat(64) },
    },
  };
  verifyModel(model, "standalone", profile);
  assert.throws(() => verifyModel(model, "standalone"), /profile/);
  assert.throws(() => verifyModel(model, "standalone", {
    ...profile, metadata: { ...profile.metadata, namespace: "other-workspace" },
  }), /namespace-local/);
  assert.throws(() => verifyModel(model, "standalone", {
    ...profile, spec: { ...profile.spec, limits: { maxCompletionTokens: 4096 } },
  }), /256/);
  assert.throws(() => verifyModel({
    ...model, spec: { ...model.spec, invocationContract: "" },
  }, "standalone", profile), /contract/);
  assert.throws(() => verifyModel({
    ...model, spec: { ...model.spec, parameters: { ...model.spec.parameters, max_tokens: "256" } },
  }, "standalone", profile), /profile/);
});

test("media verification measures the three-to-five-minute threshold and exact output shape", () => {
  const probe = {
    streams: [{ codec_type: "video", codec_name: "h264", width: 1920, height: 1080 }],
    format: { duration: "200.25" },
  };
  assert.deepEqual(verifyMedia(probe), { durationSeconds: 200.25, width: 1920, height: 1080, codec: "h264" });
  for (const duration of ["179.99", "300.01", "NaN"]) {
    assert.throws(() => verifyMedia({ ...probe, format: { duration } }));
  }
  assert.throws(() => verifyMedia({ ...probe, streams: [{ ...probe.streams[0], width: 800 }] }));
  assert.throws(() => verifyMedia({ ...probe, streams: [] }));
});

test("captions follow observed scene timing and disclose automation", () => {
  const captions = buildCaptions([{ startSeconds: 1.25, endSeconds: 26.75, caption: "Create the Agent." }]);
  assert.match(captions, /00:00:01,250 --> 00:00:26,750/);
  assert.match(captions, /AUTOMATED LIVE PILOT: Create the Agent\./);
});

test("authenticated proxy forwards real payloads, refuses other writes and does not follow redirects", async t => {
  const observed = [];
  const upstream = http.createServer(async (request, response) => {
    let body = "";
    for await (const chunk of request) body += chunk;
    observed.push({ path: request.url, authorization: request.headers.authorization, body });
    if (request.url.endsWith("/redirect")) {
      response.writeHead(302, { location: "http://127.0.0.1:1/" }).end();
    } else {
      response.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({ persisted: body }));
    }
  });
  await new Promise(resolve => upstream.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${upstream.address().port}`;
  const errors = [];
  const proxy = await startAuthProxy(origin, "test-operator-credential".padEnd(40, "x"), options, error => errors.push(error));
  t.after(async () => {
    await new Promise(resolve => proxy.server.close(resolve));
    await new Promise(resolve => upstream.close(resolve));
  });
  const prefix = "/apis/kognition.dev/v1alpha1/namespaces/default";
  const payload = JSON.stringify({ kind: "Agent", metadata: { name: "pilot-test", namespace: "default" } });
  const result = await fetch(`${proxy.origin}${prefix}/agents`, { method: "POST", body: payload });
  assert.equal(result.status, 200);
  assert.equal((await result.json()).persisted, payload);
  assert.equal(observed[0].body, payload);
  assert.match(observed[0].authorization, /^Bearer /);
  const denied = await fetch(`${proxy.origin}${prefix}/agents/pilot-test`, { method: "DELETE", body: "{}" });
  assert.equal(denied.status, 403);
  assert.equal(observed.length, 1);
  const redirected = await fetch(`${proxy.origin}${prefix}/redirect`);
  assert.equal(redirected.status, 502);
  assert.deepEqual(errors, ["authenticated API redirect rejected"]);
  assert.equal(observed.length, 2);
  for (let invocation = 0; invocation < 2; invocation++) {
    const chat = await fetch(`${proxy.origin}${prefix}/agents/pilot-test/chat`, {
      method: "POST", body: JSON.stringify({ message: "hello" }),
    });
    assert.equal(chat.status, 200);
    await chat.text();
  }
  const capped = await fetch(`${proxy.origin}${prefix}/agents/pilot-test/chat`, {
    method: "POST", body: JSON.stringify({ message: "hello" }),
  });
  assert.equal(capped.status, 429);
  assert.equal(observed.length, 4);
});

test("authenticated proxy pins its upstream destination independently of incoming request targets", async t => {
  const observed = [];
  let attackerRequests = 0;
  const upstream = http.createServer((request, response) => {
    observed.push(request.url);
    response.writeHead(200).end("approved upstream");
  });
  const attacker = http.createServer((_request, response) => {
    attackerRequests++;
    response.writeHead(200).end("unapproved upstream");
  });
  await new Promise(resolve => upstream.listen(0, "127.0.0.1", resolve));
  await new Promise(resolve => attacker.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${upstream.address().port}`;
  const attackerOrigin = `http://127.0.0.1:${attacker.address().port}`;
  const errors = [];
  const proxy = await startAuthProxy(`${origin}/`, "test-operator-credential".padEnd(40, "x"), options, error => errors.push(error));
  t.after(async () => {
    await new Promise(resolve => proxy.server.close(resolve));
    await new Promise(resolve => upstream.close(resolve));
    await new Promise(resolve => attacker.close(resolve));
  });
  const prefix = "/apis/kognition.dev/v1alpha1/namespaces/default";
  const rawRequest = requestTarget => new Promise((resolve, reject) => {
    const request = http.get({
      hostname: "127.0.0.1", port: proxy.server.address().port, path: requestTarget,
      headers: { host: new URL(attackerOrigin).host },
    }, response => {
      response.resume();
      response.on("end", () => resolve(response.statusCode));
    });
    request.on("error", reject);
  });
  for (const target of [
    `${attackerOrigin}${prefix}/agents`,
    `//127.0.0.1:${attacker.address().port}${prefix}/agents`,
    `/\\127.0.0.1:${attacker.address().port}${prefix}/agents`,
    `${origin}${prefix}/agents`,
    `${prefix}/../../../../other-api`,
  ]) {
    assert.equal(await rawRequest(target), 403, target);
  }
  assert.equal(observed.length, 0);
  const approvedPath = `${prefix}/agents?continue=${encodeURIComponent(attackerOrigin)}`;
  assert.equal(await rawRequest(approvedPath), 200);
  assert.deepEqual(observed, [approvedPath]);
  assert.equal(attackerRequests, 0);
  assert.deepEqual(errors, []);
});

test("authenticated proxy preserves literal IPv6 loopback destinations", async t => {
  const upstream = http.createServer((_request, response) => response.writeHead(200).end("IPv6 upstream"));
  try {
    await new Promise((resolve, reject) => {
      upstream.once("error", reject);
      upstream.listen(0, "::1", resolve);
    });
  } catch (error) {
    if (error.code !== "EAFNOSUPPORT" && error.code !== "EADDRNOTAVAIL") throw error;
    t.skip("IPv6 loopback is unavailable");
    return;
  }
  const errors = [];
  const proxy = await startAuthProxy(`http://[::1]:${upstream.address().port}`, "test-operator-credential".padEnd(40, "x"), options, error => errors.push(error));
  t.after(async () => {
    await new Promise(resolve => proxy.server.close(resolve));
    await new Promise(resolve => upstream.close(resolve));
  });
  const response = await fetch(`${proxy.origin}/apis/kognition.dev/v1alpha1/agents`);
  assert.equal(response.status, 200);
  assert.equal(await response.text(), "IPv6 upstream");
  assert.deepEqual(errors, []);
});
