// A person at lapilli serve in a headless Chromium, for service.sh: signs in
// at Dex through its password form, as a person would, then opens the UI's
// main pages, checks each answers and says what it should, and saves a
// screenshot of each.
//
// BASE is where people reach the UI (--external-url); RESOLVE, Chromium's
// --host-resolver-rules, which send BASE's name and Dex's to the addresses
// in front of them, as DNS would; WHO and PASSWORD, who signs in; OUT, the
// directory the screenshots go to; PAGES, a line for each page: its path,
// a tab, and a regular expression its text must match. Exits 1 when a page
// doesn't answer, doesn't say what it should, or has an error in it.
import { mkdirSync } from 'node:fs';
import { chromium } from 'playwright';

const base = process.env.BASE;
const out = process.env.OUT;
const pages = process.env.PAGES.trim().split('\n').map((line) => line.split('\t'));
mkdirSync(out, { recursive: true });

const browser = await chromium.launch({ args: [`--host-resolver-rules=${process.env.RESOLVE}`] });
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, colorScheme: 'light' });
const page = await context.newPage();
const problems = [];
page.on('pageerror', (e) => problems.push(`${page.url()}: ${e.message}`));
page.on('console', (m) => m.type() === 'error' && problems.push(`${page.url()}: console: ${m.text()}`));
page.on('response', (r) => {
  if (r.url().startsWith(base) && r.status() >= 400) problems.push(`${r.url()}: ${r.status()}`);
});

// Not signed in, a page sends the browser to Dex, and back after.
await page.goto(`${base}/rules`);
await page.waitForSelector('input[name="login"]');
await page.screenshot({ path: `${out}/00-dex.png` });
await page.fill('input[name="login"]', process.env.WHO);
await page.fill('input[name="password"]', process.env.PASSWORD);
await Promise.all([page.waitForURL(`${base}/rules`), page.click('button[type="submit"]')]);
console.log(`ok: ${process.env.WHO} signed in at Dex, and came back to ${page.url()}`);

let failed = 0;
for (const [i, [path, want]] of pages.entries()) {
  const res = await page.goto(base + path);
  const text = await page.innerText('body');
  const name = `${String(i + 1).padStart(2, '0')}-${path.replace(/^\/$/, 'overview').replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, '')}.png`;
  await page.screenshot({ path: `${out}/${name}`, fullPage: true });
  if (res.status() !== 200 || !new RegExp(want).test(text)) {
    console.log(`FAILED: ${path}: ${res.status()}, its text doesn't match /${want}/:\n${text.slice(0, 2000)}`);
    failed++;
  } else {
    console.log(`ok: ${path} says /${want}/ (${name})`);
  }
}
for (const p of problems) {
  console.log(`FAILED: ${p}`);
  failed++;
}
await browser.close();
process.exitCode = failed ? 1 : 0;
