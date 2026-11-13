// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { i18nLoader } from '@astrojs/starlight/loaders';
import { docsSchema, i18nSchema } from '@astrojs/starlight/schema';
import { docsURL, pages } from '../site.mjs';

const pageIDs = new Map(pages.map((page) => [page.source, page.id]));

export const collections = {
  i18n: defineCollection({ loader: i18nLoader(), schema: i18nSchema() }),
  docs: defineCollection({
    loader: glob({
      base: docsURL,
      pattern: pages.map((page) => page.source),
      generateId: ({ entry }) => {
        const id = pageIDs.get(entry.replaceAll('\\', '/'));
        if (!id) throw new Error(`Unlisted documentation source: ${entry}`);
        return id;
      },
    }),
    schema: docsSchema(),
  }),
};
