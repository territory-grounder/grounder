// Console e2e — THE "GROUND TRUTH" CITATION ACTUALLY OPENS THE OBSERVATION (TG-272).
//
// ★ WHAT THIS REPLACES. Under every reasoning step the design renders a button reading "ground truth <tool>",
// and the view's closing line tells the operator "every claim, one click from ground truth". The click ran
// `toast("Pivoting to ground truth: " + cite)` — it NAMED the pivot instead of performing it. The owner
// clicked them and reported: "none of these buttons producing anything".
//
// It could not have been wired: nothing stored what a tool returned. agent_step.observation holds a 30–40
// character reference ("observed incident-history-dc1pve01"), there was no artifact table, and the
// screened payload was shown to the model and dropped. Measured live 2026-08-03: 3241 sessions, 17759 steps,
// zero stored tool results. Migration 0053 keeps it; this proves the console opens it.
//
// THE TRAP THIS SUITE IS BUILT AROUND: h() binds handlers with addEventListener, so re-assigning .onclick
// would ADD a listener and the original toast would still fire. The button must be REPLACED. A test that
// only asserted "a panel appeared" would pass with the toast still firing underneath it, so this asserts the
// toast's own wording is ABSENT.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node reasoning-ground-truth-opens.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const REF = 'librenms-dc1-183957';
const sessions = [{ external_ref: REF, host: 'dc1pve01', band: '', classified_at: '2026-08-03T08:53:01Z' }];

// Verbatim shape of GET /v1/sessions/{ref}: one cycle WITH stored evidence (ev set) and one WITHOUT — the
// second is every session recorded before capture existed, and the console must say so rather than hang.
const trace = {
  id: REF, ref: REF, title: 'Service-up/down', host: 'dc1pve01', status: 'stopped', conf: 0,
  nodes: [
    { t: 'ingest', lb: 'Ingested', pay: 'Service-up/down · critical · dc1pve01', conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 4 — check-host-services', st: 'investigate', src: 'agent-cycle',
      pay: 'check-host-services returned unreachable/errored, so I cannot yet name the failing unit.',
      plan: ['check-host-services'], ev: 'check-host-services-dc1pve01', conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 5 — get-incident-history', st: 'investigate', src: 'agent-cycle',
      pay: 'Checking TG\'s own record for this Service-up/down family on this host.',
      plan: ['get-incident-history'], conf: 0 },   // NO ev: nothing was stored for this step
  ],
};

const EVIDENCE_BODY =
  '● faultgen-restore-101101011.service loaded failed failed [systemd-run] /usr/sbin/pct start 101101011\n' +
  '● faultgen-restore-101101013.service loaded failed failed [systemd-run] /usr/sbin/pct start 101101013';

async function mount(page, { evidence = 'ok' } = {}) {
  const hits = [];
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions, total: 1871 } });
    if (p.includes('/evidence/')) {
      hits.push(p);
      if (evidence === '404') return route.fulfill({ status: 404, json: { error: 'not found' } });
      if (evidence === '403') return route.fulfill({ status: 403, json: { error: 'forbidden' } });
      return route.fulfill({ json: {
        ref: REF, cycle: 4, id: 'check-host-services-dc1pve01', tool: 'check-host-services',
        payload: EVIDENCE_BODY, truncated: false, full_bytes: EVIDENCE_BODY.length,
      } });
    }
    if (p.startsWith('/v1/sessions/')) return route.fulfill({ json: trace });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#reasoning', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Wait for the REAL trace to be on screen — not merely for SOME `.cite` button, which also exists on the
  // design-fixture fallback views.reasoning() shows mid-boot before liveState.reasonDto has loaded (a
  // `.cite`-only condition resolves against that fixture and returns before the real walk ever renders).
  // "check-host-services" is unique to this file's trace fixture and is exactly what the first check below
  // reads, so waiting on it is both discriminating and derived from the real postcondition.
  const booted = () => /check-host-services/.test(document.querySelector('#view')?.innerText || '');
  await page.waitForFunction(booted, null, { timeout: 20000 }).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'reasoning'); if (a) a.click(); });
  await page.waitForFunction(booted, null, { timeout: 20000 }).catch(() => {});
  return hits;
}

// Click the citation whose label names `tool`. Returns false (recorded) rather than throwing, so one missing
// control cannot hide every assertion after it behind a Playwright timeout.
async function clickCite(page, tool, why) {
  const found = await page.evaluate(t => {
    const b = [...document.querySelectorAll('#view .cite')].find(x => (x.innerText || '').includes(t));
    if (!b) return false;
    b.click();
    return true;
  }, tool);
  if (!found) ok(false, `no "ground truth ${tool}" citation exists on the page, so ${why} cannot be exercised`);
  // Wait for the drawer to open WITH ITS FINAL CONTENT — but only if a click actually happened; waiting on
  // a condition nothing will ever trigger would just burn the full timeout. liveOpenEvidence() paints the
  // drawer TWICE: an immediate "Reading…" placeholder, then the real content once the evidence fetch (or
  // its 404/403) resolves. A wait on `.open` alone is satisfied by that first paint — this reads exactly
  // like the mount() trap above, just one render deeper — so the condition must also rule out the
  // placeholder text, or drawerText() below can read "Reading…" instead of the answer.
  if (found) await page.waitForFunction(() => {
    const d = document.querySelector('#drawer');
    return !!d && d.classList.contains('open') && !d.innerText.includes('Reading…');
  }).catch(() => {});
  return found;
}

const drawerText = page => page.evaluate(() => {
  const d = document.querySelector('#drawer');
  return (d && d.classList.contains('open')) ? (d.innerText || '') : '';
});

const browser = await chromium.launch();
try {
  // ---- 1. the click FETCHES and RENDERS what the tool returned ------------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    const hits = await mount(page);

    const text0 = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(text0.length > 200, `#reasoning rendered only ${text0.length} chars — nothing below is meaningful`);
    ok(/check-host-services/.test(text0), 'the citation is not on the page; the walk did not render');

    const clicked = await clickCite(page, 'check-host-services', 'the load-bearing "does the citation open" check');
    if (clicked) {
      ok(hits.length > 0,
        'clicking the citation issued NO /v1/sessions/{ref}/evidence/{id} read — THE DEFECT: the button ' +
        'raised a toast naming the pivot instead of performing it');

      const d = await drawerText(page);
      ok(d.length > 0, 'no panel opened — the click still goes nowhere');
      ok(/faultgen-restore-101101011/.test(d),
        'the panel opened but does NOT contain what the tool returned — the citation shows something other ' +
        'than the ground truth it promises');
      ok(/check-host-services/.test(d), 'the panel does not name which tool produced this observation');

      // THE TOAST MUST BE GONE, not merely covered. h() binds via addEventListener, so a fix that assigns
      // .onclick leaves the original handler live and this whole suite would pass with it still firing.
      const toastTxt = await page.evaluate(() => document.querySelector('#toast')?.innerText || '');
      ok(!/Pivoting to ground truth/i.test(toastTxt),
        `the old stub handler ALSO fired (toast: "${toastTxt}") — the button was rebound, not replaced, so ` +
        'the original addEventListener handler is still attached');
    }
    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 2. a step with NO stored evidence says so, and never issues a read -------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    const hits = await mount(page);

    if (await clickCite(page, 'get-incident-history', 'the "nothing was recorded" path')) {
      const d = await drawerText(page);
      ok(d.length > 0, 'clicking a citation with no stored evidence opened nothing at all — silent dead end');
      ok(/predates evidence capture|never kept/i.test(d),
        'a step whose ground truth was never stored does not SAY so — an operator cannot tell "nothing was ' +
        'kept" from "the tool returned nothing", and only one of those is about the estate');
      ok(hits.length === 0,
        'a read was issued for a step carrying no evidence id — that is a guaranteed 404 round-trip on every click');
    }
    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 3. a 404 from the server is the same ordinary answer, not an error banner ------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, { evidence: '404' });

    if (await clickCite(page, 'check-host-services', 'the server-404 path')) {
      const d = await drawerText(page);
      ok(/predates evidence capture|never kept/i.test(d),
        'a 404 rendered as a failure — for a pre-0053 session that is the ORDINARY answer, and calling it an ' +
        'error tells the operator the platform is broken when it is not');
      ok(!/\bfailed\b.*\b404\b/i.test(d), 'a raw status code leaked into operator-facing copy');
    }
    ok(errs.length === 0, `uncaught page errors on the 404 path: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 4. a 403 is stated as the admin gate, not as "no evidence" ---------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, { evidence: '403' });

    if (await clickCite(page, 'check-host-services', 'the 403 path')) {
      const d = await drawerText(page);
      ok(/admin/i.test(d),
        'a 403 did not explain the admin gate — the operator sees an empty or vague panel and concludes the ' +
        'agent observed nothing, when in fact they are simply not permitted to read it');
      ok(!/predates evidence capture/i.test(d),
        'a 403 was rendered as "nothing was recorded" — that is a false statement about the estate, produced ' +
        'by collapsing "not permitted" into "not stored"');
    }
    ok(errs.length === 0, `uncaught page errors on the 403 path: ${errs.join(' | ')}`);
    await page.close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('GROUND-TRUTH E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('GROUND-TRUTH E2E PASS — the citation fetches and renders the stored observation, the stub toast is proven gone, a step with nothing recorded says so without a wasted read, and 404/403 are told apart.');
