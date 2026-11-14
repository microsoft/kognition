// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { fileURLToPath } from 'node:url';

export const repositoryRoot = fileURLToPath(new URL('../', import.meta.url));
export const docsURL = new URL('../docs/', import.meta.url);
export const docsDirectory = fileURLToPath(docsURL);
export const repositoryURL = 'https://github.com/microsoft/kognition';

export const pages = [
  { source: 'not-found.md', id: '404' },
  { source: 'README.md', id: 'index' },
  { source: 'overview.md', id: 'overview' },
  { source: 'getting-started/README.md', id: 'getting-started' },
  { source: 'getting-started/local.md', id: 'getting-started/local' },
  { source: 'getting-started/kubernetes.md', id: 'getting-started/kubernetes' },
  { source: 'getting-started/aks.md', id: 'getting-started/aks' },
  { source: 'getting-started/first-agent.md', id: 'getting-started/first-agent' },
  { source: 'concepts/resource-model.md', id: 'concepts/resource-model' },
  { source: 'guides/README.md', id: 'guides' },
  { source: 'guides/video-tutorial.md', id: 'guides/video-tutorial' },
  { source: 'operations/README.md', id: 'operations' },
  { source: 'reference/README.md', id: 'reference' },
  { source: 'reference/cli.md', id: 'reference/cli' },
  { source: 'content-map.md', id: 'content-map' },
];

export function getSiteConfig(environment = process.env) {
  const origin = new URL(environment.DOCS_SITE_URL ?? 'https://microsoft.github.io');
  if (origin.protocol !== 'https:' || origin.username || origin.password ||
      origin.pathname !== '/' || origin.search || origin.hash) {
    throw new Error('DOCS_SITE_URL must be an HTTPS origin without a path, credentials, query or fragment.');
  }
  const base = environment.DOCS_BASE_PATH ?? '/kognition/';
  if (!base.startsWith('/') || /[\\?#\s]/.test(base) || base.includes('//') ||
      base.split('/').some((part) => part === '.' || part === '..')) {
    throw new Error('DOCS_BASE_PATH must be an absolute URL path without traversal, whitespace, query or fragment.');
  }
  return { origin: origin.origin, base: base.endsWith('/') ? base : `${base}/` };
}

export const site = getSiteConfig();

export function pagePath(id, configuration = site) {
  return `${configuration.base}${id === 'index' ? '' : `${id}/`}`;
}
