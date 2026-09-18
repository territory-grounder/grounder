// Console e2e — TG-236 oracles 1, 2 and 4: the proposals view as a DECISION surface.
//
// The 9-column chronological table it replaces was correct, live and zero-fabrication — and an operator
// facing 200 rows of it learned nothing they could act on. These three oracles are the epic's own
// definition of done, and each has a failure mode that a screenshot would not reveal.
//
// The payload is the SERVER'S shape (core/httpapi/proposals.go): flat rows plus an optional
// `counterfactual`. Nothing here is invented for the renderer's convenience — the sibling suite that did
// exactly that is why the candidates queue showed "0× / 0 host(s)" in production for months.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

/* `band` is part of the row because the surface DERIVES "why it can't" from it. A fixture that omitted it
   is how the original copy ("no registered op-class covers this") went unchallenged for every row while
   production had a registered op-class on 1,242 of 1,485 — the fixture simply never showed the case the
   operator actually sees. Fixtures here carry the fields production writes. */
const row = (host, rule, op, opClass, when, rationale, band) => ({
  external_ref: 'librenms-' + host + '-' + when, host, alert_rule: rule, op, op_class: opClass,
  band: band === undefined ? 'POLL_PAUSE' : band,
  rationale, undo_sketch: 'stop the unit', confidence: 0.9, attribution: '', created_at: when,
});
// Eight occurrences, THREE shapes — the collapse this surface exists for.
const proposals = [
  row('app01', 'Service-up/down', 'restart nginx', 'restart-proxy', '2026-08-01T04:00:00Z', 'proxy wedged'),
  row('app02', 'Service-up/down', 'restart nginx', 'restart-proxy', '2026-08-01T03:00:00Z', 'proxy wedged'),
  row('app03', 'Devices-up/down', 'restart nginx', 'restart-proxy', '2026-08-01T02:00:00Z', 'proxy wedged'),
  row('db01', 'DiskFull-90', 'prune journal', 'prune-journal', '2026-08-01T01:00:00Z', 'journal filled /'),
  row('db01', 'DiskFull-90', 'prune journal', 'prune-journal', '2026-07-31T23:00:00Z', 'journal filled /'),
  row('cache01', 'Service-up/down', 'flush cache', 'flush-cache', '2026-07-31T22:00:00Z', 'stale keys'),
];

async function mount(page, counterfactual, rowsOverride) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/proposals') {
      const rows = rowsOverride || proposals;
      const body = { proposals: rows, total: rows.length };
      if (counterfactual !== undefined) body.counterfactual = counterfactual;
      return route.fulfill({ json: body });
    }
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#proposals', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'proposals'), null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'proposals'); if (a) a.click(); });
  await page.waitForFunction(() => {
    const t = document.querySelector('#view')?.innerText || '';
    return t.trim().length > 0 && !/Loading live shadow/i.test(t);
  }, null, { timeout: 20000 });
}

const browser = await chromium.launch();
try {
  // ORACLE 2 + 1 — shapes, and each shape answering all three questions.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { window_days: 7, incidents: 17, addressed: 14, executed: 5 });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');

    const shapes = await page.evaluate(() => document.querySelectorAll('#view [data-q="shape"]').length);
    ok(shapes === 3, `oracle 2: 6 occurrences must collapse into 3 shapes, got ${shapes}`);

    // Grouping is by (op_class, op) — cross-HOST recurrence is the point, so three hosts sharing a
    // remedy must be ONE shape, not three. This is the assertion that fails if someone "improves" the
    // grouping by adding host or rule to the key.
    ok(/3× across 3 hosts/.test(text),
       'oracle 2: a remedy seen on three hosts is ONE shape with a cross-host count');

    // Most-recurring first: the shape worth ratifying should be met before the singletons.
    const firstShape = await page.evaluate(() => document.querySelector('#view [data-q="shape"]')?.innerText || '');
    ok(/restart-proxy/.test(firstShape), `oracle 2: shapes must be ordered most-recurring first, got ${JSON.stringify(firstShape.slice(0, 40))}`);

    // Case-insensitive on purpose: the labels are uppercased by CSS (text-transform), so innerText
    // returns what the OPERATOR sees rather than what the source says. Asserting the source spelling
    // would fail on a styling change that altered nothing an operator experiences.
    for (const q of ['What broke', 'What TG would do', "Why it can't"]) {
      ok(new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i').test(text),
         `oracle 1: every shape must answer "${q}" on this surface`);
    }
    /* The reason must be STRUCTURAL (a property of the shape) but not IDENTICAL for every shape: an
       unregistered class needs ratifying, a POLL_PAUSE needs a vote, and telling an operator the wrong one
       sends them to the wrong place. Every shape here has a registered op-class and stopped at the vote. */
    ok(/requires a human vote/i.test(text),
       'oracle 1: a shape whose occurrences all stopped at POLL_PAUSE must say so — the blocker is the vote');
    ok(!/no registered op-class covers this/.test(text),
       'oracle 1: shapes WITH a registered op-class must not claim none covers them (the old hardcoded copy)');

    // ORACLE 4 — the headline, with a real denominator.
    ok(/This week TG proposed a remedy for 14 of 17 incidents/.test(text),
       'oracle 4: the counterfactual headline must render the server\'s numbers verbatim');
    ok(/carried out 5\b/.test(text) && /other 9\b/.test(text),
       'oracle 4: the headline must SPLIT what TG did (5) from what it was stopped before (14-5=9) — ' +
       'one blended figure invites granting a capability that is partly already granted');

    // The log survives as evidence — a summary nobody can audit is a summary nobody should trust.
    ok(/Every occurrence, newest first \(6 rows\)/.test(text),
       'the chronological log must remain available beneath the shapes');
    await page.context().close();
  }

  // ORACLE 4, degraded — the server omits the headline (its store could not answer). The surface must
  // stay useful and must NOT invent a ratio; "0 of 0" would read as "TG did nothing".
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, undefined);
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    // Assert the ABSENCE OF THE ELEMENT, not the absence of one phrasing. Forbidding only the string
    // "would have addressed" let a real mutation through: defaulting an unserved counterfactual to
    // {incidents: 0} renders the QUIET-WEEK headline instead, which passes a text check while telling
    // the operator "no incidents in the last 7 days" when the truth is "the store could not answer".
    // A silence rendered as a clean bill of health is the exact failure this surface exists to prevent.
    const headlines = await page.evaluate(() =>
      document.querySelectorAll('#view [data-q="headline"]').length);
    ok(headlines === 0,
       `oracle 4: with no counterfactual served, NO headline may be invented (found ${headlines})`);
    const shapes = await page.evaluate(() => document.querySelectorAll('#view [data-q="shape"]').length);
    ok(shapes === 3, 'the shapes must still render when the headline is absent');
    await page.context().close();
  }

  /* The OTHER branch of "why it can't": a shape with NO registered op-class. Both branches need a case,
     because a derived sentence that is only ever exercised on one input is a hardcoded sentence wearing a
     function's clothes — which is exactly what this replaced. */
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, undefined, [
      row('edge01', 'Novel-alert', 'rotate wireguard key', '', '2026-08-01T05:00:00Z', 'no class for this', 'POLL_PAUSE'),
      row('edge02', 'Novel-alert', 'rotate wireguard key', '', '2026-08-01T04:30:00Z', 'no class for this', 'POLL_PAUSE'),
    ]);
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/no registered op-class covers this/.test(text),
       'oracle 1: a shape with NO registered op-class must say so — that shape needs ratifying, not a vote');
    ok(!/requires a human vote/i.test(text),
       'oracle 1: an unregistered shape must not be reported as merely awaiting a vote — ratifying is the ' +
       'action that unblocks it, and naming the wrong blocker sends the operator to the wrong surface');
    await page.context().close();
  }

  // A quiet week is not a broken one.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { window_days: 7, incidents: 0, addressed: 0 });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/No incidents in the last 7 days/.test(text) && /quiet, not broken/.test(text),
       'oracle 4: a zero-incident week must read as quiet, never as "addressed 0 of 0"');
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('proposals-decision-surface FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('proposals-decision-surface: OK');
