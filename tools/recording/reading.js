// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const crypto = require("node:crypto");
const { expect } = require("@playwright/test");

const READING_POLICY = Object.freeze({
  wordsPerMinute: 150, orientationSeconds: 2, minimumSeconds: 5, capped: false,
});
const CONFIGURATION_READING_POLICY = Object.freeze({
  wordsPerMinute: 120, orientationSeconds: 3, minimumSeconds: 12, capped: false,
});

function durationSeconds(text, policy) {
  if (typeof text !== "string" || !text.trim()) throw new Error("Reading requires non-empty rendered text");
  const words = text.trim().split(/\s+/).length;
  return Math.max(policy.minimumSeconds,
    policy.orientationSeconds + Math.ceil(words * 60 / policy.wordsPerMinute));
}

function readingWindowSeconds(text) { return durationSeconds(text, READING_POLICY); }
function configurationReadingWindowSeconds(text) { return durationSeconds(text, CONFIGURATION_READING_POLICY); }

function verifyConfigurationReadingWindow({ configuredText, displayedText, fullyVisible, visibleSeconds }) {
  const requiredSeconds = configurationReadingWindowSeconds(configuredText);
  if (displayedText !== configuredText) throw new Error("Reading must show the complete configured content");
  if (fullyVisible !== true) throw new Error("Configured content must be fully visible during its reading window");
  if (!Number.isFinite(visibleSeconds) || visibleSeconds < requiredSeconds) {
    throw new Error(`Configured content requires ${requiredSeconds} seconds of readable exposure`);
  }
  return { requiredSeconds, visibleSeconds, wordCount: configuredText.trim().split(/\s+/).length,
    textSHA256: crypto.createHash("sha256").update(configuredText).digest("hex") };
}

async function holdReadableText(page, content, policy = READING_POLICY) {
  await content.scrollIntoViewIfNeeded();
  await expect(content).toBeVisible();
  const renderedText = () => content.evaluate(element =>
    ["INPUT", "TEXTAREA"].includes(element.tagName) ? element.value : element.innerText);
  const text = await renderedText();
  const requiredSeconds = durationSeconds(text, policy);
  const geometry = await content.evaluate(element => {
    const internal = element.scrollHeight > element.clientHeight + 1;
    const history = element.closest(".chat-history");
    const viewport = internal ? element.clientHeight : history?.clientHeight || window.innerHeight - 160;
    return { height: element.scrollHeight, viewport: Math.max(1, viewport) };
  });
  const maximum = Math.max(0, geometry.height - geometry.viewport);
  const stride = Math.max(1, Math.floor(geometry.viewport * 0.8));
  const offsets = [0];
  for (let offset = stride; offset < maximum; offset += stride) offsets.push(offset);
  if (maximum > 0) offsets.push(maximum);
  const segmentSeconds = Math.max(policy.minimumSeconds, requiredSeconds / offsets.length);
  const started = performance.now();
  for (const offset of offsets) {
    await content.evaluate((element, position) => {
      if (element.scrollHeight > element.clientHeight + 1) element.scrollTop = position;
      else {
        const history = element.closest(".chat-history");
        if (history) {
          history.scrollTop += element.getBoundingClientRect().top - history.getBoundingClientRect().top + position;
        } else {
          window.scrollBy(0, element.getBoundingClientRect().top + position);
        }
      }
    }, offset);
    await expect(content).toBeVisible();
    await page.waitForTimeout(segmentSeconds * 1000);
  }
  await expect(content).toBeVisible();
  if (await renderedText() !== text) throw new Error("Content changed during its reading window");
  return { wordCount: text.trim().split(/\s+/).length, requiredSeconds,
    visibleSeconds: (performance.now() - started) / 1000, segments: offsets.length,
    textSHA256: crypto.createHash("sha256").update(text).digest("hex") };
}

async function holdReadableConfiguration(page, content) {
  return holdReadableText(page, content, CONFIGURATION_READING_POLICY);
}

module.exports = { READING_POLICY, CONFIGURATION_READING_POLICY, readingWindowSeconds, configurationReadingWindowSeconds,
  verifyConfigurationReadingWindow, holdReadableText, holdReadableConfiguration };
