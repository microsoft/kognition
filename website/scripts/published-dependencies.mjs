// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { load } from 'cheerio';
import { tutorial } from '../tutorial.mjs';

const hash = (content) => createHash('sha256').update(content).digest('hex');
const slash = (value) => value.replaceAll('\\', '/');
const executable = /\.(?:[cm]?js|css|wasm|node|exe|dll|so|a)$/i;
const searchCode = new Set([
  'pagefind.js', 'pagefind-worker.js', 'pagefind-highlight.js',
  'pagefind-ui.js', 'pagefind-ui.css', 'pagefind-modular-ui.js', 'pagefind-modular-ui.css',
  'pagefind-component-ui.js', 'pagefind-component-ui.css',
]);
const generatedNotices = new Set(['third-party/NOTICE', 'third-party/inventory.json']);

function filesIn(directory) {
  if (!existsSync(directory)) return [];
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const file = path.join(directory, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`Static publication cannot contain symlinks: ${entry.name}`);
    return entry.isDirectory() ? filesIn(file) : [file];
  });
}

export function sourcePackages(moduleIDs, projectRoot, lock) {
  const root = `${slash(path.resolve(projectRoot))}/`;
  const paths = Object.keys(lock.packages).filter(Boolean).sort((a, b) => b.length - a.length);
  const selected = new Set();
  for (const id of moduleIDs) {
    const moduleID = slash(id);
    if (!moduleID.includes('/node_modules/')) continue;
    const index = moduleID.indexOf(`${root}node_modules/`);
    if (index < 0) throw new Error(`Bundled package is outside the pinned npm project: ${id}`);
    const relative = moduleID.slice(index + root.length).split('?')[0];
    const source = paths.find((candidate) => relative === candidate || relative.startsWith(`${candidate}/`));
    if (!source) throw new Error(`Bundled module has no exact lockfile entry: ${relative}`);
    selected.add(source);
  }
  return [...selected].sort();
}

export function createPublishedManifest({ projectRoot, directory, outputs, clientModules, styleModules }) {
  const lockfile = readFileSync(path.join(projectRoot, 'package-lock.json'));
  const packageJSON = readFileSync(path.join(projectRoot, 'package.json'));
  const lock = JSON.parse(lockfile);
  const requirePackage = (name) => {
    const relative = `node_modules/${name}`;
    if (!lock.packages[relative] || !existsSync(path.join(projectRoot, relative, 'package.json'))) {
      throw new Error(`Published runtime source package is unavailable: ${name}`);
    }
    return relative;
  };
  const renderer = ['astro', '@astrojs/starlight'].map(requirePackage);
  const runtimeHelpers = [requirePackage('vite'), ...Object.keys(lock.packages).filter((relative) =>
    /(?:^|\/)node_modules\/rolldown$/.test(relative) &&
      existsSync(path.join(projectRoot, relative, 'package.json')))];
  if (runtimeHelpers.length < 2) throw new Error('Bundler runtime-helper provenance is unavailable.');
  const expressive = Object.keys(lock.packages).filter((relative) =>
    /^node_modules\/(?:@expressive-code\/[^/]+|(?:astro-|rehype-)?expressive-code|@shikijs\/themes)$/.test(relative));
  if (!expressive.length) throw new Error('Expressive Code asset provenance is unavailable.');
  const search = [requirePackage('pagefind'), ...Object.keys(lock.packages).filter((relative) =>
    /^node_modules\/@pagefind\/(?!default-ui$)[^/]+$/.test(relative) &&
      existsSync(path.join(projectRoot, relative, 'package.json')))];
  if (search.length < 2) throw new Error('Pagefind runtime generator package is unavailable.');
  const client = sourcePackages(clientModules, projectRoot, lock);
  const styles = sourcePackages(styleModules, projectRoot, lock);
  const tutorialFiles = new Map(Object.values(tutorial.assets).map((asset) =>
    [`media/tutorial/${tutorial.releaseTag}/${asset.name}`, asset]));
  const publicRoot = path.join(projectRoot, 'public');
  for (const file of filesIn(publicRoot)) {
    const relative = slash(path.relative(publicRoot, file));
    if (executable.test(relative) || /\.html$/i.test(relative) || relative.startsWith('pagefind/')) {
      throw new Error(`Copied executable content has no bundler provenance: public/${relative}`);
    }
  }
  const packages = new Set();
  const files = [];
  for (const file of filesIn(directory).sort()) {
    const relative = slash(path.relative(directory, file));
    if (generatedNotices.has(relative)) continue;
    const content = readFileSync(file);
    if (/\.svg$/i.test(relative) && /<script\b|\bon[a-z]+\s*=|javascript:/i.test(content.toString('utf8'))) {
      throw new Error(`Active SVG content has no executable source provenance: ${relative}`);
    }
    let origin, sources;
    const output = outputs.get(relative);
    const media = tutorialFiles.get(relative);
    if (/\.html$/i.test(relative)) {
      origin = 'rendered-page';
      sources = [...renderer, ...expressive];
      const $ = load(content.toString('utf8'));
      for (const element of $('script[src], link[rel=stylesheet][href]').toArray()) {
        const target = $(element).attr('src') ?? $(element).attr('href');
        if (target && /^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(target)) {
          throw new Error(`External executable content is outside static publication evidence: ${relative}`);
        }
      }
    } else if (output?.kind === 'chunk') {
      origin = 'vite-client';
      sources = [...client, ...runtimeHelpers, ...sourcePackages(output.modules, projectRoot, lock)];
    } else if (output && /^_astro\/ec\.[a-z0-9_-]+\.(?:js|css)$/i.test(relative)) {
      origin = 'expressive-code';
      sources = expressive;
    } else if (output && /\.css$/i.test(relative)) {
      origin = 'rendered-style';
      sources = [...renderer, ...styles];
    } else if (relative.startsWith('pagefind/')) {
      const name = relative.slice('pagefind/'.length);
      if (executable.test(name) && !searchCode.has(name)) {
        throw new Error(`Unknown executable Pagefind output: ${relative}`);
      }
      if (!searchCode.has(name) && !/^(?:pagefind-entry\.json|wasm\.[\w-]+\.pagefind|[\w./-]+\.pf_(?:meta|index|fragment))$/.test(name)) {
        throw new Error(`Unknown generated search asset: ${relative}`);
      }
      origin = 'pagefind';
      sources = search;
    } else if (media) {
      if (content.length !== media.bytes || hash(content) !== media.sha256) {
        throw new Error(`Published tutorial asset differs from the approved Release: ${relative}`);
      }
      origin = 'verified-tutorial';
      sources = [];
    } else if (executable.test(relative)) {
      throw new Error(`Executable output has no bundled or generated provenance: ${relative}`);
    } else if (!/\.(?:svg|xml|png|jpe?g|gif|webp|ico|woff2?|ttf|txt)$/i.test(relative)) {
      throw new Error(`Unaccounted static publication asset: ${relative}`);
    } else {
      origin = 'first-party-static';
      sources = [];
    }
    if (content.subarray(0, 2).toString() === 'MZ' || content.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46]))) {
      throw new Error(`Native build binary cannot be published with static Pages: ${relative}`);
    }
    const attributed = [...new Set(sources)].sort();
    attributed.forEach((source) => packages.add(source));
    files.push({ path: relative, sha256: hash(content), packages: attributed, origin });
  }
  if (!clientModules.length || !files.some((file) => file.origin === 'vite-client') ||
      !files.some((file) => file.origin === 'pagefind')) {
    throw new Error('Static publication evidence is incomplete: client or search build is missing.');
  }
  for (const source of packages) {
    const installed = JSON.parse(readFileSync(path.join(projectRoot, source, 'package.json'), 'utf8'));
    if (installed.version !== lock.packages[source]?.version) {
      throw new Error(`Published source package version differs from the lockfile: ${source}`);
    }
  }
  return {
    schemaVersion: 1, kind: 'kognition-static-pages-npm',
    lockfileSHA256: hash(lockfile), packageJsonSHA256: hash(packageJSON),
    packages: [...packages].sort(), files,
  };
}

export default function publishedDependencies() {
  const outputs = new Map();
  const clientModules = new Set();
  const styleModules = new Set();
  let projectRoot;
  return {
    name: 'kognition-published-dependencies',
    hooks: {
      'astro:config:done': ({ config }) => {
        if (config.output !== 'static') throw new Error('Published dependency evidence supports static output only.');
        projectRoot = fileURLToPath(config.root);
      },
      'astro:build:setup': ({ updateConfig }) => {
        updateConfig({ plugins: [{
          name: 'kognition-published-dependencies',
          generateBundle(_options, bundle) {
            const ids = [...this.getModuleIds()];
            const client = this.environment.name === 'client';
            if (client) ids.forEach((id) => clientModules.add(id));
            ids.filter((id) => /\.css(?:\?|$)|[?&]type=style(?:&|$)/.test(id))
              .forEach((id) => styleModules.add(id));
            for (const output of Object.values(bundle)) {
              if (!client && !/\.css$/i.test(output.fileName)) continue;
              outputs.set(output.fileName, { kind: output.type,
                modules: output.type === 'chunk' ? Object.keys(output.modules) : [] });
            }
          },
        }] });
      },
      'astro:build:done': ({ dir, logger }) => {
        const evidence = createPublishedManifest({
          projectRoot, directory: fileURLToPath(dir), outputs,
          clientModules: [...clientModules], styleModules: [...styleModules],
        });
        const filename = path.join(projectRoot, '.astro', 'published-files.json');
        mkdirSync(path.dirname(filename), { recursive: true });
        writeFileSync(filename, `${JSON.stringify(evidence, null, 2)}\n`);
        logger.info(`Recorded ${evidence.files.length} published files and ${evidence.packages.length} source packages for strict notices.`);
      },
    },
  };
}
