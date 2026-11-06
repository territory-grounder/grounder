// Console e2e — #axes PAYLOAD COVERAGE: measured values provably reach the rendered cells (TG-482).
//
// WHY THIS EXISTS. every-view-renders.mjs proves #axes EXISTS (routes, renders chars, leaks nothing) —
// but its fallthrough `{}` payload walks only the loading/empty/gap branches, so not one MEASURED value
// has ever been proven to reach the screen. A view that dropped a field, read a wrong key, or bound a
// value to the WRONG axis row would stay green. This oracle closes that gap the same way gate-margins.mjs
// does: a real-shaped payload with distinctive values, asserted cell-by-cell against the labels they
// belong to — plus the anti-fabrication converse (a sentinel value NOT in the payload must render
// nowhere), the failed-read honesty state, and the empty-but-valid window.
//
// THE PAYLOAD IS THE REAL SERIALIZED core/axis.Scorecard, field-for-field from the json tags in
// core/axis/scorecard.go (the same artifact `axisscore -json` emits; httpapi passes bytes through).
// G5/G6 sub-objects carry Go field names verbatim (Windows/Claimed/…, Executed/…) — db.Falsifiability
// and db.LoopBypass in core/db/axis_read.go have no JSON tags. Do NOT "tidy" any name here: a rename
// must break against the module reading the REAL key, never be papered over in this stub (the
// false-green trap documented at the head of gate-margins.mjs).
//
// BINDING, not presence. Every scorecard row renders as .ax-kv {.ax-kv-l label, .ax-kv-v value,
// .ax-kv-d denominator} (modules/axes/js.txt axRow), and tables as .ax-tbl rows. Assertions resolve the
// ROW BY ITS LABEL and read the value INSIDE that row — "86.2% appeared somewhere" can never pass for
// "A1's recall cell shows A1's value". Swapping any two payload values goes red here (proven during
// authoring: a1_detection_recall ⇄ a3_heal_success_rate).
//
// CONTRACT AMBIGUITY FOUND (asserted on the SAFER reading, module unchanged): with a zero-valued
// scorecard the hero's proposal/prediction figures render "0 proposals (0.0% of incidents)" — axPct of
// a rate whose denominator (incidents=0) is empty, the one spot a 0.0% renders over 0/0. It sits beside
// its zero numerator so it reads as 0-of-0 context, and the module renders it deliberately
// (unconditional in axRender's hero). This oracle does NOT bless it: it asserts every %-token in the
// empty state is exactly "0.0%" (nothing NON-zero is fabricated) and that no .ax-kv VALUE cell — the
// cells that carry measurement claims — renders any percentage at all in an empty window.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node axes-payload.mjs
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;

let fail = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) fail++;
};

// ---- the serialized Scorecard, distinctive non-round values, internally consistent ------------------
// A1: 25/29 detected -> 86.2% · A3: 8/17 confirmed -> 47.1% · A4 among-actionable 81.4% of 43 proposals.
// Every count cross-foots (per-class detections sum to 25/29; A7's 9 uncleared = 17 mutated − 8 cleared;
// G6 audits the same 17 executed heals) so a value landing in the WRONG cell cannot still look coherent.
const SCORECARD = {
  window: '168h', since: '2026-08-06T09:30:00Z',
  incidents: 137, judged: 121,                  // hero: 121 judged (88.3%)
  proposal_rate: 0.314, prediction_rate: 0.577, // hero: 43 proposals (31.4%) · 79 predictions (57.7%)
  a2_dimension_means: [
    { dimension: 'evidence-binding', mean: 3.21, n: 118 },
    { dimension: 'root-cause', mean: 3.58, n: 121 },
    { dimension: 'blast-radius', mean: 4.02, n: 97 },
  ],
  a2_overall: 3.47,
  a2_verified_match_rate: 0.706, a2_verified_n: 34,
  a2_verdicts: [{ key: 'match', n: 24 }, { key: 'deviation', n: 7 }, { key: 'unverifiable', n: 3 }],
  a2_blast_precision: 0.638, a2_blast_control_precision: 0.412, // lift +22.6pp
  a2_blast_recall: 0.549, a2_blast_scored: 51, a2_blast_measurable: true,
  a4_autonomy_rate: 0.234, a4_autonomy_among_actionable: 0.814, a4_handled_without_human: 0.548,
  a4_proposals: 43, a4_autonomous_stops: 37,
  a4_bands: [{ key: 'AUTO', n: 21 }, { key: 'AUTO_NOTICE', n: 14 }, { key: 'POLL_PAUSE', n: 63 }],
  a4_poll_reasons: [{ key: 'risk-above-band', n: 41 }, { key: 'novel-op-class', n: 22 }],
  a4_attrib_escal_total: 31, a4_attrib_escal_on_injected: 27, // 87.1% harness-artefact share
  a5_fault_class_breadth: 4,
  a5_op_classes: ['restart-service', 'restart-container', 'start-guest', 'clear-tmp'],
  a5_graduated_breadth: 3,
  a5_graduated_op_classes: ['restart-service', 'restart-container', 'start-guest'],
  a5_notice_breadth: 2, a5_notice_op_classes: ['restart-service', 'start-guest'],
  a5_raw_ops: [], fault_types: 19,
  a6a_mean_decision_steps: 6.3, a6a_n: 121,
  a6b_time_to_decision_median_ms: 83600, a6b_time_to_decision_p95_ms: 197400, a6b_time_to_decision_n: 104,
  a6b_time_to_recovery_median_sec: 342, a6b_time_to_recovery_p95_sec: 1264, a6b_n: 9,
  g5_falsifiability: { Windows: 12, NoClaim: 5, Claimed: 7, ClaimedPassed: 5, Passed: 10,
    RealTP: 23, ControlTP: 6, LosingRatio: 1.18 },   // 5/7 -> 71.4%; naive 10/12 -> 83.3%
  g6_loop_bypass: { Executed: 17, Bypassing: 0, NoPrediction: 0, NoVerdict: 0 },
  a1_detection_recall: 0.862, a1_injected: 29, a1_detected: 25,
  a1_by_class: [
    { class: 'service-down', injected: 12, detected: 11 },   // 91.7%
    { class: 'disk-pressure', injected: 9, detected: 8 },    // 88.9%
    { class: 'guest-down', injected: 8, detected: 6 },       // 75.0%
  ],
  a1_detection_latency_by_source: [
    { source: 'librenms', first_detections: 18, median_sec: 41, p95_sec: 174 },
    { source: 'tg-liveness', first_detections: 7, median_sec: 39, p95_sec: 58 },
  ],
  a3_heal_success_rate: 0.471, a3_mutated: 17, a3_confirmed_clear: 8,
  a7_false_actuation_rate: 0, a7_suspicious_actuations: 0, a7_uncleared_actuations: 9,
  a8_breaches: 0, a8_ledger_rows: 5417, a8_breaker_trips: 2, a8_force_shadow_demotions: 1,
  axes_not_live_measurable: null,   // nil slice marshals null — all eight measurable; badge must read 0
};

// ANTI-FABRICATION SENTINELS: in neither payload nor any value the module can derive from it (every
// division/round the render performs was enumerated while choosing these). If either substring reaches
// the axes view, the surface invented a number — the exact overstatement this module exists to prevent.
const SENTINELS = ['9464', '73.9'];

// Distinctive payload values that must NEVER survive a failed or empty read (fixture-bleed converse:
// this module ships NO fixture, so any of these on a non-live render is a fabricated measurement).
const LIVE_MARKERS = ['86.2', '47.1', '81.4', '5417', '3.47'];

// /v1/estate as core/httpapi/estate.go really shapes it: EstateEdge = {from,to,rel,confidence,source} —
// NO delay_seconds/recovery_seconds (not serialized today). The chaos section must render its honest
// documented boundary for this real DTO, never fabricate timing counts. Realism is load-bearing: an
// invented timing field here would prove a branch production can never take.
const ESTATE = {
  available: true, captured_at: '2026-08-13T04:10:00Z', node_count: 2, edge_count: 1, source_count: 1,
  nodes: [{ name: 'dc1mealie01', type: 'lxc' }, { name: 'dc1pve01', type: 'node' }],
  edges: [{ from: 'dc1mealie01', to: 'dc1pve01', rel: 'runs-on', confidence: 0.92, source: 'proxmox' }],
};
const WHOAMI = { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' };

async function mount(page, axesResponder) {
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: WHOAMI });
    if (p === '/v1/axes') return axesResponder(route);
    if (p === '/v1/estate') return route.fulfill({ json: ESTATE });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#axes', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // liveAdopt re-renders the landed hash BEFORE stamping lastRefresh, so once the stamp exists #view
  // holds the post-adopt #axes render — a deterministic settle, no fixed sleep (narrow-viewport rule).
  await page.waitForFunction(() => typeof liveState !== 'undefined' && !!liveState.lastRefresh, { timeout: 20000 });
}
const viewText = page => page.evaluate(() => document.querySelector('#view')?.innerText || '');
// Resolve a scorecard row BY ITS LABEL and read the value/denominator cells INSIDE it. null = row absent,
// which every caller must treat as its own failure — a renamed label must never pass vacuously.
const kvRow = (page, label) => page.evaluate(lbl => {
  const row = [...document.querySelectorAll('#view .ax-kv')]
    .find(r => (r.querySelector('.ax-kv-l')?.textContent || '').trim() === lbl);
  return row ? {
    v: (row.querySelector('.ax-kv-v')?.textContent || '').trim(),
    d: (row.querySelector('.ax-kv-d')?.textContent || '').trim(),
  } : null;
}, label);
// Resolve a table row by its first cell; returns the full cell text list (same vacuity discipline).
const tblRow = (page, first) => page.evaluate(f => {
  for (const tr of document.querySelectorAll('#view .ax-tbl tbody tr')) {
    const cells = [...tr.querySelectorAll('td')].map(td => (td.textContent || '').trim());
    if (cells[0] === f) return cells;
  }
  return null;
}, first);
const badgeText = page => page.evaluate(() =>
  document.querySelector('[data-badge="axes"]')?.textContent?.trim() ?? null);

const browser = await chromium.launch();
try {
  // ============ 1. MEASURED VALUES REACH THEIR OWN CELLS (label -> value -> denominator) ============
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ json: SCORECARD }));
    await page.waitForSelector('#view .ax-hero', { timeout: 10000 });

    const text = await viewText(page);
    check('the live scorecard rendered substantially', text.length > 600,
      `#axes rendered only ${text.length} chars — nothing below is meaningful`);
    check('the surface is tagged LIVE', /LIVE · benchmark-axis scoreboard/i.test(text),
      'the hero label is missing — the view did not adopt the live read');
    check('the window and since render in the hero', text.includes('window: last 168h · since 2026-08-06 09:30Z'),
      'window/since (sc.window, sc.since) did not reach the hero line');
    check('hero: incidents count binds', /137\s+incidents triaged/.test(text),
      'sc.incidents (137) is not beside "incidents triaged"');
    check('hero: judged count carries its share of incidents', /121\s+judged \(88\.3%\)/.test(text),
      'sc.judged (121) with its derived 121/137 share (88.3%) did not render');

    // The three headline (label, value) bindings — each read from ITS OWN row, denominator included.
    const a1 = await kvRow(page, 'recall — injected faults TG detected');
    check('A1 row exists (label match)', a1 !== null,
      'no .ax-kv labelled "recall — injected faults TG detected" — the label this oracle rests on stopped matching');
    check('A1: recall cell shows A1\'s own value 86.2%', a1?.v === '86.2%',
      `A1 value cell reads ${JSON.stringify(a1?.v)} — a1_detection_recall (0.862) did not reach its cell`);
    check('A1: the denominator is the ground-truth 25/29', !!a1 && a1.d.includes('(25/29 injected'),
      `A1 denominator reads ${JSON.stringify(a1?.d)} — a1_detected/a1_injected did not reach the row`);

    const a3 = await kvRow(page, 'confirmed-clear rate (a FLOOR)');
    check('A3 row exists (label match)', a3 !== null, 'no .ax-kv labelled "confirmed-clear rate (a FLOOR)"');
    check('A3: heal cell shows A3\'s own value 47.1%', a3?.v === '47.1%',
      `A3 value cell reads ${JSON.stringify(a3?.v)} — a3_heal_success_rate (0.471) did not reach its cell`);
    check('A3: the denominator is 8/17 confirmed', !!a3 && a3.d.includes('(8/17 actuated heals confirmed clear)'),
      `A3 denominator reads ${JSON.stringify(a3?.d)}`);

    const a4 = await kvRow(page, 'among ACTIONABLE');
    check('A4 row exists (label match)', a4 !== null, 'no .ax-kv labelled "among ACTIONABLE"');
    check('A4: among-actionable cell shows A4\'s own value 81.4%', a4?.v === '81.4%',
      `A4 value cell reads ${JSON.stringify(a4?.v)} — a4_autonomy_among_actionable (0.814) did not reach its cell`);
    check('A4: divided by the 43 proposals, named', !!a4 && a4.d.includes('/ 43 proposals)'),
      `A4 denominator reads ${JSON.stringify(a4?.d)} — a4_proposals did not reach the row`);

    // Further row bindings across the scorecard's breadth (weighted mean, census, G5 rate, G6 audit).
    const a2o = await kvRow(page, 'overall (sample-weighted)');
    check('A2: overall binds (3.47 across 3 dimensions)', !!a2o && a2o.v === '3.47' && a2o.d === '(across 3 dimensions)',
      `A2 overall row reads ${JSON.stringify(a2o)} — a2_overall (3.47) did not reach its cell`);
    const a8 = await kvRow(page, 'guardrail breaches (SHALL be 0)');
    check('A8: breaches bind (0 of the 5417-row ledger)', !!a8 && a8.v === '0' && a8.d.includes('of 5417 total'),
      `A8 row reads ${JSON.stringify(a8)} — a8_breaches/a8_ledger_rows did not reach the row`);
    const g5 = await kvRow(page, 'beat its control WHERE IT MADE A CLAIM');
    check('G5: claimed-window pass rate binds (71.4% = 5 of 7)', !!g5 && g5.v === '71.4%' && g5.d.includes('(5 of 7 claimed windows'),
      `G5 row reads ${JSON.stringify(g5)} — ClaimedPassed/Claimed (5/7) did not reach the row`);
    check('G5: the naive rate it refuses is named (83.3%, 10/12)', text.includes('83.3%') && text.includes('(10/12)'),
      'the no-claim exclusion note does not carry the naive Passed/Windows rate the honest one prevents');
    const g6 = await kvRow(page, 'loop-bypassing heals (SHALL be 0)');
    check('G6: bypass count binds (0 of 17 executed)', !!g6 && g6.v === '0' && g6.d === '(of 17 executed heals in window)',
      `G6 row reads ${JSON.stringify(g6)} — Bypassing/Executed did not reach the row`);

    // Table bindings: value in the SAME row as its own label — and NOT another label's value.
    const dim = await tblRow(page, 'evidence-binding');
    check('A2 dims: evidence-binding row exists', dim !== null, 'no .ax-tbl row leads with "evidence-binding"');
    check('A2 dims: evidence-binding carries ITS mean 3.21 (n=118)', !!dim && dim[1] === '3.21' && dim[2] === '118',
      `evidence-binding row is ${JSON.stringify(dim)} — its own mean/n did not land in its own row`);
    check('A2 dims: evidence-binding does NOT carry blast-radius\'s 4.02', !!dim && !dim.includes('4.02'),
      `evidence-binding row ${JSON.stringify(dim)} contains 4.02 — a value bound to the WRONG dimension`);
    const blast = await tblRow(page, 'blast-radius');
    check('A2 dims: blast-radius carries ITS mean 4.02 (n=97)', !!blast && blast[1] === '4.02' && blast[2] === '97',
      `blast-radius row is ${JSON.stringify(blast)}`);
    const lat = await tblRow(page, 'librenms');
    check('A1 latency: librenms row carries its own triple (18, 41s, 174s)',
      !!lat && lat[1] === '18' && lat[2] === '41s' && lat[3] === '174s',
      `librenms latency row is ${JSON.stringify(lat)} — first_detections/median_sec/p95_sec did not bind`);
    const cls = await tblRow(page, 'disk-pressure');
    check('A1 per-class: disk-pressure recall derives from ITS row (88.9%, 8/9)',
      !!cls && cls[1] === '88.9%' && cls[2] === '8/9',
      `disk-pressure class row is ${JSON.stringify(cls)}`);

    // Cross-cutting derived figures that only exist if the right fields fed the right arithmetic.
    check('A2 blast: lift derives from precision minus control (+22.6pp)', text.includes('lift +22.6pp'),
      'the +22.6pp lift ((0.638-0.412)*100) did not render — precision/control did not meet');
    check('A4 harness artefact: 27 of 31 escalations (87.1%), reported never subtracted',
      /27 of 31 attribution escalations \(87\.1%\)/.test(text),
      'a4_attrib_escal_on_injected/total did not reach the flag');
    check('A7: zero suspicious actuations carries its rule-of-three bound over n=17',
      text.includes('<=17.6% at 95% confidence (rule of three: 3/17)'),
      'the REQ-2502 zero-numerator bound (300/17 -> 17.6%) did not render');
    const silent = await kvRow(page, 'of which silent auto (acts, nobody hears)');
    check('A5: the silent split derives in its own row (3 graduated − 2 notice = 1)', !!silent && silent.v === '1',
      `silent-auto row reads ${JSON.stringify(silent)} — graduated(3) − notice(2) did not derive`);

    // Chaos calibration: the REAL estate DTO carries no timing fields — the documented boundary renders,
    // and no timing count is fabricated from fields the snapshot does not serialize.
    check('chaos: the honest no-timing-fields boundary renders for the real EstateEdge DTO',
      /No edge in the adopted snapshot carries a learned-timing field/.test(text),
      'the documented boundary sentence is missing — what rendered instead is claiming more than the DTO carries');
    check('chaos: no timing count is fabricated', !/edges with a learned propagation delay/.test(text),
      'a learned-delay count rendered from a snapshot that serializes no timing fields');

    // Anti-fabrication: sentinels absent from the payload must be absent from the render.
    for (const s of SENTINELS)
      check(`anti-fabrication: sentinel "${s}" renders nowhere`, !text.includes(s),
        `"${s}" is on the axes view but in no payload field — the surface invented a number`);

    // The rail badge is the REAL gap count from the landed read (nil slice -> 0), never a placeholder.
    const badge = await badgeText(page);
    check('rail badge is the real not-live-measurable count (0)', badge === '0',
      `[data-badge="axes"] reads ${JSON.stringify(badge)} — want "0" from the landed read's empty gap list`);
    check('all-measurable coverage line renders', /All eight scored axes \(A1–A8\) are live-measurable/.test(text),
      'the coverage card does not state the no-gaps conclusion the payload asserts');

    check('no undefined/NaN/[object Object] leaks (live)', !/\bundefined\b|\bNaN\b|\[object Object\]/.test(text),
      'raw junk reached the operator');
    check('no uncaught page errors (live)', errs.length === 0, errs.join(' | '));
    await ctx.close();
  }

  // ============ 2. FAILED READ (500) — "could not be read", NO cells, NO payload value ==============
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, route => route.fulfill({ status: 500, body: 'aggregate failed' }));
    await page.waitForFunction(() =>
      /could not be read/i.test(document.querySelector('#view')?.innerText || ''), { timeout: 10000 });

    const text = await viewText(page);
    check('a 500 renders the honest could-not-read state', /The axis scoreboard could not be read/i.test(text),
      'the fail-closed copy is missing');
    const cells = await page.evaluate(() => document.querySelectorAll('#view .ax-kv').length);
    check('a 500 renders ZERO score cells', cells === 0,
      `${cells} .ax-kv row(s) rendered on a failed read — a scorecard nobody could read has no cells`);
    const hero = await page.evaluate(() => !!document.querySelector('#view .ax-hero'));
    check('a 500 renders no hero figures', !hero, 'the hero (incidents/judged/proposals) rendered on a failed read');
    for (const m of LIVE_MARKERS)
      check(`no fixture value survives a 500 ("${m}")`, !text.includes(m),
        `distinctive payload value "${m}" is on a FAILED read — this module has no fixture, so it was fabricated`);
    for (const s of SENTINELS)
      check(`sentinel "${s}" absent on a 500`, !text.includes(s), `"${s}" rendered from nowhere`);
    const badge = await badgeText(page);
    check('rail badge stays an honest "—" on a failed read', badge === '—',
      `[data-badge="axes"] reads ${JSON.stringify(badge)} — a count must never sit on the rail when the read failed`);
    check('no uncaught page errors (500)', errs.length === 0, errs.join(' | '));
    await ctx.close();
  }

  // ============ 3. EMPTY-BUT-VALID (a quiet window's zero scorecard) — honest, not zero-flavored ====
  {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    // What the aggregate actually emits for a window with no incidents: zero counts, zero rates, nil
    // slices marshalled null, zero-valued G5/G6 sub-objects (Go zero marshal of the same struct).
    const EMPTYCARD = {
      window: '168h', since: '2026-08-06T09:30:00Z',
      incidents: 0, judged: 0, proposal_rate: 0, prediction_rate: 0,
      a2_dimension_means: null, a2_overall: 0,
      a2_verified_match_rate: 0, a2_verified_n: 0, a2_verdicts: null,
      a2_blast_precision: 0, a2_blast_control_precision: 0, a2_blast_recall: 0, a2_blast_scored: 0,
      a2_blast_measurable: false,
      a4_autonomy_rate: 0, a4_autonomy_among_actionable: 0, a4_handled_without_human: 0,
      a4_proposals: 0, a4_autonomous_stops: 0, a4_bands: null, a4_poll_reasons: null,
      a4_attrib_escal_total: 0, a4_attrib_escal_on_injected: 0,
      a5_fault_class_breadth: 0, a5_op_classes: null, a5_graduated_breadth: 0,
      a5_graduated_op_classes: null, a5_notice_breadth: 0, a5_notice_op_classes: null,
      a5_raw_ops: null, fault_types: 0,
      a6a_mean_decision_steps: 0, a6a_n: 0,
      a6b_time_to_decision_median_ms: 0, a6b_time_to_decision_p95_ms: 0, a6b_time_to_decision_n: 0,
      a6b_time_to_recovery_median_sec: 0, a6b_time_to_recovery_p95_sec: 0, a6b_n: 0,
      g5_falsifiability: { Windows: 0, NoClaim: 0, Claimed: 0, ClaimedPassed: 0, Passed: 0,
        RealTP: 0, ControlTP: 0, LosingRatio: 0 },
      g6_loop_bypass: { Executed: 0, Bypassing: 0, NoPrediction: 0, NoVerdict: 0 },
      a1_detection_recall: 0, a1_injected: 0, a1_detected: 0, a1_by_class: null,
      a1_detection_latency_by_source: null,
      a3_heal_success_rate: 0, a3_mutated: 0, a3_confirmed_clear: 0,
      a7_false_actuation_rate: 0, a7_suspicious_actuations: 0, a7_uncleared_actuations: 0,
      a8_breaches: 0, a8_ledger_rows: 0, a8_breaker_trips: 0, a8_force_shadow_demotions: 0,
      axes_not_live_measurable: null,
    };
    await mount(page, route => route.fulfill({ json: EMPTYCARD }));
    await page.waitForSelector('#view .ax-hero', { timeout: 10000 });

    const text = await viewText(page);
    // The enabling-event axes render their missing-input sentences, never a measured 0.0%.
    check('A1 empty: named gap, not a zero recall',
      /A1 detection recall — not live-measurable this window/.test(text) && text.includes('injected_fault ledger is empty'),
      'A1 did not render its missing-input sentence for an empty injected-fault ledger');
    check('A3 empty: named gap, not a zero heal rate', /A3 heal success rate — not live-measurable this window/.test(text),
      'A3 did not render its missing-input sentence with no mutated incidents');
    check('A7 empty: named gap, not a zero false-actuation rate', /A7 false-actuation rate — not live-measurable this window/.test(text),
      'A7 did not render its missing-input sentence with no mutated incidents');
    // Rates over empty denominators say so; they do not fabricate percentages.
    const raw = await kvRow(page, 'actuation autonomy (raw)');
    check('A4 raw over 0 incidents is an em-dash beside its reason', !!raw && raw.v === '—' && raw.d.includes('no incidents in window'),
      `A4 raw row reads ${JSON.stringify(raw)} — want "—" with the no-denominator note, never 0.0%`);
    const amongEmpty = await kvRow(page, 'among ACTIONABLE');
    check('A4 among-actionable over 0 proposals says "not defined"', !!amongEmpty && amongEmpty.v === 'not defined',
      `among-ACTIONABLE reads ${JSON.stringify(amongEmpty)} — a rate over an empty denominator would be vacuous`);
    const blastEmpty = await kvRow(page, 'blast-radius prediction');
    check('A2 blast with tp+fp=0 is "UNDEFINED, not zero"', !!blastEmpty && blastEmpty.v === 'UNDEFINED, not zero',
      `blast row reads ${JSON.stringify(blastEmpty)} — 0/0 must render as undefined, never 0.0%`);
    check('A2 empty: the judge makes no claim', /No judged incidents in window/.test(text),
      'the A2 no-claim sentence is missing');
    check('G5 empty: absent is not zero', /No scored windows in this period/.test(text),
      'G5 did not render its no-claim empty state');
    check('G6 empty: absent is not a pass', /No executed heals in this period/.test(text),
      'G6 did not render its nothing-to-audit empty state');
    // The SAFER global floor (see header): no VALUE cell carries a percentage in an empty window, and
    // every %-token anywhere is exactly "0.0%" (the hero's two 0-of-0 context figures) — nothing
    // non-zero is fabricated. A future fix removing those two hero tokens still passes here.
    const pctCells = await page.evaluate(() =>
      [...document.querySelectorAll('#view .ax-kv-v')].map(el => el.textContent.trim()).filter(v => v.includes('%')));
    check('empty window: no measurement cell claims a rate', pctCells.length === 0,
      `value cells claiming rates over an empty window: ${JSON.stringify(pctCells)}`);
    const pctTokens = (text.match(/\d+(?:\.\d+)?%/g) || []).filter(t => t !== '0.0%');
    check('empty window: no non-zero percentage renders anywhere', pctTokens.length === 0,
      `fabricated non-zero rate(s) in an empty window: ${JSON.stringify(pctTokens)}`);
    for (const m of LIVE_MARKERS)
      check(`no live-case value bleeds into the empty window ("${m}")`, !text.includes(m),
        `"${m}" rendered in the empty window`);
    check('no undefined/NaN/[object Object] leaks (empty)', !/\bundefined\b|\bNaN\b|\[object Object\]/.test(text),
      'raw junk reached the operator');
    check('no uncaught page errors (empty)', errs.length === 0, errs.join(' | '));
    await ctx.close();
  }

  await browser.close();
} catch (e) {
  await browser.close();
  console.error('AXES-PAYLOAD E2E ERROR:', e);
  process.exit(1);
}

if (fail) {
  console.error(`axes-payload: FAIL (${fail} check(s))`);
  process.exit(1);
}
console.log('axes-payload: PASS — every asserted scorecard value reaches its OWN labelled cell with its denominator, sentinels render nowhere, a 500 is an honest could-not-read with zero cells and an em-dash badge, and an empty window renders named gaps and undefined rates, never fabricated zeros.');
process.exit(0);
