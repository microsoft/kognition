// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { createPublishedManifest, sourcePackages } from '../scripts/published-dependencies.mjs';

function fixture(t) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'kognition-published-dependencies-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const packages = { '': { name: 'fixture' } };
  for (const name of [
    'astro', '@astrojs/starlight', '@pagefind/default-ui', '@pagefind/linux-x64',
    'pagefind', 'vite', 'rolldown', 'astro-expressive-code', '@expressive-code/core', 'am-i-vibing',
    'piccolore', 'process-ancestry', '@img/sharp-libvips-linux-x64',
  ]) {
    const relative = `node_modules/${name}`;
    packages[relative] = { version: '1.0.0', license: 'MIT' };
    mkdirSync(path.join(root, relative), { recursive: true });
    writeFileSync(path.join(root, relative, 'package.json'), JSON.stringify({ name, version: '1.0.0' }));
  }
  writeFileSync(path.join(root, 'package.json'), '{"name":"fixture"}');
  writeFileSync(path.join(root, 'package-lock.json'), JSON.stringify({ lockfileVersion: 3, packages }));
  const directory = path.join(root, 'dist');
  mkdirSync(path.join(directory, '_astro'), { recursive: true });
  mkdirSync(path.join(directory, 'pagefind'), { recursive: true });
  writeFileSync(path.join(directory, 'index.html'), '<script src="/_astro/app.js"></script>');
  writeFileSync(path.join(directory, '_astro', 'app.js'), 'console.log("app");');
  writeFileSync(path.join(directory, '_astro', 'ec.abc.js'), 'console.log("code copy");');
  writeFileSync(path.join(directory, '_astro', 'common.css'), 'body{color:black}');
  writeFileSync(path.join(directory, 'pagefind', 'pagefind.js'), 'console.log("search");');
  writeFileSync(path.join(directory, 'pagefind', 'pagefind.en_abc123.pf_meta'), 'index metadata');
  const clientModules = [path.join(root, 'node_modules', '@pagefind', 'default-ui', 'ui.js')];
  return {
    root, directory, lock: { packages }, inputs: {
      projectRoot: root, directory,
      outputs: new Map([
        ['_astro/app.js', { kind: 'chunk', modules: clientModules }],
        ['_astro/ec.abc.js', { kind: 'asset', modules: [] }],
        ['_astro/common.css', { kind: 'asset', modules: [] }],
      ]),
      clientModules,
      styleModules: [path.join(root, 'node_modules', '@astrojs', 'starlight', 'style.css')],
    },
  };
}

test('publication evidence accounts for bundled code, renderer styles, generated search and inline-page providers', (t) => {
  const { root, directory, inputs } = fixture(t);
  const evidence = createPublishedManifest(inputs);
  assert.equal(evidence.kind, 'kognition-static-pages-npm');
  assert.equal(evidence.files.length, 6);
  for (const entry of evidence.files) {
    assert.equal(entry.sha256, createHash('sha256').update(readFileSync(path.join(directory, entry.path))).digest('hex'));
    assert(!entry.path.includes(root), 'Evidence must not expose local absolute paths.');
  }
  assert(evidence.packages.includes('node_modules/@pagefind/default-ui'));
  assert(evidence.packages.includes('node_modules/@pagefind/linux-x64'));
  assert(evidence.packages.includes('node_modules/@expressive-code/core'));
  assert(evidence.packages.includes('node_modules/astro'));
  assert(evidence.packages.includes('node_modules/vite'));
  assert(evidence.packages.includes('node_modules/rolldown'));
  for (const name of ['am-i-vibing', 'piccolore', 'process-ancestry', '@img/sharp-libvips-linux-x64']) {
    assert(!evidence.packages.includes(`node_modules/${name}`), 'Build-only packages are not declared shipped.');
  }
});

test('a previously build-only package becomes subject to publication notices when it enters the client graph', (t) => {
  const { root, inputs } = fixture(t);
  inputs.clientModules.push(path.join(root, 'node_modules', 'am-i-vibing', 'index.js'));
  assert(createPublishedManifest(inputs).packages.includes('node_modules/am-i-vibing'));
});

test('nested and query-bearing bundled modules resolve to their exact lockfile package', (t) => {
  const { root, lock } = fixture(t);
  lock.packages['node_modules/outer/node_modules/inner'] = { version: '2.0.0' };
  const modules = [path.join(root, 'node_modules', 'outer', 'node_modules', 'inner', 'index.js') + '?commonjs-proxy'];
  assert.deepEqual(sourcePackages(modules, root, lock), ['node_modules/outer/node_modules/inner']);
  assert.throws(() => sourcePackages([path.join(root, 'node_modules', 'unknown', 'index.js')], root, lock), /no exact lockfile entry/);
  assert.throws(() => sourcePackages(['/outside/node_modules/unknown/index.js'], root, lock), /outside/);
});

test('a bundled module under an unlisted nested node_modules directory is rejected, not attributed to its parent', (t) => {
  const { root, lock } = fixture(t);
  lock.packages['node_modules/outer'] = { version: '1.0.0' };
  const nested = path.join(root, 'node_modules', 'outer', 'node_modules', 'inner', 'index.js');
  assert.throws(() => sourcePackages([nested], root, lock), /unlisted node_modules boundary/);
  assert.deepEqual(sourcePackages([path.join(root, 'node_modules', 'outer', 'lib', 'index.js')], root, lock), ['node_modules/outer']);
  lock.packages['node_modules/outer/node_modules/inner'] = { version: '2.0.0' };
  assert.deepEqual(sourcePackages([nested], root, lock), ['node_modules/outer/node_modules/inner']);
});

test('inline executable scripts need chunk or renderer-package evidence and are attributed to their source', (t) => {
  const { root, directory, inputs } = fixture(t);
  const authored = path.join(root, 'node_modules', '@astrojs', 'starlight', 'components');
  mkdirSync(authored, { recursive: true });
  writeFileSync(path.join(authored, 'Theme.astro'), '---\n---\n<script is:inline>\n  window.theme = () => 1;\n</script>');
  const chunkCode = 'var a=1;console.log(a);';
  inputs.outputs.set('_astro/inline.js', {
    kind: 'chunk', modules: [path.join(root, 'node_modules', 'am-i-vibing', 'index.js')],
    codeSHA256: createHash('sha256').update(chunkCode).digest('hex'),
  });
  writeFileSync(path.join(directory, 'index.html'), [
    '<script src="/_astro/app.js"></script>',
    '<script>window.theme   =  () => 1;</script>',
    `<script type="module">${chunkCode}\n</script>`,
    '<script type="application/ld+json">{"@type":"WebSite"}</script>',
  ].join(''));
  const evidence = createPublishedManifest(inputs);
  const page = evidence.files.find((file) => file.path === 'index.html');
  assert(page.packages.includes('node_modules/@astrojs/starlight'));
  assert(page.packages.includes('node_modules/am-i-vibing'), 'Inline chunk source packages are attributed to the page.');
  for (const body of ['fetch("https://example.test/x")', 'window.theme = () => 2;']) {
    writeFileSync(path.join(directory, 'index.html'), `<script>${body}</script>`);
    assert.throws(() => createPublishedManifest(inputs), /Inline executable script has no source-package evidence/, body);
  }
});

test('copied scripts, unaccounted executable output and unknown generated search code block the build', (t) => {
  for (const mode of ['public-script', 'unaccounted-code', 'search-code', 'native', 'active-svg', 'external-code']) {
    const { root, directory, inputs } = fixture(t);
    if (mode === 'public-script') {
      mkdirSync(path.join(root, 'public'));
      writeFileSync(path.join(root, 'public', 'copied.js'), 'unknown code');
    } else if (mode === 'unaccounted-code') {
      writeFileSync(path.join(directory, 'copied.js'), 'unknown code');
    } else if (mode === 'search-code') {
      writeFileSync(path.join(directory, 'pagefind', 'unexpected.js'), 'unknown code');
    } else if (mode === 'native') {
      writeFileSync(path.join(directory, '_astro', 'app.js'), Buffer.from([0x7f, 0x45, 0x4c, 0x46]));
    } else if (mode === 'active-svg') {
      writeFileSync(path.join(directory, 'active.svg'), '<svg><script>alert(1)</script></svg>');
    } else if (mode === 'external-code') {
      writeFileSync(path.join(directory, 'index.html'), '<script src="https://example.test/unaccounted.js"></script>');
    }
    assert.throws(() => createPublishedManifest(inputs), /provenance|Unknown executable|Native|External executable/i, mode);
  }
});

test('missing generator evidence or changed installed package versions never produce publishable evidence', (t) => {
  const { root, inputs } = fixture(t);
  assert.throws(() => createPublishedManifest({ ...inputs, clientModules: [] }), /incomplete/);
  writeFileSync(path.join(root, 'node_modules', 'pagefind', 'package.json'), '{"name":"pagefind","version":"2.0.0"}');
  assert.throws(() => createPublishedManifest(inputs), /version differs/);
});
