// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { existsSync, statSync } from 'node:fs';
import path from 'node:path';
import type { Root } from 'mdast';
import type { Plugin } from 'unified';
import { visit } from 'unist-util-visit';
import { docsDirectory, pages, pagePath, repositoryRoot, repositoryURL } from '../site.mjs';

const published = new Map(pages.map((page) => [path.resolve(docsDirectory, page.source), page.id]));

const repositoryLinks: Plugin<[], Root> = () => (tree, file) => {
  // Keep the canonical H1 for GitHub; Starlight renders its own page title.
  if (tree.children[0]?.type === 'heading' && tree.children[0].depth === 1) {
    tree.children.shift();
  }
  visit(tree, (node) => {
    if (node.type !== 'link' && node.type !== 'image' && node.type !== 'definition') return;
    const href = node.url;
    if (href.startsWith('#') || /^(https?:|mailto:|tel:|\/\/)/i.test(href)) return;
    if (/^[a-z][a-z\d+.-]*:/i.test(href)) {
      throw new Error(`${file.path}: unsupported link protocol: ${href}`);
    }
    const match = /^([^?#]*)(.*)$/.exec(href);
    if (!match) throw new Error(`${file.path}: invalid link: ${href}`);
    const target = path.resolve(path.dirname(file.path), decodeURIComponent(match[1]));
    const relative = path.relative(repositoryRoot, target);
    if (relative.startsWith(`..${path.sep}`) || relative === '..' || path.isAbsolute(relative)) {
      throw new Error(`${file.path}: link escapes the repository: ${href}`);
    }
    if (!existsSync(target)) throw new Error(`${file.path}: missing link target: ${href}`);
    const id = published.get(target);
    if (id) {
      node.url = `${pagePath(id)}${match[2]}`;
    } else {
      const location = relative.split(path.sep).map(encodeURIComponent).join('/');
      const kind = statSync(target).isDirectory() ? 'tree' : 'blob';
      node.url = `${repositoryURL}/${kind}/main/${location}${match[2]}`;
    }
  });
};

export default repositoryLinks;
