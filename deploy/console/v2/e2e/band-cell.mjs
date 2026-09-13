// Console e2e — an UNCLASSIFIED session must not be painted as a POLL_PAUSE decision.
//
// The audit-spine row renderer chose the chip class with a two-branch ternary that FELL THROUGH to "pause":
//   class: "bandtag " + (r.band==="AUTO" ? "auto" : r.band==="AUTO_NOTICE" ? "notice" : "pause")
// A session that has not been classified yet carries no band, so it rendered
//   <span class="bandtag pause"></span>
// — an empty chip wearing the pause tint. Two lies in one cell: a coloured artifact where there is no
// datum, and an unclassified row reading as "a human was asked". On the live console ~10 of the 25 visible
// Command rows were in exactly this state.
//
// The verdict column in the same row already handles absence correctly (r.verdict || "pending"), which is
// the standard this cell has to meet.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8099 node band-cell.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// One row per band the classifier can emit, plus the two shapes of ABSENCE the API actually produces:
// an empty string and a missing key. Both must render as "no band", never as a band.
const sessions = [
  { external_ref: 'ref-auto',    band: 'AUTO',        risk_level: 'low',    verdict: 'match',   action_id: 'a1' },
  { external_ref: 'ref-notice',  band: 'AUTO_NOTICE', risk_level: 'medium', verdict: 'partial', action_id: 'a2' },
  { external_ref: 'ref-pause',   band: 'POLL_PAUSE',  risk_level: 'high',   verdict: null,      action_id: 'a3' },
  { external_ref: 'ref-empty',   band: '',            risk_level: '',       verdict: null,      action_id: '' },
  { external_ref: 'ref-missing', /* no band key */    risk_level: '',       verdict: null,      action_id: '' },
];

async function mock(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 0, last_24h: 0 } } });
    return route.fulfill({ json: {} });
  });
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await mock(page);
  await page.goto(BASE + '/index.html#command', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Wait for the mocked spine rows to render rather than a fixed sleep — deterministic, and if they never
  // arrive the row-count assertion below reports the precise miss instead of an opaque timeout throw.
  await page.waitForFunction(() => document.querySelectorAll('#view table.tbl tbody tr').length >= 5, null, { timeout: 20000 }).catch(() => {});

  // Read the BAND cell (column index 1) for every rendered spine row, keyed by ref.
  const cells = await page.evaluate(() => {
    const out = {};
    for (const tr of document.querySelectorAll('#view table.tbl tbody tr')) {
      const ref = tr.children[0]?.innerText.trim();
      const cell = tr.children[1];
      if (!ref || !cell) continue;
      const chip = cell.querySelector('.bandtag');
      out[ref] = {
        text: cell.innerText.trim(),
        hasChip: !!chip,
        chipClass: chip ? chip.className : null,
        chipText: chip ? chip.innerText.trim() : null,
      };
    }
    return out;
  });

  ok(Object.keys(cells).length >= 5, `expected the 5 mocked rows to render, saw ${Object.keys(cells).length} — the fixture is not reaching the spine table`);

  // The classified rows must still show their band, with the right tint. If this half breaks, the fix
  // over-reached and started suppressing real data.
  ok(cells['ref-auto']?.chipText === 'AUTO' && /\bauto\b/.test(cells['ref-auto']?.chipClass || ''),
    `AUTO row lost its chip: ${JSON.stringify(cells['ref-auto'])}`);
  ok(cells['ref-notice']?.chipText === 'AUTO_NOTICE' && /\bnotice\b/.test(cells['ref-notice']?.chipClass || ''),
    `AUTO_NOTICE row lost its chip: ${JSON.stringify(cells['ref-notice'])}`);
  ok(cells['ref-pause']?.chipText === 'POLL_PAUSE' && /\bpause\b/.test(cells['ref-pause']?.chipClass || ''),
    `POLL_PAUSE row lost its chip: ${JSON.stringify(cells['ref-pause'])}`);

  // THE DEFECT: absence must not be rendered as a band.
  for (const ref of ['ref-empty', 'ref-missing']) {
    const c = cells[ref];
    ok(!(c?.hasChip && c.chipText === ''),
      `${ref} rendered an EMPTY band chip (class ${JSON.stringify(c?.chipClass)}) — a coloured artifact ` +
      `where the session has no band at all`);
    ok(!/\bpause\b/.test(c?.chipClass || ''),
      `${ref} has no band but was painted with the POLL_PAUSE tint (class ${JSON.stringify(c?.chipClass)}) — ` +
      `an unclassified session reads to an operator as "a human was asked to decide"`);
    ok((c?.text || '') !== '',
      `${ref} rendered a completely blank BAND cell — absence should be stated (the verdict column in the ` +
      `same row already renders "pending" rather than nothing)`);
  }

  // ---- the band EDGE: the band must be readable while scanning, not only by reading a chip ----------
  // Command's subtitle promises "does it need me". A POLL_PAUSE hold is the answer, and before this it was
  // distinguishable only by a 9px chip in the second column, identical in weight to every other row.
  const edges = await page.evaluate(() => {
    const out = {};
    for (const tr of document.querySelectorAll('#view table.tbl tbody tr')) {
      const ref = tr.children[0]?.innerText.trim();
      if (ref) out[ref] = tr.getAttribute('data-band');
    }
    return out;
  });
  ok(edges['ref-pause'] === 'pause',
    `the POLL_PAUSE row carries data-band=${JSON.stringify(edges['ref-pause'])}, so it has no band edge — ` +
    `the one row that needs a human looks exactly like the rest while scanning`);
  ok(edges['ref-auto'] === 'auto' && edges['ref-notice'] === 'notice',
    `classified rows lost their band edge: ${JSON.stringify(edges)}`);
  ok(edges['ref-empty'] === '' && edges['ref-missing'] === '',
    `an unclassified row must carry an EMPTY data-band so it recedes rather than borrowing a decision's ` +
    `colour: ${JSON.stringify(edges)}`);

  // ---- the hero is the decision surface, not a reference doc ----------------------------------------
  const heroText = await page.evaluate(() => document.querySelector('#view')?.innerText.slice(0, 400) || '');
  ok(!/API contract|Browse every endpoint/i.test(heroText),
    'the API-contract card is back in Command\'s hero — a browsable endpoint map occupying the top of the ' +
    'surface whose stated job is "does it need me". The API view keeps its rail item and posture-bar button.');

  ok(pageErrors.length === 0, `uncaught page errors: ${pageErrors.join(' | ')}`);
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('BAND-CELL E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('BAND-CELL E2E PASS — a session with no band renders as absent, never as an empty or pause-tinted chip; classified rows keep their band.');
