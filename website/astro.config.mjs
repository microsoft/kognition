// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { defineConfig } from 'astro/config';
import { unified } from '@astrojs/markdown-remark';
import starlight from '@astrojs/starlight';
import repositoryLinks from './src/repository-links';
import { repositoryURL, site } from './site.mjs';

export default defineConfig({
  site: site.origin,
  base: site.base,
  trailingSlash: 'always',
  markdown: { processor: unified({ remarkPlugins: [repositoryLinks] }) },
  integrations: [
    starlight({
      title: 'Kognition',
      description: 'Declarative orchestration for AI agents.',
      logo: {
        replacesTitle: true,
        light: '../docs/assets/kognition-logo.svg',
        dark: '../docs/assets/kognition-logo-dark.svg',
      },
      social: [{ icon: 'github', label: 'GitHub', href: repositoryURL }],
      sidebar: [
        { label: 'Documentation home', slug: 'index' },
        { label: 'Overview', slug: 'overview' },
        {
          label: 'Get started',
          items: [
            { label: 'Choose a path', slug: 'getting-started' },
            { label: 'Local development', slug: 'getting-started/local' },
            { label: 'Kubernetes setup', slug: 'getting-started/kubernetes' },
            { label: 'AKS setup', slug: 'getting-started/aks' },
            { label: 'First agent', slug: 'getting-started/first-agent' },
          ],
        },
        { label: 'Concepts', items: [{ slug: 'concepts/resource-model' }] },
        { label: 'Guides and samples', slug: 'guides' },
        { label: 'Operations', slug: 'operations' },
        { label: 'Reference', items: [{ slug: 'reference' }, { slug: 'reference/cli' }] },
        {
          label: 'Contribute',
          items: [
            { slug: 'content-map' },
            { label: 'Contribution policy', link: `${repositoryURL}/blob/main/CONTRIBUTING.md` },
          ],
        },
      ],
    }),
  ],
});
