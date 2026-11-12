// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const { RANCHER_TARGET, BASELINE_V3_TARGET, BASELINE_V3_APPROVED } = require("./live-authoring");
const { CLI_SHA256, STUDIO_UI_V1, WORKFLOW_UI_V1, verifyStudioUI,
  verifyWorkflowStudioUI } = require("./live-production-session");

function proof() {
  return { passed: true, backendSourceChanges: 0, clusterImageChanges: 0,
    frozenCLISHA256: CLI_SHA256, cliSHA256: STUDIO_UI_V1.cliSHA256,
    changedFiles: ["app.js", "index.html", "resources.js", "style.css"].map(relative => ({ relative })) };
}

test("the recording UI derivative requires exact reviewed binary/proof bytes and preserves backend provenance", () => {
  verifyStudioUI(proof(), STUDIO_UI_V1.cliSHA256, STUDIO_UI_V1.qualificationSHA256, RANCHER_TARGET);
  for (const change of [
    { passed: false }, { backendSourceChanges: 1 }, { clusterImageChanges: 1 },
    { frozenCLISHA256: "replacement" }, { cliSHA256: "replacement" },
    { changedFiles: [{ relative: "studio.go" }] },
    { changedFiles: ["app.js", "index.html", "resources.js", "resources.js"].map(relative => ({ relative })) },
  ]) {
    assert.throws(() => verifyStudioUI({ ...proof(), ...change },
      STUDIO_UI_V1.cliSHA256, STUDIO_UI_V1.qualificationSHA256, RANCHER_TARGET));
  }
  assert.throws(() => verifyStudioUI(proof(), "different-binary", STUDIO_UI_V1.qualificationSHA256, RANCHER_TARGET));
  assert.throws(() => verifyStudioUI(proof(), STUDIO_UI_V1.cliSHA256, "different-proof", RANCHER_TARGET));
  assert.throws(() => verifyStudioUI(proof(), STUDIO_UI_V1.cliSHA256, STUDIO_UI_V1.qualificationSHA256, undefined));
});

test("the Workflow UI derivative binds the isolated baseline and one qualified asset only", () => {
  const descriptor = WORKFLOW_UI_V1;
  assert(descriptor, "The Workflow UI revision must be explicitly pinned");
  const qualified = {
    passed: true, revision: descriptor.revision, target: BASELINE_V3_TARGET,
    backendSourceChanges: 0, clusterImageChanges: 0,
    frozenCLISHA256: BASELINE_V3_APPROVED.cliSHA256,
    baselineSourceSHA256: BASELINE_V3_APPROVED.baselineSourceSHA256,
    cliSHA256: descriptor.cliSHA256,
    changedFiles: [{ relative: "cmd\\kognitionctl\\cmd\\studio_assets\\app.js",
      originalSHA256: descriptor.originalAssetSHA256, replacementSHA256: descriptor.assetSHA256 }],
    browser: { passed: 77, skipped: 1 },
    nativeLiveAuthoring: { passed: true, mockedAPI: false, providerInvocations: 0,
      createdWorkflowUID: "qualification-uid", projectedTokensBefore: 9948, projectedTokensAfter: 9948 },
  };
  const verify = (proof = qualified, cli = descriptor.cliSHA256,
    digest = descriptor.qualificationSHA256, target = BASELINE_V3_TARGET) =>
    verifyWorkflowStudioUI(proof, cli, digest, target);
  verify();
  for (const change of [
    { passed: false }, { target: RANCHER_TARGET }, { revision: "unqualified" },
    { frozenCLISHA256: "wrong" }, { baselineSourceSHA256: "wrong" },
    { backendSourceChanges: 1 }, { clusterImageChanges: 1 }, { cliSHA256: "wrong" },
    { changedFiles: [...qualified.changedFiles, qualified.changedFiles[0]] },
    { changedFiles: [{ ...qualified.changedFiles[0], relative: "app.js" }] },
    { changedFiles: [{ ...qualified.changedFiles[0], replacementSHA256: "wrong" }] },
    { browser: { passed: 0, skipped: 78 } },
    { nativeLiveAuthoring: { ...qualified.nativeLiveAuthoring, passed: false } },
    { nativeLiveAuthoring: { ...qualified.nativeLiveAuthoring, mockedAPI: true } },
    { nativeLiveAuthoring: { ...qualified.nativeLiveAuthoring, providerInvocations: 1 } },
    { nativeLiveAuthoring: { ...qualified.nativeLiveAuthoring, projectedTokensAfter: 10000 } },
  ]) assert.throws(() => verify({ ...qualified, ...change }));
  assert.throws(() => verify(qualified, "wrong"));
  assert.throws(() => verify(qualified, descriptor.cliSHA256, "wrong"));
  assert.throws(() => verify(qualified, descriptor.cliSHA256, descriptor.qualificationSHA256, RANCHER_TARGET));
});
