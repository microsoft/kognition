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
    '<script>window.theme = () => 1;</script>',
    `<script type="module">${chunkCode}\n</script>`,
    '<script type="application/ld+json">{"@type":"WebSite"}</script>',
  ].join(''));
  const evidence = createPublishedManifest(inputs);
  const page = evidence.files.find((file) => file.path === 'index.html');
  assert(page.packages.includes('node_modules/@astrojs/starlight'));
  assert(page.packages.includes('node_modules/am-i-vibing'), 'Inline chunk source packages are attributed to the page.');
  for (const body of [
    'fetch("https://example.test/x")', 'window.theme = () => 2;',
    'window.theme   =  () => 1;', 'window.theme = () =>\n1;',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), `<script>${body}</script>`);
    assert.throws(() => createPublishedManifest(inputs), /Inline executable script has no source-package evidence/, body);
  }
});

test('inline code that differs from provider code only inside a string literal is not attributed to the provider', (t) => {
  const { root, directory, inputs } = fixture(t);
  const authored = path.join(root, 'node_modules', '@astrojs', 'starlight', 'components');
  mkdirSync(authored, { recursive: true });
  writeFileSync(path.join(authored, 'Beacon.astro'), '<script is:inline>\nsend("a b", `x  y`); // note one\n</script>');
  writeFileSync(path.join(directory, 'index.html'), '<script>send("a b", `x  y`); // note one</script>');
  createPublishedManifest(inputs);
  for (const body of ['send("a  b", `x  y`); // note one', 'send("a b", `x y`); // note one', 'send("a b", `x  y`); // note  one']) {
    writeFileSync(path.join(directory, 'index.html'), `<script>${body}</script>`);
    assert.throws(() => createPublishedManifest(inputs), /no source-package evidence/, body);
  }
});

test('executable HTML attributes are rejected because they have no source-package evidence', (t) => {
  const { directory, inputs } = fixture(t);
  const base = '<script src="/_astro/app.js"></script>';
  for (const html of [
    '<img src="/x.png" onerror="fetch(1)">', '<button ONCLICK="go()">x</button>',
    '<a href="javascript:go()">x</a>', '<a href="  javascript:go()">x</a>',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), base + html);
    assert.throws(() => createPublishedManifest(inputs), /Executable HTML attribute/, html);
  }
  writeFileSync(path.join(directory, 'index.html'), `${base}<a href="/guide/" class="once" data-online="1">ok</a>`);
  createPublishedManifest(inputs);
});

test('external stylesheet imports and url() targets are rejected in emitted CSS and inline styles', (t) => {
  const { directory, inputs } = fixture(t);
  const css = path.join(directory, '_astro', 'common.css');
  for (const body of [
    '@import "https://cdn.example.test/unaccounted.css";',
    "@import 'https://cdn.example.test/unaccounted.css';",
    '@import url(https://cdn.example.test/unaccounted.css);',
    '@import url( "//cdn.example.test/unaccounted.css" ) screen;',
    '@IMPORT "\\68ttps://cdn.example.test/unaccounted.css";',
    '/* note */ body{color:red} @import "https://cdn.example.test/a.css";',
    'a{background:url(https://cdn.example.test/a.png)}',
    'a{background:url("//cdn.example.test/a.png")}',
    '@media print{@import "https://cdn.example.test/a.css";}',
  ]) {
    writeFileSync(css, body);
    assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/, body);
  }
  for (const body of [
    '/* @import "https://cdn.example.test/a.css"; */ body{color:red}',
    'a::after{content:"@import url(https://cdn.example.test/a.css)"}',
    '@import "./local.css"; a{background:url(/img/a.png)}',
    'a{background:url(data:image/svg+xml;base64,AAAA)}',
  ]) {
    writeFileSync(css, body);
    createPublishedManifest(inputs);
  }
  writeFileSync(css, 'body{color:black}');
  for (const html of [
    '<style>@import "https://cdn.example.test/a.css";</style>',
    '<div style="background:url(https://cdn.example.test/a.png)"></div>',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), `<script src="/_astro/app.js"></script>${html}`);
    assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/, html);
  }
});

test('external script and stylesheet references are rejected however the URL is spelled', (t) => {
  const { directory, inputs } = fixture(t);
  const forms = [
    '<script src=" https://cdn.example.test/a.js"></script>',
    '<script src="\thttps://cdn.example.test/a.js"></script>',
    '<script src="ht&#10;tps://cdn.example.test/a.js"></script>',
    '<script src="\\\\cdn.example.test\\a.js"></script>',
    '<script src="/\\cdn.example.test/a.js"></script>',
    '<script src="//cdn.example.test/a.js"></script>',
    '<link rel="stylesheet" href=" //cdn.example.test/a.css">',
  ];
  for (const html of forms) {
    writeFileSync(path.join(directory, 'index.html'), html);
    assert.throws(() => createPublishedManifest(inputs), /External executable content/, html);
  }
  writeFileSync(path.join(directory, 'index.html'), '<script src="/_astro/app.js"></script><script src="\\_astro\\app.js"></script>');
  createPublishedManifest(inputs);
});

test('external loaders, <base>, srcdoc and alternate or preloaded resources are rejected, local ones are not', (t) => {
  const { directory, inputs } = fixture(t);
  const base = '<script src="/_astro/app.js"></script>';
  for (const html of [
    '<base href="https://cdn.example.test/">',
    '<BASE HREF=" //cdn.example.test/">',
    '<iframe src="https://cdn.example.test/"></iframe>',
    '<object data="https://cdn.example.test/a.swf"></object>',
    '<embed src="//cdn.example.test/a.swf">',
    '<link rel="modulepreload" href="https://cdn.example.test/a.js">',
    '<link rel="preload" as="script" href="https://cdn.example.test/a.js">',
    '<link rel="alternate stylesheet" href="https://cdn.example.test/a.css">',
    '<link rel="Stylesheet" href="https://cdn.example.test/a.css">',
    '<svg><image href="https://cdn.example.test/a.png"/></svg>',
    '<svg><use xlink:href="https://cdn.example.test/a.svg#x"/></svg>',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), base + html);
    assert.throws(() => createPublishedManifest(inputs), /External executable content/, html);
  }
  for (const html of [
    '<iframe srcdoc="<script>alert(1)</script>"></iframe>',
    '<iframe srcdoc="<p>hi</p>"></iframe>',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), base + html);
    assert.throws(() => createPublishedManifest(inputs), /Inline iframe document/, html);
  }
  for (const html of [
    '<svg><circle fill="url(https://cdn.example.test/a.svg#g)"/></svg>',
    '<svg><rect filter="url(//cdn.example.test/a.svg#f)"/></svg>',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), base + html);
    assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/, html);
  }
  writeFileSync(path.join(directory, 'index.html'), `${base}
    <base href="/kognition/"><iframe src="/embed/"></iframe><object data="/a.pdf"></object>
    <link rel="canonical" href="https://kognition.example.test/guide/">
    <link rel="alternate" type="application/rss+xml" href="https://kognition.example.test/rss.xml">
    <link rel="preload" as="style" href="/_astro/common.css"><link rel="modulepreload" href="/_astro/app.js">
    <svg><use href="#icon"/><circle fill="url(#gradient)"/><image href="/a.png"/></svg>
    <a href="https://github.com/microsoft/kognition">source</a>`);
  createPublishedManifest(inputs);
});

test('image functions with bare-string URLs are external stylesheet references like url()', (t) => {
  const { directory, inputs } = fixture(t);
  const css = path.join(directory, '_astro', 'common.css');
  for (const body of [
    'a{background:image-set("https://cdn.example.test/a.png" 1x)}',
    "a{background:-webkit-image-set('//cdn.example.test/a.png' 1x, 'https://cdn.example.test/b.png' 2x)}",
    'a{background:IMAGE-SET("/local.png" 1x, "https://cdn.example.test/b.png" 2x)}',
    'a{background:image("https://cdn.example.test/a.png")}',
    'a{background:image-set(url(https://cdn.example.test/a.png) 1x)}',
    'a{background:cross-fade("https://cdn.example.test/a.png", "/b.png", 50%)}',
    'a{background:image-set(linear-gradient(red, blue) 1x, "\\68ttps://cdn.example.test/a.png" 2x)}',
  ]) {
    writeFileSync(css, body);
    assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/, body);
  }
  for (const body of [
    'a{background:image-set("/a.png" 1x, "./b.png" 2x, "data:image/png;base64,AAAA" 3x)}',
    'a::after{content:"https://cdn.example.test/a.png"}',
    'a{background:image-set("/a.png" 1x)} b::after{content:"https://cdn.example.test/a.png"}',
    'a{font-family:image,sans-serif;content:"https://cdn.example.test/"}',
    'a{background:image("/a.png") ; content:"https://cdn.example.test/"}',
  ]) {
    writeFileSync(css, body);
    createPublishedManifest(inputs);
  }
});

test('standalone SVG files cannot import external CSS or load external images', (t) => {
  const { directory, inputs } = fixture(t);
  const svg = path.join(directory, 'logo.svg');
  for (const [body, message] of [
    ['<svg xmlns="http://www.w3.org/2000/svg"><style>@import url(https://cdn.example.test/a.css);</style></svg>', /External stylesheet content/],
    ['<svg xmlns="http://www.w3.org/2000/svg"><style><![CDATA[@import "https://cdn.example.test/a.css";]]></style></svg>', /External stylesheet content/],
    ['<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url(https://cdn.example.test/a.svg#g)"/></svg>', /External stylesheet content/],
    ['<svg xmlns="http://www.w3.org/2000/svg"><rect fill="url(https://cdn.example.test/a.svg#g)"/></svg>', /External stylesheet content/],
    ['<svg xmlns="http://www.w3.org/2000/svg"><image href="https://cdn.example.test/a.png"/></svg>', /External executable content/],
    ['<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><image xlink:href="//cdn.example.test/a.png"/></svg>', /External executable content/],
    ['<svg xmlns="http://www.w3.org/2000/svg"><feImage href="https://cdn.example.test/a.png"/></svg>', /External executable content/],
    ['<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>', /Active SVG content/],
  ]) {
    writeFileSync(svg, body);
    assert.throws(() => createPublishedManifest(inputs), message, body);
  }
  writeFileSync(svg, '<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:url(#g)}</style><use href="#a"/>' +
    '<image href="data:image/png;base64,AAAA"/><a href="https://github.com/microsoft/kognition"><rect/></a></svg>');
  createPublishedManifest(inputs);
});

test('SVG script and template references, noscript content and xml-stylesheet are treated as loaders', (t) => {
  const { directory, inputs } = fixture(t);
  const base = '<script src="/_astro/app.js"></script>';
  for (const html of [
    '<svg><script href="https://cdn.example.test/a.js"></script></svg>',
    '<svg><script xlink:href="//cdn.example.test/a.js"></script></svg>',
    '<svg><linearGradient href="https://cdn.example.test/a.svg#g"/></svg>',
    '<svg><pattern href="https://cdn.example.test/a.svg#p"/></svg>',
    '<svg><text><textPath href="https://cdn.example.test/a.svg#p">t</textPath></text></svg>',
    '<noscript><link rel="stylesheet" href="https://cdn.example.test/a.css"></noscript>',
    '<noscript><iframe src="https://cdn.example.test/"></iframe></noscript>',
    '<template><script src="https://cdn.example.test/a.js"></script></template>',
  ]) {
    writeFileSync(path.join(directory, 'index.html'), base + html);
    assert.throws(() => createPublishedManifest(inputs), /External executable content/, html);
  }
  writeFileSync(path.join(directory, 'index.html'),
    `${base}<noscript><style>.js-only{display:none}</style></noscript><template><a href="https://github.com/">x</a></template>` +
    '<svg><linearGradient href="#g"/><script href="/_astro/app.js"></script></svg>');
  createPublishedManifest(inputs);

  const svg = path.join(directory, 'logo.svg');
  writeFileSync(path.join(directory, 'index.html'), base);
  writeFileSync(svg, '<?xml version="1.0"?><?xml-stylesheet href="https://cdn.example.test/a.css" type="text/css"?><svg xmlns="http://www.w3.org/2000/svg"/>');
  assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/);
  writeFileSync(svg, "<?xml-stylesheet type='text/css' href='//cdn.example.test/a.css'?><svg xmlns='http://www.w3.org/2000/svg'/>");
  assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/);
  writeFileSync(svg, '<?xml-stylesheet href="local.css" type="text/css"?><svg xmlns="http://www.w3.org/2000/svg"/>');
  createPublishedManifest(inputs);

  writeFileSync(svg, '<svg xmlns="http://www.w3.org/2000/svg"/>');
  const css = path.join(directory, '_astro', 'common.css');
  writeFileSync(css, 'a{background:src("https://cdn.example.test/a.png")}');
  assert.throws(() => createPublishedManifest(inputs), /External stylesheet content/);
  writeFileSync(css, '@font-face{font-family:f;src:local("x"),url(/f.woff2) format("woff2")}');
  createPublishedManifest(inputs);
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
