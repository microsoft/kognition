// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { load } from 'cheerio';
import { site } from '../site.mjs';
import { tutorialPaths } from '../tutorial.mjs';

function htmlFiles(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const filename = path.join(directory, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`Site artifact must not contain symlinks: ${filename}`);
    return entry.isDirectory() ? htmlFiles(filename) : entry.name.endsWith('.html') ? [filename] : [];
  });
}

export function checkSite(directory, configuration = site, { allowMissingTutorialMedia = false } = {}) {
  const root = path.resolve(directory);
  const files = htmlFiles(root);
  if (files.length === 0) throw new Error(`No rendered HTML found in ${root}`);
  const documents = new Map();
  const document = (filename) => {
    if (!documents.has(filename)) documents.set(filename, load(readFileSync(filename, 'utf8')));
    return documents.get(filename);
  };
  const errors = [];
  let checked = 0;
  const missingTutorialMedia = new Set();
  const tutorialMedia = new Set(Object.values(tutorialPaths(configuration.base)));
  for (const filename of files) {
    const relative = path.relative(root, filename).split(path.sep).join('/').replace(/index\.html$/, '');
    const sourceURL = new URL(`${configuration.base}${relative}`, configuration.origin);
    const $ = document(filename);
    for (const element of $('a[href], link[href], script[src], img[src], video[src], video[poster], source[src], track[src]').toArray()) {
      if (element.tagName === 'link' && $(element).attr('rel') === 'canonical') continue;
      const href = $(element).attr('href') ?? $(element).attr('src') ?? $(element).attr('poster');
      if (!href || /^(mailto:|tel:|data:)/i.test(href)) continue;
      const targetURL = new URL(href, sourceURL);
      if (targetURL.origin !== configuration.origin) continue;
      checked++;
      const pathname = decodeURIComponent(targetURL.pathname);
      const base = configuration.base;
      if (!pathname.startsWith(base) && pathname !== base.slice(0, -1)) {
        errors.push(`${relative}: link escapes site base ${base}: ${href}`);
        continue;
      }
      const local = pathname === base.slice(0, -1) ? '' : pathname.slice(base.length);
      let target = path.resolve(root, local);
      const withinRoot = path.relative(root, target);
      if (withinRoot === '..' || withinRoot.startsWith(`..${path.sep}`) || path.isAbsolute(withinRoot)) {
        errors.push(`${relative}: link escapes artifact: ${href}`);
        continue;
      }
      if (existsSync(target) && statSync(target).isDirectory()) target = path.join(target, 'index.html');
      if (!existsSync(target)) {
        if (allowMissingTutorialMedia && tutorialMedia.has(pathname)) {
          missingTutorialMedia.add(pathname);
          continue;
        }
        errors.push(`${relative}: missing target: ${href}`);
        continue;
      }
      if (targetURL.hash && target.endsWith('.html')) {
        const anchor = decodeURIComponent(targetURL.hash.slice(1));
        if (!anchor) continue;
        const targetDocument = document(target);
        const anchors = targetDocument('[id], a[name]').toArray().map((node) =>
          targetDocument(node).attr('id') ?? targetDocument(node).attr('name'));
        if (!anchors.includes(anchor)) errors.push(`${relative}: missing anchor: ${href}`);
      }
    }
  }
  if (errors.length) throw new Error(`Documentation link validation failed:\n${errors.join('\n')}`);
  return { pages: files.length, links: checked, missingTutorialMedia: missingTutorialMedia.size };
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const args = process.argv.slice(2);
  if (args.some((arg) => arg !== '--without-tutorial-media')) throw new Error('Unsupported link-check option.');
  const result = checkSite(fileURLToPath(new URL('../dist/', import.meta.url)), site,
    { allowMissingTutorialMedia: args.includes('--without-tutorial-media') });
  console.log(`Validated ${result.pages} HTML pages and ${result.links} internal links/assets.`);
  if (args.length) console.log(`Non-publishing validation: ${result.missingTutorialMedia} pinned tutorial media targets not installed. Publication requires media verification.`);
}
