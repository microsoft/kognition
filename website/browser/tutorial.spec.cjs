// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { test, expect } = require("@playwright/test");
const base = process.env.DOCS_BASE_PATH || "/kognition/";
const route = `${base}guides/video-tutorial/`;

test("the tutorial has an accessible same-origin player, transcript, downloads and no autoplay", async ({ page }) => {
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(route);
  const video = page.locator("#tutorial-video");
  await expect(video).toBeVisible();
  await expect(video).toHaveAttribute("controls", "");
  await expect(video).toHaveAttribute("preload", "none");
  await expect(video).not.toHaveAttribute("autoplay", "");
  await expect(video.locator("track[kind=captions]")).toHaveAttribute("srclang", "en");
  await expect(video.locator("track[kind=chapters]")).toHaveAttribute("src", `${base}media/tutorial/tutorial-2026-11-14/kognition-tutorial.chapters.en-us.vtt`);
  const resources = await video.evaluate(element => [element.poster, ...[...element.querySelectorAll("source, track")].map(node => node.src)]);
  expect(resources.every(url => new URL(url).origin === new URL(page.url()).origin)).toBe(true);
  await expect(page.getByRole("navigation", { name: "Chapters", exact: true }).getByRole("link")).toHaveCount(11);
  await expect(page.getByRole("heading", { name: "Transcript", exact: true })).toHaveCount(1);
  await expect(page.locator("main")).toContainText("Thank you for watching this Kognition tutorial.");
  await expect(page.getByRole("link", { name: "Download MP4 (84 MiB)", exact: true })).toHaveAttribute("download", "");
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect(errors).toEqual([]);
});

test("keyboard chapter navigation seeks the real video and text tracks retain exact cue counts", async ({ page }) => {
  await page.goto(route);
  const chapter = page.locator('[data-tutorial-time="527.68"]');
  await chapter.focus();
  await chapter.press("Enter");
  await expect.poll(() => page.locator("video").evaluate(video => video.readyState)).toBeGreaterThan(0);
  await expect.poll(() => page.locator("video").evaluate(video => video.currentTime)).toBeCloseTo(527.68, 1);
  expect(await page.locator("video").evaluate(video => video.paused)).toBe(true);
  await expect(page).toHaveURL(/#t=527\.68$/);
  await page.locator("video").evaluate(video => {
    for (const track of video.textTracks) track.mode = "hidden";
  });
  await expect.poll(() => page.locator("track[kind=captions]").evaluate(track => track.track.cues?.length || 0)).toBe(268);
  await expect.poll(() => page.locator("track[kind=chapters]").evaluate(track => track.track.cues?.length || 0)).toBe(11);
  expect(await page.locator("track[kind=captions]").evaluate(track => track.track.cues[0].text)).toBe("Welcome to this Kognition tutorial.");
});

test("timestamp links work on initial load and media supports byte-range playback", async ({ page, request }) => {
  await page.goto(`${route}#t=224.64`);
  await expect.poll(() => page.locator("video").evaluate(video => video.currentTime)).toBeCloseTo(224.64, 1);
  const src = await page.locator("video source").getAttribute("src");
  const response = await request.get(src, { headers: { Range: "bytes=0-127" } });
  expect(response.status()).toBe(206);
  expect(response.headers()["content-range"]).toBe("bytes 0-127/87738980");
  expect((await response.body()).length).toBe(128);
});

test("a failed media request is explicit and leaves the transcript available", async ({ page }) => {
  await page.route("**/media/tutorial/**/*.mp4", request => request.abort("failed"));
  await page.goto(route);
  await page.locator('[data-tutorial-time="224.64"]').click();
  await expect(page.locator(".tutorial-status")).toBeVisible();
  await expect(page.locator(".tutorial-status")).toContainText("could not be loaded");
  await expect(page.getByRole("heading", { name: "Transcript", exact: true })).toHaveCount(1);
  await expect(page.getByRole("link", { name: "English captions (SRT)", exact: true })).toHaveAttribute("download", "");
});

test("documentation search finds the tutorial transcript", async ({ page }) => {
  await page.goto(route);
  await page.locator("button[data-open-modal]").click();
  await page.locator(".pagefind-ui__search-input").fill("TrustProfile");
  await expect(page.locator(".pagefind-ui__results")).toContainText("Build, govern and verify an assistant");
  await expect(page.locator(".pagefind-ui__results")).toContainText("10:20 Understand TrustProfiles");
});
