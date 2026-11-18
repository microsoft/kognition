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
const dataScriptTypes = new Set(['application/json', 'application/ld+json', 'importmap', 'speculationrules']);
// Script bodies are compared byte for byte; only line endings and the outer whitespace may differ.
const scriptBody = (value) => value.replace(/\r\n?/g, '\n').trim();
// Browsers strip surrounding whitespace and embedded tabs/newlines, and treat
// backslashes as slashes in http(s) URLs, so normalize before looking for a scheme or network path.
const isExternalReference = (value) => {
  const url = value.replace(/^[\u0000-\u0020]+|[\u0000-\u0020]+$/g, '').replace(/[\t\n\r]/g, '').replaceAll('\\', '/');
  return /^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(url);
};

const unescapeCSS = (value) => value
  .replace(/\\([0-9a-f]{1,6})[ \t\n\r\f]?/gi, (_, hex) => {
    const code = Number.parseInt(hex, 16);
    return code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : '\ufffd';
  })
  .replace(/\\(.)/gs, '$1');

// CSS image functions that accept a bare string as a URL, not only url().
const imageFunctions = new Set(['image', 'image-set', '-webkit-image-set', '-webkit-image', 'cross-fade', '-webkit-cross-fade', 'src']);

// Returns the first external target of an @import, url() or image-function string in a stylesheet, skipping comments.
export function externalCSSReference(css) {
  const ident = /[a-z0-9_-]|\\(?:[0-9a-f]{1,6}[ \t\n\r\f]?|[^\n])|[^\u0000-\u007f]/iy;
  const quoted = (start) => {
    const quote = css[start];
    let end = start + 1;
    while (end < css.length && css[end] !== quote) end += css[end] === '\\' ? 2 : 1;
    return { value: unescapeCSS(css.slice(start + 1, end)), end: end + 1 };
  };
  const external = (target) => {
    const url = target.replace(/^[\u0000-\u0020]+|[\u0000-\u0020]+$/g, '').replace(/[\t\n\r]/g, '');
    return isExternalReference(url) ? url : undefined;
  };
  let importing = false;
  let depth = 0;
  const imageDepths = [];
  for (let i = 0; i < css.length;) {
    const char = css[i];
    if (css.startsWith('/*', i)) {
      const end = css.indexOf('*/', i + 2);
      i = end < 0 ? css.length : end + 2;
    } else if (char === '"' || char === "'") {
      const { value, end } = quoted(i);
      const target = external(value);
      if (target && (importing || (imageDepths.length && !/^data:/i.test(target)))) return value;
      importing = false;
      i = end;
    } else if (char === '@') {
      let end = i + 1;
      for (;;) {
        ident.lastIndex = end;
        const match = ident.exec(css);
        if (!match) break;
        end = ident.lastIndex;
      }
      importing = unescapeCSS(css.slice(i + 1, end)).toLowerCase() === 'import';
      i = Math.max(end, i + 1);
    } else if (/[a-z\\]/i.test(char) || char === '-') {
      let end = i;
      for (;;) {
        ident.lastIndex = end;
        if (!ident.exec(css)) break;
        end = ident.lastIndex;
      }
      end = Math.max(end, i + 1);
      if (css[end] === '(' && imageFunctions.has(unescapeCSS(css.slice(i, end)).toLowerCase())) {
        imageDepths.push(depth + 1);
      }
      if (css[end] === '(' && unescapeCSS(css.slice(i, end)).toLowerCase() === 'url') {
        let start = end + 1;
        while (/\s/.test(css[start] ?? '')) start++;
        let value;
        if (css[start] === '"' || css[start] === "'") {
          ({ value, end } = quoted(start));
          const close = css.indexOf(')', end);
          end = close < 0 ? css.length : close + 1;
        } else {
          const close = css.indexOf(')', start);
          value = unescapeCSS(css.slice(start, close < 0 ? css.length : close));
          end = close < 0 ? css.length : close + 1;
        }
        const target = external(value);
        if (target && (importing || !/^data:/i.test(target))) return target;
        importing = false;
      }
      i = end;
    } else {
      if (char === ';' || char === '{') importing = false;
      if (char === '(') depth++;
      if (char === ')') {
        if (imageDepths.at(-1) === depth) imageDepths.pop();
        depth = Math.max(0, depth - 1);
      }
      i++;
    }
  }
  return undefined;
}

const loadingRels = new Set(['stylesheet', 'modulepreload', 'preload', 'prefetch', 'prerender']);
// Elements whose attributes load a document, script or image from the given URL.
const svgReference = ['href', 'xlink:href'];
const loadingAttributes = {
  script: ['src', ...svgReference], iframe: ['src'], frame: ['src'], embed: ['src'], object: ['data'], base: ['href'],
  image: svgReference, use: svgReference, feimage: svgReference, lineargradient: svgReference, radialgradient: svgReference,
  pattern: svgReference, filter: svgReference, textpath: svgReference, mpath: svgReference, tref: svgReference,
};
const cssAttributes = new Set(['fill', 'stroke', 'filter', 'mask', 'clip-path', 'marker-start', 'marker-mid', 'marker-end', 'cursor', 'style']);

const svgNamespace = 'http://www.w3.org/2000/svg';
const xhtmlNamespace = 'http://www.w3.org/1999/xhtml';
const xlinkNamespace = 'http://www.w3.org/1999/xlink';
const bindingsByElement = new WeakMap();

// xmlns bindings in scope for an element, inherited from its ancestors. The xlink prefix is bound by default
// because HTML parsing never declares it.
function namespaceBindings(element) {
  if (!element) return { xlink: xlinkNamespace };
  const cached = bindingsByElement.get(element);
  if (cached) return cached;
  const bindings = { ...namespaceBindings(element.parent) };
  for (const [key, value] of Object.entries(element.attribs ?? {})) {
    if (key === 'xmlns') bindings[''] = value;
    else if (key.startsWith('xmlns:')) bindings[key.slice('xmlns:'.length)] = value;
  }
  bindingsByElement.set(element, bindings);
  return bindings;
}

// Element name without a prefix bound to SVG or XHTML, so <s:image> is matched as <image>.
function localName(element) {
  const name = element.name ?? '';
  const split = name.indexOf(':');
  if (split > 0 && [svgNamespace, xhtmlNamespace].includes(namespaceBindings(element)[name.slice(0, split)])) {
    return name.slice(split + 1).toLowerCase();
  }
  return name.toLowerCase();
}

// Attribute name with its prefix resolved: any prefix bound to XLink becomes xlink:, one bound to SVG or XHTML is dropped.
function resolveAttributeName(element, key) {
  const split = key.indexOf(':');
  const namespace = split > 0 ? namespaceBindings(element)[key.slice(0, split)] : undefined;
  const local = key.slice(split + 1).toLowerCase();
  if (namespace === xlinkNamespace) return `xlink:${local}`;
  return [svgNamespace, xhtmlNamespace].includes(namespace) ? local : key.toLowerCase();
}

const attributeEntries = (element) => Object.entries(element.attribs ?? {})
  .map(([key, value]) => [resolveAttributeName(element, key), value]);

const styleSheets = ($) => $('*').toArray().filter((element) => localName(element) === 'style')
  .map((element) => $(element).text());

const xmlReferences = { lt: '<', gt: '>', amp: '&', quot: '"', apos: "'" };
const decodeXMLReferences = (value) => value.replace(/&(?:#x([0-9a-f]+)|#(\d+)|(lt|gt|amp|quot|apos));/gi, (match, hex, decimal, named) => {
  if (named) return xmlReferences[named.toLowerCase()];
  const code = Number.parseInt(hex ?? decimal, hex ? 16 : 10);
  return code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : match;
});

// href values of every <?xml-stylesheet?> instruction. Markup is tokenized in document order so comments, CDATA
// sections and other instructions are skipped as units, not scanned for look-alike text. The instruction data
// is split into quoted pseudoattributes, so href text inside another value, or a longer name such as data-href,
// is never mistaken for the real one.
function xmlStylesheetTargets(text) {
  const targets = [];
  const token = /<!--[\s\S]*?(?:-->|$)|<!\[CDATA\[[\s\S]*?(?:\]\]>|$)|<\?([^\s?]+)([\s\S]*?)(?:\?>|$)/g;
  for (const [, target, data] of text.matchAll(token)) {
    if (target?.toLowerCase() !== 'xml-stylesheet') continue;
    for (const [, name, double, single] of data.matchAll(/([^\s=]+)\s*=\s*(?:"([^"]*)"|'([^']*)')/g)) {
      if (name === 'href') targets.push(decodeXMLReferences(double ?? single));
    }
  }
  return targets;
}

// Rejects markup that pulls in external or unattributed content: loaders, <base>, srcdoc and CSS in attributes.
function assertNoExternalContent($, relative) {
  for (const element of $('*').toArray()) {
    const name = localName(element);
    const attributes = attributeEntries(element);
    const values = (wanted) => attributes.filter(([key]) => key === wanted).map(([, value]) => value);
    if (name === 'iframe' && values('srcdoc').length) {
      throw new Error(`Inline iframe document has no source-package evidence: ${relative}`);
    }
    // xml:base re-roots every relative URL below it, so an external one makes any local-looking loader external.
    if (values('xml:base').some(isExternalReference)) {
      throw new Error(`External executable content is outside static publication evidence: ${relative}`);
    }
    if (['set', 'animate'].includes(name)) {
      const assigned = resolveAttributeName(element, (values('attributename')[0] ?? '').trim());
      const animated = ['to', 'from', 'by', 'values'].flatMap((key) => values(key).flatMap((value) => [value, ...value.split(';')]));
      if (['xml:base', 'href', 'xlink:href'].includes(assigned)) {
        if (animated.some(isExternalReference)) {
          throw new Error(`External executable content is outside static publication evidence: ${relative}`);
        }
      } else if (cssAttributes.has(assigned) && animated.some(externalCSSReference)) {
        throw new Error(`External stylesheet content is outside static publication evidence: ${relative}`);
      }
    }
    const rels = values('rel').flatMap((rel) => rel.toLowerCase().split(/\s+/));
    const loads = name === 'link' && rels.some((rel) => loadingRels.has(rel)) ? ['href']
      : (Object.hasOwn(loadingAttributes, name) ? loadingAttributes[name] : []);
    for (const wanted of loads) {
      for (const target of values(wanted)) {
        const inlineImage = ['image', 'feimage'].includes(name) && /^[\u0000-\u0020]*data:/i.test(target);
        if (!inlineImage && isExternalReference(target)) {
          throw new Error(`External executable content is outside static publication evidence: ${relative}`);
        }
      }
    }
    for (const [key, value] of attributes) {
      if (cssAttributes.has(key) && externalCSSReference(value)) {
        throw new Error(`External stylesheet content is outside static publication evidence: ${relative}`);
      }
    }
  }
}

function astroFiles(directory) {
  if (!existsSync(directory)) return [];
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const file = path.join(directory, entry.name);
    if (entry.isSymbolicLink() || entry.name === 'node_modules') return [];
    if (entry.isDirectory()) return astroFiles(file);
    return entry.name.endsWith('.astro') ? [file] : [];
  });
}

// Bodies of <script> blocks authored in a package's .astro sources.
function packageScriptBodies(directory) {
  const bodies = new Set();
  for (const file of astroFiles(directory)) {
    for (const match of readFileSync(file, 'utf8').matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)) {
      bodies.add(scriptBody(match[1]));
    }
  }
  return bodies;
}

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
    if (relative.slice(source.length + 1).includes('node_modules/')) {
      throw new Error(`Bundled module crosses an unlisted node_modules boundary: ${relative}`);
    }
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
  const chunkModules = new Map();
  for (const output of outputs.values()) {
    if (output.kind === 'chunk' && output.codeSHA256) chunkModules.set(output.codeSHA256, output.modules);
  }
  let providerScripts;
  // An inline executable block must be a bundled chunk or a script authored by a renderer package.
  const inlineScriptSources = (body, relative) => {
    const exact = scriptBody(body);
    const chunk = chunkModules.get(hash(body.trim()));
    if (chunk) return sourcePackages(chunk, projectRoot, lock);
    providerScripts ??= [...renderer, ...expressive].map((source) =>
      ({ source, bodies: packageScriptBodies(path.join(projectRoot, source)) }));
    const sources = providerScripts.filter((provider) => provider.bodies.has(exact)).map((provider) => provider.source);
    if (!sources.length) {
      throw new Error(`Inline executable script has no source-package evidence: ${relative}: ${exact.slice(0, 80)}`);
    }
    return sources;
  };
  for (const file of filesIn(directory).sort()) {
    const relative = slash(path.relative(directory, file));
    if (generatedNotices.has(relative)) continue;
    const content = readFileSync(file);
    if (/\.svg$/i.test(relative)) {
      const text = content.toString('utf8');
      if (/<(?:[\w.-]+:)?script\b|\bon[a-z]+\s*=|javascript:/i.test(text)) {
        throw new Error(`Active SVG content has no executable source provenance: ${relative}`);
      }
      const $svg = load(text, { xmlMode: true });
      for (const href of xmlStylesheetTargets(text)) {
        if (isExternalReference(href)) {
          throw new Error(`External stylesheet content is outside static publication evidence: ${relative}`);
        }
      }
      assertNoExternalContent($svg, relative);
      if (styleSheets($svg).some((css) => externalCSSReference(css))) {
        throw new Error(`External stylesheet content is outside static publication evidence: ${relative}`);
      }
    }
    if (/\.css$/i.test(relative) && externalCSSReference(content.toString('utf8'))) {
      throw new Error(`External stylesheet content is outside static publication evidence: ${relative}`);
    }
    let origin, sources;
    const output = outputs.get(relative);
    const media = tutorialFiles.get(relative);
    if (/\.html$/i.test(relative)) {
      origin = 'rendered-page';
      sources = [...renderer, ...expressive];
      // Browsers parse <noscript> as markup or as raw text depending on scripting, so each parse must pass on its own.
      const html = content.toString('utf8');
      for (const scriptingEnabled of [true, false]) {
        const $ = load(html, { scriptingEnabled });
        for (const element of $('script:not([src])').toArray()) {
          const type = ($(element).attr('type') ?? '').trim().toLowerCase();
          if (dataScriptTypes.has(type)) continue;
          const body = $(element).text();
          if (body.trim()) sources.push(...inlineScriptSources(body, relative));
        }
        assertNoExternalContent($, relative);
        for (const element of $('*').toArray()) {
          for (const [name, value] of Object.entries(element.attribs ?? {})) {
            if (/^on/i.test(name) || /^[\u0000-\u0020]*javascript:/i.test(value.replace(/[\t\n\r]/g, ''))) {
              throw new Error(`Executable HTML attribute has no source-package evidence: ${relative}: <${element.name} ${name}>`);
            }
          }
        }
        const inlineStyles = [...styleSheets($), ...$('[style]').toArray().map((element) => $(element).attr('style'))];
        if (inlineStyles.some((css) => externalCSSReference(css))) {
          throw new Error(`External stylesheet content is outside static publication evidence: ${relative}`);
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
                modules: output.type === 'chunk' ? Object.keys(output.modules) : [],
                codeSHA256: output.type === 'chunk' ? hash(output.code.trim()) : undefined });
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
