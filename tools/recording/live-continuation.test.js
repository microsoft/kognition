// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { RANCHER_TARGET, RANCHER_APPROVED } = require("./live-authoring");
const { parseContinuationOptions, readContinuation } = require("./live-production-session");

test("continuation options preserve normal consent and reject duplicate or missing report arguments", () => {
  const args = ["--runtime", "runtime", "--output", "unused-continuation-output",
    "--run-id", "rancher-gov-unit", "--target", RANCHER_TARGET, "--authorize-live-writes"];
  const parsed = parseContinuationOptions([...args, "--continue-from", path.join("runtime", "governance-prior", "report.json")]);
  assert.equal(parsed.target, RANCHER_TARGET);
  assert.equal(path.basename(parsed.continuationPath), "report.json");
  assert.throws(() => parseContinuationOptions([...args, "--continue-from"]));
  assert.throws(() => parseContinuationOptions([...args, "--continue-from", "first", "--continue-from", "second"]));
  assert.throws(() => parseContinuationOptions(args.filter(item => item !== "--authorize-live-writes")));
});

test("recovery requires the exact failed report, reviewed target, original recording and matching run ID", t => {
  const runtime = fs.mkdtempSync(path.join(os.tmpdir(), "kognition-continuation-"));
  t.after(() => fs.rmSync(runtime, { recursive: true, force: true }));
  const folder = path.join(runtime, "governance-prior");
  fs.mkdirSync(folder);
  const video = path.join(folder, "production.failed.webm");
  fs.writeFileSync(video, "test fixture bytes");
  const file = path.join(folder, "report.json");
  const options = { runtime, continuationPath: file, runId: "rancher-gov-unit", target: RANCHER_TARGET };
  const base = { passed: false, failure: "Controlled viewer failure.", batch: "governance-denials",
    runId: options.runId, approvedTarget: RANCHER_APPROVED, recording: { path: video, bytes: fs.statSync(video).size } };
  fs.writeFileSync(file, JSON.stringify(base));
  const recovered = readContinuation(options, "governance-denials", "governance-");
  assert.equal(recovered.reportPath, file);
  assert.match(recovered.reportSHA256, /^[a-f0-9]{64}$/);
  assert.match(recovered.sourceVideoSHA256, /^[a-f0-9]{64}$/);
  for (const change of [
    { passed: true }, { runId: "different" }, { batch: "different" },
    { approvedTarget: { ...RANCHER_APPROVED, controllerUID: "replacement" } },
    { recording: { path: video, bytes: 1 } },
    { recording: { path: path.join(runtime, "outside-take.webm"), bytes: base.recording.bytes } },
  ]) {
    fs.writeFileSync(file, JSON.stringify({ ...base, ...change }));
    assert.throws(() => readContinuation(options, "governance-denials", "governance-"));
  }
  assert.throws(() => readContinuation({ ...options, continuationPath: path.join(runtime, "..", "report.json") },
    "governance-denials", "governance-"));
});
