// Console e2e — the POLICY packet-tracer is a LIVE, READ-ONLY surface: it POSTs a hypothetical candidate
// to /api/v1/policy/trace and renders the worker's REAL composed verdict. It answers "may TG act on host X
// with op-class Y?" and actuates nothing.
//
// spec/015 TG-105 slice 2. Six properties, each with a real failure mode behind it:
//   1. IDLE IS HONEST: before a trace, the surface shows an explicit "no trace run yet" state — never a
//      pre-filled or fabricated verdict.
//   2. THE MODE DEFAULTS TO THE LIVE POSTURE (review finding): an operator who leaves `mode` untouched is
//      asking "what would happen RIGHT NOW", so the select defaults to liveState.policyMode.mode — NOT the
//      fail-closed Shadow — and the POST carries that live mode. Defaulting to Shadow here would silently
//      answer a DIFFERENT question than the one asked.
//   3. THE VERDICT RENDERS FAITHFULLY: a 200 renders the composed-verdict chip, the composed band, the
//      matched rule id, the reason prose, and the matched_rules provenance — all from the response, never
//      invented.
//   4. THE CHIP MAPPING IS THE ASA PACKET-TRACER CONVENTION: auto → chip-ok (permitted), approve →
//      chip-warn (human-gated), deny → chip-bad (blocked) — the same chip polarity the mode hero uses.
//   5. THE RATE-GOVERNOR CAVEAT IS MANDATORY: rate_governor_simulated is always false, and the surface
//      must SAY the trace does not model the runtime rate governor (a composed auto may still be
//      rate-clamped to approve at real actuation). Hiding it would over-promise the trace's fidelity.
//   6. AN UNREACHABLE ENGINE IS AN HONEST ERROR, NEVER A VERDICT: a 503 renders a failed-trace state and
//      NO verdict chip — the one thing a safety surface must never fabricate (INV-15).
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const POLICY_MODE_HITL = { mode: 'HITL', may_auto_actuate: false, requires_human_vote: true, persisted: true,
  posture: 'HITL — an allowed action is proposed and waits for a human vote before it may actuate.' };

const result = (over) => Object.assign({
  verdict: 'approve',
  matched_rule_id: 'approve-restart-mealie',
  composed_band: 'AUTO_NOTICE',
  approve_by: ['sre-oncall'],
  mode: 'HITL',
  reason: 'base approve (rule approve-restart-mealie) → confidence 0.70 ≥ min 0.60 → band AUTO_NOTICE → graduation auto_notice → never-auto floor not triggered.',
  never_auto_floor: false,
  bundle_version: 'sha256:ab12cd34ef',
  matched_rules: [{ rule_id: 'approve-restart-mealie', verdict: 'approve' }, { rule_id: 'default', verdict: 'deny' }],
  rate_governor_simulated: false,
}, over || {});

// mount: stub the whole /api surface (including POST /v1/policy/trace off a mutable `state`), land on
// #policy, and wait for live adoption to finish AND the tracer to render — the same deterministic waits
// manifest-review uses (wait for lastRefresh, never a fixed sleep, or the assertions read a mid-rebuild view).
async function mount(page, { policyMode, state }) {
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    if (p === '/v1/policy/trace' && req.method() === 'POST') {
      try { state.lastPost = req.postDataJSON(); } catch (e) { state.lastPost = null; }
      const tr = state.traceReply || { status: 503, body: 'no reply configured' };
      if (tr.status) return route.fulfill({ status: tr.status, body: tr.body || '' });
      return route.fulfill({ json: tr.json });
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
  await page.waitForFunction(() => /packet-tracer/i.test(document.querySelector('#view')?.innerText || ''),
    null, { timeout: 20000 });
}

// fill the two required fields (which enables the gated Trace button) and run the trace, then wait for the
// surface to leave its loading state — either a rendered result (its mandatory caveat) or an honest error.
async function fillAndTrace(page, { op = 'restart-service', host = 'dc1mealie01' } = {}) {
  await page.getByPlaceholder(/^op-class/).fill(op);
  await page.getByPlaceholder(/^host/).fill(host);
  await page.getByRole('button', { name: 'Trace' }).click();
  await page.waitForFunction(() => {
    const v = document.querySelector('#view'); if (!v) return false;
    return !!v.querySelector('.pol-tresult') || !!v.querySelector('.pol-terr');
  }, null, { timeout: 20000 });
}

// the verdict chip lives ONLY inside the tracer result (.pol-tverdict) — scoped so the mode-hero's own
// chip-ok/chip-warn/chip-bad posture chips can never be mistaken for a traced verdict.
const verdictChip = page => page.evaluate(() => {
  const el = document.querySelector('#view .pol-tverdict .chip-ok, #view .pol-tverdict .chip-warn, #view .pol-tverdict .chip-bad');
  return el ? { cls: el.className.trim(), txt: el.textContent.trim() } : null;
});

const browser = await chromium.launch();
try {
  // 1 + 2. idle state + the mode select defaulting to the LIVE mode (HITL), not the fail-closed Shadow.
  {
    const state = { traceReply: null, lastPost: null };
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: POLICY_MODE_HITL, state });
    const text = await page.evaluate(() => document.querySelector('#view').innerText);
    ok(/No trace run yet/i.test(text), 'idle: before any trace the honest idle state must render');
    ok(!(await page.$('#view .pol-tresult')), 'idle: no result block may exist before a trace is run');
    ok(!(await verdictChip(page)), 'idle: no verdict chip may render before a trace — never a pre-filled verdict');
    const modeVal = await page.$eval('#view select[aria-label="mode"]', el => el.value);
    ok(modeVal === 'HITL', `mode-default: the select must default to the LIVE mode HITL, got ${JSON.stringify(modeVal)}`);
    ok(/right now/i.test(text), 'mode-default: the caption must say the trace answers "what would happen right now"');
    await page.context().close();
  }

  // 3 + 5. a 200 (approve) renders the verdict, band, matched rule, reason, provenance, and the MANDATORY
  //        rate-governor caveat — and the POST carries the live HITL mode the operator left untouched.
  {
    const state = { traceReply: { json: result() }, lastPost: null };
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: POLICY_MODE_HITL, state });
    await fillAndTrace(page);
    const chip = await verdictChip(page);
    ok(chip && chip.cls === 'chip-warn' && chip.txt === 'APPROVE', `approve: verdict must render as a chip-warn APPROVE badge, got ${JSON.stringify(chip)}`);
    const rtext = await page.evaluate(() => document.querySelector('#view .pol-tresult').innerText);
    ok(/AUTO_NOTICE/.test(rtext), 'approve: the composed band must render');
    ok(/approve-restart-mealie/.test(rtext), 'approve: the matched rule id must render');
    ok(/graduation auto_notice/.test(rtext), 'approve: the multi-clause reason string must render readably');
    ok(/sre-oncall/.test(rtext), 'approve: approve_by must render on an approve verdict');
    ok(/sha256:ab12cd34ef/.test(rtext), 'approve: the bundle version must render');
    // THE MANDATORY honesty note — never hidden.
    const caveat = await page.evaluate(() => { const el = document.querySelector('#view .pol-tcaveat'); return el ? el.innerText : ''; });
    ok(/rate governor not simulated/i.test(caveat), 'caveat: the result MUST state the rate governor is not simulated');
    ok(/rate_governor_simulated=false/.test(caveat), 'caveat: the honest false flag must be shown verbatim');
    ok(/rate-clamped/i.test(caveat), 'caveat: it must warn a composed auto may still be rate-clamped to approve at real actuation');
    // THE MODE-FROM-LIVESTATE behaviour, proven on the wire: the POST carried HITL, not Shadow.
    ok(state.lastPost && state.lastPost.op_class === 'restart-service', `post: op_class must be forwarded, got ${JSON.stringify(state.lastPost)}`);
    ok(state.lastPost.host === 'dc1mealie01', 'post: host must be forwarded');
    ok(state.lastPost.mode === 'HITL', `post: an untouched mode must send the LIVE mode HITL, not Shadow — got ${JSON.stringify(state.lastPost.mode)}`);
    ok(state.lastPost.band === 'AUTO', `post: the default band AUTO must be sent, got ${JSON.stringify(state.lastPost.band)}`);
    ok(state.lastPost.confidence === 0.7, `post: the default confidence 0.7 must be sent, got ${JSON.stringify(state.lastPost.confidence)}`);
    await page.context().close();
  }

  // 4. the chip-verdict mapping: auto → chip-ok, deny → chip-bad (approve → chip-warn proven above).
  for (const [verdict, cls, floor] of [['auto', 'chip-ok', false], ['deny', 'chip-bad', true]]) {
    const state = { traceReply: { json: result({ verdict, never_auto_floor: floor, approve_by: [] }) }, lastPost: null };
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: POLICY_MODE_HITL, state });
    await fillAndTrace(page);
    const chip = await verdictChip(page);
    ok(chip && chip.cls === cls && chip.txt === verdict.toUpperCase(),
      `mapping: ${verdict} must render as a ${cls} ${verdict.toUpperCase()} badge, got ${JSON.stringify(chip)}`);
    await page.context().close();
  }

  // 6. a 503 is an honest failed-trace state — NO verdict chip, never a fabricated verdict.
  {
    const state = { traceReply: { status: 503, body: 'policy trace unavailable' }, lastPost: null };
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: POLICY_MODE_HITL, state });
    await fillAndTrace(page);
    const text = await page.evaluate(() => document.querySelector('#view').innerText);
    ok(/TRACE FAILED/i.test(text), '503: the surface must show an explicit failed-trace state');
    ok(/unavailable|not reachable|unreachable/i.test(text), '503: it must say the engine was unreachable (fail-closed)');
    ok(!(await page.$('#view .pol-tresult')), '503: no result block may render on a failed trace');
    ok(!(await verdictChip(page)), '503: NO verdict chip may render on a failed trace — a safety surface never fabricates a verdict');
    await page.context().close();
  }

  // 2b. an UNKNOWN live mode (policy/mode carries no mode) falls back to a LABELED Shadow, not a silent one.
  {
    const state = { traceReply: null, lastPost: null };
    const page = await (await browser.newContext()).newPage();
    await mount(page, { policyMode: {}, state });
    const modeVal = await page.$eval('#view select[aria-label="mode"]', el => el.value);
    ok(modeVal === 'Shadow', `mode-unknown: with no live mode the select must default to Shadow, got ${JSON.stringify(modeVal)}`);
    const text = await page.evaluate(() => document.querySelector('#view').innerText);
    ok(/live mode unknown/i.test(text) && /fail-closed/i.test(text),
      'mode-unknown: the caption must say the live mode is unknown and the Shadow default is fail-closed');
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('policy-tracer FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('policy-tracer: OK');
