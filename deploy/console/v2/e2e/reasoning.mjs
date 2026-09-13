// Console e2e — THE #reasoning SURFACE RENDERS THE AGENT'S REAL RECORDED WALK.
//
// ★ THIS ORACLE PASSED WHILE THE FEATURE HAD NEVER ONCE RENDERED IN PRODUCTION, and that is the finding
// worth keeping. The live view and this test were written together against the SAME IMAGINED DTO —
// dto.external_ref, dto.steps[], st.kind, st.reason, st.label, st.confidence, st.tools[], dto.alert_rule.
// The server sends NONE of those. It sends {id, ref, title, host, band, status, risk, conf, action, verdict,
// nodes[]} with nodes of {t, lb, ts, st, src, pay, plan[], gate, band, hash, verdict, conf, min_conf}
// (core/httpapi/session_detail.go). So `if(!dto.external_ref) return liveOrigReasoning()` was always true and
// the surface fell back to the labelled fixture on every load since it shipped — while liveState.reasonDto
// sat beside it fully populated with six real cycles. Verified live 2026-07-29 on the real console: the
// #reasoning view showed "FIXTURE · REPRESENTATIVE, NOT LIVE" over an invented host.
//
// A test that builds its own request AND its own response can only ever prove the code agrees with itself.
// THE PAYLOAD BELOW IS COPIED FROM A REAL RESPONSE of GET /v1/sessions/{ref} on the live control plane
// (session librenms-dc1-181284, 2026-07-29) — field names and shape verbatim. Do not "tidy" the names.
// core/httpapi/session_detail_contract_test.go pins the same key set on the Go side, so a rename breaks the
// build rather than silently reverting this surface to a fixture.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node reasoning.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const REF = 'librenms-dc1-181284';
const sessions = [{ external_ref: REF, band: 'AUTO', risk_level: 'high', verdict: 'match' }];

// --- verbatim shape of a real GET /v1/sessions/{ref} response ---
const trace = {
  id: REF, ref: REF, title: 'Service-up/down', host: 'dc1actualbudget01',
  band: 'AUTO', status: 'executed', risk: 'high', conf: 0.85, verdict: 'match',
  nodes: [
    { t: 'ingest', lb: 'Ingested (librenms)', ts: '2026-07-29T00:18:03Z', st: 'ok', src: 'ingest',
      pay: 'Service-up/down · critical · dc1actualbudget01', conf: 0, min_conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 1 — get-device-status', ts: '2026-07-29T00:18:53Z',
      st: 'investigate', src: 'agent-cycle',
      pay: 'Investigating alert: Service-up/down on dc1actualbudget01. Step 1: get device status to see if host is up, DISABLED, or healthy.',
      plan: ['get-device-status'], verdict: 'investigate', conf: 0, min_conf: 0 },
    { t: 'propose', lb: 'Proposal', ts: '2026-07-29T00:19:48Z', st: 'ok', src: 'propose',
      pay: 'The actualbudget container is exited (Exit code 143). A restart is the standard reversible fix per precedent.',
      plan: ['prompt: preamble/1'], band: 'AUTO', conf: 0.85, min_conf: 0 },
    // no honest reasoning glyph exists for a gate boundary — it must be OMITTED, never recoloured into one
    { t: 'gate', lb: 'Gate: execute', ts: '2026-07-29T00:19:30Z', st: 'ok', src: 'gate',
      pay: 'interceptor pass', gate: 'execute', verdict: 'pass', conf: 0, min_conf: 0 },
  ],
};

async function mount(page, traceResponse) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions } });
    if (p.startsWith('/v1/sessions/')) return traceResponse(route);
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#reasoning', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // liveReasonLoad(ref) is fully awaited inside liveAdopt() (it sets liveState.reasonDto to the DTO, the
  // "forbidden" sentinel on a 403, or null on any other failure — synchronously, no further async work) well
  // before liveState.lastRefresh, the last statement of liveAdopt(). Waiting for lastRefresh is the one signal
  // that is correct for BOTH scenarios this file mounts (a real trace, and a 403), unlike a content marker
  // that only exists for one of them.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'reasoning'); if (a) a.click(); });
  // The click above just re-invokes route('reasoning') synchronously over data that is already loaded (the
  // wait above already guaranteed that) — a reflow flush is enough margin for the DOM to settle, not a guess
  // at fetch latency that no longer applies here.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
}

const browser = await chromium.launch();
try {
  // ---- 1. the recorded walk renders ------------------------------------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ json: trace }));

    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/Step 1: get device status/.test(text),
      'the agent\'s recorded thought is not rendered — the surface is not reading the trace');
    ok(text.includes('get-device-status'),
      'the tool is not rendered as the citation; the "pivot to ground truth" control has nothing behind it');
    ok(text.includes('dc1actualbudget01'),
      'the real host is missing from the header');

    // THE ASSERTION THAT WOULD HAVE CAUGHT THE LIVE DEFECT. The fallback is silent by design: it renders a
    // complete, plausible, correctly-labelled page. Only naming the fixture's own content distinguishes
    // "live walk" from "fixture that looks fine".
    ok(!/REPRESENTATIVE, NOT LIVE/i.test(text),
      'the FIXTURE fallback is on screen — the live chain was not adopted. This was the exact production ' +
      'state on 2026-07-29: a complete-looking page, correctly labelled, built from invented data');
    ok(!/Disk pressure eviction cascade|payments-api|demo-w3/i.test(text),
      'FIXTURE CONTENT is still on screen next to the live walk');

    // A boundary with no honest glyph is omitted, not recoloured into one that exists.
    ok(!text.includes('interceptor pass'),
      'a gate boundary was rendered anyway — omit it rather than mislabel it as reasoning');

    // conf 0 means "not stated" in this DTO and must not draw a confidence trajectory. Only the proposal
    // states one (0.85), so exactly one trajectory may appear.
    const trajectories = await page.evaluate(() => document.querySelectorAll('#view .conf-traj').length);
    ok(trajectories === 1,
      `drew ${trajectories} confidence trajectories, want exactly 1 — only the step that STATED a confidence ` +
      `(0.85) may show one; a 0 is "not stated", not total no-confidence`);

    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 2. a 403 is stated, never rendered as an empty mind ---------------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ status: 403, json: { error: 'forbidden' } }));

    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/admin/i.test(text),
      'a 403 from the admin-gated trace read did not produce an explanation — a plain operator sees a blank ' +
      'or fixture chain and cannot tell "not permitted" from "the agent thought nothing"');
    ok(!/Step 1: get device status/.test(text), 'trace content rendered despite a 403');
    ok(errs.length === 0, `uncaught page errors on the 403 path: ${errs.join(' | ')}`);
    await page.close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('REASONING E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('REASONING E2E PASS — the SERVER-SHAPED walk renders with real thoughts, tools and stated confidence; the fixture fallback is proven absent; unglyphed boundaries are omitted; a 403 is stated plainly.');
