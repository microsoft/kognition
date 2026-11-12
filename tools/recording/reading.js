// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const crypto = require("node:crypto");
const { expect } = require("@playwright/test");

const READING_POLICY = Object.freeze({
  wordsPerMinute: 150, orientationSeconds: 2, minimumSeconds: 5, capped: false,
});

function readingWindowSeconds(text) {
  if (typeof text !== "string" || !text.trim()) throw new Error("Reading requires non-empty rendered text");
  const words = text.trim().split(/\s+/).length;
  return Math.max(READING_POLICY.minimumSeconds,
    READING_POLICY.orientationSeconds + Math.ceil(words * 60 / READING_POLICY.wordsPerMinute));
}

async function holdReadableText(page, content) {
  await content.scrollIntoViewIfNeeded();
  await expect(content).toBeVisible();
  const renderedText = () => content.evaluate(element =>
    ["INPUT", "TEXTAREA"].includes(element.tagName) ? element.value : element.innerText);
  const text = await renderedText();
  const requiredSeconds = readingWindowSeconds(text);
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
  const segmentSeconds = Math.max(READING_POLICY.minimumSeconds, requiredSeconds / offsets.length);
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

module.exports = { READING_POLICY, readingWindowSeconds, holdReadableText };
