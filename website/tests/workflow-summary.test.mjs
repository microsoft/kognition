// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { parse } from 'yaml';

const script = new URL('../scripts/report-documentation.mjs', import.meta.url);
const workflowFile = new URL('../../.github/workflows/pages.yml', import.meta.url);
const ciFile = new URL('../../.github/workflows/ci.yml', import.meta.url);
const complete = {
  checkout: 'success', node: 'success', install: 'success', tests: 'success', types: 'success',
  media: 'skipped', site: 'success', links: 'skipped', preview_links: 'success',
  notices: 'success', upload: 'skipped',
};
const unresolved = [
  { component: '@img/sharp-libvips-linux-x64', version: '1.3.4', license: 'LGPL-3.0-or-later' },
  { component: '@img/sharp-libvips-linuxmusl-x64', version: '1.3.4', license: 'LGPL-3.0-or-later' },
  { component: 'am-i-vibing', version: '0.5.0', license: 'MIT' },
  { component: 'piccolore', version: '0.1.3', license: 'ISC' },
  { component: 'process-ancestry', version: '0.1.0', license: 'MIT' },
];

function summary(t, outcomes, inventory, context = {}) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'kognition-docs-summary-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const output = path.join(root, 'summary.md');
  writeFileSync(output, 'Existing step summary.\n');
  if (inventory !== undefined) {
    mkdirSync(path.join(root, 'dist', 'third-party'), { recursive: true });
    writeFileSync(path.join(root, 'dist', 'third-party', 'inventory.json'), JSON.stringify(inventory));
  }
  const result = spawnSync(process.execPath, [fileURLToPath(script)], { cwd: root, encoding: 'utf8', env: {
    ...process.env, GITHUB_STEP_SUMMARY: output, DOCS_STEP_OUTCOMES: JSON.stringify(outcomes),
    DOCS_PUBLISH_ENABLED: 'false', GITHUB_REF: 'refs/pull/17/merge', GITHUB_EVENT_NAME: 'pull_request',
    ...context,
  } });
  assert.equal(result.status, 0, result.stderr);
  const content = readFileSync(output, 'utf8');
  assert(content.startsWith('Existing step summary.\n'), 'Reporting must append to GITHUB_STEP_SUMMARY.');
  return content;
}

test('job display names distinguish application builds, documentation validation and Pages deployment', () => {
  const docs = parse(readFileSync(workflowFile, 'utf8'));
  const ci = parse(readFileSync(ciFile, 'utf8'));
  assert.equal(ci.jobs.build.name, 'Build application binaries');
  assert.equal(docs.jobs.build.name, 'Validate documentation and notices');
  assert.equal(docs.jobs.deploy.name, 'Deploy documentation to Pages');
  assert.equal(docs.jobs.deploy.needs, 'build', 'Display-name changes must preserve dependency IDs.');
});

test('the final documentation summary runs after notices and artifact upload even when validation fails', () => {
  const workflow = parse(readFileSync(workflowFile, 'utf8'));
  const steps = workflow.jobs.build.steps;
  const report = steps.find((step) => step.id === 'summary');
  assert(report, 'The workflow must include a final outcome-aware summary.');
  assert.equal(report.if, 'always()');
  assert.equal(report.run, 'node scripts/report-documentation.mjs');
  assert.equal(steps.at(-1), report);
  assert(steps.findIndex((step) => step.id === 'notices') < steps.indexOf(report));
  assert(steps.findIndex((step) => step.id === 'upload') < steps.indexOf(report));
  assert.equal(steps.find((step) => step.id === 'notices')['continue-on-error'], undefined);
  assert.equal(steps.find((step) => step.id === 'notices').run, 'npm run notices');
  const outcomes = JSON.parse(report.env.DOCS_STEP_OUTCOMES);
  for (const id of Object.keys(complete)) {
    assert.equal(outcomes[id], `\${{ steps.${id}.outcome }}`);
    assert.equal(steps.filter((step) => step.id === id).length, 1);
  }
  assert(!steps.some((step) => step.run?.includes('Documentation build and link validation passed.')),
    'No early unconditional success-shaped summary may remain.');
});

test('a notice failure is the failed result even when the site and links passed and publishing is disabled', (t) => {
  const content = summary(t, { ...complete, notices: 'failure' },
    { unresolved: 5, components: unresolved.map((entry) => ({ ...entry, installed: true, texts: [] })) });
  assert.match(content, /\*\*Result: failed\.\*\*/);
  assert.match(content, /\| Website build \| Passed \|/);
  assert.match(content, /\| Distribution notices \| Failed \|/);
  assert.match(content, /5 unresolved installed components/);
  for (const entry of unresolved) {
    assert(content.includes(entry.component));
    assert(content.includes(entry.version));
    assert(content.includes(entry.license));
  }
  assert.match(content, /Publishing disabled[\s\S]*not a validation failure/);
  assert.doesNotMatch(content, /Documentation validation passed|Deployment succeeded/);
});

test('early failures and skipped checks do not claim a successful build or consume stale inventory', (t) => {
  const skipped = Object.fromEntries(Object.keys(complete).map((key) => [key, 'skipped']));
  const content = summary(t, { ...skipped, checkout: 'success', node: 'success', install: 'failure' },
    { unresolved: 99, components: [] });
  assert.match(content, /\| Install dependencies \| Failed \|/);
  assert.match(content, /\| Website build \| Skipped \|/);
  assert.match(content, /Notice inventory was not generated in this run/);
  assert.doesNotMatch(content, /99 unresolved|Documentation validation passed/);
});

test('missing or inconsistent notice inventories are reported explicitly without hiding the notice failure', (t) => {
  for (const inventory of [undefined, { unresolved: 5, components: [] }]) {
    const content = summary(t, { ...complete, notices: 'failure' }, inventory);
    assert.match(content, /\*\*Result: failed\.\*\*/);
    assert.match(content, /Notice inventory unavailable:/);
    assert.match(content, /Verify distribution notices/);
    assert.doesNotMatch(content, /Documentation validation passed/);
  }
});

test('successful PR validation is separate from publication eligibility', (t) => {
  const content = summary(t, complete, { unresolved: 0, components: [] });
  assert.match(content, /\*\*Result: documentation validation passed\.\*\*/);
  assert.match(content, /Publishing disabled/);
  assert.doesNotMatch(content, /Deployment succeeded|artifact uploaded/);
  const enabled = summary(t, complete, { unresolved: 0, components: [] }, { DOCS_PUBLISH_ENABLED: 'true' });
  assert.match(enabled, /not eligible to publish/);
  assert.doesNotMatch(enabled, /artifact uploaded|Deployment succeeded/);
});

test('trusted main reports artifact upload separately and does not claim deployment success', (t) => {
  const content = summary(t, { ...complete, media: 'success', links: 'success', preview_links: 'skipped', upload: 'success' },
    { unresolved: 0, components: [] }, {
      DOCS_PUBLISH_ENABLED: 'true', GITHUB_REF: 'refs/heads/main', GITHUB_EVENT_NAME: 'push',
    });
  assert.match(content, /Verified Pages artifact uploaded/);
  assert.match(content, /Deploy documentation to Pages/);
  assert.doesNotMatch(content, /Deployment succeeded|Publishing disabled/);
});

test('cancelled or incomplete validation is never presented as passed', (t) => {
  const cancelled = summary(t, { ...complete, types: 'cancelled', notices: 'skipped' });
  assert.match(cancelled, /\*\*Result: cancelled\.\*\*/);
  const incomplete = summary(t, { ...complete, notices: 'skipped' });
  assert.match(incomplete, /\*\*Result: incomplete\.\*\*/);
  assert.doesNotMatch(cancelled + incomplete, /Documentation validation passed/);
});

test('notice metadata cannot inject HTML or break the summary table', (t) => {
  const content = summary(t, { ...complete, notices: 'failure' }, {
    unresolved: 1, components: [{
      component: '<img src=x>|`fake`\nname', version: '1.0.0', license: 'MIT',
      installed: true, texts: [],
    }],
  });
  assert.match(content, /&lt;img src=x&gt;&#124;&#96;fake&#96; name/);
  assert.doesNotMatch(content, /<img src=x>/);
});
