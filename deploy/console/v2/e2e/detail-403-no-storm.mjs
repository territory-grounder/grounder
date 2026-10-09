// Console e2e — A REJECTED DETAIL LOAD MUST NOT BECOME A REQUEST STORM.
//
// Measured live on production 2026-07-28: one idle tab, left on a workflow run whose detail endpoint answers
// 403, produced 28,094 rejected requests against 1 success — ~75 req/s, sustained for hours, through the same
// nginx that carries the LibreNMS and Alertmanager ingest lanes. The console was DoSing its own control
// plane from a single browser tab, and nothing in the product noticed.
//
// The mechanism was a CYCLE, not a retry policy: wfOnSelect's catch cleared `_loading` and called route(),
// route() re-rendered the workflows view, the view called its wfSelect hook for the still-selected run, and
// the guard let it through again because `_loaded` was false and `_loading` had just been reset. No delay
// anywhere in the loop, so it spun as fast as the network answered.
//
// THE ASSERTION IS A COUNT, NOT A BEHAVIOUR. A test that only checked "the error renders" passed throughout
// the outage — the error DID render, tens of thousands of times. What was missing was a bound on how many
// times the client may ask a question it has already been refused, so that is what this oracle measures.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const REF = 'librenms-dc1-181145';
const sessions = {
  sessions: [{ external_ref: REF, band: 'AUTO', risk_level: 'low', verdict: 'match', status: 'verified' }],
  total: 1,
};

// Each status is a DIFFERENT reason to refuse, and every one of them re-armed the loop before the latch:
// 403 (role denied — the production case), 500 (server fault), 404 (absent trace).
for (const status of [403, 500, 404]) {
  const browser = await chromium.launch();
  try {
    const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(c => c.newPage());
    const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e).slice(0, 100)));

    let detailHits = 0;
    await page.route('**/api/**', route => {
      const p = route.request().url().split('/api')[1].split('?')[0];
      if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
      if (p === '/v1/sessions') return route.fulfill({ json: sessions });
      if (p === '/v1/sessions/' + REF) { detailHits++; return route.fulfill({ status, body: 'denied' }); }
      if (p.endsWith('/stream')) return route.fulfill({ status, body: '' });
      return route.fulfill({ json: {} });
    });

    await page.goto(BASE + '/index.html#workflows', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
    // Wait for the run row itself — the exact element the click below looks for — rather than a boot-settle
    // guess. Mirrors the same find() the click uses, so it never resolves against the "Loading the recorded
    // sessions…" mid-boot placeholder (that text creates none of these matches).
    await page.waitForFunction(() => !!document.querySelector('.wf-run') || !!document.querySelector('[data-wf-run]') ||
      [...document.querySelectorAll('#view *')].some(n => /librenms-dc1/.test(n.textContent || '') && n.tagName !== 'BODY')
    ).catch(() => {});

    // Select the run — this is the click that armed the storm.
    await page.evaluate(() => {
      const el = document.querySelector('.wf-run') || document.querySelector('[data-wf-run]') ||
        [...document.querySelectorAll('#view *')].find(n => /librenms-dc1/.test(n.textContent || '') && n.tagName !== 'BODY');
      if (el) el.click();
    });
    // Wait for the detail pane to reach its TERMINAL state for this selection — excluding the two known
    // non-final paints ("Select a run." before anything is chosen, "Loading the governed walk…" while the
    // fetch is in flight) so this cannot resolve early against an intermediate placeholder (the same trap as
    // the "Reading…" drawer paint elsewhere in this suite). detailHits is already incremented by the time
    // this text lands, since the mock's route handler runs before the fetch promise the render awaits
    // resolves.
    await page.waitForFunction(() => {
      const t = (document.querySelector('.wf-empty')?.innerText || '').trim();
      return t.length > 0 && t !== 'Select a run.' && t !== 'Loading the governed walk…' && t !== 'No runs match this facet.';
    }).catch(() => {});
    const afterSelect = detailHits;

    // Then SIT IDLE, exactly as the production tab did. A latched client asks zero further times; the
    // storm added thousands in this window. Class-3 measurement window: intentional fixed wait, MUST NOT
    // become a condition-wait (proves a request storm does NOT happen over a real window — there is no DOM
    // event for "nothing fired again" to wait on instead).
    await page.waitForTimeout(4000);
    const afterIdle = detailHits;

    ok(afterSelect <= 2, `status ${status}: the detail endpoint was called ${afterSelect} times for ONE selection — ` +
      `a refused load must be latched, not retried in the render cycle`);
    ok(afterIdle === afterSelect, `status ${status}: an IDLE tab issued ${afterIdle - afterSelect} further ` +
      `request(s) over 4s (${afterSelect} -> ${afterIdle}) — this is the production storm: 28,094 rejected ` +
      `requests from one tab, at ~75 req/s, against the nginx that also carries alert ingest`);
    ok(pageErrors.length === 0, `status ${status}: uncaught JS error — ${pageErrors.join(' | ')}`);

    // The operator must still be TOLD. A silent latch would trade a storm for a blank panel.
    //
    // This asserts an explanation EXISTS, deliberately not its wording. The first version matched
    // /unavailable|retry/ — the exact words of the message at the time — and went RED the moment that copy
    // was corrected, even though the property it cares about still held. An oracle that pins another
    // oracle's subject turns every intentional change into a false failure; the wording is owned by
    // detail-error-copy.mjs, which asserts what the sentence must MEAN per status.
    const shown = await page.evaluate(() => {
      const el = [...document.querySelectorAll('.wf-empty')].map(n => n.innerText.trim()).filter(Boolean);
      return el.join(' ');
    });
    ok(shown.length >= 20, `status ${status}: the refusal was latched but the operator is told nothing — a ` +
      `silent latch trades a request storm for a blank panel. Rendered explanation: "${shown}"`);
  } finally { await browser.close(); }
}

if (failures.length) { console.error('DETAIL-403-NO-STORM E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('DETAIL-403-NO-STORM E2E PASS — a refused workflow-detail load is latched (bounded request count), an idle tab issues no further requests, and the operator is still told why.');
