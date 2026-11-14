// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { parse } from 'yaml';
import { unified } from 'unified';
import repositoryLinks from '../src/repository-links.ts';
import { checkSite } from '../scripts/check-links.mjs';
import { docsDirectory, getSiteConfig, pagePath, pages, repositoryRoot } from '../site.mjs';

test('public and private Pages configurations produce correct route bases', () => {
  assert.equal(pagePath('index', getSiteConfig({})), '/kognition/');
  assert.equal(pagePath('getting-started/local', getSiteConfig({})), '/kognition/getting-started/local/');
  const privateSite = getSiteConfig({ DOCS_SITE_URL: 'https://example.pages.github.io', DOCS_BASE_PATH: '/' });
  assert.equal(pagePath('overview', privateSite), '/overview/');
  for (const environment of [
    { DOCS_SITE_URL: '' }, { DOCS_SITE_URL: 'http://example.org' },
    { DOCS_SITE_URL: 'https://example.org/path' }, { DOCS_SITE_URL: 'https://user:secret@example.org' },
    { DOCS_BASE_PATH: '' }, { DOCS_BASE_PATH: 'kognition' }, { DOCS_BASE_PATH: '/../secret/' },
    { DOCS_BASE_PATH: '/kognition?query' },
  ]) assert.throws(() => getSiteConfig(environment));
});

test('publication is an explicit unique list of reader pages', () => {
  assert.equal(new Set(pages.map((page) => page.id)).size, pages.length);
  assert.equal(new Set(pages.map((page) => page.source)).size, pages.length);
  for (const page of pages) {
    assert.notEqual(page.source, 'maintaining-docs.md');
    assert.doesNotMatch(page.source, /(^|\/)(development-plans|release-preparation|\.local)(\/|$)/);
    assert.doesNotMatch(page.source, /(^|\/)\.\.(\/|$)/);
    const source = readFileSync(path.join(docsDirectory, page.source), 'utf8');
    const frontmatter = /^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/.exec(source);
    assert.ok(frontmatter, `Missing frontmatter: ${page.source}`);
    const metadata = parse(frontmatter[1]);
    assert.equal(typeof metadata.title, 'string');
    assert.ok(metadata.title.trim());
    assert.equal(typeof metadata.description, 'string');
    assert.ok(metadata.description.trim());
  }
});

test('installation entry points stay generic and preserve the selected AKS target', () => {
  for (const relative of [
    'README.md', 'QUICKSTART.md', 'deploy/QUICKSTART.md', 'deploy/aks/README.md',
    'docs/bootstrapping.md', 'docs/getting-started/kubernetes.md', 'docs/getting-started/aks.md',
  ]) {
    const source = readFileSync(path.join(repositoryRoot, relative), 'utf8');
    assert.doesNotMatch(source, /az\s+aks\s+(?:start|stop)\b/i, relative);
    assert.doesNotMatch(source, /automatic[^\n]*(?:bootstrap|quickstart)[^\n]*blocked/i, relative);
  }
  const aks = readFileSync(path.join(docsDirectory, 'getting-started', 'aks.md'), 'utf8');
  assert.equal((aks.match(/^\$Kubeconfig\s*=/gm) ?? []).length, 1);
  assert.equal((aks.match(/^\$Context\s*=/gm) ?? []).length, 1);
  assert.match(aks, /helm show crds \$Chart \| kubectl --kubeconfig \$Kubeconfig --context \$Context apply -f -/);
  const bootstrap = readFileSync(path.join(docsDirectory, 'bootstrapping.md'), 'utf8');
  assert.match(bootstrap, /^### Configure an Azure tenant$/m);
  const verification = readFileSync(path.join(docsDirectory, 'features', 'knowledge-provider-execution', 'bootstrap-verification-contract.md'), 'utf8');
  assert.match(verification, /Nine policies and nine bindings/);
});

test('canonical Markdown links resolve to site routes or repository sources and fail closed', async () => {
  const source = { path: path.join(docsDirectory, 'README.md') };
  const processor = unified().use(repositoryLinks);
  const tree = (href) => ({
    type: 'root',
    children: [
      { type: 'heading', depth: 1, children: [{ type: 'text', value: 'Documentation' }] },
      { type: 'paragraph', children: [{ type: 'link', url: href, children: [{ type: 'text', value: 'Guide' }] }] },
    ],
  });
  const published = await processor.run(tree('getting-started/local.md#1-prepare-the-checkout-and-tools'), source);
  assert.equal(published.children.length, 1);
  assert.equal(published.children[0].children[0].url,
    `${pagePath('getting-started/local')}#1-prepare-the-checkout-and-tools`);
  const repository = await processor.run(tree('../CONTRIBUTING.md'), source);
  assert.equal(repository.children[0].children[0].url,
    'https://github.com/microsoft/kognition/blob/main/CONTRIBUTING.md');
  const external = await processor.run(tree('https://example.org/guide'), source);
  assert.equal(external.children[0].children[0].url, 'https://example.org/guide');
  await assert.rejects(processor.run(tree('missing-guide.md'), source), /missing link target/);
  await assert.rejects(processor.run(tree('../../outside.md'), source), /escapes the repository/);
  await assert.rejects(processor.run(tree('javascript:alert(1)'), source), /unsupported link protocol/);
});

test('PR validation has no deploy permissions and publication requires explicit trusted-main opt-in', () => {
  const workflow = parse(readFileSync(new URL('../../.github/workflows/pages.yml', import.meta.url), 'utf8'));
  assert.deepEqual(workflow.permissions, { contents: 'read' });
  assert.equal(workflow.env.ASTRO_TELEMETRY_DISABLED, '1');
  assert.equal(workflow.jobs.build.permissions, undefined);
  const checkout = workflow.jobs.build.steps.find((step) => step.uses?.startsWith('actions/checkout@'));
  assert.equal(checkout.with['persist-credentials'], false);
  assert.equal(workflow.jobs.deploy.permissions.pages, 'write');
  assert.equal(workflow.jobs.deploy.permissions['id-token'], 'write');
  assert.equal(workflow.jobs.deploy.needs, 'build');
  const deploymentGuard = workflow.jobs.deploy.if;
  assert.equal(deploymentGuard,
    "github.ref == 'refs/heads/main' && github.event_name != 'pull_request' && vars.DOCS_PAGES_ENABLED == 'true'");
  const upload = workflow.jobs.build.steps.find((step) => step.uses?.startsWith('actions/upload-pages-artifact@'));
  assert.equal(upload.if, deploymentGuard);
  assert.equal(upload.with.path, 'website/dist');
  const notices = workflow.jobs.build.steps.find((step) => step.run === 'npm run notices');
  assert.equal(notices.if, deploymentGuard);
  assert.ok(workflow.jobs.build.steps.indexOf(notices) < workflow.jobs.build.steps.indexOf(upload));
  const manifest = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
  assert.match(manifest.scripts.notices, / --strict(?:\s|$)/);
  for (const job of Object.values(workflow.jobs)) {
    for (const step of job.steps) {
      if (step.uses) assert.match(step.uses, /^actions\/[\w-]+@[0-9a-f]{40}$/);
    }
  }
});

test('all Astro entry points disable build-tool telemetry', () => {
  const manifest = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
  for (const command of ['dev', 'build', 'preview', 'check']) {
    assert.match(manifest.scripts[command], /ASTRO_TELEMETRY_DISABLED=1/);
  }
  assert.equal(manifest.dependencies['@microsoft/clarity'], undefined);
});

test('rendered link validation checks pages, anchors, assets and deployment base paths', (t) => {
  const root = mkdtempSync(path.join(os.tmpdir(), 'kognition-docs-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(path.join(root, 'guide'));
  writeFileSync(path.join(root, 'guide', 'index.html'), '<h1 id="setup">Setup</h1>');
  writeFileSync(path.join(root, 'logo.svg'), '<svg/>');
  const index = path.join(root, 'index.html');
  writeFileSync(index, '<a href="/kognition/guide/#setup">Guide</a><img src="/kognition/logo.svg">');
  assert.equal(checkSite(root, getSiteConfig({})).pages, 2);
  writeFileSync(index, '<a href="/kognition/guide/#missing">Broken anchor</a>');
  assert.throws(() => checkSite(root, getSiteConfig({})), /missing anchor/);
  writeFileSync(index, '<a href="/kognition/missing/">Broken page</a>');
  assert.throws(() => checkSite(root, getSiteConfig({})), /missing target/);
  writeFileSync(index, '<img src="/kognition/missing.svg">');
  assert.throws(() => checkSite(root, getSiteConfig({})), /missing target/);
  writeFileSync(index, '<a href="/guide/">Wrong base</a>');
  assert.throws(() => checkSite(root, getSiteConfig({})), /escapes site base/);
  writeFileSync(index, '<a href="https://example.org/remote">External</a>');
  assert.equal(checkSite(root, getSiteConfig({})).links, 0);
  writeFileSync(index, '<link rel="canonical" href="/kognition/404/">');
  assert.equal(checkSite(root, getSiteConfig({})).links, 0);
  writeFileSync(index, '<a href="/guide/#setup">Private site</a>');
  assert.equal(checkSite(root, getSiteConfig({ DOCS_BASE_PATH: '/' })).pages, 2);
});
