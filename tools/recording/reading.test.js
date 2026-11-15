// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const test = require("node:test");
const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const { chromium } = require("@playwright/test");
const { holdReadableText, holdReadableConfiguration, configurationReadingWindowSeconds,
  verifyConfigurationReadingWindow } = require("./reading");

const prompt = "You help fictional Cedar Labs with Orbital Mark 7. Reply only to the current request. Keep answers under 90 words.";

test("configuration reviews budget time for the complete configured content", () => {
  assert.equal(configurationReadingWindowSeconds(prompt), 13);
  assert(configurationReadingWindowSeconds("configured ".repeat(120)) >= 63);
});

test("a long scene cannot substitute for a readable completed configuration window", () => {
  assert.throws(() => verifyConfigurationReadingWindow({
    configuredText: prompt, displayedText: prompt, fullyVisible: true,
    visibleSeconds: 5.08, sceneDurationSeconds: 16.2,
  }), /requires 13 seconds/);
  assert.throws(() => verifyConfigurationReadingWindow({
    configuredText: prompt, displayedText: "You help fictional", fullyVisible: true, visibleSeconds: 60,
  }), /complete configured content/);
  assert.throws(() => verifyConfigurationReadingWindow({
    configuredText: prompt, displayedText: prompt, fullyVisible: false, visibleSeconds: 60,
  }), /fully visible/);
  const result = verifyConfigurationReadingWindow({
    configuredText: prompt, displayedText: prompt, fullyVisible: true, visibleSeconds: 13,
  });
  assert.equal(result.requiredSeconds, 13);
  assert.equal(result.textSHA256, crypto.createHash("sha256").update(prompt).digest("hex"));
});

test("configuration capture holds the finished editable value for its full reading budget", async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1200, height: 600 } });
    await page.setContent('<textarea aria-label="Prompt content" style="width:1100px;height:120px"></textarea>');
    const content = page.getByLabel("Prompt content");
    await content.fill(prompt);
    const result = await holdReadableConfiguration(page, content);
    assert.equal(result.requiredSeconds, 13);
    assert(result.visibleSeconds >= 13);
    assert.equal(result.textSHA256, crypto.createHash("sha256").update(prompt).digest("hex"));
    assert.equal(await content.inputValue(), prompt);
  } finally {
    await browser.close();
  }
});

test("manifest reading uses the actual editable value, not empty textarea text content", async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 800, height: 300 } });
    await page.setContent('<textarea aria-label="Manifest" style="width:700px;height:80px"></textarea>');
    const manifest = page.getByLabel("Manifest");
    const text = "kind: Prompt";
    await manifest.fill(text);
    const result = await holdReadableText(page, manifest);
    assert.equal(result.wordCount, 2);
    assert.equal(result.textSHA256, crypto.createHash("sha256").update(text).digest("hex"));
    assert.ok(result.visibleSeconds >= result.requiredSeconds);
    assert.equal(await manifest.inputValue(), text);
  } finally {
    await browser.close();
  }
});

test("reading exposes the beginning and end of clipped answers before returning", async t => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const mode of ["output", "chat", "document"]) {
      await t.test(mode, async () => {
        const page = await browser.newPage({ viewport: { width: 800, height: 300 } });
        try {
          await page.setContent(`
            <style>
              body { margin: 0; font: 16px/40px monospace; }
              p { margin: 0; height: ${mode === "document" ? 120 : 40}px; }
              ${mode === "output" ? "#answer { height: 100px; overflow-y: auto; }" : ""}
              ${mode === "chat" ? ".chat-history { height: 100px; overflow-y: auto; }" : ""}
            </style>
            <div class="${mode === "chat" ? "chat-history" : ""}">
              <div id="answer">
                <p id="first">First visible line.</p>
                <p>Second visible line.</p>
                <p>Third visible line.</p>
                <p id="last">Last visible line.</p>
              </div>
            </div>
          `);
          await page.evaluate(() => {
            window.exposure = { first: 0, last: 0 };
            const answer = document.getElementById("answer");
            const container = answer.scrollHeight > answer.clientHeight
              ? answer : answer.closest(".chat-history");
            let previous = performance.now();
            window.exposureTimer = setInterval(() => {
              const now = performance.now();
              const clip = container?.getBoundingClientRect() || { top: 0, bottom: innerHeight };
              for (const id of ["first", "last"]) {
                const rect = document.getElementById(id).getBoundingClientRect();
                if (rect.top >= Math.max(0, clip.top) && rect.bottom <= Math.min(innerHeight, clip.bottom)) {
                  window.exposure[id] += now - previous;
                }
              }
              previous = now;
            }, 20);
          });
          const result = await holdReadableText(page, page.locator("#answer"));
          const exposure = await page.evaluate(() => {
            clearInterval(window.exposureTimer);
            return window.exposure;
          });
          assert.ok(result.visibleSeconds >= result.requiredSeconds);
          assert.ok(exposure.first >= 4900, `${mode}: beginning visible for ${exposure.first}ms`);
          assert.ok(exposure.last >= 4900, `${mode}: ending visible for ${exposure.last}ms`);
          if (mode !== "document") assert.ok(result.segments > 1);
        } finally {
          await page.close();
        }
      });
    }
  } finally {
    await browser.close();
  }
});
