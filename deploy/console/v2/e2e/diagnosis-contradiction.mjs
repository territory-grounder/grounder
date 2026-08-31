// Console e2e — A CONTRADICTED CLAIM SAYS SO, ON THE SCREEN (TG-201).
//
// ★ WHAT THIS GUARDS. core/proposal/diagnosis.go gave the agent a typed CLAIM — root cause, mechanism,
// supporting evidence, CONTRADICTING evidence, ruled-out alternatives — with every reference carrying a
// `cited` flag decided in agent/loop.go against the ToolResults the ORCHESTRATOR captured. An operator could
// not see any of it: nothing stored the claim and no route served it, so this view rendered the same free
// text as before and the contradiction stayed in an unread transcript.
//
// The recorded A2 failure is TG proposing a restart while HOLDING the observation that the guest was stopped
// deliberately. The whole value of a typed claim is that a HUMAN can check the reasoning against the
// evidence, so the load-bearing property is not "a diagnosis renders" — it is that the CONTRADICTING lane
// renders, and that an assertion the model could not ground is MARKED rather than quietly dropped.
//
// KILLING MUTATION (executed 2026-08-04, deploy/console/v2/modules/_live/js.txt): delete the
// liveDiagLane("CONTRADICTS", …) append from liveDiagBlock — i.e. render the claim without the evidence
// against it — re-run assemble.py, run this file. It goes RED on:
//   "the CONTRADICTING evidence is not on the page — an operator reading this claim would never learn the
//    agent held an observation against its own root cause, which is the A2 failure this type exists for".
// Restored, it passes. A second mutation — dropping the "uncited" marker so an ungrounded assertion renders
// like a cited one — goes RED on the uncited assertions below.
//
// VACUITY FLOOR. Every assertion below is a match over rendered text, so a render that produced NOTHING would
// satisfy the negative checks and could satisfy nothing else. Section 0 fails the run outright unless the
// claim block exists and the view carries real text — a scan that can pass on an empty page is not a test.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node diagnosis-contradiction.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const REF = 'librenms-dc1-184311';
const OTHER = 'librenms-dc1-184999';

// GET /v1/sessions — OTHER is newest, so "newest wins" alone would NOT select REF. Section 2 deep-links.
const sessions = [
  { external_ref: OTHER, host: 'dc1pve02', band: '', classified_at: '2026-08-04T09:10:00Z' },
  { external_ref: REF, host: 'dc1pve01', band: '', classified_at: '2026-08-04T08:41:00Z' },
];

const walk = ref => ({
  id: ref, ref, title: 'Service-up/down', host: 'dc1pve01', status: 'proposed', conf: 0.72,
  nodes: [
    { t: 'ingest', lb: 'Ingested', pay: 'Service-up/down · critical · dc1pve01', conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 1 — get-incident-history', st: 'investigate', src: 'agent-cycle',
      pay: 'Checking whether this guest went down on its own.', plan: ['get-incident-history'],
      ev: 'incident-history-dc1pve01', conf: 0 },
    { t: 'propose', lb: 'Proposal', st: 'ok', src: 'propose', pay: 'restart the guest', conf: 0.72 },
  ],
});

// GET /v1/sessions/{ref}/diagnosis — verbatim SessionDiagnosisDTO shape (core/httpapi/session_diagnosis.go).
// This is the A2 case: the model proposes a restart while HOLDING the PVE task-history observation saying an
// operator stopped it deliberately. One supporting ref is deliberately UNCITED with an id — a citation the
// orchestrator never captured, which must render as a fabricated citation and not as evidence.
const CONTRA_CLAIM = 'PVE task history shows root@pam ran vzstop on 101 four minutes before the alert — the guest was stopped deliberately';
const UNCITED_CLAIM = 'the unit is configured to restart on boot';
const diagnosis = {
  ref: REF,
  root_cause: 'the guest 101 is down because its service unit failed to start after an unclean shutdown',
  mechanism: 'systemd gave up after 3 restart attempts inside 60s and left the unit in failed state',
  supporting: [
    { id: 'incident-history-dc1pve01', claim: 'incident history shows two prior unclean shutdowns on this guest', cited: true },
    { id: 'pve-unit-config-101', claim: UNCITED_CLAIM, cited: false },
  ],
  contradicting: [
    { id: 'pve-task-history-101', claim: CONTRA_CLAIM, cited: true },
  ],
  ruled_out: [
    { cause: 'host out of memory', reason: 'the node reports 41% memory in use', id: 'host-metrics-dc1pve01', cited: true },
  ],
  contradicted: true,
  uncited: 1,
};

async function mount(page, { deepLink = false, diag = 'ok' } = {}) {
  const reads = [];
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime', role: 'trace-read' } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions, total: 2 } });
    // MATCHED BEFORE the generic /v1/sessions/ arm below — the claim route is a SUFFIX of the walk route.
    if (p.endsWith('/diagnosis')) {
      reads.push(p);
      if (diag === '404') return route.fulfill({ status: 404, json: { error: 'not found' } });
      if (diag === '403') return route.fulfill({ status: 403, json: { error: 'forbidden' } });
      // Only REF has a recorded claim: OTHER answers 404, the ordinary "none recorded" case.
      if (!p.includes(REF)) return route.fulfill({ status: 404, json: { error: 'not found' } });
      return route.fulfill({ json: diagnosis });
    }
    if (p.includes('/evidence/')) return route.fulfill({ json: {
      ref: REF, cycle: 1, id: 'pve-task-history-101', tool: 'get-incident-history',
      payload: 'UPID:dc1pve01:vzstop:101:root@pam: OK', truncated: false, full_bytes: 41 } });
    if (p.startsWith('/v1/sessions/')) return route.fulfill({ json: walk(p.split('/v1/sessions/')[1]) });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  // THE SELECTION LIVES IN location.search, NOT THE HASH: route() writes location.hash on every navigation and
  // the hashchange handler does `if(views[n]) route(n)`, so a hash deep-link is eaten. This is the URL an
  // operator would paste into an incident review.
  const url = deepLink ? `${BASE}/index.html?session=${REF}#reasoning` : `${BASE}/index.html#reasoning`;
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 30000 });
  // WAIT ON THE ADOPT STATE, NOT ON A CLOCK. A fixed sleep here made this file report the claim block as
  // ABSENT on a loaded machine: adopt had not finished, liveState.reasonDto was still unset, and the view
  // correctly fell back to the labelled design fixture — which carries plenty of text and no claim. That is a
  // slow box, not a defect, and a test that calls it a defect is a test nobody will trust. The predicate is
  // on liveState (has the claim read RESOLVED?), never on the rendered markup, so it cannot mask the very
  // render failures the assertions below exist to catch.
  try {
    // `typeof liveState`, NOT `window.liveState`: the bundle declares it with const at top level, which
    // creates a global LEXICAL binding that never lands on window — a window.liveState probe is permanently
    // undefined and the wait would always time out (measured while writing this file).
    await page.waitForFunction(
      () => typeof liveState !== 'undefined' && liveState.on && liveState.diagDto !== undefined,
      null, { timeout: 30000 });
  } catch (e) {
    ok(false, 'the console never resolved a claim read at all (liveState.diagDto stayed undefined after ' +
      'adopt) — the diagnosis read is not wired into the walk load, so no session can ever show its claim');
  }
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'reasoning'); if (a) a.click(); });
  // route() unconditionally rebuilds #view (there is no same-view early return — see console.html's route()),
  // and the click's route('reasoning') call is synchronous, so by the time click() returns the view already
  // reflects the now-resolved liveState.diagDto awaited above; a reflow flush is enough margin for the DOM
  // to settle, not a guess at fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  return reads;
}

const viewText = page => page.evaluate(() => document.querySelector('#view')?.innerText || '');

const browser = await chromium.launch();
try {
  // ---- 1. the claim renders, and the CONTRADICTION is visible on it ------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    // Select REF explicitly through the picker path so section 1 grades the RENDER, not the deep link
    // (section 2 grades the deep link on its own).
    const reads = await mount(page, { deepLink: true });

    // ---- VACUITY FLOOR: a scan that matches nothing must FAIL, not pass quietly ----
    const text = await viewText(page);
    ok(text.length > 200,
      `#reasoning rendered only ${text.length} characters — the view produced nothing, so every check below ` +
      'would be vacuous. This is the floor, not a warning.');
    const block = await page.evaluate(() => !!document.querySelector('#view [data-q="diagnosis"]'));
    ok(block,
      'no typed-claim block exists in the rendered view at all — the surface this ticket delivers is absent, ' +
      'and an operator is back to reading prose');
    ok(reads.length > 0,
      'the console never issued a GET /v1/sessions/{ref}/diagnosis — the claim on screen (if any) cannot be ' +
      'the recorded one');

    // ---- THE LOAD-BEARING ASSERTION: the evidence AGAINST the root cause is on the page ----
    ok(text.includes('vzstop'),
      'the CONTRADICTING evidence is not on the page — an operator reading this claim would never learn the ' +
      'agent held an observation against its own root cause, which is the A2 failure this type exists for');
    ok(/CONTRADICTS/i.test(text),
      'the contradicting lane has no heading, so a reader cannot tell evidence FOR the claim from evidence ' +
      'AGAINST it — the two rendered as one undifferentiated list is the flat evidence_ids list again');
    const contradictedChip = await page.evaluate(() => !!document.querySelector('#view [data-q="contradicted"]'));
    ok(contradictedChip,
      'the claim is served with contradicted=true and the view shows no contradiction marker — the single ' +
      'fact this surface exists to surface is left for the operator to notice by reading');

    // ---- the claim itself, and its mechanism ----
    ok(text.includes('service unit failed to start'), 'the root cause is not rendered');
    ok(text.includes('systemd gave up after 3 restart attempts'), 'the mechanism is not rendered');
    ok(/RULED OUT/i.test(text) && text.includes('host out of memory'),
      'the ruled-out alternatives are missing — the operator cannot see what the agent considered and discarded');

    // ---- an UNGROUNDED assertion is marked, never silently promoted or dropped ----
    ok(text.includes(UNCITED_CLAIM),
      'an uncited assertion was DROPPED from the render — hiding what the model could not ground is the ' +
      'opposite of what a claim surface is for');
    // ASSERTED ON THE MARKER ELEMENT, NOT ON PAGE TEXT. A plain /uncited/i scan over innerText passes on the
    // row's own "cited/uncited" gutter label (CSS text-transform uppercases it), so it stayed GREEN under the
    // mutation that strips the marker — measured 2026-08-04. The element and its wording are the real claim.
    const mark = await page.evaluate(() => {
      const el = document.querySelector('#view [data-q="uncited"]');
      return el ? (el.textContent || '') : null;
    });
    ok(mark !== null,
      'an assertion the orchestrator never grounded renders with no marker element, so a fabricated citation ' +
      'reads exactly like a real one');
    ok(mark !== null && /matched no observation/i.test(mark),
      `the uncited marker reads "${mark}" — it does not say the cited id matched nothing the orchestrator ` +
      'captured, which is the difference between a sloppy citation and a fabricated one');
    ok(mark !== null && mark.includes('pve-unit-config-101'),
      'the ungrounded citation id is not shown — "asserted with no citation" and "cited an id nobody ' +
      'captured" are different failures and only the second is a fabrication');

    // ---- untrusted text is a TEXT NODE, never markup ----
    const injected = await page.evaluate(() => document.querySelector('#view [data-q="root-cause"]')?.querySelector('*') || null);
    ok(injected === null,
      'the root-cause payload produced child ELEMENTS — it was inserted as innerHTML, and this text is ' +
      'model- and host-derived');

    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 2. the deep link selects the claim's own walk, via location.search --------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    const reads = await mount(page, { deepLink: true });

    ok(reads.some(p => p.includes(REF)),
      `no claim was read for the deep-linked ${REF} — ?session= was ignored, so a claim quoted in an incident ` +
      'review resolves to whatever session is newest at the time it is opened');
    ok(!reads.some(p => p.includes(OTHER) && !p.includes(REF)) || reads.some(p => p.includes(REF)),
      'the claim read was issued for a DIFFERENT session than the walk on screen — the two must move together');
    const text = await viewText(page);
    ok(text.includes('vzstop'),
      'the deep-linked session rendered without its contradiction, so the URL an operator cites does not ' +
      'reproduce what they saw');
    ok(errs.length === 0, `uncaught page errors on the deep-link path: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 3. NO recorded claim is an honest absence, never an empty claim -----------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, { deepLink: true, diag: '404' });

    const text = await viewText(page);
    ok(text.length > 200, `#reasoning rendered only ${text.length} characters on the 404 path — vacuous`);
    ok(/No typed claim was recorded/i.test(text),
      'a session with no recorded claim renders nothing at all, so "the agent recorded no claim" is ' +
      'indistinguishable from "this console does not show claims"');
    ok(!/CONTRADICTED/.test(text),
      'a contradiction marker appeared for a session that has NO recorded claim — the render is carrying ' +
      'state from a previous session');
    ok(errs.length === 0, `uncaught page errors on the 404 path: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 4. a 403 is the trace gate speaking, not an absent claim ------------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, { deepLink: true, diag: '403' });

    const text = await viewText(page);
    ok(/admin session/i.test(text),
      'a 403 on the claim read does not explain the admin gate — the operator concludes the agent claimed ' +
      'nothing when in fact they are simply not permitted to read it');
    ok(!/No typed claim was recorded/i.test(text),
      'a 403 was rendered as "no claim recorded" — a false statement about the session, produced by ' +
      'collapsing "not permitted" into "not stored"');
    ok(errs.length === 0, `uncaught page errors on the 403 path: ${errs.join(' | ')}`);
    await page.close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('TYPED-CLAIM E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('TYPED-CLAIM E2E PASS — the claim renders with its contradiction visible and marked, ungrounded ' +
  'assertions are shown as uncited, the deep link reproduces the same claim, and 404/403 are told apart.');
