// Console e2e — Estate Depth (#estatedepth) live-wiring guard. Mocks /v1/estate + /v1/alerts with DISTINCTIVE
// node names (so a pass proves the injected LIVE graph replaced the fixtures, not that fixtures happened to
// match) and lands directly on #estatedepth. Asserts: the real nodes appear in the node list, a node's health
// is DERIVED from its live alert (crit alert → crit chip), a live dependency edge renders, and the two
// panels that USED to be synthetic on the live shell (golden signals, incident master-clock) now state their
// absence instead of drawing a hash-PRNG series beside real data. No JS error.
// Run: CONSOLE_BASE=... node estatedepth.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// tgnode-crit-01 and tgnode-dep-02 each appear TWICE — as a network 'host' AND its proxmox type — exactly
// as the real /v1/estate does (~150 names dual-typed). The list MUST dedupe them to one row per name.
// 40 extra unique filler nodes make the deduped list (43 rows) TALLER than the inspector, so the gap
// assertion below genuinely exercises the tall-list-forces-a-gap regression (with only 3 short rows the
// list is shorter than the inspector and the gap check would pass trivially).
const fillers = Array.from({ length: 40 }, (_, i) => ({ name: 'tgnode-fill-' + String(i).padStart(2, '0'), type: 'vm' }));
const estate = {
  available: true, node_count: 5 + fillers.length, edge_count: 2, source_count: 1,
  nodes: [
    { name: 'tgnode-crit-01', type: 'host' }, { name: 'tgnode-crit-01', type: 'lxc' },
    { name: 'tgnode-dep-02', type: 'host' }, { name: 'tgnode-dep-02', type: 'pve_node' },
    { name: 'tgnode-ok-03', type: 'vm' },
    ...fillers,
  ],
  edges: [{ from: 'tgnode-crit-01', to: 'tgnode-dep-02', rel: 'runs_on', confidence: 0.95, source: 'pve' },
          { from: 'tgnode-ok-03', to: 'tgnode-dep-02', rel: 'runs_on', confidence: 0.80, source: 'pve' }],
};
const alerts = { alerts: [
  { external_ref: 'a1', source_type: 'librenms', host: 'tgnode-crit-01', alert_rule: 'DiskPressure', severity: 'critical', observed_at: '2026-07-21T14:31:02Z' },
] };

async function mock(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime', role: 'trace-read' } });
    if (p === '/v1/estate') return route.fulfill({ json: estate });
    if (p === '/v1/alerts') return route.fulfill({ json: alerts });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [] } });
    return route.fulfill({ json: {} });
  });
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 950 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await mock(page);
  await page.goto(BASE + '/index.html#estatedepth', { waitUntil: 'domcontentloaded' });
  // lastRefresh is set only as the LAST statement of liveAdopt() — after the try/catch-wrapped
  // liveInjectEstateDepth() call and the post-adopt route() re-render — so it is exactly the "boot + adopt +
  // inject + re-render" signal this file was guessing at (same idiom boot-resilience.mjs uses for its own
  // direct #estatedepth landing).
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});

  const r = await page.evaluate(() => {
    const body = document.body.innerText;
    // node-list rows: the id is in .e2-lid
    const ids = [...document.querySelectorAll('.e2-lid')].map(e => e.innerText.trim());
    const dupNames = ids.filter((x, i) => ids.indexOf(x) !== i);
    // the deduped tgnode-crit-01 row must show the SPECIFIC type (lxc), not the generic host
    const critRow = [...document.querySelectorAll('.e2-lrow')].find(e => /tgnode-crit-01/.test(e.innerText));
    // GAP measurement: the master-clock (.e2-clockpane) must sit right below the columns, not far below
    const cols = document.querySelector('.e2-cols'), clock = document.querySelector('.e2-clockpane');
    let gap = null, colsH = null;
    if (cols) colsH = Math.round(cols.getBoundingClientRect().height);
    if (cols && clock) gap = Math.round(clock.getBoundingClientRect().top - cols.getBoundingClientRect().bottom);
    // inspector content height (the right pane) — cols height should track this, not the 217-row list
    const insp = document.querySelectorAll('.e2-cols > .e2-pane')[1];
    const inspH = insp ? Math.round(insp.getBoundingClientRect().height) : null;
    return {
      realNodesInList: /tgnode-crit-01/.test(body) && /tgnode-ok-03/.test(body),
      noFixtureNodes: !/dc1k8s-w3|dc2nas01/.test(body),
      uniqueNodeCount: new Set(ids).size, totalRows: ids.length, dupNames,
      critTypeSpecific: !!critRow && /lxc/i.test(critRow.innerText),   // dedupe kept the specific type
      critHealthDerived: /CRIT/i.test(body),
      depRendered: /tgnode-dep-02/.test(body),
      liveAlertRule: /DiskPressure/.test(body),
      // ★ THE LIVE SHELL MAY NOT DRAW AN INVENTED SERIES. Until 2026-07-30 the golden-signal sparklines were
      // a hash-PRNG walk rendered under a REAL node id (with a small "synthetic" chip doing the apologising),
      // and the master-clock was a scripted incident for a fixture session on an invented host. Both panels
      // now STATE their absence on a live shell; the synthetic renders survive only on the unauthenticated
      // design preview. These checks pin all of that.
      noSyntheticChip: !/synthetic/i.test(body),
      noSparklines: document.querySelectorAll('.e2-sig').length === 0,
      signalsAbsenceStated: /GOLDEN SIGNALS/i.test(body) && /no per-node metrics|no metrics read surface/i.test(body),
      clockAbsenceStated: /INCIDENT MASTER-CLOCK/i.test(body) && /neither per-session telemetry nor committed/i.test(body),
      noFixtureSession: !/s-8f31|Disk pressure eviction cascade|dc1demo/.test(body),
      // The header now names BOTH populations ("43 shown · 45 in estate") because labelling a rendered
      // SUBSET as "in estate" understated the estate by 152 nodes live. This must read the SHOWN count —
      // the number whose job is to prove dedupe happened — not the estate total beside it.
      nodeHeader: (body.match(/(\d+) shown/) || body.match(/(\d+) in estate/) || [])[1],
      nodeHeaderRaw: (body.match(/[^\n]*in estate[^\n]*/) || [])[0],
      gap, colsH, inspH, clockRendered: !!clock,
    };
  });
  ok(r.realNodesInList, 'the live estate nodes did not populate the Estate Depth node list');
  ok(r.noFixtureNodes, 'the OLD fixture nodes are still showing — live injection did not replace ESTATE');
  // BUG 1 — no duplicate hostnames: 5 nodes with 2 dual-typed names must render 3 rows, each name once.
  ok(r.dupNames.length === 0, 'DUPLICATE hostnames in the list (bug 1 not fixed): ' + r.dupNames.join(', '));
  ok(r.totalRows === 43 && r.uniqueNodeCount === 43, 'expected 43 deduped rows (3 dual-typed → 3, +40 fillers), got ' + r.totalRows + ' (unique ' + r.uniqueNodeCount + ')');
  ok(r.nodeHeader === '43', 'node-count header should read 43 SHOWN after dedupe, got ' + r.nodeHeader + ' from "' + r.nodeHeaderRaw + '"');
  ok(r.critTypeSpecific, 'dedupe kept the generic "host" type instead of the specific "lxc"');
  // BUG 2 — no huge gap: the master-clock sits just below the columns, and cols height tracks the inspector
  // (not a runaway list). Gap must be small (the flex gap ~10px), never hundreds of px.
  ok(r.clockRendered, 'the incident master-clock did not render');
  ok(r.gap !== null && r.gap < 60, 'HUGE gap before the master-clock (bug 2 not fixed): ' + r.gap + 'px');
  ok(r.inspH !== null && r.colsH !== null && r.colsH <= r.inspH + 40, 'the columns block is far taller than the inspector — the list is stretching the row (gap bug): cols=' + r.colsH + ' insp=' + r.inspH);
  ok(r.critHealthDerived, 'node health was not derived from the live alert (no CRIT chip)');
  ok(r.depRendered, 'the live dependency edge did not render');
  ok(r.liveAlertRule, 'the live alert did not render in the active-alerts panel');
  ok(r.noSyntheticChip, 'a "synthetic" chip is on the LIVE shell — an invented panel is rendering beside real data');
  ok(r.noSparklines, 'golden-signal sparklines rendered on the LIVE shell — those series are a hash-PRNG walk, not telemetry');
  ok(r.signalsAbsenceStated, 'the golden-signals panel neither draws nor states its absence — a silent gap reads as "no signals"');
  ok(r.clockAbsenceStated, 'the master-clock pane neither renders honestly nor says why there is no clock');
  ok(r.noFixtureSession, 'fixture-session content (demo host / scripted incident) leaked onto the live inspector');
  ok(pageErrors.length === 0, 'uncaught JS errors: ' + pageErrors.join(' | '));
} finally { await browser.close(); }

if (failures.length) { console.error('ESTATEDEPTH E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('ESTATEDEPTH E2E PASS — the live estate graph + alerts populate the node inspector (real nodes, alert-derived health, live deps), no synthetic series on the live shell (absences stated), no JS error.');
