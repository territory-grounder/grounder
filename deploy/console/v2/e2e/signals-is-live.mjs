// #signals WAS THE LAST WHOLLY-SYNTHETIC VIEW ON THE PRODUCTION CONSOLE.
//
// Eleven views had a live override; this one had none, so it rendered its design fixture verbatim. Every value
// came from `sigNoise(t,seed) = Math.sin(t*12.9898 + seed*78.233)*43758.5453` — a hash PRNG — and sigP99()
// scripted an invented incident: a "rev 119 regression -> ~1.8s", a "rollback recovery", a "w3 eviction
// wobble". The rail badge was `sigAnomalyCount(){ return 2; }`, a hard-coded 2 beside real badges. The toolbar
// said "fixture · representative, not live", which a page of charts comfortably drowns out.
//
// The reason this needed an oracle and not just a fix: the failure mode is INVISIBLE. Synthetic series look
// exactly like real ones. The only mechanical distinction is whether the numbers move with the data, so that
// is what these checks assert — the rendered totals must equal the fetched rows, and must CHANGE when the rows
// change. A screenshot comparison could never catch it.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const iso = minsAgo => new Date(Date.now() - minsAgo * 60000).toISOString();
// Distinctive, checkable cardinalities — nothing here is a round number a fixture would plausibly also show.
const ALERTS = [7, 19, 42, 61, 130, 200].map((m, i) => ({
  external_ref: `al-${i}`, host: `dc1h${i}`, alert_rule: 'Device-Down', severity: 'critical',
  received_at: iso(m), observed_at: iso(m), source_type: 'librenms',
}));
const SESSIONS = [3, 11, 25, 55, 99].map((m, i) => ({
  external_ref: `run-${i}`, band: 'AUTO', verdict: i === 0 ? 'deviation' : 'match',
  action_id: `a${i}`, op_class: 'restart-service', classified_at: iso(m),
}));
const ACTIONS = [5, 15, 45].map((m, i) => ({
  action_id: `act-${i}`, op: 'restart-service', op_class: 'restart-service', target: `dc1h${i}`,
  band: 'AUTO', verdict: i === 0 ? 'deviation' : 'match', reversible: true,
  classified: true, predicted: true, approved: true, executed: true, verified: i !== 0,
  has_confidence: false, sealed_at: iso(m),
}));
const CAPS = { capabilities: [
  { surface: 'ingest',    source_type: 'librenms', capability: 'ingest.librenms', enabled: true },
  { surface: 'ingest',    source_type: 'crowdsec', capability: 'ingest.crowdsec', enabled: true },
  { surface: 'actuation', source_type: 'ssh',      capability: 'actuation.ssh',   enabled: false },
  { surface: 'actuation', source_type: 'proxmox',  capability: 'actuation.proxmox', enabled: false },
  { surface: 'cmdb',      source_type: 'netbox',   capability: 'cmdb.netbox',     enabled: true },
] };
const DECISIONS = { decisions: [0, 1, 2, 3, 4, 5, 6].map(i => ({
  external_ref: `d-${i}`, action_id: `ad${i}`, band: 'POLL_PAUSE', reversible: true,
  caller_can_act: true, prediction: 'p', plan: { approaches: ['a'] },
})) };

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1500, height: 1000 } })).newPage();
  let alerts = ALERTS;
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/alerts')) return j({ alerts, counts: { total: alerts.length } });
    if (u.includes('/v1/sessions')) return j({ total: SESSIONS.length, sessions: SESSIONS });
    if (u.includes('/v1/actions')) return j({ actions: ACTIONS, counts: { total: ACTIONS.length, verified: 2, deviations: 1 } });
    if (u.includes('/v1/decisions')) return j(DECISIONS);
    if (u.includes('/v1/capabilities')) return j(CAPS);
    if (u.includes('/v1/estate')) return j({ available: true, node_count: 369, edge_count: 4, source_count: 1,
      captured_at: '2026-07-30T00:00:00Z',
      nodes: Array.from({ length: 217 }, (_, i) => ({ name: `dc1h${i}`, health: i < 10 ? 'crit' : 'ok' })),
      edges: [] });
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above (alerts/sessions/actions/decisions/capabilities/estate are
  // all read in its sequential in-chain, so liveState is already settled) — one frame is enough margin for
  // the DOM route('signals') call below, not a guess at fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  const r = await page.evaluate(() => {
    route('signals');
    const v = document.querySelector('#view');
    const text = v.innerText || '';
    const charts = Array.from(v.querySelectorAll('svg.sig-chart'));
    return {
      text,
      citesEndpoints: /\/v1\/alerts/.test(text) && /\/v1\/decisions/.test(text),
      badge: (document.querySelector('[data-badge="signals"]') || {}).textContent,
      nums: Array.from(v.querySelectorAll('.sig-tv')).map(e => (e.textContent || '').trim()),
      hasFixtureMarker: /representative, not live/i.test(text),
      mentionsForecast: /forecast|envelope/i.test(text),

      // ---- DESIGN INTEGRITY (the half this oracle used to be blind to) ----
      charts: charts.length,
      // a chart is only a chart if it has a y-scale and a time axis
      gridLines: charts.reduce((n, c) => n + c.querySelectorAll('line.sig-grid').length, 0),
      axisLabels: charts.reduce((n, c) => n + c.querySelectorAll('text.sig-ax').length, 0),
      rangeChips: Array.from(v.querySelectorAll('.sig-rchip')).map(e => (e.textContent || '').trim()),
      tiles: Array.from(v.querySelectorAll('.sig-tile .lbl')).map(e => (e.textContent || '').trim()),
      sections: Array.from(v.querySelectorAll('.sig-sec')).length,
      sectionTitles: Array.from(v.querySelectorAll('.sig-sect')).map(e => (e.textContent || '').trim()),
      heatCells: v.querySelectorAll('.sig-cell').length,
      legendItems: v.querySelectorAll('.sig-lgi').length,
      panelTitles: Array.from(v.querySelectorAll('.sig-ptitle, .sig-phead .sig-pt')).map(e => (e.textContent || '').trim()),
      // the x-axis must be anchored to the REAL clock, not the fixture's 14:31
      axisTexts: charts.length ? Array.from(charts[0].querySelectorAll('text.sig-ax')).map(e => e.textContent) : [],
      // sampled in the SAME evaluate so the comparison uses one clock (the browser's), minute precision
      browserNowMinutes: (() => { const d = new Date(); return d.getHours() * 60 + d.getMinutes(); })(),
      // the facet that filters ON a predicted overlay must not survive when no panel has one
      facetChips: Array.from(document.querySelectorAll('[data-facet], .facet-chip')).map(e => (e.textContent || '').trim()),
      predFacetOffered: (typeof FACETS !== 'undefined' && Array.isArray(FACETS.signals))
        ? FACETS.signals.some(f => f[0] === 'pred') : null,
      sparkLabels: charts.map(e => e.getAttribute('aria-label')).filter(Boolean),
    };
  });

  // ---- THE REGRESSION THIS SECTION EXISTS TO CATCH ----
  // The first live wiring of #signals REPLACED the designed view with two unlabelled polylines and three bare
  // numbers. Every liveness assertion below still passed, because they only ever asked "is the number real?".
  // Wiring a fixture must not cost the design: these assert the chrome the design provides.
  check('the designed chart renderer is used (svg.sig-chart), not an ad-hoc sparkline',
    r.charts >= 2, `${r.charts} sig-chart elements — a hand-rolled polyline instead of sigMetricPanel loses axes, gridlines, thresholds and event markers`);
  check('charts have a y-scale and gridlines', r.gridLines >= 8,
    `${r.gridLines} gridlines across ${r.charts} charts — a line floating in white space is not a chart`);
  check('charts have axis labels', r.axisLabels >= 8, `${r.axisLabels} axis labels`);
  check('the 1h/6h/24h range selector is present and wired', r.rangeChips.length >= 3,
    `range chips = ${JSON.stringify(r.rangeChips)} — hard-coding one window drops a control the design offers`);
  check('the summary-tile strip is present', r.tiles.length >= 3, `tiles = ${JSON.stringify(r.tiles)}`);
  check('panels are grouped into sections with summaries', r.sections >= 2,
    `${r.sections} sections — a flat list of cards loses the design's summarize-first grouping`);
  check('the estate heat row is rendered (it was already live and simply not shown)',
    r.heatCells > 0, `${r.heatCells} heat cells — HOSTS is real on the live path, so omitting this discards real data`);
  check('the shared legend is rendered', r.legendItems >= 2, `${r.legendItems} legend items`);
  // The old predicate asserted the fixture's magic now ("14:31") is ABSENT — which false-fails whenever the
  // real clock's tick window legitimately contains 14:31 (it did, in CI, at 14:31Z: a once-an-hour flake).
  // Assert the POSITIVE property instead: the newest time-shaped tick equals the browser's own clock
  // (±3 min for render/sample skew). A chart still anchored to the fixture is hours stale and fails this
  // regardless of what time it is.
  const timeTicks = r.axisTexts.filter(t => /^\d{2}:\d{2}$/.test(t));
  const newestTick = timeTicks.length ? timeTicks[timeTicks.length - 1] : null;
  const tickMinutes = newestTick ? (parseInt(newestTick.slice(0, 2), 10) * 60 + parseInt(newestTick.slice(3), 10)) : null;
  const skew = tickMinutes === null ? null
    : Math.min(Math.abs(tickMinutes - r.browserNowMinutes), 1440 - Math.abs(tickMinutes - r.browserNowMinutes));
  check('the x-axis is anchored to the REAL clock (newest tick == browser now ±3min)',
    timeTicks.length > 0 && skew !== null && skew <= 3,
    `axis ticks = ${JSON.stringify(r.axisTexts)}, newest time tick = ${newestTick}, browser now = ${Math.floor(r.browserNowMinutes / 60)}:${String(r.browserNowMinutes % 60).padStart(2, '0')} — a chart not re-anchored to sigNOWMIN labels live data with stale times`);
  check('the "Predicted overlay" facet is withdrawn when no panel carries one',
    r.predFacetOffered === false,
    'a facet that filters on band/futureBand/forecast would match zero live panels and blank the page');

  check('the live view cites the endpoints it derived from', r.citesEndpoints === true, JSON.stringify(r.text.slice(0, 200)));
  check('the stale "fixture · representative" marker is gone on the live path', r.hasFixtureMarker === false,
    'the fixture toolbar is still rendering');

  // THE LOAD-BEARING CHECK: the rendered series equal the fetched rows.
  // Keyed on the panel TITLE the design renders, not a whole-page regex — a page-wide match gave a FALSE PASS
  // once when the actions panel drew nothing and the digit turned up in the deviation ratio instead.
  const titles = r.sectionTitles.join(' | ');
  check('sections name the real pipeline stages', /intake/i.test(titles) && /actuation/i.test(titles),
    `section titles = ${JSON.stringify(r.sectionTitles)}`);
  check('the tile strip reports the real deviation ratio and backlog',
    r.nums.join(' ').includes(`${DECISIONS.decisions.length}`) && r.nums.some(n => n.includes('/')),
    `tiles = ${JSON.stringify(r.nums)} — want the ${DECISIONS.decisions.length} open decisions and a dev/total ratio`);
  check('the rail badge reports the live backlog, not a hard-coded 2',
    String(r.badge).trim() === String(DECISIONS.decisions.length), `badge="${r.badge}" want "${DECISIONS.decisions.length}"`);
  check('no forecast or envelope is CLAIMED anywhere on the live path', r.mentionsForecast === false,
    'a predicted band would be the same fabrication in a new costume');
  check('each chart exposes an accessible summary', r.sparkLabels.length >= 2,
    JSON.stringify(r.sparkLabels));

  // ---- THE SERIES MUST MOVE WITH THE DATA ----
  // This is what separates a real series from a convincing one. A fixture passes every check above whenever its
  // constants happen to match; nothing survives this. Compare the CHART GEOMETRY, because the tiles could move
  // while the drawn line stayed synthetic.
  const moved = await page.evaluate(() => {
    // ★ FULL path strings, deliberately UNTRUNCATED. A first draft compared `d.slice(0, 120)` — the left
    // edge of each chart, which is the OLD end of the time window. Recent rows land at the RIGHT edge, so
    // the slice compared exactly the region the data never touches and reported "inert" over a series that
    // was moving. Truncation belongs in the failure MESSAGE, never in the compared value.
    const g = () => Array.from(document.querySelectorAll('#view svg.sig-chart path'))
      .map(e => e.getAttribute('d') || '');
    const before = g();
    liveState.alerts = liveState.alerts.slice(0, 2);      // the estate quietened
    route('signals');
    const after = g();
    return { before, after, changed: JSON.stringify(before) !== JSON.stringify(after) };
  });
  check('the DRAWN SERIES changes when the underlying rows change', moved.changed === true,
    `before ${JSON.stringify(moved.before).slice(0,160)} after ${JSON.stringify(moved.after).slice(0,160)} — a synthetic series is inert to the data`);

  // ---- AN EMPTY WINDOW STILL RENDERS THE PANEL, and says the window is empty ----
  // The stripped version replaced the whole panel with a prose apology. The design keeps the axes and reports
  // zero rows in its chip, so an operator can still see the window, the scale and the time range.
  const emptied = await page.evaluate(() => {
    liveState.alerts = [];
    route('signals');
    const v = document.querySelector('#view');
    return {
      stillHasChart: v.querySelectorAll('svg.sig-chart').length >= 2,
      saysNoRows: /no rows in this window/i.test(v.innerText || ''),
    };
  });
  check('an empty window still renders the designed panel (axes + scale), not a prose apology',
    emptied.stillHasChart === true, 'replacing a panel with a paragraph loses the window, the scale and the range');
  check('and it states that the window held no rows', emptied.saysNoRows === true,
    'silence about an empty window reads as a measured zero');

  // ---- #modules: THE REGISTRY IS THE VIEW, not a JSON blob over fixture tiles ----
  const mods = await page.evaluate(() => {
    route('modules');
    const v = document.querySelector('#view');
    const text = v.innerText || '';
    const rows = Array.from(v.querySelectorAll('table.tbl tbody tr')).map(tr =>
      Array.from(tr.querySelectorAll('td')).map(td => (td.textContent || '').trim()));
    return {
      // innerText reflects RENDERED case and .lbl is text-transform:uppercase, so this must be
      // case-insensitive — the same trap that made an earlier assertion here fail on /V1/CAPABILITIES.
      citesEndpoint: /\/v1\/capabilities/i.test(text),
      claimsFixture: /fixture/i.test(text),
      rawJsonDump: /"capability":/.test(text),
      rows, rowCount: rows.length,
      enabled: rows.filter(r => r[2] === 'ENABLED').length,
      disabled: rows.filter(r => r[2] === 'disabled').length,
    };
  });
  check('#modules cites /v1/capabilities', mods.citesEndpoint === true, JSON.stringify(mods));
  check('#modules no longer claims to be a fixture', mods.claimsFixture === false,
    'the fixture connector list is still being rendered beneath the live data');
  check('#modules does not dump raw JSON at the operator', mods.rawJsonDump === false, 'raw payload is being shown instead of rendered');
  check('#modules renders one row per declared capability', mods.rowCount === CAPS.capabilities.length,
    `${mods.rowCount} rows for ${CAPS.capabilities.length} capabilities`);
  const wantOn = CAPS.capabilities.filter(c => c.enabled).length;
  check('#modules reports the ENABLED count from the payload', mods.enabled === wantOn,
    `${mods.enabled} enabled rendered, payload says ${wantOn}`);
  check('and a disabled capability is shown as disabled, not omitted', mods.disabled === CAPS.capabilities.length - wantOn,
    `${mods.disabled} disabled rendered — hiding them would overstate what the plane can reach`);

  // ---- #estatedepth: the header must not call a rendered SUBSET "the estate" ----
  const depth = await page.evaluate(() => {
    route('estatedepth');
    const note = document.querySelector('#view .e2-hnote');
    return { note: note ? (note.textContent || '').trim() : null,
             estateTotal: (liveState.estate || {}).node_count };
  });
  check('#estatedepth names the estate total, not just what it drew',
    !!depth.note && depth.note.includes(String(depth.estateTotal)),
    `header="${depth.note}" while /v1/estate reports ${depth.estateTotal} nodes — a rendered subset labelled "in estate"`);
  check('#estatedepth distinguishes shown from total when they differ',
    !!depth.note && (/shown/.test(depth.note) || !/in estate/.test(depth.note) ),
    `header="${depth.note}"`);
} finally { await browser.close(); }

console.log(failed ? `signals-is-live: ${failed} FAILED` : 'signals-is-live: all checks passed');
process.exit(failed ? 1 : 0);
