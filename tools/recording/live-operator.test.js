// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { chromium } = require("@playwright/test");
const { APPROVED, RANCHER_TARGET, RANCHER_APPROVED, finalizeRecording } = require("./live-authoring");
const { READING_POLICY } = require("./reading");
const { RUNTIME, PRODUCTION, RANCHER_RUNTIME, RANCHER_PRODUCTION, COMPONENT_PATTERN, parseOptions, inspectFrozen,
  readPriorComponent, displayCommand, buildPlan, createCommandRunner, startViewer } = require("./live-operator");

const options = () => ({ runtime: RUNTIME, output: path.join(PRODUCTION, `operator-test-${crypto.randomUUID()}`),
  runId: "operator-test", authorizedLiveWrites: true });
const args = input => ["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
  "--authorize-live-writes"];

function directory(t) {
  const result = path.resolve(`operator-unit-${crypto.randomUUID()}`);
  fs.mkdirSync(result);
  t.after(() => fs.rmSync(result, { recursive: true, force: true }));
  return result;
}

test("strict options reject unknown, duplicate, non-production and reused outputs", t => {
  const input = options();
  assert.equal(parseOptions(args(input)).runtime, RUNTIME);
  assert.throws(() => parseOptions([...args(input), "--mutation"]), /Unknown option/);
  assert.throws(() => parseOptions([...args(input), "--run-id", "again"]), /Duplicate/);
  assert.throws(() => parseOptions(args({ ...input, runtime: path.resolve("other-runtime") })), /exact approved runtime/);
  assert.throws(() => parseOptions(args({ ...input, output: path.resolve("operator-wrong-root") })), /full-production/);
  const existing = directory(t);
  assert.throws(() => parseOptions(args({ ...input, output: existing })), /already exists/);
  assert.throws(() => parseOptions(args(input).filter(value => value !== "--authorize-live-writes")), /required/);
});

test("the complete plan has fourteen qualified scenes, never blanket acceptance", () => {
  const scenes = buildPlan(options());
  assert.deepEqual(scenes.map(scene => scene.id), Array.from({ length: 14 }, (_, index) => `S${index + 34}`));
  for (const scene of scenes) {
    assert.ok(scene.behaviors.length && scene.subactions.length && scene.commands.length);
    assert.ok(scene.narration.length > 70 && scene.limits.startsWith("No ") ||
      scene.limits.includes("not deployed acceptance"));
  }
  assert.deepEqual(READING_POLICY, { wordsPerMinute: 150, orientationSeconds: 2, minimumSeconds: 5, capped: false });
});

test("all cluster commands explicitly bind kubeconfig, context, namespace and read-only verbs", () => {
  for (const command of buildPlan(options()).flatMap(scene => scene.commands)) {
    if (command.program === "kubectl") {
      assert.equal(command.args[command.args.indexOf("--kubeconfig") + 1], path.join(RUNTIME, "kubeconfig"));
      assert.equal(command.args[command.args.indexOf("--context") + 1], APPROVED.context);
      assert.equal(command.args[5], "get");
      assert.ok(command.args.includes("-o"));
      assert.ok(!command.args.some(argument => /^(secrets?|--raw|exec|apply|create|patch|delete|scale|rollout)$/i.test(argument)));
      const format = command.args[command.args.indexOf("-o") + 1];
      assert.match(format, /^(jsonpath|custom-columns)=/);
      assert.doesNotMatch(format, /binaryData|\.data|checkpoint|capability|password|authToken|internalToken/i);
      if (format.includes(".value")) assert.match(format, /MEMORY_BACKEND_TYPE/);
    } else if (command.program.endsWith("kognitionctl.exe")) {
      assert.ok(["get", "revisions", "cost"].includes(command.args[0]));
      assert.equal(command.environment.KUBECONFIG, path.join(RUNTIME, "kubeconfig"));
      assert.equal(command.args[command.args.indexOf("--kube-context") + 1], APPROVED.context);
      assert.equal(command.args[command.args.indexOf("--namespace") + 1], APPROVED.namespace);
      assert.match(displayCommand(command), /KUBECONFIG=/);
    }
  }
});

test("Rancher operator reads use its exact target and current time window without changing historical commands", () => {
  const input = { runtime: RANCHER_RUNTIME, output: path.join(RANCHER_PRODUCTION, "operator-unit-unused"),
    runId: "operator-rancher-unit", authorizedLiveWrites: true, target: RANCHER_TARGET };
  const parsed = parseOptions([...args(input), "--target", RANCHER_TARGET]);
  assert.equal(parsed.target, RANCHER_TARGET);
  assert.throws(() => parseOptions([...args(input)]));
  for (const command of buildPlan(parsed).flatMap(scene => scene.commands)) {
    if (command.program === "kubectl") {
      assert.equal(command.args[command.args.indexOf("--kubeconfig") + 1], path.join(RANCHER_RUNTIME, "kubeconfig"));
      assert.equal(command.args[command.args.indexOf("--context") + 1], RANCHER_APPROVED.context);
      assert.equal(command.args[5], "get");
      assert(!command.args.some(argument => /^(exec|apply|create|patch|delete|scale|rollout)$/i.test(argument)));
    }
    if (command.id === "normalized-cost") assert.equal(command.args[command.args.indexOf("--start") + 1], "2026-11-05T00:00:00Z");
    if (command.id === "migration-job-status") assert.equal(command.args[command.args.indexOf("-n") + 1], "tutorial-system");
  }
});

test("the optional component command selects only inspected database-free tests", () => {
  const commands = buildPlan(options()).flatMap(scene => scene.commands);
  const command = commands.find(item => item.id === "pgvector-component");
  assert.equal(commands.filter(item => item.program === "go").length, 1);
  assert.deepEqual(command.args, ["test", ".\\providers\\pgvector", "-run", COMPONENT_PATTERN,
    "-count=1", "-v", "-timeout=90s"]);
  assert.match(command.scope, /component contract test \/ simulated dependencies \/ not deployed acceptance/);
  assert.equal(command.environment.GOPROXY, "off");
  assert.equal(command.environment.GOFLAGS, "-mod=readonly");
  assert.ok(command.environment.GOTMPDIR.startsWith(PRODUCTION));
  assert.match(command.cwd, /cold-start-repair-v1\\source$/);
  assert.match(displayCommand(command), /GOTMPDIR=/);
  assert.match(displayCommand(command), /GOPROXY='off'/);
});

test("source inspection refuses paths and identities outside its fixed allowlist", () => {
  assert.throws(() => inspectFrozen(RUNTIME, "..\\kubeconfig"), /allowlisted/);
  assert.throws(() => inspectFrozen(path.resolve("wrong-runtime"), "probes"), /allowlisted/);
});

test("a disclosed continuation does not replay prior reads or claim the failed component test passed", () => {
  const input = options(), reportPath = path.join(PRODUCTION, "operator-a1", "report.json");
  const parsed = parseOptions([...args(input), "--continue-from", reportPath]);
  const scenes = buildPlan(parsed);
  assert.deepEqual(scenes.map(scene => scene.id), ["S43", "S44", "S45", "S46", "S47"]);
  assert.match(scenes[0].narration, /They did not pass/);
  assert.ok(!scenes.flatMap(scene => scene.commands).some(command => command.program === "go"));
  assert.equal(scenes[0].commands[1].id, "preserved-component-failure");
  assert.throws(() => parseOptions([...args(input), "--continue-from"]), /requires one/);
  assert.throws(() => readPriorComponent(path.resolve("untrusted-report.json")), /production operator report/);
});

test("real subprocess stdout, stderr, failing exit and pre-execution PID persist without rewriting", async t => {
  const output = directory(t), observations = [];
  const commands = [
    { id: "success", program: process.execPath, args: ["-e", 'process.stdout.write("actual\\n<output>");process.stderr.write("warning\\n")'],
      expectedExit: 0, timeout: 5000 },
    { id: "expected-failure", program: process.execPath, args: ["-e", 'process.stderr.write("real missing resource\\n");process.exit(1)'],
      expectedExit: 1, timeout: 5000 },
    { id: "unexpected-failure", program: process.execPath, args: ["-e", 'process.stdout.write("not success");process.exit(3)'],
      expectedExit: 0, timeout: 5000 },
  ];
  const run = createCommandRunner(commands, output, record => observations.push(structuredClone(record)));
  const success = await run("success");
  assert.equal(success.stdout, "actual\n<output>");
  assert.equal(success.stderr, "warning\n");
  assert.equal(fs.readFileSync(success.stdoutPath, "utf8"), success.stdout);
  assert.ok(observations.some(item => item.pid && !item.completedAt));
  assert.ok(success.startedAt <= success.completedAt && success.pid > 0 && success.passed);
  const denied = await run("expected-failure");
  assert.equal(denied.exitCode, 1);
  assert.equal(denied.stderr, "real missing resource\n");
  assert.equal(denied.passed, true);
  const failed = await run("unexpected-failure");
  assert.equal(failed.passed, false);
  assert.equal(failed.exitCode, 3);
  await assert.rejects(run("success"), /repeated/);
  await assert.rejects(run("unreviewed-command"), /Unknown/);
});

test("the real loopback viewer rejects foreign/out-of-order requests and records literal subprocess output", async t => {
  const output = directory(t);
  const command = { id: "viewer-one", program: process.execPath,
    args: ["-e", 'process.stdout.write("real <script> text\\n");process.stderr.write("actual stderr\\n")'],
    expectedExit: 0, timeout: 10000 };
  const scenes = [{ id: "TEST", title: "Real recording-only viewer", narration: "Read-only test of recording infrastructure.",
    limits: "Not Kognition deployed behavior.", commands: [command] }];
  const runner = createCommandRunner([command], output);
  const viewer = await startViewer(scenes, runner);
  let browser, context;
  try {
    const foreign = await fetch(`${viewer.origin}/execute`, { method: "POST", headers: { Origin: "https://foreign.invalid" },
      body: JSON.stringify({ id: command.id }) });
    assert.equal(foreign.status, 403);
    const unknown = await fetch(`${viewer.origin}/execute`, { method: "POST", headers: { Origin: viewer.origin },
      body: JSON.stringify({ id: "arbitrary" }) });
    assert.equal(unknown.status, 400);
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport: { width: 1920, height: 1080 },
      recordVideo: { dir: output, size: { width: 1920, height: 1080 } } });
    const page = await context.newPage(), video = page.video();
    await page.goto(viewer.origin);
    await page.evaluate(id => window.loadReviewedCommand(id), command.id);
    await page.locator("#run").click();
    await page.locator('#output[data-completed="viewer-one"]').waitFor();
    assert.equal(await page.locator("#output").innerText(),
      "STDOUT (verbatim)\nreal <script> text\n\nSTDERR (verbatim)\nactual stderr\n");
    assert.match(await page.locator("header").innerText(), /Recording-only terminal viewer, not Kognition Studio/);
    assert.equal(await page.locator("#output script").count(), 0);
    assert.match(await page.locator("#stamp").innerText(), /Exit 0/);
    await page.waitForTimeout(300);
    const recording = await finalizeRecording(context, browser, video, path.join(output, "verified.webm"));
    context = undefined; browser = undefined;
    assert.ok(recording.bytes > 0);
    const replay = await fetch(`${viewer.origin}/execute`, { method: "POST", headers: { Origin: viewer.origin },
      body: JSON.stringify({ id: command.id }) });
    assert.equal(replay.status, 400);
  } finally {
    if (context) await context.close();
    if (browser) await browser.close();
    await viewer.close();
  }
});
