// Console e2e — A REFUSAL MUST NAME ITS CAUSE AND AN ACTION THAT WORKS.
//
// The workflow detail view answered every failed load with "The trace endpoint is unavailable (403) —
// retry." That was wrong in both halves. The endpoint was fine: the session simply did not carry the
// elevated trace-read role, because the grant lived in a process-local map that every restart emptied while
// the cookie stayed valid (REQ-527). And "retry" is the one instruction that cannot work — the pre-!683
// client followed it 28,094 times at ~75 req/s, and the operator reading the same sentence retried by hand
// and lost time before logging out and back in, which was the actual fix.
//
// THE ASSERTION IS ABOUT MEANING, NOT PRESENCE. A test that only checked "an error is shown" passed
// throughout: an error WAS shown, tens of thousands of times, saying the wrong thing. So this oracle pins
// three properties per status — the cause is named, the advice is the advice that works, and the advice that
// does NOT work is absent from the case where it would mislead.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const REF = 'librenms-dc1-181284';
const sessions = { sessions: [{ external_ref: REF, band: 'AUTO', risk_level: 'low', verdict: 'match', status: 'verified' }], total: 1 };

const cases = [
  {
    status: 403,
    mustSay: [/trace-read role/i, /sign in again/i],
    mustNotSay: [/unavailable/i, /^.*\bretry\b.*$/i],
    why: 'a 403 here is a missing ROLE, and signing in again is what restores it; "unavailable" misnames the ' +
      'cause and "retry" is the instruction that produced a 28,094-request storm',
  },
  {
    status: 404,
    mustSay: [/not found/i],
    mustNotSay: [/trace-read role/i],
    why: 'an absent session is not a role problem, and telling an operator to sign in again would send them ' +
      'to fix an account that is not broken',
  },
  {
    status: 500,
    mustSay: [/500/, /retry/i],
    mustNotSay: [/trace-read role/i],
    why: 'a server fault IS the case where retrying is reasonable, so the advice belongs here and only here',
  },
];

const browser = await chromium.launch();
try {
  for (const c of cases) {
    const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(x => x.newPage());
    const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e).slice(0, 100)));
    await page.route('**/api/**', route => {
      const p = route.request().url().split('/api')[1].split('?')[0];
      if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
      if (p === '/v1/sessions') return route.fulfill({ json: sessions });
      if (p === '/v1/sessions/' + REF) return route.fulfill({ status: c.status, body: 'denied' });
      if (p.endsWith('/stream')) return route.fulfill({ status: c.status, body: '' });
      return route.fulfill({ json: {} });
    });
    await page.goto(BASE + '/index.html#workflows', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
    // Wait for the detail pane to reach its TERMINAL state — excluding the two known non-final paints
    // ("Select a run." before anything is chosen, "Loading the governed walk…" while the detail fetch is
    // in flight) so this cannot resolve early against an intermediate placeholder (same trap as the
    // "Reading…" drawer paint elsewhere in this suite; mirrors detail-403-no-storm.mjs's equivalent wait).
    const settled = () => {
      const t = (document.querySelector('.wf-empty')?.innerText || '').trim();
      return t.length > 0 && t !== 'Select a run.' && t !== 'Loading the governed walk…' && t !== 'No runs match this facet.';
    };
    await page.waitForFunction(settled).catch(() => {});
    await page.evaluate(() => { const r = document.querySelectorAll('.wf-run'); if (r.length) r[0].click(); });
    // The click re-selects the same (only) run — wfOnSelect's _loaded/_failed latch makes its fetch a
    // no-op the second time, so this is already satisfied; kept as its own wait in case the click ever
    // targets a different row.
    await page.waitForFunction(settled).catch(() => {});

    const msg = await page.evaluate(() => {
      const el = [...document.querySelectorAll('.wf-empty')].map(n => n.innerText).join(' ');
      return el || '(no .wf-empty rendered)';
    });
    for (const re of c.mustSay) {
      ok(re.test(msg), `status ${c.status}: the message does not satisfy ${re} — ${c.why}. Got: "${msg}"`);
    }
    for (const re of c.mustNotSay) {
      ok(!re.test(msg), `status ${c.status}: the message still says ${re}, which misleads — ${c.why}. Got: "${msg}"`);
    }
    ok(pageErrors.length === 0, `status ${c.status}: uncaught JS error — ${pageErrors.join(' | ')}`);
    await page.close();
  }
} finally { await browser.close(); }

if (failures.length) { console.error('DETAIL-ERROR-COPY E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('DETAIL-ERROR-COPY E2E PASS — 403 names the missing role and the action that restores it, 404 says the session is absent, and "retry" appears only where retrying can actually help.');
