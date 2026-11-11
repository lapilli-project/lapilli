// Checks every page lapilli serve gives TestA11yServe with axe-core, against
// WCAG 2.2 AA, in each language, in light and in dark; and that no page
// scrolls sideways at 320 CSS px (WCAG 1.4.10). Run by axe.sh.
//
// BASE is where the pages are served. Prints the violations by rule, and
// writes them all as JSON to OUT when it is set. Exits 1 when there are any.
import { writeFileSync } from 'node:fs';
import * as axe from '@axe-core/playwright';
import { chromium } from 'playwright';

const AxeBuilder = axe.AxeBuilder ?? axe.default?.default ?? axe.default;
const base = process.env.BASE;
const langs = ['en', 'es', 'ja', 'ko'];
const schemes = ['light', 'dark'];
const tags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];

const paths = (await (await fetch(`${base}/a11y-paths`)).text()).trim().split('\n');
const browser = await chromium.launch();
const found = []; // one per rule, page, language and scheme

async function check(lang, scheme) {
  const context = await browser.newContext({ colorScheme: scheme, extraHTTPHeaders: { 'Accept-Language': lang } });
  const page = await context.newPage();
  for (const path of paths) {
    await page.goto(base + path);
    const { violations } = await new AxeBuilder({ page }).withTags(tags).analyze();
    for (const v of violations) {
      found.push({ rule: v.id, impact: v.impact, path, lang, scheme, nodes: v.nodes.length,
        targets: v.nodes.slice(0, 3).map((n) => n.target.join(' ')), help: v.help });
    }
  }
  await context.close();
}

// reflow is what scrolls sideways at 320 CSS px, a page at a time.
async function reflow(lang) {
  const context = await browser.newContext({ viewport: { width: 320, height: 640 }, extraHTTPHeaders: { 'Accept-Language': lang } });
  const page = await context.newPage();
  for (const path of paths) {
    await page.goto(base + path);
    const wide = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    if (wide > 0) {
      found.push({ rule: 'reflow-320', impact: 'serious', path, lang, scheme: 'light', nodes: 1, targets: [`${wide}px too wide`],
        help: 'The page scrolls sideways at 320 CSS px' });
    }
  }
  await context.close();
}

await Promise.all(langs.flatMap((l) => [...schemes.map((s) => check(l, s)), reflow(l)]));
await browser.close();

const rules = new Map();
for (const f of found) {
  const r = rules.get(f.rule) ?? { impact: f.impact, nodes: 0, pages: new Set(), runs: 0, help: f.help, example: f };
  r.nodes += f.nodes;
  r.runs += 1;
  r.pages.add(f.path);
  rules.set(f.rule, r);
}
console.log(`${paths.length} pages, ${langs.length} languages, ${schemes.join(' and ')}: ${found.length} violations`);
for (const [id, r] of [...rules].sort((a, b) => b[1].nodes - a[1].nodes)) {
  console.log(`${id}\t${r.impact}\t${r.nodes} nodes in ${r.runs} page runs\t${[...r.pages].join(' ')}`);
  console.log(`\t${r.help}; e.g. ${r.example.lang} ${r.example.scheme} ${r.example.path}: ${r.example.targets.join(' | ')}`);
}
if (process.env.OUT) {
  writeFileSync(process.env.OUT, JSON.stringify(found, null, 1));
}
process.exitCode = found.length ? 1 : 0;
