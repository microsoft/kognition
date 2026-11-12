// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { test, expect } = require("@playwright/test");

test.beforeEach(async ({ request }) => {
  const meta = await (await request.get("/api/meta")).json();
  const response = await request.post("/api/tutorial", {
    headers: { "X-Kognition-Practice": meta.tutorial.token },
    data: { action: "reset", generation: meta.tutorial.progress.generation },
  });
  expect(response.ok()).toBeTruthy();
});

async function next(page, text) {
  await expect(page.locator("#tutorial-next")).toBeEnabled();
  await page.locator("#tutorial-next").click();
  await expect(page.locator("#tutorial-step-title")).toContainText(text);
}

async function createAgent(page) {
  await page.locator("#tutorial-start").click();
  await next(page, "Create your first Agent");
  await page.locator("#btn-create").click();
  await page.locator("#btn-new-agent").click();
  const identity = page.locator("#builder-host details").filter({ hasText: "Identity" }).first();
  await identity.locator('input[type="text"]').first().fill("learner-agent");
  await page.locator("#builder-host").getByLabel(/^Model\s*\*?$/).selectOption("tutorial-model");
  await next(page, "Tell the Agent");
  await page.locator('[data-tutorial-target="instructions"] textarea').fill("Explain a useful next step briefly.");
  await next(page, "Connect the form");
}

test("first-Agent practice completes through real served synthetic APIs without outbound traffic", async ({ page }) => {
  // TC-0341: use the real practice server, not browser route stubs.
  const external = [];
  page.on("request", request => {
    if (new URL(request.url()).origin !== "http://127.0.0.1:4186") external.push(request.url());
  });
  await page.goto("/");
  await expect(page.locator("#practice-banner")).toContainText("PRACTICE ONLY");
  await expect(page.locator("#namespace-select")).toBeDisabled();
  await createAgent(page);
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await expect(page.locator("#yaml-area")).toHaveValue(/learner-agent/);
  await page.getByRole("tab", { name: "Builder", exact: true }).click();
  await next(page, "Save and find");
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("Saved in practice");
  await expect(page.locator("#list-agents")).toContainText("learner-agent");
  await next(page, "Try a simulated");
  await page.getByRole("tab", { name: "Preview", exact: true }).click();
  await page.locator(".chat-input-row textarea").fill("What should I do next?");
  await page.locator(".chat-input-row").getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.locator("#preview-host")).toContainText("Simulated response");
  await next(page, "Try a small change");
  await page.getByRole("tab", { name: "Builder", exact: true }).click();
  await page.locator('[data-tutorial-target="instructions"] textarea').fill("Use one concise sentence instead.");
  await page.locator("#tutorial-answer").selectOption("live");
  await expect(page.locator("#tutorial-count")).not.toHaveText("7 of 7 tasks completed");
  await page.locator("#tutorial-answer").selectOption("practice");
  await expect(page.locator("#tutorial-step-title")).toHaveText("Practice complete");
  expect(external).toEqual([]);
  expect(await page.evaluate(() => ({ local: Object.keys(localStorage), session: Object.keys(sessionStorage) }))).toEqual({ local: [], session: [] });
  const geometry = await page.evaluate(() => ({
    width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth,
  }));
  expect(geometry.scroll).toBeLessThanOrEqual(geometry.width);
});

test("pause, keyboard guidance, reset and reload preserve honest task progress", async ({ page }) => {
  // TC-0343
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/");
  await expect(page.locator("#tutorial-next")).toBeDisabled();
  await page.locator("#tutorial-start").click();
  await next(page, "Create your first Agent");
  await page.locator("#tutorial-show").click();
  await expect(page.locator("#btn-new-agent")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.locator("#create-menu")).toBeHidden();
  await expect(page.locator("#tutorial-step-title")).not.toHaveText("Lesson paused");
  await page.locator("#tutorial-pause").click();
  await expect(page.locator("#tutorial-step-title")).toHaveText("Lesson paused");
  await page.locator("#tutorial-resume").click();
  await page.reload();
  await expect(page.locator("#tutorial-step-title")).toContainText("Create your first Agent");
  page.once("dialog", dialog => dialog.accept());
  await page.locator("#tutorial-reset").click();
  await expect(page.locator("#tutorial-count")).toHaveText("0 of 7 tasks completed");
});

test("unsupported and cross-origin practice requests fail instead of falling back to live services", async ({ page, request }) => {
  const meta = await (await request.get("/api/meta")).json();
  const token = meta.tutorial.token;
  for (const endpoint of ["/api/invoke/tool/real", "/api/invoke/workflow/real", "/api/gallery/starter/import", "/api/resources/runtimeapprovals"]) {
    const response = await request.post(endpoint, { headers: { "X-Kognition-Practice": token }, data: {} });
    expect(response.status()).toBeGreaterThanOrEqual(400);
  }
  const response = await request.post("/api/tutorial", {
    headers: { "X-Kognition-Practice": token, Origin: "https://attacker.invalid" },
    data: { action: "reset", generation: meta.tutorial.progress.generation },
  });
  expect(response.status()).toBe(403);
  await page.goto("/");
  const blocked = await page.evaluate(async () => {
    try { await fetch("https://example.invalid/should-not-connect"); return false; }
    catch { return true; }
  });
  expect(blocked).toBeTruthy();
});
