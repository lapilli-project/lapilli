// Takes the README's screenshots of lapilli serve from the demo ledger
// TestDemoServe serves: the overview and a noisy rule's page, 1240×980, in
// English, light and dark. Run by shots.sh.
//
// BASE is where the demo is served; OUT, the directory the PNGs are
// written to.
import { chromium } from 'playwright';

const base = process.env.BASE;
const out = process.env.OUT ?? '.';
const shots = [
  ['/', 'serve-dashboard'],
  ['/rules/CheckoutLatencyP99?weeks=4', 'serve-rule'],
];

const browser = await chromium.launch();
for (const scheme of ['light', 'dark']) {
  const context = await browser.newContext({
    viewport: { width: 1240, height: 980 }, deviceScaleFactor: 1, colorScheme: scheme,
    locale: 'en-US', timezoneId: 'UTC', extraHTTPHeaders: { 'Accept-Language': 'en' },
  });
  const page = await context.newPage();
  for (const [path, name] of shots) {
    const res = await page.goto(base + path, { waitUntil: 'networkidle' });
    if (!res.ok()) {
      throw new Error(`${path}: ${res.status()}`);
    }
    await page.evaluate(() => document.fonts.ready);
    const file = `${out}/${name}${scheme === 'dark' ? '-dark' : ''}.png`;
    await page.screenshot({ path: file });
    console.log(file);
  }
  await context.close();
}
await browser.close();
