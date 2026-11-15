// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { parse } from 'yaml';
import { load } from 'cheerio';
import { tutorial, validateTutorial, tutorialPaths, parseSubtitles, captionsVTT, chaptersVTT,
  transcriptMarkdown, formatTime } from '../tutorial.mjs';
import { fetchTutorialAssets, verifyTutorialAssets } from '../scripts/fetch-tutorial.mjs';
import { checkSite } from '../scripts/check-links.mjs';
import { getSiteConfig } from '../site.mjs';

const srt = '1\n00:00:00,250 --> 00:00:01,750\nWelcome to\nKognition.\n\n' +
  '2\n00:00:04,000 --> 00:00:05,000\nUse <read> & write.\n';
const chapters = [{ title: 'Welcome', startSeconds: 0 }, { title: 'Configure', startSeconds: 3 }];

test('tutorial paths stay same-origin under both private and project Pages bases', () => {
  for (const base of ['/', '/kognition/']) {
    const urls = tutorialPaths(base);
    for (const [role, asset] of Object.entries(tutorial.assets)) {
      assert.equal(urls[role], `${base}media/tutorial/${tutorial.releaseTag}/${asset.name}`);
    }
  }
  for (const invalid of ['', 'https://example.org/', '/../private/', '/no-trailing-slash', '//host/']) {
    assert.throws(() => tutorialPaths(invalid));
  }
});

test('media identity rejects moving releases, unsafe names, missing checksums and invalid chapters', () => {
  for (const change of [
    { releaseTag: 'latest' },
    { repository: 'another/repository' },
    { assets: { ...tutorial.assets, video: { ...tutorial.assets.video, name: '../movie.mp4' } } },
    { assets: { ...tutorial.assets, video: { ...tutorial.assets.video, sha256: '' } } },
    { assets: { ...tutorial.assets, video: { ...tutorial.assets.video, bytes: 0 } } },
    { assets: { ...tutorial.assets, unexpected: { ...tutorial.assets.video, name: 'extra.mp4' } } },
    { chapters: [{ title: 'Late', startSeconds: 1 }] },
    { chapters: [{ title: 'First', startSeconds: 0 }, { title: 'Same', startSeconds: 0 }] },
    { chapters: [{ title: 'First', startSeconds: 0 }, { title: 'Outside', startSeconds: tutorial.durationSeconds }] },
  ]) assert.throws(() => validateTutorial({ ...tutorial, ...change }));
});

test('WebVTT captions and the searchable transcript preserve every spoken word and timestamp', async () => {
  const cues = parseSubtitles(srt, 6);
  assert.equal(cues.length, 2);
  assert.deepEqual(cues[0], { startSeconds: .25, endSeconds: 1.75, text: 'Welcome to\nKognition.' });
  assert.match(captionsVTT(cues), /^WEBVTT\n\n1\n00:00:00\.250 --> 00:00:01\.750\nWelcome to\nKognition\./);
  assert.match(captionsVTT(cues), /Use &lt;read&gt; &amp; write\./);
  assert.match(chaptersVTT(chapters, 6), /00:00:03\.000 --> 00:00:06\.000\nConfigure/);
  const transcript = transcriptMarkdown(cues, chapters, 6);
  assert.match(transcript, /### 00:00 Welcome\n\n\*\*00:00\*\* Welcome to Kognition\./);
  assert.match(transcript, /### 00:03 Configure\n\n\*\*00:04\*\* Use \\<read\\> \\& write\./);
  const { createMarkdownProcessor } = await import('@astrojs/markdown-remark');
  const hostile = [{ startSeconds: 0, endSeconds: 5, text: 'Run <read> & *write* `code` [link](x) a_b | ~c~' }];
  const rendered = await (await createMarkdownProcessor()).render(
    transcriptMarkdown(hostile, [{ title: 'A <b> & *c*', startSeconds: 0 }], 5));
  const page = load(rendered.code);
  assert.equal(page('p').text().replace(/^\d\d:\d\d /, ''), 'Run <read> & *write* `code` [link](x) a_b | ~c~');
  assert.equal(page('h3').text(), '00:00 A <b> & *c*');
  assert.equal(page('read, b, em, code, a, del').length, 0);
  assert.equal(formatTime(tutorial.durationSeconds), '23:50');
  for (const invalid of [
    srt.replace('1\n', '2\n'), srt.replace('00:00:04,000', '00:00:01,000'),
    srt.replace('00:00:05,000', '00:01:05,000'), srt.replace('00:00:00,250', '00:61:00,250'),
    srt.replace('00:00:01,750', '00:00:00,100'),
  ]) assert.throws(() => parseSubtitles(invalid, 6));
});

function fixtureManifest() {
  const manifest = structuredClone(tutorial);
  for (const asset of Object.values(manifest.assets)) {
    const data = Buffer.from(`verified ${asset.name}`);
    asset.bytes = data.length;
    asset.sha256 = createHash('sha256').update(data).digest('hex');
  }
  return manifest;
}

async function writeAssets(directory, manifest) {
  for (const asset of Object.values(manifest.assets)) {
    await writeFile(path.join(directory, asset.name), `verified ${asset.name}`);
  }
}

test('private release media is verified before installation and cached assets are reverified', async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'kognition-tutorial-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const manifest = fixtureManifest();
  let downloads = 0;
  const download = async (directory, requested) => {
    assert.equal(requested.releaseTag, tutorial.releaseTag);
    downloads++;
    await writeAssets(directory, requested);
  };
  const directory = await fetchTutorialAssets(root, { manifest, download });
  assert.equal(await verifyTutorialAssets(directory, manifest), 5);
  assert.equal(await fetchTutorialAssets(root, { manifest, download }), directory);
  assert.equal(downloads, 1);
  const file = path.join(directory, manifest.assets.video.name);
  const original = await readFile(file);
  original[0] ^= 1;
  await writeFile(file, original);
  await assert.rejects(fetchTutorialAssets(root, { manifest, download }), /checksum mismatch/);
  assert.equal(downloads, 1, 'A changed cached asset must fail rather than be silently replaced.');
});

test('missing or altered downloaded assets fail closed without installing a partial release', async (t) => {
  for (const mode of ['missing', 'changed', 'extra', 'unavailable']) {
    const root = await mkdtemp(path.join(os.tmpdir(), 'kognition-tutorial-failure-'));
    t.after(() => rm(root, { recursive: true, force: true }));
    const manifest = fixtureManifest();
    await assert.rejects(fetchTutorialAssets(root, { manifest, download: async (directory) => {
      if (mode === 'unavailable') throw new Error('Release unavailable');
      await writeAssets(directory, manifest);
      if (mode === 'missing') await rm(path.join(directory, manifest.assets.captions.name));
      if (mode === 'changed') await writeFile(path.join(directory, manifest.assets.video.name), 'changed');
      if (mode === 'extra') await writeFile(path.join(directory, 'unexpected.txt'), 'unexpected');
    } }));
    assert.deepEqual(await readdir(root), [], 'No partially verified media may be published.');
  }
});

test('rendered media validation checks video, poster and tracks; preview exceptions are narrowly scoped', async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'kognition-tutorial-links-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const media = tutorialPaths('/kognition/');
  await writeFile(path.join(root, 'index.html'), `<video poster="${media.poster}">
    <source src="${media.video}"><track src="${media.captions}"><track src="${media.chapters}"></video>`);
  assert.throws(() => checkSite(root, getSiteConfig({})), /missing target/);
  assert.equal(checkSite(root, getSiteConfig({}), { allowMissingTutorialMedia: true }).missingTutorialMedia, 4);
  const directory = path.join(root, 'media', 'tutorial', tutorial.releaseTag);
  await mkdir(directory, { recursive: true });
  await writeAssets(directory, tutorial);
  assert.equal(checkSite(root, getSiteConfig({})).missingTutorialMedia, 0);
  await writeFile(path.join(root, 'index.html'), '<video poster="/kognition/unlisted.webp"><source src="/kognition/unlisted.mp4"></video>');
  assert.throws(() => checkSite(root, getSiteConfig({}), { allowMissingTutorialMedia: true }), /unlisted/);
});

test('a video poster is validated independently of the video src on the same element', async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'kognition-poster-links-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(path.join(root, 'movie.mp4'), 'video');
  await writeFile(path.join(root, 'index.html'), '<video src="/kognition/movie.mp4" poster="/kognition/missing.webp"></video>');
  assert.throws(() => checkSite(root, getSiteConfig({})), /missing target: \/kognition\/missing\.webp/);
  await writeFile(path.join(root, 'index.html'), '<video src="/kognition/movie.mp4" poster="https://example.test/outside.webp"></video>');
  assert.equal(checkSite(root, getSiteConfig({})).links, 1);
  await writeFile(path.join(root, 'index.html'), '<video src="/kognition/movie.mp4" poster="/outside/poster.webp"></video>');
  assert.throws(() => checkSite(root, getSiteConfig({})), /escapes site base/);
});

test('the canonical transcript contains the narration for every chapter and no production-local paths', async () => {
  const markdown = await readFile(new URL('../../docs/guides/video-tutorial.md', import.meta.url), 'utf8');
  for (const chapter of tutorial.chapters) assert.ok(markdown.includes(`### ${formatTime(chapter.startSeconds)} ${chapter.title}`));
  assert.match(markdown, /Welcome to this Kognition tutorial/);
  assert.match(markdown, /Thank you for watching this Kognition tutorial/);
  assert.doesNotMatch(markdown, /C:\\Users\\|session-state|\.copilot|\.runtime\\/);
});

test('only trusted publication receives a token for pinned private media and requires strict validation', async () => {
  const workflow = parse(await readFile(new URL('../../.github/workflows/pages.yml', import.meta.url), 'utf8'));
  const steps = workflow.jobs.build.steps;
  const download = steps.find((step) => step.run === 'npm run tutorial:fetch');
  assert.equal(download.if, workflow.jobs.deploy.if);
  assert.equal(download.env.GH_TOKEN, '${{ github.token }}');
  assert.equal(workflow.env.GH_TOKEN, undefined);
  const build = steps.find((step) => step.run === 'npm run build');
  assert(steps.indexOf(download) < steps.indexOf(build));
  const strict = steps.find((step) => step.run === 'npm run check-links');
  assert.equal(strict.if, download.if);
  const preview = steps.find((step) => step.run === 'npm run check-links -- --without-tutorial-media');
  assert.match(preview.if, /github\.event_name == 'pull_request'/);
});
