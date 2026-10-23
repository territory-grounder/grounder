// A TABLE THAT CLAIMS A RANKING MUST HAVE ONE, AND MUST NOT RESHUFFLE WHEN NOTHING CHANGED.
//
// #estate's edge table sorted on confidence alone and showed the first 80 under the caption
// "showing the 80 highest-confidence of N edges". Measured on the live estate: confidence takes exactly TWO
// values across the whole graph (0.90 on 196 edges, 0.95 on 194), so that caption describes a ranking the data
// cannot support — the 80th row and the 194th are equally confident.
//
// Worse, the tie left the order to whatever the server serialized (a Go map iteration, re-randomised per
// snapshot). Over a 7-minute soak ONE edge was added and 79 of 80 row positions changed: 50 visible rows
// vanished and 50 different ones appeared, describing an estate that had not changed. An operator reading the
// dependency table twice saw two different answers with no way to know nothing had moved.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// The live shape: many edges, only TWO distinct confidences, so the 80-row cut lands inside a huge tie.
const mkEdges = (n, shuffleSeed) => {
  const out = [];
  for (let i = 0; i < n; i++) {
    out.push({ from: `dc1h${String(i % 40).padStart(2, '0')}`, to: `dc1pve0${(i % 3) + 1}`,
               rel: i % 2 ? 'runs_on' : 'depends_on', confidence: i % 2 ? 0.95 : 0.90, source: 'pve' });
  }
  // deterministic "server reserialised the map" permutation
  for (let i = out.length - 1; i > 0; i--) {
    const j = (i * 7919 + shuffleSeed * 104729) % (i + 1);
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out;
};
const estate = (n, seed) => ({
  available: true, node_count: 40, edge_count: n, source_count: 1,
  captured_at: '2026-07-29T19:03:14Z',
  nodes: Array.from({ length: 40 }, (_, i) => ({ name: `dc1h${String(i).padStart(2, '0')}` })),
  edges: mkEdges(n, seed),
});

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1500, height: 1000 } })).newPage();
  let seed = 1, count = 390;
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/estate')) return j(estate(count, seed));
    if (u.includes('/v1/sessions')) return j({ total: 0, sessions: [] });
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });

  const rowsNow = async () => page.evaluate(async () => {
    try { await liveAdopt(); } catch (e) {}
    route('estate');
    const trs = Array.from(document.querySelectorAll('#view table.tbl tbody tr'));
    return {
      keys: trs.map(tr => Array.from(tr.querySelectorAll('td')).slice(0, 4).map(td => (td.textContent || '').trim()).join('|')),
      caption: Array.from(document.querySelectorAll('#view .lbl')).map(e => e.textContent || '').find(t => /showing/i.test(t)) || '',
    };
  });

  const a = await rowsNow();
  check('the edge table rendered 80 rows', a.keys.length === 80, `${a.keys.length} rows`);

  // ---- 1. THE SAME DATA RENDERS THE SAME WAY (idempotence) ----
  const a2 = await rowsNow();
  check('re-rendering the SAME snapshot gives the identical order',
    JSON.stringify(a.keys) === JSON.stringify(a2.keys), 'the order is not a function of the data alone');

  // ---- 2. A RESERIALISED SNAPSHOT (same edges, different array order) MUST NOT RESHUFFLE ----
  seed = 2;
  const b = await rowsNow();
  const moved = b.keys.filter((k, i) => k !== a.keys[i]).length;
  const vanished = a.keys.filter(k => !b.keys.includes(k)).length;
  check('the server reserialising the SAME edges does not reshuffle the table', moved === 0,
    `${moved}/80 row positions changed and ${vanished} rows vanished, describing an estate that did not change`);

  // ---- 3. ADDING ONE EDGE PERTURBS AT MOST A LITTLE ----
  // The original defect turned a 1-edge addition into a 50-row swap. A deterministic order cannot do that.
  seed = 3; count = 391;
  const c = await rowsNow();
  const churn = a.keys.filter(k => !c.keys.includes(k)).length;
  check('adding ONE edge does not swap out half the table', churn <= 2,
    `${churn} of 80 rows vanished after a single edge was added`);

  // ---- 4. THE CAPTION DOES NOT CLAIM A RANK THE DATA CANNOT SUPPORT ----
  check('the caption is present', /showing/i.test(c.caption), JSON.stringify(c.caption));
  check('the caption discloses that the cut-off is a TIE', /tie/i.test(c.caption) && /share the cut-off/i.test(c.caption),
    JSON.stringify(c.caption.slice(0, 240)));
  check('the caption states how many equally-confident edges are hidden', /\d+ further edge/i.test(c.caption),
    JSON.stringify(c.caption.slice(0, 240)));
  check('the caption no longer calls them the strongest dependencies', !/the 80 highest-confidence of/i.test(c.caption),
    JSON.stringify(c.caption.slice(0, 240)));
  check('and it names the ordering actually used', /confidence then by name/i.test(c.caption),
    JSON.stringify(c.caption.slice(0, 240)));

  // ---- 5. THE OTHER DIRECTION: WHERE THERE IS NO TIE AT THE CUT, THE OLD WORDING IS CORRECT ----
  // Without this, a "fix" that always cried "tie" would pass while being wrong on well-ranked data.
  await page.evaluate(() => {
    liveState.estate = Object.assign({}, liveState.estate, {
      edges: liveState.estate.edges.map((e, i) => Object.assign({}, e, { confidence: 1 - i / 1000 })),
    });
    route('estate');
  });
  const distinct = await page.evaluate(() =>
    Array.from(document.querySelectorAll('#view .lbl')).map(e => e.textContent || '').find(t => /showing/i.test(t)) || '');
  check('with genuinely distinct confidences it reports a real top-80', /the 80 highest-confidence of/i.test(distinct),
    `${JSON.stringify(distinct.slice(0, 200))} — the tie disclosure must be conditional on there BEING a tie`);
} finally { await browser.close(); }

console.log(failed ? `edge-ranking-stability: ${failed} FAILED` : 'edge-ranking-stability: all checks passed');
process.exit(failed ? 1 : 0);
