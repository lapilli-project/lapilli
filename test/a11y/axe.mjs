// Checks every page lapilli serve gives TestA11yServe with axe-core, against
// WCAG 2.2 AA, in each language, in light and in dark; that no page
// scrolls sideways at 320 CSS px (WCAG 1.4.10); that the rules list's
// table, with rules named as long as a real set's (longNamed), and the
// overview's table of the weeks fit their boxes on a laptop's screen, 1280
// CSS px and wider; that a checkbox is on the line of its words, at 320,
// 1280, 1440 and 1920 CSS px; and that a disabled field looks it, in light
// and in dark. Run by axe.sh.
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
    // A disabled field is drawn as a disabled button is, whatever rule
    // is more specific: dim words on the panel, a dashed edge.
    const plain = await page.evaluate(() => {
      const probe = document.createElement('div');
      probe.style.color = 'var(--dim)';
      probe.style.backgroundColor = 'var(--panel)';
      document.body.append(probe);
      const { color, backgroundColor } = getComputedStyle(probe);
      probe.remove();
      return [...document.querySelectorAll('input:disabled, select:disabled, textarea:disabled')]
        .filter((x) => x.offsetParent && !['checkbox', 'radio', 'hidden'].includes(x.type))
        .filter((x) => {
          const s = getComputedStyle(x);
          return s.borderTopStyle !== 'dashed' || s.backgroundColor !== backgroundColor || s.color !== color;
        })
        .map((x) => x.name || x.id || x.tagName);
    });
    if (plain.length) {
      found.push({ rule: 'disabled-look', impact: 'moderate', path, lang, scheme, nodes: plain.length, targets: plain.slice(0, 3),
        help: 'A disabled field is drawn as one to fill' });
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

// fits are the tables that fit the box they scroll in on a laptop's
// screen and wider: the rules list's (at 1280 and 1440 CSS px the page is
// the table's, at 1600 and 1920 the facets are beside it), and the
// overview's table of the weeks. Narrower, or zoomed in, a table scrolls
// in its box, as a wide table may.
const fits = [
  { pages: (p) => p === '/rules' || p.startsWith('/rules?'), boxes: '.rules-main .scroll', what: 'The rules table' },
  { pages: (p) => p === '/' || p.startsWith('/?'), boxes: '.scroll[aria-labelledby="by-week"]', what: 'The overview\'s table of the weeks' },
];

// fit is a table of fits wider than its box, at the widths of a laptop's
// screen and wider.
async function fit(lang) {
  for (const width of [1280, 1440, 1600, 1920]) {
    const context = await browser.newContext({ viewport: { width, height: 900 }, extraHTTPHeaders: { 'Accept-Language': lang } });
    const page = await context.newPage();
    for (const f of fits) {
      for (const path of paths.filter(f.pages)) {
        await page.goto(base + path);
        const over = await page.evaluate((sel) => [...document.querySelectorAll(sel)]
          .filter((box) => box.offsetParent && box.querySelector('table'))
          .map((box) => box.querySelector('table').scrollWidth - box.clientWidth)
          .filter((x) => x > 0), f.boxes);
        if (over.length) {
          found.push({ rule: 'table-fit', impact: 'serious', path, lang, scheme: `${width}px`, nodes: over.length,
            targets: over.map((x) => `${x}px wider than its box`), help: `${f.what} is wider than its box on a laptop's screen` });
        }
      }
    }
    await context.close();
  }
}

// boxed is what has a checkbox apart from its words: a label holding one
// draws it on the line of its words, before them (the stylesheet's
// label:has(> input[type=checkbox])), at a phone's width and a laptop's
// and wider. As a field's label it stood on a line of its own, centred.
async function boxed(lang) {
  for (const width of [320, 1280, 1440, 1920]) {
    const context = await browser.newContext({ viewport: { width, height: 900 }, extraHTTPHeaders: { 'Accept-Language': lang } });
    const page = await context.newPage();
    for (const path of paths) {
      await page.goto(base + path);
      const apart = await page.evaluate(() => [...document.querySelectorAll('label')]
        .filter((l) => l.offsetParent && l.querySelector(':scope > input[type=checkbox]'))
        .filter((l) => {
          const box = l.querySelector(':scope > input[type=checkbox]').getBoundingClientRect();
          const words = [...l.childNodes].filter((n) => n.nodeName !== 'INPUT' && n.textContent.trim());
          if (!words.length) {
            return false;
          }
          const r = document.createRange();
          r.setStartBefore(words[0]);
          r.setEndAfter(words[words.length - 1]);
          const first = [...r.getClientRects()].find((x) => x.width > 0);
          return !first || box.right > first.left + 1 || box.bottom <= first.top || box.top >= first.bottom;
        })
        .map((l) => l.textContent.trim().slice(0, 40)));
      if (apart.length) {
        found.push({ rule: 'checkbox-line', impact: 'moderate', path, lang, scheme: `${width}px`, nodes: apart.length,
          targets: apart, help: 'A checkbox stands apart from its words' });
      }
    }
    await context.close();
  }
}

await Promise.all(langs.flatMap((l) => [...schemes.map((s) => check(l, s)), reflow(l), fit(l), boxed(l)]));
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
