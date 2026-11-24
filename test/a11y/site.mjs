// Checks lapilli.dev, the front page and the docs, with axe-core against
// WCAG 2.2 AA, in light and in dark, and that no page scrolls sideways at
// 320 CSS px (WCAG 1.4.10): every page its sitemap.xml lists, which
// jekyll-sitemap writes from every page there is. And that every result of
// the docs' search goes to a section there is. Run by
// .github/workflows/pages.yml over the site it builds, or by hand:
//
//   cd test/a11y && npm ci && BASE=https://lapilli.dev node site.mjs
//
// BASE is where the site is served; or SITE_DIR, a directory of it as
// Jekyll built it, which this serves itself on a port of loopback. Prints the violations by rule, and
// writes them all as JSON to OUT when it is set. Exits 1 when there are any.
import { writeFileSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { extname, join, resolve, sep } from 'node:path';
import * as axe from '@axe-core/playwright';
import { chromium } from 'playwright';

const AxeBuilder = axe.AxeBuilder ?? axe.default?.default ?? axe.default;
let base = (process.env.BASE ?? '').replace(/\/$/, '');
let server;
if (process.env.SITE_DIR) {
  const root = resolve(process.env.SITE_DIR);
  const types = { '.html': 'text/html', '.css': 'text/css', '.png': 'image/png', '.svg': 'image/svg+xml', '.woff2': 'font/woff2', '.xml': 'application/xml', '.txt': 'text/plain', '.js': 'text/javascript' };
  server = createServer(async (req, res) => {
    let file = join(root, decodeURIComponent(new URL(req.url, 'http://x').pathname));
    if (file !== root && !file.startsWith(root + sep)) return res.writeHead(403).end();
    if (file.endsWith(sep) || file === root) file = join(file, 'index.html');
    try {
      const body = await readFile(file).catch(() => readFile(join(file, 'index.html')));
      res.writeHead(200, { 'Content-Type': types[extname(file)] ?? 'text/html' }).end(body);
    } catch {
      res.writeHead(404).end();
    }
  });
  await new Promise((ok) => server.listen(0, '127.0.0.1', ok));
  base = `http://127.0.0.1:${server.address().port}`;
}
if (!base) {
  console.error('BASE or SITE_DIR, please');
  process.exit(2);
}
const tags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];

// The sitemap names pages at the site's own URL: each is read here as a
// path, at BASE.
const sitemap = await (await fetch(`${base}/sitemap.xml`)).text();
const paths = [...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => new URL(m[1]).pathname);
if (paths.length < 10) {
  console.error(`sitemap.xml at ${base} lists ${paths.length} pages: too few to be the site's`);
  process.exit(1);
}

const browser = await chromium.launch();
const found = [];
for (const scheme of ['light', 'dark']) {
  const context = await browser.newContext({ colorScheme: scheme });
  const page = await context.newPage();
  for (const path of paths) {
    await page.goto(base + path, { waitUntil: 'networkidle' });
    const r = await new AxeBuilder({ page }).withTags(tags).analyze();
    for (const v of r.violations) {
      found.push({ scheme, path, id: v.id, impact: v.impact, help: v.help, targets: v.nodes.map((n) => n.target.join(' ')) });
    }
  }
  await context.close();
}
const narrow = await browser.newContext({ viewport: { width: 320, height: 640 } });
const page = await narrow.newPage();
for (const path of paths) {
  await page.goto(base + path, { waitUntil: 'networkidle' });
  const w = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  if (w[0] > w[1]) found.push({ scheme: '320px', path, id: 'reflow', help: `scrolls sideways at 320 CSS px: ${w[0]} > ${w[1]}` });
}
// The docs' search (site/docs.sh writes its indexes, /docs/search.json and,
// where there are translations, /ko/docs/search.json): every result goes to
// a page there is, and to a section of it there is, by the id kramdown gave
// the heading.
for (const where of ['/docs/search.json', '/ko/docs/search.json']) {
  const res = await fetch(base + where);
  if (where !== '/docs/search.json' && res.status === 404) continue;
  const index = await res.json();
  if (index.length < (where === '/docs/search.json' ? 10 : 1)) found.push({ scheme: 'search', path: where, id: 'search-index', help: `lists ${index.length} pages: too few to be the docs'` });
  for (const entry of index) {
    const path = new URL(entry.url, base).pathname;
    const r = await page.goto(base + path, { waitUntil: 'domcontentloaded' });
    if (!r || !r.ok()) {
      found.push({ scheme: 'search', path, id: 'search-page', help: `${where} lists a page that is not there` });
      continue;
    }
    // The ids of the page's ## and ### headings, in order, are exactly the
    // index's: a heading kramdown numbered (-1) and the index didn't, or the
    // other way round, is a result that goes to the wrong section.
    const ids = await page.evaluate(() => [...document.querySelectorAll('article.doc :is(h2, h3)[id]')].map((e) => e.id));
    const want = entry.headings.map((h) => h[0]);
    for (let i = 0; i < Math.max(ids.length, want.length); i++) {
      if (ids[i] !== want[i]) {
        found.push({ scheme: 'search', path, id: 'search-anchor', help: `${where}: the ${i + 1}th heading is #${ids[i]} on the page and #${want[i]} ("${entry.headings[i]?.[1]}") in the index` });
        break;
      }
    }
  }
}
await browser.close();
server?.close();

if (process.env.OUT) writeFileSync(process.env.OUT, JSON.stringify(found, null, 1));
const byRule = new Map();
for (const f of found) {
  const k = `${f.id}${f.impact ? ` (${f.impact})` : ''}: ${f.help}`;
  byRule.set(k, [...(byRule.get(k) ?? []), `${f.path} ${f.scheme}`]);
}
for (const [k, where] of byRule) console.log(`${k}\n  ${where.slice(0, 8).join('\n  ')}${where.length > 8 ? `\n  and ${where.length - 8} more` : ''}`);
console.log(`${paths.length} pages, light and dark, and at 320 CSS px: ${found.length} violations`);
process.exit(found.length ? 1 : 0);
