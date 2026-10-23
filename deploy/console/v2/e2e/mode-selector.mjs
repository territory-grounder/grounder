// Console e2e — the POLICY autonomy-mode WRITE control (TG-106). The grounder NEVER flips the mode itself
// (spec/015 REQ-1502): this UI composes the operator's order and POSTs {to, expected_from, reason} to
// /api/v1/mode, where the worker's chokepoint-bound ModeController is the LAST gate. This oracle drives the
// stubbed /api surface and asserts the surface is honest and safe by construction. Seven properties, each with
// a real failure mode behind it:
//   1. THE LIVE CURRENT MODE RENDERS and enables the control: the mode-change control is live ONLY when the
//      current mode is known (it is the compare-and-swap expected_from) — an unknown live mode fails closed to
//      a truthful disabled control.
//   2. FULL-AUTO RAISES THE RED DOUBLE-WARN: switching INTO Full-auto (the allow-all posture that removes the
//      deny-floor) throws a RED confirm carrying the mandatory "authorizes autonomous, potentially irreversible
//      action across your estate" wording. A normal transition never does.
//   3. CANCELLING THE DOUBLE-WARN POSTS NOTHING: the RED confirm is WARN, never BLOCK — but backing out of it
//      must never actuate a mode change (no POST reaches /api/v1/mode).
//   4. A NORMAL TRANSITION USES THE PLAIN CONFIRM: Semi-auto/HITL/Shadow use a single plain confirm with no
//      RED and no irreversibility wording.
//   5. A CONFIRMED TRANSITION DRIVES THE SHARED ELEVATION THEN POSTS {to, expected_from, reason}: a 401 drives
//      the SAME admin step-up config/secret writes use (not a new flow); after elevation the write completes
//      and the honest committed ModeTransitionOutcome renders.
//   6. A 409 IS THE HONEST "CHANGED UNDERNEATH YOU" STATE, NEVER A FABRICATED SUCCESS: a stale expected_from
//      shows a re-read-and-retry refusal with NO success chip and NO silent overwrite.
//   7. A 503 IS AN HONEST ERROR, NEVER A FABRICATED SUCCESS: an unreachable worker shows a refusal state with
//      NO success chip — a safety surface never fabricates a mode or a success (INV-15).
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const MODE_HITL = { mode: 'HITL', may_auto_actuate: false, requires_human_vote: true, persisted: true,
  posture: 'HITL — every candidate action waits for a human vote before it may actuate.' };

const mkState = () => ({ posts: [], modeReplies: [], elevatePosts: 0, pageErrors: [] });

// mount: stub the whole /api surface (POST /v1/mode captures the body + replies off a mutable queue; POST
// /v1/session/elevate mints an elevation), land on #policy, and wait for live adoption AND the mode section to
// render — the same deterministic waits policy-tracer.mjs uses (wait for lastRefresh + a real node, never a
// fixed sleep, or the assertions read a mid-rebuild view).
async function mount(page, { policyMode, state }) {
  page.on('pageerror', e => state.pageErrors.push(String(e)));
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    if (p === '/v1/mode' && req.method() === 'POST') {
      let body = null; try { body = req.postDataJSON(); } catch (e) {}
      state.posts.push(body);
      const rep = state.modeReplies.length ? state.modeReplies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      // default 200 — the HONEST committed outcome computed from the order (mode after the flip = the target).
      const to = (body && body.to) || 'Shadow';
      const from = (body && body.expected_from) || '';
      return route.fulfill({ json: { mode: to, from, to } });
    }
    if (p === '/v1/session/elevate' && req.method() === 'POST') {
      state.elevatePosts++;
      return route.fulfill({ json: { admin_until: new Date(Date.now() + 15 * 60000).toISOString() } });
    }
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/policy/mode') return route.fulfill({ json: policyMode });
    if (p === '/v1/policy/rules') return route.fulfill({ json: { present: false, rules: [] } });
    if (p === '/v1/policy/graduation') return route.fulfill({ json: { classes: [] } });
    if (p === '/v1/policy/decisions') return route.fulfill({ json: { decisions: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#policy', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'policy'),
    null, { timeout: 20000 });
  await page.waitForFunction(() => typeof liveState === 'undefined' || !liveState.on || !!liveState.lastRefresh,
    null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'policy'); if (a) a.click(); });
  await page.waitForSelector('#view .pol-mode', { timeout: 20000 });
}

// open the write dialog and reach the select step.
async function openDialog(page) {
  await page.click('#view .pol-modebtn');
  await page.waitForSelector('#polModeDlg #polModeTarget', { timeout: 10000 });
}
// pick a target mode + reason (this enables the gated Review button via the form's live sync()).
async function pick(page, target, reason) {
  await page.selectOption('#polModeTarget', target);
  if (reason !== undefined) await page.fill('#polModeReason', reason);
}
// advance select → confirm (the plain-or-RED double-warn).
async function review(page) {
  await page.click('#polModeReview');
  await page.waitForSelector('#polModeProceed', { timeout: 10000 });
}
// confirm → submit the POST.
async function proceed(page) { await page.click('#polModeProceed'); }
// drive the SHARED admin step-up modal (the same one config/secret writes raise).
async function elevate(page) {
  await page.waitForSelector('#cfgElevate #cfgAdmName', { timeout: 10000 });
  await page.fill('#cfgAdmName', 'admin');
  await page.fill('#cfgAdmTok', 'break-glass-token');
  await page.click('#cfgElevate button[type=submit]');
}
const bodyText = page => page.evaluate(() => document.querySelector('#polModeBody').innerText);

const browser = await chromium.launch();
try {
  // 1. the live current mode renders in the hero AND the write control is live (enabled), not the placeholder.
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: MODE_HITL, state });
    const text = await page.evaluate(() => document.querySelector('#view').innerText);
    ok(/HITL/.test(text), 'current: the live active mode HITL must render in the mode hero');
    const btn = await page.$('#view .pol-modebtn');
    ok(!!btn, 'current: the LIVE "Change mode…" control must render when the current mode is known');
    const enabled = btn ? await page.$eval('#view .pol-modebtn', el => !el.disabled) : false;
    ok(enabled, 'current: the mode-change control must be ENABLED (the disabled placeholder is gone)');
    ok(state.pageErrors.length === 0, 'current: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 1b. an UNKNOWN live mode (policy/mode carries no mode) fails closed to a truthful DISABLED control — a
  //     compare-and-swap change has no baseline, so the surface must not offer one.
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: {}, state });
    ok(!(await page.$('#view .pol-modebtn')), 'unknown-mode: no LIVE change control may render without a known current mode');
    const dis = await page.$('#view .pol-mode ~ .pol-disabled, #view .pol-disabled');
    ok(!!dis, 'unknown-mode: the control must fail closed to a truthful disabled placeholder');
    await page.context().close();
  }

  // 2 + 3. Full-auto raises the RED double-warn with the mandatory wording; cancelling it POSTs NOTHING.
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: MODE_HITL, state });
    await openDialog(page);
    await pick(page, 'Full-auto', 'go full auto for the maintenance window');
    ok(!!(await page.$('#polModeBody .pol-mwarn-red')), 'full-auto: the select step must show the inline RED allow-all warning');
    await review(page);
    ok(!!(await page.$('#polModeBody .pol-mconfirm.red')), 'full-auto: the confirm must be the RED double-warn (.pol-mconfirm.red)');
    const ctext = await bodyText(page);
    ok(/autonomous, potentially irreversible action across your estate/i.test(ctext),
      'full-auto: the RED confirm must carry the mandatory irreversibility wording');
    ok(!!(await page.$('#polModeProceed.danger')), 'full-auto: the proceed button must be the .danger (RED) variant');
    // CANCEL (Back) — WARN not BLOCK, but backing out must actuate nothing.
    await page.click('#polModeBack');
    await page.waitForSelector('#polModeTarget', { timeout: 10000 });
    ok(state.posts.length === 0, `full-auto cancel: cancelling the RED double-warn must POST nothing, got ${state.posts.length}`);
    ok(state.pageErrors.length === 0, 'full-auto: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 4. a normal transition (HITL → Shadow) uses the PLAIN confirm — no RED, no irreversibility wording — and
  //    a confirmed (already-elevated) transition POSTs {to, expected_from, reason} and renders the outcome.
  {
    const state = mkState(); // empty modeReplies → the first POST is a 200 (an already-elevated session)
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: MODE_HITL, state });
    await openDialog(page);
    await pick(page, 'Shadow', 'stand down to read-only for the audit');
    ok(!(await page.$('#polModeBody .pol-mwarn-red')), 'normal: no RED warning in the select step for a non-allow-all target');
    await review(page);
    ok(!(await page.$('#polModeBody .pol-mconfirm.red')), 'normal: the confirm must be the PLAIN confirm (not .red)');
    const ctext = await bodyText(page);
    ok(/Confirm mode change/i.test(ctext), 'normal: the plain-confirm heading must render');
    ok(!/irreversible/i.test(ctext), 'normal: the plain confirm must NOT carry the RED irreversibility wording');
    await proceed(page);
    await page.waitForSelector('#polModeDone', { timeout: 10000 });
    const rtext = await bodyText(page);
    ok(/MODE CHANGED/.test(rtext), 'normal: the honest committed outcome must render');
    ok(!!(await page.$('#polModeBody .chip-ok')), 'normal: a successful transition renders the chip-ok MODE CHANGED');
    ok(state.posts.length === 1, `normal: exactly one POST, got ${state.posts.length}`);
    const post = state.posts[0];
    ok(post && post.to === 'Shadow', `normal: POST must carry to=Shadow, got ${JSON.stringify(post)}`);
    ok(post && post.expected_from === 'HITL', `normal: POST must carry expected_from=HITL (the CAS baseline), got ${JSON.stringify(post)}`);
    ok(post && post.reason === 'stand down to read-only for the audit', `normal: POST must carry the verbatim reason, got ${JSON.stringify(post)}`);
    ok(state.pageErrors.length === 0, 'normal: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 5. a confirmed transition drives the SHARED elevation step-up on a 401, then POSTs and renders the outcome.
  {
    const state = mkState();
    state.modeReplies = [{ status: 401, body: 'unauthenticated' }]; // first POST → 401 (elevate); second → 200
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: MODE_HITL, state });
    await openDialog(page);
    await pick(page, 'Semi-auto', 'resume supervised automation after the change freeze');
    await review(page);
    await proceed(page);                                    // POST#1 → 401 → the shared step-up appears
    await elevate(page);                                    // reuse the config/secret admin step-up (not a new flow)
    await page.waitForSelector('#polModeProceed', { timeout: 10000 }); // re-opened straight at the confirm
    await proceed(page);                                    // POST#2 → 200
    await page.waitForSelector('#polModeDone', { timeout: 10000 });
    ok(state.elevatePosts === 1, `elevated: the SHARED step-up must have been driven exactly once, got ${state.elevatePosts}`);
    ok(state.posts.length === 2, `elevated: two POSTs (401 then 200), got ${state.posts.length}`);
    const post = state.posts[state.posts.length - 1];
    ok(post && post.to === 'Semi-auto' && post.expected_from === 'HITL' && post.reason === 'resume supervised automation after the change freeze',
      `elevated: the completed POST must carry {to, expected_from, reason}, got ${JSON.stringify(post)}`);
    const rtext = await bodyText(page);
    ok(/MODE CHANGED/.test(rtext) && /Semi-auto/.test(rtext), 'elevated: the honest committed outcome (now Semi-auto) must render');
    ok(state.pageErrors.length === 0, 'elevated: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 6. a 409 (stale expected_from) is the honest "changed underneath you" state — NO fabricated success.
  {
    const state = mkState();
    state.modeReplies = [{ status: 409, body: 'refused: expected_from no longer matches the active mode — re-read and retry' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: MODE_HITL, state });
    await openDialog(page);
    await pick(page, 'Shadow', 'stand down');
    await review(page);
    await proceed(page);
    await page.waitForSelector('#polModeReread', { timeout: 10000 });
    const etext = await bodyText(page);
    ok(/changed underneath you/i.test(etext), '409: the honest "changed underneath you" state must render');
    ok(/re-read/i.test(etext), '409: it must tell the operator to re-read and retry');
    ok(!(await page.$('#polModeBody .chip-ok')), '409: NO success chip — a stale CAS must never render a fabricated success');
    ok(!(await page.$('#polModeDone')), '409: no Done/success affordance on a stale-CAS refusal');
    ok(state.posts.length === 1, `409: exactly one POST and no silent retry/overwrite, got ${state.posts.length}`);
    ok(state.pageErrors.length === 0, '409: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 7. a 503 (unreachable worker) is an honest refusal — NO fabricated success.
  {
    const state = mkState();
    state.modeReplies = [{ status: 503, body: 'mode transition failed — retry' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: MODE_HITL, state });
    await openDialog(page);
    await pick(page, 'Shadow', 'stand down');
    await review(page);
    await proceed(page);
    await page.waitForSelector('#polModeBackErr', { timeout: 10000 });
    const etext = await bodyText(page);
    ok(/CHANGE REFUSED/i.test(etext), '503: an honest refusal state must render');
    ok(/unavailable|NOT changed|fail-closed/i.test(etext), '503: it must say the flip was not accepted (fail-closed), the mode NOT changed');
    ok(!(await page.$('#polModeBody .chip-ok')), '503: NO success chip on an unreachable-worker error');
    ok(state.pageErrors.length === 0, '503: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('mode-selector FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('mode-selector: OK');
