// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const { chromium } = require("@playwright/test");

async function main() {
  const [dashboardURL, runID, resultPath, screenshotPath] = process.argv.slice(2);
  if (!dashboardURL || !runID || !resultPath || !screenshotPath) {
    throw new Error("dashboard URL, run ID, result path, and screenshot path are required");
  }

  const startedAt = new Date();
  const started = performance.now();
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
    const pageErrors = [];
    page.on("pageerror", (error) => pageErrors.push(error.message));
    await page.goto(dashboardURL, { waitUntil: "networkidle" });
    await page.locator("#trust-focus").waitFor({ state: "visible" });
    await page.locator("#trust-focus-connection").filter({ hasText: "Live" }).waitFor();
    const runProof = page.locator("#trust-run-proof");
    await runProof.waitFor({ state: "visible" });
    const proof = {
      scenario: (await page.locator("#trust-run-proof-scenario").textContent()).trim(),
      protectedAction: (await page.locator("#trust-run-proof-action").textContent()).trim(),
      mode: (await page.locator("#trust-run-proof-mode").textContent()).trim(),
      status: (await page.locator("#trust-run-proof-status").textContent()).trim(),
      assertions: (await page.locator("#trust-run-proof-assertions").textContent()).trim(),
    };

    const expectedIDs = [
      `${runID}:tool-allow`,
      `${runID}:memory-allow`,
      `${runID}:tool-deny`,
      `${runID}:memory-revoked`,
    ];
    const stream = page.locator("#trust-focus-stream-body [data-decision-id]");
    if (await stream.count() !== expectedIDs.length) {
      throw new Error(`expected ${expectedIDs.length} decisions, observed ${await stream.count()}`);
    }

    const observedIDs = [];
    for (const id of expectedIDs) {
      const decision = page.locator(`#trust-focus-stream-body [data-decision-id="${id}"]`);
      await decision.click();
      await page.locator("#trust-evidence-state").filter({ hasText: id }).waitFor();
      observedIDs.push(id);
    }

    const emptyStates = await page.locator("#trust-focus .empty").allTextContents();
    if (emptyStates.length > 0) {
      throw new Error(`unexpected empty Trust Focus state: ${emptyStates.join("; ")}`);
    }
    if (pageErrors.length > 0) {
      throw new Error(`browser errors: ${pageErrors.join("; ")}`);
    }

    await page.screenshot({ path: screenshotPath, fullPage: true });
    const completedAt = new Date();
    fs.writeFileSync(resultPath, JSON.stringify({
      schemaVersion: 1,
      verificationScope: "technical-fallback-traversal",
      narratedRehearsalVerified: false,
      recordingReady: false,
      runId: runID,
      startedAt: startedAt.toISOString(),
      completedAt: completedAt.toISOString(),
      elapsedSeconds: Math.round((performance.now() - started) / 10) / 100,
      viewport: { width: 1440, height: 900 },
      initialScreen: "Trust Focus",
      finalScreen: await page.locator("#trust-focus").isVisible() ? "Trust Focus" : "unknown",
      connection: await page.locator("#trust-focus-connection").textContent(),
      runProof: proof,
      observedDecisionIds: observedIDs,
      emptyStates,
      pageErrors,
      screenshotPath,
    }, null, 2));
  } finally {
    await browser.close();
  }
}

main().catch((error) => {
  console.error(error.stack || error.message);
  process.exitCode = 1;
});