// TWO NUMBERS FOR "THE SIZE OF THE ESTATE", AND NEITHER SAID WHICH IT WAS.
//
// The rail badge and the #estate Nodes tile publish node_count (every entity in the snapshot). The
// infragraph legend on the SAME VIEW derives its node set from the EDGE LIST, so it only ever sees entities
// that participate in a relationship. Measured live on the published snapshot 2026-07-29:
//
//   node_count 367 · edge_count 390 · 217 distinct names appearing in any edge
//
// So the console said 367 in one place and 217 in another, on one screen, with no qualifier — and an
// operator judging blast radius reads whichever they happen to look at. Both numbers are true; the defect
// was that neither named its population, and that the 150-entity difference (41% of the graph with NO
// recorded relationship) was invisible.
//
// This oracle drives the REAL renderer with a snapshot whose two populations are KNOWN and different, so it
// fails if either number goes unlabelled or if the unconnected remainder stops being surfaced.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// 10 entities in the snapshot; only 6 of them appear in an edge. The live shape (367 / 217) in miniature.
const SNAPSHOT = {
  available: true,
  node_count: 10,
  edge_count: 5,
  source_count: 2,
  captured_at: '2026-07-29T20:00:00Z',
  nodes: Array.from({ length: 10 }, (_, i) => ({ name: `dc1h${i}`, kind: 'host' })),
  edges: [
    { from: 'dc1h1', to: 'dc1h0', rel: 'runs_on', confidence: 0.9, source: 'pve' },
    { from: 'dc1h2', to: 'dc1h0', rel: 'runs_on', confidence: 0.9, source: 'pve' },
    { from: 'dc1h3', to: 'dc1h0', rel: 'runs_on', confidence: 0.8, source: 'pve' },
    { from: 'dc1h4', to: 'dc1h5', rel: 'runs_on', confidence: 0.7, source: 'pve' },
    { from: 'dc1h5', to: 'dc1h0', rel: 'runs_on', confidence: 0.7, source: 'pve' },
  ],
};
const CONNECTED = 6;  // h0..h5 appear in an edge
const ISOLATED = 4;   // h6..h9 do not

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  // Wait for the boot script to have parsed (views populated) rather than a fixed guess — the same
  // reveal-then-wait idiom aria-state.mjs uses for this exact setGate/hidden trick. The evaluate below
  // reaches straight for the `liveState`/`route` globals it defines, so this is the real precondition.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  const r = await page.evaluate((snap) => {
    liveState.on = true;
    liveState.estate = snap;
    route('estate');
    const legend = document.querySelector('#view .legend');
    const tiles = Array.from(document.querySelectorAll('#view .tile')).map(t => ({
      lbl: (t.querySelector('.lbl') || {}).textContent || '',
      big: (t.querySelector('.big') || {}).textContent || '',
      sub: (t.querySelector('.sub') || {}).textContent || '',
    }));
    return { legend: legend ? legend.innerText : null, tiles };
  }, SNAPSHOT);

  check('the estate view rendered a legend', !!r.legend, 'no .legend — the graph did not render');
  check('the Nodes tile is present', r.tiles.some(t => /nodes/i.test(t.lbl)), JSON.stringify(r.tiles.map(t => t.lbl)));

  const nodesTile = r.tiles.find(t => /nodes/i.test(t.lbl)) || {};
  check('the Nodes tile still shows the SNAPSHOT total', nodesTile.big === String(SNAPSHOT.node_count),
    `"${nodesTile.big}" want "${SNAPSHOT.node_count}"`);
  check('and its caption names that population, not "in the graph"',
    /snapshot/i.test(nodesTile.sub) && !/^entities in the graph$/i.test((nodesTile.sub || '').trim()),
    `"${nodesTile.sub}" — "entities in the graph" reads as the smaller, connected-only number shown below it`);

  const legend = r.legend || '';
  check('the legend names ITS population (connected entities)', /connected/i.test(legend),
    JSON.stringify(legend.slice(0, 160)));
  check('the legend states the connected count', legend.includes(String(CONNECTED)),
    `expected ${CONNECTED} connected in: ${JSON.stringify(legend.slice(0, 160))}`);
  check('the ISOLATED remainder is surfaced', legend.includes(String(ISOLATED)) && /no recorded edge/i.test(legend),
    `expected ${ISOLATED} entities with no edge to be named: ${JSON.stringify(legend.slice(0, 220))}`);
  check('and it says why that matters for blast radius', /blast-radius|blast radius/i.test(legend),
    JSON.stringify(legend.slice(0, 220)));

  // THE OTHER DIRECTION: a fully-connected snapshot must NOT invent an isolated population. A surface that
  // always warns is a surface nobody reads.
  const full = await page.evaluate(() => {
    liveState.estate = {
      available: true, node_count: 3, edge_count: 2, source_count: 1,
      captured_at: '2026-07-29T20:00:00Z',
      nodes: [{ name: 'a' }, { name: 'b' }, { name: 'c' }],
      edges: [{ from: 'b', to: 'a', rel: 'runs_on', confidence: 0.9 }, { from: 'c', to: 'a', rel: 'runs_on', confidence: 0.9 }],
    };
    route('estate');
    const l = document.querySelector('#view .legend');
    return l ? l.innerText : null;
  });
  check('a fully-connected snapshot reports NO isolated entities', !!full && !/no recorded edge/i.test(full),
    `the warning fired with every entity connected: ${JSON.stringify((full || '').slice(0, 200))}`);
  check('and says so positively instead of staying silent', !!full && /all of them connected/i.test(full),
    JSON.stringify((full || '').slice(0, 200)));
} finally { await browser.close(); }

console.log(failed ? `estate-count-populations: ${failed} FAILED` : 'estate-count-populations: all checks passed');
process.exit(failed ? 1 : 0);
