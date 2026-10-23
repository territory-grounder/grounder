// Console e2e — the header clock tells the REAL time.
//
// It was a fixture epoch (new Date(2026,6,16,14,31,12)) ticking forward at 1s: it read 14:31 while the real
// time was 23:33, sitting beside genuinely live counters — a ticking timestamp next to live data reads as
// live, and an operator timestamping an incident from it would be hours off. The oracle compares the
// rendered #clock against the PAGE'S OWN clock (same JS realm, so timezone and skew cancel) and rejects any
// fixture base: a wrong-by-hours clock cannot pass, and a right clock cannot fail on a slow CI runner
// (tolerance covers render latency, not drift).
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await page.route('**/api/**', r => {
    const u = r.request().url().split('/api')[1].split('?')[0];
    if (u === '/v1/whoami') return r.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    return r.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });

  const read = () => page.evaluate(() => {
    const el = document.querySelector('#clock');
    if (!el || !el.textContent.trim()) return null;
    const [h, m, s] = el.textContent.trim().split(':').map(Number);
    const now = new Date();
    const shown = h * 3600 + m * 60 + s;
    const real = now.getHours() * 3600 + now.getMinutes() * 60 + now.getSeconds();
    // circular distance across midnight
    const diff = Math.min(Math.abs(shown - real), 86400 - Math.abs(shown - real));
    return { text: el.textContent.trim(), diff };
  });

  const first = await read();
  ok(first !== null, 'the #clock element is missing or empty');
  if (first) {
    ok(first.diff <= 5, `the header clock reads ${first.text}, ${first.diff}s away from the page's own time — ` +
      'a fixture epoch presented as live; an operator timestamping an incident from it would be wrong');
  }
  // And it must actually TICK — a frozen correct clock becomes a wrong clock within a minute. Wait for the
  // #clock text to differ from the first sample (the same "wait for a ticker to visibly change" idiom
  // approval-header-never-invents.mjs uses for .wf-el) rather than guessing an interval long enough to
  // contain one tick: console.html's tick()/setInterval(tick,1000) advances the clock every 1000ms, so this
  // resolves in about a second instead of blocking a fixed 2300ms, and still cannot pass on a frozen clock.
  await page.waitForFunction((prev) => {
    const el = document.querySelector('#clock');
    const t = el ? el.textContent.trim() : '';
    return t !== '' && t !== prev;
  }, first && first.text).catch(() => {});
  const second = await read();
  ok(second !== null && first !== null && second.text !== first.text,
    `the clock did not advance (${first && first.text} -> ${second && second.text}) — frozen right is soon wrong`);
  ok(pageErrors.length === 0, 'uncaught JS errors: ' + pageErrors.join(' | '));
} finally { await browser.close(); }

if (failures.length) { console.error('CLOCK E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('CLOCK E2E PASS — the header clock matches the real time and ticks.');
