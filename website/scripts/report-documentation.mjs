// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { appendFileSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const checks = {
  checkout: 'Checkout',
  node: 'Node.js runtime',
  install: 'Install dependencies',
  tests: 'Documentation tests',
  types: 'Types and content',
  media: 'Verified tutorial release assets',
  site: 'Website build',
  links: 'Publication links and assets',
  preview_links: 'Validation-only links',
  notices: 'Distribution notices',
  evidence: 'Retain notice verification evidence',
  upload: 'Upload Pages artifact',
};
const labels = { success: 'Passed', failure: 'Failed', skipped: 'Skipped', cancelled: 'Cancelled', '': 'Not run' };
const escape = (text) => String(text).replaceAll('&', '&amp;').replaceAll('<', '&lt;')
  .replaceAll('>', '&gt;').replaceAll('|', '&#124;').replaceAll('`', '&#96;').replace(/\r?\n/g, ' ');

function readNoticeInventory(filename, label = 'dist/third-party/inventory.json') {
  let text;
  try {
    text = readFileSync(filename, 'utf8');
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    return { inventoryError: `${label} was not produced.` };
  }
  try {
    return { inventory: JSON.parse(text) };
  } catch (error) {
    if (!(error instanceof SyntaxError)) throw error;
    return { inventoryError: `${label} is not valid JSON.` };
  }
}

function unresolvedComponents(inventory) {
  if (!inventory || !Array.isArray(inventory.components) ||
      !Number.isSafeInteger(inventory.unresolved) || inventory.unresolved < 0 ||
      inventory.components.some((entry) => !entry || typeof entry.component !== 'string' ||
        typeof entry.version !== 'string' || typeof entry.license !== 'string' ||
        typeof entry.installed !== 'boolean' || !Array.isArray(entry.texts))) {
    return { inventoryError: 'notice inventory has an invalid shape.' };
  }
  const unresolved = inventory.components.filter((entry) =>
    entry.installed && (!entry.texts.length || entry.license === 'UNRESOLVED'));
  if (unresolved.length !== inventory.unresolved) {
    return { inventoryError: 'notice inventory count does not match its unresolved components.' };
  }
  return { unresolved };
}

export function documentationSummary({
  outcomes, publishEnabled, ref, eventName, inventory, inventoryError, buildInventory, buildInventoryError,
}) {
  if (typeof publishEnabled !== 'boolean' || typeof ref !== 'string' || typeof eventName !== 'string' ||
      !outcomes || Object.keys(checks).some((id) => !Object.hasOwn(labels, outcomes[id]))) {
    throw new Error('Documentation reporting requires explicit publication context and valid step outcomes.');
  }
  const eligible = publishEnabled && ref === 'refs/heads/main' && eventName !== 'pull_request';
  const required = ['checkout', 'node', 'install', 'tests', 'types', 'site', 'notices', 'evidence',
    ...(eligible ? ['media', 'links'] : ['preview_links'])];
  const failed = Object.keys(checks).filter((id) => outcomes[id] === 'failure');
  const cancelled = Object.keys(checks).some((id) => outcomes[id] === 'cancelled');
  const noticesRan = ['success', 'failure'].includes(outcomes.notices);
  const noticeReport = !noticesRan ? null : inventoryError ? { inventoryError } : unresolvedComponents(inventory);
  const noticeProblem = noticeReport && (noticeReport.inventoryError || noticeReport.unresolved.length > 0);
  let result;
  if (failed.length || noticeProblem) result = 'failed';
  else if (cancelled) result = 'cancelled';
  else if (required.every((id) => outcomes[id] === 'success')) result = 'documentation validation passed';
  else result = 'incomplete';
  const lines = [
    '## Documentation validation and publication',
    '',
    `**Result: ${result}.**`,
    '',
    'This reports the Documentation workflow, separately from the CI application build.',
    '',
    '| Check | Outcome |',
    '| --- | --- |',
    ...Object.entries(checks).map(([id, title]) => `| ${title} | ${labels[outcomes[id]]} |`),
    '',
  ];
  if (noticeReport && !noticeReport.inventoryError && inventory.publicationEvidence) {
    const evidence = inventory.publicationEvidence;
    if (!Number.isSafeInteger(evidence.filesVerified) || evidence.filesVerified < 1 ||
        !Array.isArray(evidence.sourcePackagePaths) || !evidence.sourcePackagePaths.length) {
      throw new Error('Published notice inventory lacks verified file and source-package evidence.');
    }
    lines.push('### Notice verification scope', '',
      `The strict publication gate covers **${evidence.filesVerified} emitted files** and ` +
      `**${evidence.sourcePackagePaths.length} published npm source packages**.`,
      'File hashes, lockfile identity and emitted package provenance were verified before collecting notices.', '');
    const buildReport = buildInventoryError ? { inventoryError: buildInventoryError } : unresolvedComponents(buildInventory);
    if (buildReport.inventoryError) {
      lines.push(`Build-tool diagnostic inventory unavailable: ${buildReport.inventoryError}`, '');
    } else {
      lines.push(`The broader installed build-tool inventory has **${buildReport.unresolved.length} unresolved components**. ` +
        'It is retained separately in `.astro/build-tool-notices/inventory.json` and is not included in the Pages artifact.',
      'Download the **documentation-notice-evidence** Actions artifact for both inventories and the published-file evidence.',
      'This does not claim legal clearance for the build environment.', '');
    }
  }
  if (failed.length) lines.push(`**Failed checks:** ${failed.map((id) => checks[id]).join(', ')}.`, '');
  if (noticeReport?.inventoryError) {
    lines.push(`Notice inventory unavailable: ${noticeReport.inventoryError}`,
      'Read the **Verify distribution notices** step for the original failure.', '');
  } else if (noticeReport?.unresolved.length) {
    lines.push(`### ${noticeReport.unresolved.length} unresolved installed components`, '',
      '| Component | Version | Declared license | Reason |',
      '| --- | --- | --- | --- |',
      ...noticeReport.unresolved.map((entry) =>
        `| ${escape(entry.component)} | ${escape(entry.version)} | ${escape(entry.license)} | ` +
        `${entry.texts.length ? 'Unresolved declared license' : 'Missing legal text'} |`),
      '', 'Strict notice verification remains enabled. Missing legal texts block publication.', '');
  } else if (!noticesRan) {
    lines.push('Notice inventory was not generated in this run because the notice step did not complete.', '');
  } else {
    lines.push(inventory.publicationEvidence
      ? 'The published npm component inventory contains no unresolved notices. This is not legal clearance.'
      : 'The notice inventory contains no unresolved installed components. This is not legal clearance.', '');
  }
  lines.push('### Publication', '');
  if (result !== 'documentation validation passed') {
    lines.push('Publication is blocked until every required validation check passes.', '');
  }
  if (!publishEnabled) {
    lines.push('Publishing disabled: `DOCS_PAGES_ENABLED` is not `true`. This is not a validation failure.',
      'Pull requests validate documentation without deploying it.');
  } else if (!eligible) {
    lines.push('This run is not eligible to publish. Deployment requires a trusted `main` run, not a pull request.');
  } else if (result === 'documentation validation passed' && outcomes.upload === 'success') {
    lines.push('Verified Pages artifact uploaded. Deployment is reported separately by **Deploy documentation to Pages**.');
  } else if (result === 'documentation validation passed') {
    lines.push('Validation passed, but the Pages artifact was not uploaded successfully. Deployment is not ready.');
  } else {
    lines.push('The publication switch is enabled, but failed or incomplete validation prevents deployment.');
  }
  return `${lines.join('\n')}\n`;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  if (!process.env.GITHUB_STEP_SUMMARY || !['true', 'false'].includes(process.env.DOCS_PUBLISH_ENABLED)) {
    throw new Error('GITHUB_STEP_SUMMARY and DOCS_PUBLISH_ENABLED are required for workflow reporting.');
  }
  const outcomes = JSON.parse(process.env.DOCS_STEP_OUTCOMES);
  const notice = ['success', 'failure'].includes(outcomes.notices)
    ? readNoticeInventory(path.resolve('dist', 'third-party', 'inventory.json')) : {};
  const buildNotice = notice.inventory?.publicationEvidence
    ? readNoticeInventory(path.resolve('.astro', 'build-tool-notices', 'inventory.json'),
      '.astro/build-tool-notices/inventory.json') : {};
  const markdown = documentationSummary({
    outcomes, publishEnabled: process.env.DOCS_PUBLISH_ENABLED === 'true',
    ref: process.env.GITHUB_REF, eventName: process.env.GITHUB_EVENT_NAME, ...notice,
    buildInventory: buildNotice.inventory, buildInventoryError: buildNotice.inventoryError,
  });
  appendFileSync(process.env.GITHUB_STEP_SUMMARY, `\n${markdown}`);
  console.log(markdown);
}
