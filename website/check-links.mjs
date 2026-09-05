// Resolves every internal href in dist/ against the built tree. Run after `astro build`.
import fs from 'node:fs';
import path from 'node:path';

const root = 'dist';
const BASE = '/wazy'; // must match `base` in astro.config.mjs
const basePrefix = new RegExp(`^${BASE}(?=/|$)`); // anchored to a path-segment boundary
const pages = [];
(function walk(d) {
  for (const f of fs.readdirSync(d, { withFileTypes: true })) {
    const p = path.join(d, f.name);
    f.isDirectory() ? walk(p) : f.name.endsWith('.html') && pages.push(p);
  }
})(root);

const isFile = (p) => fs.statSync(p, { throwIfNoEntry: false })?.isFile() ?? false;
const exists = (u) => {
  const clean = u.replace(/[?#].*$/, '');
  if (!basePrefix.test(clean)) return false; // a link that drops the base 404s in production
  const rel = clean.replace(basePrefix, '');
  return [path.join(root, rel), path.join(root, rel, 'index.html'), path.join(root, rel.replace(/\/$/, '') + '.html')]
    .some(isFile); // a directory with no index.html is a 404, not a hit
};

let bad = 0;
for (const p of pages) {
  const html = fs.readFileSync(p, 'utf8');
  const pageUrl = BASE + p.replace(root, '').replace(/index\.html$/, '');
  for (const [, u] of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
    if (/^(https?:|mailto:|#|data:|\/\/)/.test(u)) continue;
    const abs = u.startsWith('/') ? u : new URL(u, 'http://x' + pageUrl).pathname;
    if (!exists(abs)) { console.log('BROKEN', pageUrl, '->', u); bad++; }
  }
}
console.log(bad ? `${bad} broken` : `all internal links OK (${pages.length} pages)`);
process.exit(bad ? 1 : 0);
