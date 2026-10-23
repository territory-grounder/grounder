// Console e2e — #reasoning IS AN INSTRUMENT, NOT A POSTER (TG-269).
//
// ★ WHY reasoning.mjs COULD NOT SEE THIS. Its fixture serves `sessions = [ONE row]`. With one session in
// the index, "the page can only ever reach the newest session" is not observable: newest IS all of them.
// The suite proved the walk renders and never asked whether the operator could move off it. In production
// the index holds ~1,900 rows, the view held exactly one, and it contained no selector, no click handler
// and no fetch of any kind — so the pane never changed for the life of the tab. The owner opened the live
// page and asked why it showed only static content. It did.
//
// That is the same failure as the blank Modules page one day earlier: A FIXTURE THAT IS A DIFFERENT
// PROGRAM FROM PRODUCTION. So this suite serves a MULTI-session index and a walk that STOPPED, because
// those are the two properties the real spine has and the old fixture did not.
//
// Every assertion below is anchored to a live observation on territory-grounder.example.net,
// 2026-08-03 15:35, session librenms-dc1-183957 (status "stopped", no verify node, agent-cycle 6 with
// an empty thought).
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node reasoning-walk-is-navigable.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const NEWEST = 'librenms-dc1-183957';
const OLDER  = 'am-JudgeFrontierDrift-dc1claude01';

// Field names and shape verbatim from GET /v1/sessions?limit=200 on the live control plane.
const sessions = [
  { external_ref: NEWEST, host: 'dc1pve01',      band: '',           risk_level: '',       classified_at: '2026-08-03T08:53:01.442454Z' },
  { external_ref: OLDER,  host: 'dc1claude01',   band: 'POLL_PAUSE', risk_level: 'medium', classified_at: '2026-08-02T04:33:00.434823Z' },
  { external_ref: 'librenms-dc1-183947', host: 'dc1pve03', band: '', risk_level: '',    classified_at: '2026-08-02T17:23:04.511832Z' },
];

// THE STOPPED WALK — verbatim shape of the live newest session. status "stopped", NO verify node, and an
// agent-cycle whose `pay` is empty while its `lb` carries only the boilerplate "ReAct cycle 6".
const stoppedTrace = {
  id: NEWEST, ref: NEWEST, title: 'Service-up/down', host: 'dc1pve01', status: 'stopped', conf: 0,
  nodes: [
    { t: 'ingest', lb: 'Ingested (librenms)', ts: '2026-08-03T08:52:04Z', st: 'ok', src: 'ingest',
      pay: 'Service-up/down · critical · dc1pve01', conf: 0, min_conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 1 — get-active-alerts', ts: '2026-08-03T08:53:01Z', st: 'investigate',
      src: 'agent-cycle', pay: 'Playbook says: first get-active-alerts on this host.',
      plan: ['get-active-alerts'], conf: 0, min_conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 6', ts: '2026-08-03T08:53:01Z', st: 'investigate', src: 'agent-cycle',
      pay: '', plan: [], conf: 0, min_conf: 0 },
    { t: 'propose', lb: 'Proposal', ts: '2026-08-03T08:53:01Z', st: 'ok', src: 'propose',
      pay: 'LibreNMS reports dc1pve01 itself UP and polling normally.',
      plan: ['prompt: preamble/1'], conf: 0, min_conf: 0 },
  ],
};

// A COMPLETED walk, so the "no verdict" notice is proven to DISCRIMINATE rather than always fire.
const completedTrace = {
  id: OLDER, ref: OLDER, title: 'Judge frontier drift', host: 'dc1claude01', status: 'executed',
  band: 'POLL_PAUSE', risk: 'medium', conf: 0.91,
  nodes: [
    { t: 'ingest', lb: 'Ingested (alertmanager)', ts: '2026-08-02T04:33:00Z', st: 'ok', src: 'ingest',
      pay: 'JudgeFrontierDrift · warning · dc1claude01', conf: 0, min_conf: 0 },
    { t: 'agent-cycle', lb: 'ReAct cycle 1 — get-scorecard', ts: '2026-08-02T04:34:00Z', st: 'investigate',
      src: 'agent-cycle', pay: 'The judge rubric drifted from the frontier; comparing scorecards.',
      plan: ['get-scorecard'], conf: 0, min_conf: 0 },
    { t: 'verify', lb: 'Verified', ts: '2026-08-02T04:36:00Z', st: 'ok', src: 'verify',
      pay: 'Rubric realigned; drift closed within tolerance.', verdict: 'match', conf: 0.91, min_conf: 0 },
  ],
};

const traceFor = ref => (ref === OLDER ? completedTrace : stoppedTrace);

async function mount(page, { query = '' } = {}) {
  const seen = [];
  await page.route('**/api/**', async route => {
    const url = route.request().url();
    const p = url.split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions, total: 1871 } });
    if (p.startsWith('/v1/sessions/')) {
      const ref = decodeURIComponent(p.slice('/v1/sessions/'.length));
      seen.push(ref);
      return route.fulfill({ json: traceFor(ref) });
    }
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html' + query + '#reasoning', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Wait for the session picker to render with the full index AND the view text to be substantial — the
  // exact preconditions the checks right after mount() read (opts.length >= 3, text0.length > 200).
  const booted = () => document.querySelectorAll('#view select option').length >= 3 &&
    (document.querySelector('#view')?.innerText || '').length > 200;
  await page.waitForFunction(booted, null, { timeout: 20000 }).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'reasoning'); if (a) a.click(); });
  await page.waitForFunction(booted, null, { timeout: 20000 }).catch(() => {});
  return seen;
}

const viewText  = page => page.evaluate(() => document.querySelector('#view')?.innerText || '');
/* THE PICKER LEGITIMATELY NAMES EVERY OTHER HOST, so "is the right walk on screen" must be asked of the
   CHAIN, not of the whole view. Asking the view was this suite's own false positive: the deep link was
   working and the assertion failed on an option label. */
const chainText = page => page.evaluate(() => document.querySelector('#view .ai-zone')?.innerText || '');

/* Selecting must not THROW when the control is missing — that was the first failure mode of this very
   suite, and it hid every assertion after the throw behind one Playwright timeout. A gate that dies at the
   first absent element reports one symptom of a defect that has four. Missing control => recorded failure
   => the remaining checks still run and still name what they found. */
async function trySelect(page, ref, why) {
  const has = await page.evaluate(() => !!document.querySelector('#view select'));
  if (!has) { ok(false, `no session selector exists on #reasoning, so ${why} cannot be exercised at all`); return false; }
  // Capture the chain text BEFORE selecting, then wait for it to actually CHANGE — the exact precondition
  // every check after trySelect() depends on (a re-render with the newly-selected walk's data). ref-agnostic
  // on purpose: trySelect is called with different refs across this suite's blocks.
  const before = await page.evaluate(() => document.querySelector('#view .ai-zone')?.innerText || '');
  await page.selectOption('#view select', ref).catch(e => ok(false, `selecting ${ref} threw: ${e.message}`));
  await page.waitForFunction(b => (document.querySelector('#view .ai-zone')?.innerText || '') !== b, before).catch(() => {});
  return true;
}

const browser = await chromium.launch();
try {
  // ---- 1. the operator can reach a walk that is not the newest -----------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    const seen = await mount(page);

    // NON-VACUITY: the page must have rendered before any of this means anything. A substring check on an
    // empty page passes for the wrong reason — that is how the blank Modules page survived its suite.
    const text0 = await viewText(page);
    ok(text0.length > 200, `#reasoning rendered only ${text0.length} chars — nothing below this can be trusted`);
    ok(/dc1pve01/.test(text0), 'the newest walk did not render, so "can we move off it" is untestable');

    const opts = await page.evaluate(() =>
      [...document.querySelectorAll('#view select option')].map(o => o.value));
    ok(opts.length >= 3,
      `the view offers ${opts.length} selectable walks, want >= 3 — THE DEFECT: the page rendered one ` +
      `session and had no selector at all, so 199 of the 200 rows in the index were unreachable from it`);
    ok(opts.includes(OLDER),
      'a walk that is not the newest is missing from the picker — the index is not what drives it');

    // THE LOAD-BEARING ONE: selecting must actually FETCH and RENDER the other walk, not just repaint.
    const moved = await trySelect(page, OLDER, 'the load-bearing "can the operator reach another walk" check');
    if (moved) ok(seen.includes(OLDER),
      'selecting another walk issued no /v1/sessions/{ref} read — the control is decorative');

    const text1 = await viewText(page);
    ok(/dc1claude01/.test(text1),
      'the selected walk did not render; the page is still showing the newest session');
    ok(/Rubric realigned/.test(text1),
      'the selected walk\'s recorded thoughts are absent — the DTO was fetched but not adopted');
    ok(!/Playbook says: first get-active-alerts/.test(await chainText(page)),
      'the PREVIOUS walk\'s steps are still on screen next to the selected one');

    // The selection has to be citable: an audit URL that always means "newest" cannot be quoted in a review.
    const q = await page.evaluate(() => location.search);
    ok(q.includes('session=' + encodeURIComponent(OLDER)) || q.includes('session=' + OLDER),
      `the URL is "${q}" after selecting a walk — the selection is not citable and will not survive a reload`);

    /* Element.append(null) stringifies — the first cut of the picker put a literal "null" on the page and
       every assertion above still passed, because none of them looked at what the control BAR said. */
    ok(!/\bnull\b|\bundefined\b|\[object Object\]/.test(text0),
      `raw JS values leaked into the rendered page: ${(text0.match(/.{0,40}(null|undefined|\[object Object\]).{0,40}/) || [])[0]}`);

    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 2. a deep link opens THAT walk, not the newest ---------------------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, { query: '?session=' + encodeURIComponent(OLDER) });

    const text = await viewText(page);
    ok(text.length > 200, `deep-linked #reasoning rendered only ${text.length} chars`);
    ok(/dc1claude01/.test(text) && /Rubric realigned/.test(text),
      'a deep link to a specific walk opened something else — the citation does not resolve to what it names');
    ok(!/dc1pve01/.test(await chainText(page)),
      'the deep link rendered the NEWEST walk instead of the one it names');
    ok(errs.length === 0, `uncaught page errors on the deep-link path: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 3. a stopped walk says it stopped; a completed one does not --------------------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page);

    const stopped = await viewText(page);
    ok(/no verdict/i.test(stopped) && /stopped/i.test(stopped),
      'a session with status "stopped" and no verify node rendered identically to one that concluded — ' +
      'on an audit surface, silently overstating completeness is the failure that matters');

    // DISCRIMINATION: the notice must not be wallpaper. A walk that DID reach a verdict must not carry it.
    await trySelect(page, OLDER, 'the discrimination check on the "no verdict" notice');
    const done = await viewText(page);
    ok(/Rubric realigned/.test(done), 'the completed walk failed to render, so the control below is vacuous');
    ok(!/no verdict/i.test(done),
      'the "no verdict" notice also fires on a walk that RECORDED a verdict — it is decoration, not a signal');

    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }

  // ---- 4. a cycle that recorded no thought is not dressed up as a hypothesis ----------------------------
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page);

    const text = await viewText(page);
    ok(/recorded no thought/i.test(text),
      'the empty cycle vanished or was mislabelled — the boundary is real and must be shown as a gap');

    // THE DEFECT ITSELF: `n.pay || n.lb` let the boilerplate label pass the "is this empty" guard, so live
    // the page showed a hypothesis whose entire content was the string "ReAct cycle 6".
    const bareLabel = await page.evaluate(() =>
      [...document.querySelectorAll('#view .cnode')].some(n => {
        const kind = n.querySelector('.kind')?.innerText || '';
        const body = (n.querySelector('.txt')?.innerText || '').trim();
        return /hypothesis/i.test(kind) && /^ReAct cycle \d+$/.test(body);
      }));
    ok(!bareLabel,
      'a step whose only text is the boilerplate label "ReAct cycle N" is rendered as a HYPOTHESIS — ' +
      'that is the agent\'s cycle counter being presented as the agent\'s reasoning');

    ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
    await page.close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('REASONING-NAVIGABLE E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('REASONING-NAVIGABLE E2E PASS — every recorded walk is reachable and citable, a deep link resolves to what it names, a stopped walk says so while a concluded one does not, and a cycle with no recorded thought is shown as a gap rather than a hypothesis.');
