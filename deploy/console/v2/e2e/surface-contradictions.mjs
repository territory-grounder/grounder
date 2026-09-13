// TWO NUMBERS ON ONE SCREEN MUST NOT DISAGREE ABOUT ONE FACT.
//
// Four defects, each a surface asserting something its own adjacent evidence contradicts. All measured on
// the live deployed bundle:
//
//   1. #actions labelled abandoned and timed-out actions "EXECUTING". Status was derived as
//      `executed ? "executing" : approved ? "executing"`, so every approved-but-never-executed manifest got
//      the IN-FLIGHT word and kept it forever — one card read "EXECUTING · 10h". On the live control plane:
//      23 obsolete + 12 timeout + 3 approved manifests have no execution row and no verdict. 38 cards
//      claiming an action is under way when nothing is running and nothing ever will. The field that says
//      why (approval_choice) was in the DTO the whole time and was never read.
//   2. #command's four triage facets were INERT. ALL / NEEDS ME / DEVIATIONS / AUTO visibly activated and
//      returned the identical unfiltered rows — because the fixture view reads facetState and the LIVE
//      wrapper returns early before reaching it. The DEVIATIONS chip listed rows whose verdict is "match",
//      which is worse than a dead control: a filter asserting a false population.
//   3. #grounding's headline read "×0.4 · at or below chance" directly above bars reading 0.40 and 0.23 —
//      a real signal 1.74x its control, captioned as a failure. Root cause server-side: the denominator was
//      floored at 1, an integer-count rule applied to a mean.
//   4. #regime asserted "Under Shadow …" four times on a screen whose own badge read "ACTUATING · MUTATION
//      GATE ON", while the estate was being actuated.
//
// The through-line: each surface computed a fact twice, from two different places, and printed both.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  // Wait for the boot script to have parsed (views populated) rather than a fixed guess — the same
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick. Everything `page` reads
  // below (FACETS, route, liveState, toast) is a script global set at parse time, not live-fetched data.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  // ---- 1. no ending is called "executing" ----
  // ★ THE FIRST DRAFT OF THIS CHECK RE-IMPLEMENTED THE DERIVATION INSIDE THE ORACLE. That is the shape that
  // makes a test unable to catch the bug it names: the copy would keep passing while the real mapper rotted.
  // This drives liveAdopt() against an intercepted /v1/actions and reads the STATUS PILLS the real
  // #actions view renders, over the closed set of approval_choice values the control plane stores.
  const ACTS = [
    { why: 'obsolete, never ran', approval_choice: 'obsolete', executed: false, verified: false, verdict: '' },
    { why: 'timeout, never ran', approval_choice: 'timeout', executed: false, verified: false, verdict: '' },
    { why: 'approved, not yet run', approval_choice: 'approved', executed: false, verified: false, verdict: '' },
    { why: 'genuinely executing', approval_choice: 'approved', executed: true, verified: false, verdict: '' },
    { why: 'verified', approval_choice: 'approved', executed: true, verified: true, verdict: 'match' },
    { why: 'deviated', approval_choice: 'approved', executed: true, verified: true, verdict: 'deviation' },
    { why: 'awaiting a human', approval_choice: '', executed: false, verified: false, verdict: '' },
  ];
  const actPage = {
    actions: ACTS.map((c, i) => ({
      action_id: String(i).repeat(32).slice(0, 32), plan_hash: '', op: 'restart', op_class: 'restart-service',
      target: 'host' + i, reversible: true, params: { unit: 'nginx' }, band: 'POLL_PAUSE',
      verdict: c.verdict, approval_choice: c.approval_choice, risk_level: 'low', has_confidence: false,
      classified: true, predicted: true, approved: c.approval_choice !== '', executed: c.executed,
      verified: c.verified, sealed_at: '2026-07-29T00:00:00Z',
    })),
    counts: { total: ACTS.length },
  };
  const actCtx = await browser.newContext();
  const actPg = await actCtx.newPage();
  await actPg.route('**/v1/**', async r => {
    const u = r.request().url();
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' }) });
    if (u.includes('/v1/actions')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(actPage) });
    return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' });
  });
  await actPg.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await actPg.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await actPg.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  const st = await actPg.evaluate((whys) => {
    route('actions');
    const rows = Array.from(document.querySelectorAll('#view .ribbon-row'));
    // pair each rendered ribbon with the case that produced it, by target host
    return rows.map(r => {
      const host = (r.querySelector('.rr-host') || {}).textContent || '';
      const idx = parseInt(String(host).replace(/\D/g, ''), 10);
      const pill = r.querySelector('.pill');
      return { why: whys[idx], label: pill ? pill.textContent.trim() : null, cls: pill ? pill.className : null };
    }).filter(x => x.why);
  }, ACTS.map(a => a.why));
  await actCtx.close();
  check('the REAL #actions view rendered every case', st.length === ACTS.length, `${st.length}/${ACTS.length} ribbons`);
  const inflight = st.filter(x => /^Executing$/i.test(x.label || ''));
  check('exactly ONE case is "Executing" — the one that is', inflight.length === 1 && inflight[0].why === 'genuinely executing', JSON.stringify(inflight));
  const dead = st.filter(x => x.why === 'obsolete, never ran' || x.why === 'timeout, never ran');
  check('obsolete and timeout say they did not run', dead.length === 2 && dead.every(d => /superseded|expired/i.test(d.label || '')), JSON.stringify(dead));
  check('an approved-not-yet-run action says so', st.some(x => x.why === 'approved, not yet run' && /not yet executed/i.test(x.label || '')), JSON.stringify(st.find(x => x.why === 'approved, not yet run')));
  check('the dead endings are not painted as in-flight', dead.every(d => !/\bexecuting\b/.test(d.cls || '')), JSON.stringify(dead.map(d => d.cls)));

  // ---- 2. #command facets filter the LIVE rows, over EVERY facet the view offers ----
  const facets = await page.evaluate(() => (FACETS.command || []).map(f => f[0]));
  check('command facets are enumerable', facets.length >= 3, JSON.stringify(facets));
  const filt = await page.evaluate((keys) => {
    // a synthetic live spine whose bands/verdicts are known, so each facet has a checkable expected count
    liveState.on = true;
    liveState.sessionTotal = 100;
    liveState.sessions = [
      { external_ref: 'r1', band: 'POLL_PAUSE', verdict: 'match', risk_level: 'low', action_id: 'a1', classified_at: '2026-07-29T00:00:00Z' },
      { external_ref: 'r2', band: 'POLL_PAUSE', verdict: 'deviation', risk_level: 'low', action_id: 'a2', classified_at: '2026-07-29T00:00:00Z' },
      { external_ref: 'r3', band: 'AUTO', verdict: 'match', risk_level: 'low', action_id: 'a3', classified_at: '2026-07-29T00:00:00Z' },
      { external_ref: 'r4', band: 'AUTO_NOTICE', verdict: '', risk_level: 'low', action_id: 'a4', classified_at: '2026-07-29T00:00:00Z' },
      { external_ref: 'r5', band: 'AUTO', verdict: 'deviation', risk_level: 'low', action_id: 'a5', classified_at: '2026-07-29T00:00:00Z' },
    ];
    const out = {};
    for (const k of keys) {
      facetState.command = k;
      route('command');
      const refs = Array.from(document.querySelectorAll('#view table.tbl tbody tr td:first-child')).map(td => td.textContent.trim()).filter(t => /^r\d$/.test(t));
      out[k] = refs;
    }
    facetState.command = 'all';
    return out;
  }, facets);
  const expected = { all: 5, pause: 2, deviation: 2, auto: 3 };
  for (const k of facets) {
    const got = (filt[k] || []).length;
    check(`facet "${k}" filters the live spine`, got === expected[k], `${got} rows, expected ${expected[k]} (${JSON.stringify(filt[k])})`);
  }
  const distinct = new Set(facets.map(k => (filt[k] || []).join(','))).size;
  check('the facets are not all the same list', distinct === facets.length, `${distinct} distinct results across ${facets.length} facets`);

  // the DEVIATIONS facet must contain ONLY deviations — the specific false-population defect
  check('DEVIATIONS lists only deviations', (filt.deviation || []).every(r => r === 'r2' || r === 'r5'), JSON.stringify(filt.deviation));

  // ---- 3. #grounding's headline agrees with its own bars ----
  const gr = await page.evaluate(() => {
    const read = () => {
      const v = document.querySelector('#view .grnd-stat-v:last-of-type');
      const stats = Array.from(document.querySelectorAll('#view .grnd-stat'));
      const sig = stats.find(s => /falsifiability/i.test(s.textContent || ''));
      const bars = Array.from(document.querySelectorAll('#view .grnd-falsify:not(.grnd-bands) .grnd-fbar-row')).map(r => ({
        lbl: r.querySelector('.grnd-fbar-lbl')?.textContent.trim(),
        val: parseFloat(r.querySelector('.grnd-fbar-val')?.textContent || 'NaN'),
      }));
      return { sig: sig ? sig.textContent.trim() : null, bars, foot: (document.querySelectorAll('#view .grnd-foot')[1] || {}).textContent };
    };
    const out = {};
    // the exact live population that produced the contradiction
    liveState.on = true;
    liveState.grounding = { verdicts: { match: 129, partial: 0, deviation: 192 }, verdict_total: 321, match_rate: 0.4,
      predictions: 321, avg_real_tp: 0.4019, avg_control_tp: 0.2305, signal_ratio: 0.4019 / 0.2305, precision: 0.5, recall: 0.5,
      avg_false_positives: 0.3, bands: {}, floor_holds: 0, control_silent: false };
    route('grounding'); out.real = read();
    // a genuinely silent control: no finite ratio is honest
    liveState.grounding = Object.assign({}, liveState.grounding, { avg_control_tp: 0, signal_ratio: 0, control_silent: true });
    route('grounding'); out.silent = read();
    // nothing scored at all: "at or below chance" would be a verdict on no evidence
    liveState.grounding = Object.assign({}, liveState.grounding, { predictions: 0, avg_real_tp: 0, avg_control_tp: 0, signal_ratio: 0, control_silent: false });
    route('grounding'); out.none = read();
    return out;
  });
  const bars = gr.real.bars;
  check('the falsifiability bars render both tracks', bars.length === 2 && bars.every(b => !isNaN(b.val)), JSON.stringify(bars));
  const impliedBeats = bars[0].val > bars[1].val;
  check('headline agrees with the bars it summarises', impliedBeats && /beats chance/i.test(gr.real.sig) && !/at or below/i.test(gr.real.sig),
    `bars ${bars[0].val} vs ${bars[1].val}, headline "${gr.real.sig}"`);
  check('the headline prints the ratio the bars imply', /×1\.7/.test(gr.real.sig), gr.real.sig);
  check('the prose cites the same two numbers', /0\.4/.test(gr.real.foot) && /0\.23/.test(gr.real.foot), String(gr.real.foot).slice(0, 120));
  check('a silent control prints no manufactured number', /∞/.test(gr.silent.sig) && /never hit/i.test(gr.silent.sig), gr.silent.sig);
  check('no scored predictions is not a verdict', /no scored predictions/i.test(gr.none.sig) && !/below chance/i.test(gr.none.sig), gr.none.sig);

  // ---- 4. #regime never names a posture the badge contradicts ----
  const rgm = await page.evaluate(() => {
    const out = {};
    liveState.on = true; liveState.postureStale = false;
    liveState.regime = { resolutions: [], actuations: [], deferred_verdicts: [], lane_coverage: [] };
    for (const [k, may, mode, stale] of [['on', true, 'Semi-auto', false], ['off', false, 'Shadow', false], ['stale', false, 'Shadow', true]]) {
      liveState.mayActuate = may; liveState.mode = mode; liveState.postureStale = stale;
      route('regime');
      const txt = document.querySelector('#view').innerText;
      out[k] = { badge: (document.querySelector('#view .rgm-mode-name') || {}).textContent, shadow: (txt.match(/Under Shadow/gi) || []).length,
                 gateOff: /mode withholds actuation/i.test(txt), gateOn: /mode permits actuation/i.test(txt), stale: /posture is stale/i.test(txt) };
    }
    liveState.postureStale = false;
    return out;
  });
  check('gate ON: badge says ACTUATING and the copy agrees', rgm.on.badge === 'ACTUATING' && rgm.on.gateOn && !rgm.on.gateOff, JSON.stringify(rgm.on));
  check('gate OFF: badge says READ-ONLY and the copy agrees', rgm.off.badge === 'READ-ONLY' && rgm.off.gateOff && !rgm.off.gateOn, JSON.stringify(rgm.off));
  check('stale posture makes no gate claim at all', rgm.stale.badge === 'UNVERIFIED' && rgm.stale.stale && !rgm.stale.gateOn && !rgm.stale.gateOff, JSON.stringify(rgm.stale));
  check('no view asserts "Under Shadow" as a constant', rgm.on.shadow === 0 && rgm.off.shadow === 0 && rgm.stale.shadow === 0,
    `on=${rgm.on.shadow} off=${rgm.off.shadow} stale=${rgm.stale.shadow}`);

  // ---- 5. the fixture liveness simulator no longer speaks into the live region ----
  const ann = await page.evaluate(async () => {
    document.querySelector('#tgAnnounce').textContent = '';
    await new Promise(r => setTimeout(r, 1500));
    return document.querySelector('#tgAnnounce').textContent;
  });
  check('no fixture verdict is announced unprompted', !/s-6c55|prune completed/i.test(ann), JSON.stringify(ann));

  // ---- 6. the toast is something you read, never something you hit ----
  // WCAG 2.4.11: a fixed z-90 box 22px off the bottom edge covered the focused control on #signals. And the
  // worse half: at opacity 0 it stayed a hit-target — elementFromPoint over a live button returned the
  // toast's span, permanently, after the first toast was ever raised.
  const toastHit = await page.evaluate(async () => {
    toast('a real notification', '');
    await new Promise(r => setTimeout(r, 60));
    const t = document.querySelector('#toast');
    const b = t.getBoundingClientRect();
    const mid = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
    const shownSteals = !!(mid && (mid === t || t.contains(mid)));
    const pe = getComputedStyle(t).pointerEvents;
    await new Promise(r => setTimeout(r, 4000));   // past the 3400ms auto-hide + the collapse
    const b2 = t.getBoundingClientRect();
    const after = document.elementFromPoint(b2.left + Math.max(1, b2.width / 2), b2.top + Math.max(1, b2.height / 2));
    return { shownSteals, pe, hiddenSteals: !!(after && (after === t || t.contains(after))), w: b2.width, h: b2.height, txt: t.textContent };
  });
  check('a VISIBLE toast does not steal the click beneath it', !toastHit.shownSteals && toastHit.pe === 'none', JSON.stringify({ steals: toastHit.shownSteals, pointerEvents: toastHit.pe }));
  // NOTE ON WHAT THESE TWO CONTROLS PROVE SEPARATELY. Restoring pointer-events:auto turns `pointerEvents`
  // red but leaves `steals` false — in this fixture state the toast happens to sit over empty page, so the
  // theft is not reproducible here and the LIVE e2e owns that half. Restoring the populated hidden box
  // turns the collapse check red. The two fixes are complementary and each control falsifies one of them;
  // neither control alone would catch both, which is why both assertions exist.
  check('a HIDDEN toast is not a permanent hit-target', !toastHit.hiddenSteals, JSON.stringify(toastHit));
  check('a hidden toast collapses to nothing', toastHit.txt === '', JSON.stringify(toastHit.txt));
} finally { await browser.close(); }

console.log(failed ? `surface-contradictions: ${failed} FAILED` : 'surface-contradictions: all checks passed');
process.exit(failed ? 1 : 0);
