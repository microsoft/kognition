// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { parse } from 'yaml';
import { unified } from 'unified';
import repositoryLinks from '../src/repository-links.ts';
import { checkSite } from '../scripts/check-links.mjs';
import { docsDirectory, getSiteConfig, pagePath, pages, repositoryRoot } from '../site.mjs';

function currentGuides(directory = repositoryRoot) {
  const excluded = new Set([
    'docs/adr', 'docs/development-plans', 'docs/release-preparation',
    'docs/maintaining-docs.md',
  ]);
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const absolute = path.join(directory, entry.name);
    const relative = path.relative(repositoryRoot, absolute).split(path.sep).join('/');
    if (excluded.has(relative)) return [];
    if (entry.isDirectory()) {
      if (directory === repositoryRoot && !['docs', 'deploy', 'samples', 'tests', 'infra'].includes(entry.name)) return [];
      return currentGuides(absolute);
    }
    if (!entry.isFile() || !entry.name.endsWith('.md')) return [];
    if (relative.startsWith('samples/docs/') && entry.name !== 'README.md') return [];
    return [{ relative, source: readFileSync(absolute, 'utf8').replace(/\r\n/g, '\n') }];
  });
}

function commandBlocks(source) {
  return [...source.matchAll(/^([ \t]*)```(bash|sh|powershell|pwsh|ps1)[^\n]*\n([\s\S]*?)^\1```[ \t]*$/gm)]
    .map((match) => ({
      language: match[2],
      code: match[3].split('\n').map((line) => line.startsWith(match[1]) ? line.slice(match[1].length) : line).join('\n'),
      line: source.slice(0, match.index).split('\n').length,
    }));
}

test('current guides pair shell-specific instructions and keep shared commands portable', () => {
  const guides = currentGuides();
  assert.ok(guides.length > 40, 'The review must include repository guides, not only website pages');
  for (const { relative, source } of guides) {
    const blocks = commandBlocks(source);
    for (const [index, block] of blocks.entries()) {
      if (['powershell', 'pwsh', 'ps1'].includes(block.language)) {
        assert.ok([blocks[index - 1], blocks[index + 1]].some((neighbor) =>
          neighbor && ['bash', 'sh'].includes(neighbor.language)),
        `${relative}:${block.line} needs an adjacent Bash alternative or shared command`);
      } else {
        assert.doesNotMatch(block.code, /\$env:|\.\x5c|[A-Za-z]:\x5c|\.exe\b|`\s*\n/,
          `${relative}:${block.line} contains Windows-only syntax in a Bash/shared example`);
        if (block.language === 'sh') {
          assert.doesNotMatch(block.code, /\\\n|<<|^\s*(?:export|source)\s|\$\(/m,
            `${relative}:${block.line} uses Bash-only syntax in a shared-shell example`);
        }
      }
    }
  }
});

test('Bash examples parse without executing deployment or provider operations', (t) => {
  const blocks = currentGuides().flatMap(({ relative, source }) =>
    commandBlocks(source).filter(({ language }) => ['bash', 'sh'].includes(language))
      .map((block) => ({ ...block, relative })));
  const input = blocks.map(({ relative, line, code }) =>
    `{\n# ${relative}:${line}\n:\n${code}\n}\n`).join('\n');
  const result = spawnSync('bash', ['-n'], { input, encoding: 'utf8', timeout: 30_000 });
  if (result.error?.code === 'ENOENT' && process.platform === 'win32') {
    t.skip('Install Bash (Git Bash or WSL) to validate Bash syntax locally; required on Linux CI');
    return;
  }
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.ok(blocks.length > 100);
});

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
  assert.equal((aks.match(/^kubeconfig=/gm) ?? []).length, 1);
  assert.equal((aks.match(/^context=/gm) ?? []).length, 1);
  assert.match(aks, /helm show crds \$Chart \| kubectl --kubeconfig \$Kubeconfig --context \$Context apply -f -/);
  assert.ok(aks.includes('helm show crds "$chart" | kubectl --kubeconfig "$kubeconfig" --context "$context" apply -f -'));
  const bashInstallation = commandBlocks(aks).find(({ language, code }) =>
    language === 'bash' && code.includes('Install-RuntimeBase.ps1'));
  assert.ok(bashInstallation);
  assert.ok(bashInstallation.code.includes('-Kubeconfig "$kubeconfig" -Context "$context" -ChartPackage "$chart"'));
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
  assert.equal(notices.if, undefined);
  assert.ok(workflow.jobs.build.steps.indexOf(notices) < workflow.jobs.build.steps.indexOf(upload));
  const manifest = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
  assert.match(manifest.scripts.notices, / --strict(?:\s|$)/);
  const noticeCommands = manifest.scripts.notices.split(' && ');
  assert.equal(noticeCommands.length, 2);
  assert.match(noticeCommands[0], /--output \.astro\/build-tool-notices/);
  assert.match(noticeCommands[1], /--output dist\/third-party --strict/);
  assert.match(noticeCommands[1], /--distribution-root dist --distribution-manifest \.astro\/published-files\.json/);
  const evidence = workflow.jobs.build.steps.find((step) => step.id === 'evidence');
  assert.match(evidence.if, /^always\(\)/);
  assert.match(evidence.uses, /^actions\/upload-artifact@[0-9a-f]{40}$/);
  assert.equal(evidence.with['include-hidden-files'], true);
  assert.equal(evidence.with['retention-days'], 14);
  assert.deepEqual(evidence.with.path.trim().split('\n'), [
    'website/.astro/published-files.json',
    'website/.astro/build-tool-notices/inventory.json',
    'website/.astro/build-tool-notices/NOTICE',
    'website/dist/third-party/inventory.json',
    'website/dist/third-party/NOTICE',
  ]);
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
