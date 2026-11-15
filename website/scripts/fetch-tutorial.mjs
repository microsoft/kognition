// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { lstat, mkdir, mkdtemp, readdir, rename, rm } from 'node:fs/promises';
import path from 'node:path';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { tutorial, validateTutorial } from '../tutorial.mjs';

const execute = promisify(execFile);

export async function verifyTutorialAssets(directory, manifest = tutorial) {
  validateTutorial(manifest);
  const expected = Object.values(manifest.assets);
  const actual = await readdir(directory);
  if (actual.length !== expected.length || actual.some((name) => !expected.some((asset) => asset.name === name))) {
    throw new Error('Tutorial media directory must contain exactly the pinned assets.');
  }
  for (const asset of expected) {
    const file = path.join(directory, asset.name);
    const metadata = await lstat(file);
    if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size !== asset.bytes) {
      throw new Error(`Tutorial asset has an unexpected type or size: ${asset.name}`);
    }
    const hash = createHash('sha256');
    for await (const chunk of createReadStream(file)) hash.update(chunk);
    if (hash.digest('hex') !== asset.sha256) throw new Error(`Tutorial asset checksum mismatch: ${asset.name}`);
  }
  return expected.length;
}

async function downloadRelease(directory, manifest) {
  try {
    await execute('gh', ['release', 'download', manifest.releaseTag, '--repo', manifest.repository,
      '--dir', directory, ...Object.values(manifest.assets).flatMap((asset) => ['--pattern', asset.name])],
    { timeout: 300_000, windowsHide: true });
  } catch {
    throw new Error(`Unable to download ${manifest.repository} release ${manifest.releaseTag}. Verify GitHub authentication and the five pinned assets.`);
  }
}

export async function fetchTutorialAssets(parent, { manifest = tutorial, download = downloadRelease } = {}) {
  validateTutorial(manifest);
  const destination = path.resolve(parent, manifest.releaseTag);
  const existing = await lstat(destination).catch((error) => {
    if (error.code === 'ENOENT') return null;
    throw error;
  });
  if (existing) {
    if (!existing.isDirectory() || existing.isSymbolicLink()) throw new Error('Tutorial destination must be a regular directory.');
    await verifyTutorialAssets(destination, manifest);
    return destination;
  }
  await mkdir(parent, { recursive: true });
  const temporary = await mkdtemp(path.join(path.resolve(parent), '.tutorial-download-'));
  try {
    await download(temporary, manifest);
    await verifyTutorialAssets(temporary, manifest);
    await rename(temporary, destination);
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
  return destination;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const parent = fileURLToPath(new URL('../public/media/tutorial/', import.meta.url));
  const directory = await fetchTutorialAssets(parent);
  console.log(`Verified ${Object.keys(tutorial.assets).length} tutorial assets for ${tutorial.releaseTag}: ${directory}`);
}
