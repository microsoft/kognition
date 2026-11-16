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

// Runs in the page. Content is addressed by its vertical offset inside the element; the readable
// window is what remains after every clipping ancestor and the viewport cut the element down.
// With a position it scrolls the element and its ancestors, then reports the offsets truly exposed.
function inPageReading(element, { position }) {
  const clips = [];
  for (let node = element.parentElement; node; node = node.parentElement) {
    if (/auto|scroll|hidden/.test(getComputedStyle(node).overflowY)) clips.push(node);
  }
  const internal = element.scrollHeight > element.clientHeight + 1;
  const height = element.scrollHeight;
  const bounds = () => {
    let top = 0;
    let bottom = window.innerHeight;
    for (const node of clips) {
      const rect = node.getBoundingClientRect();
      top = Math.max(top, rect.top + node.clientTop);
      bottom = Math.min(bottom, rect.top + node.clientTop + node.clientHeight);
    }
    return { top, bottom };
  };
  if (position === null) {
    const window_ = bounds();
    return { height, viewport: Math.max(1, Math.min(internal ? element.clientHeight : height, window_.bottom - window_.top)) };
  }
  const scrolled = internal ? Math.min(Math.max(0, position), height - element.clientHeight) : 0;
  if (internal) element.scrollTop = scrolled;
  const alignedTop = () => element.getBoundingClientRect().top + element.clientTop + position - scrolled - bounds().top;
  for (const node of clips) {
    if (node.scrollHeight > node.clientHeight + 1) node.scrollTop += alignedTop();
  }
  window.scrollBy(0, alignedTop());
  const rect = element.getBoundingClientRect();
  const origin = rect.top + element.clientTop;
  const visible = bounds();
  const shown = internal ? element.clientHeight : height;
  const from = Math.max(visible.top, origin) - origin + (internal ? element.scrollTop : 0);
  const to = Math.min(visible.bottom, origin + shown) - origin + (internal ? element.scrollTop : 0);
  return { from, to };
}

function firstUnexposedOffset(intervals, height) {
  const tolerance = 2;
  let covered = 0;
  for (const { from, to } of [...intervals].sort((a, b) => a.from - b.from)) {
    if (from > covered + tolerance) break;
    covered = Math.max(covered, to);
  }
  return covered >= height - tolerance ? null : covered;
}

async function holdReadableText(page, content, policy = READING_POLICY) {
  await content.scrollIntoViewIfNeeded();
  await expect(content).toBeVisible();
  const renderedText = () => content.evaluate(element =>
    ["INPUT", "TEXTAREA"].includes(element.tagName) ? element.value : element.innerText);
  const text = await renderedText();
  const requiredSeconds = durationSeconds(text, policy);
  const geometry = await content.evaluate(inPageReading, { position: null });
  const maximum = Math.max(0, geometry.height - geometry.viewport);
  const stride = Math.max(1, Math.floor(geometry.viewport * 0.8));
  const offsets = [0];
  for (let offset = stride; offset < maximum; offset += stride) offsets.push(offset);
  if (maximum > 0) offsets.push(maximum);
  const segmentSeconds = Math.max(policy.minimumSeconds, requiredSeconds / offsets.length);
  const exposed = [];
  const started = performance.now();
  for (const offset of offsets) {
    exposed.push(await content.evaluate(inPageReading, { position: offset }));
    await expect(content).toBeVisible();
    await page.waitForTimeout(segmentSeconds * 1000);
  }
  await expect(content).toBeVisible();
  const unexposed = firstUnexposedOffset(exposed, geometry.height);
  if (unexposed !== null) {
    throw new Error(`Reading could not expose content at ${Math.round(unexposed)}px of ${geometry.height}px; a clipping pane hides it`);
  }
  if (await renderedText() !== text) throw new Error("Content changed during its reading window");
  return { wordCount: text.trim().split(/\s+/).length, requiredSeconds,
    visibleSeconds: (performance.now() - started) / 1000, segments: offsets.length,
    textSHA256: crypto.createHash("sha256").update(text).digest("hex") };
}

async function holdReadableConfiguration(page, content) {
  return holdReadableText(page, content, CONFIGURATION_READING_POLICY);
}

module.exports = { READING_POLICY, CONFIGURATION_READING_POLICY, readingWindowSeconds, configurationReadingWindowSeconds,
  verifyConfigurationReadingWindow, holdReadableText, holdReadableConfiguration, firstUnexposedOffset };
