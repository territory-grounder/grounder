// Console e2e — #logs IS A TERMINAL, NOT A POSTER OF ONE (TG-273).
//
// ★ WHAT THIS REPLACES. The live #logs surface rendered 90 rows and did NOTHING: 0 clickable rows on the
// view the nav calls "the terminal drill target" — while every alert row rendered the session ref the
// reasoning deep link (TG-269) needs; governance detail clipped mid-word in a no-wrap span with the
// remainder unreachable; "90 EVENTS" against a rail badge of 11,732 with the cap unstated; the designed
// terminal (search, filters, severity tint, stick-to-tail) thrown away and replaced by a bare list — the
// regression pattern the signals view in the same file documents as already caught once.
//
// The fixtures reproduce what the LIVE spine actually serves (shapes verbatim from /v1/ledger and
// /v1/alerts on 2026-08-03), including the two rows that carried the defects: a ledger entry whose reason
// is a multi-hundred-character dark-seam report, and alerts carrying session refs.
//
// Run: CONSOLE_BASE=http://127.0.0.1:8137 node logs-terminal.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// Poll NODE-side state (e.g. the traceReads array a page.route handler pushes into) rather than
// page.waitForFunction, which can only observe the browser. Same idiom, different side of the CDP boundary.
async function waitForNode(predicate, timeout = 20000) {
  const start = Date.now();
  while (Date.now() - start < timeout) {
    if (predicate()) return true;
    await new Promise(r => setTimeout(r, 50));
  }
  return predicate();
}

const LONG_DETAIL = '2 dark wiring seam(s) at boot: - discovery.service: declared-dark — no service-observing ' +
  'discovery probe is wired: estate.TypeService has no producer, so the world model can never draft a ' +
  'KindUnit or KindContainer entry and TG_ACTUATION_ALLOWED_UNITS stays the only way a unit becomes an ' +
  'actuation target — while the world.discovery seam still reports LIVE for the host/VM kinds that do work';

const REF = 'librenms-dc1-183833';

const ledger = { entries: [
  { seq: 8739, created_at: '2026-08-03T16:12:40Z', decision: 'config:gap-at-boot', reason: 'carve-outs do not cover 5 allowlisted guest(s)', withheld: false },
  { seq: 8740, created_at: '2026-08-03T16:12:41Z', decision: 'wiring:dark-seam-at-boot', reason: LONG_DETAIL, withheld: true },
]};
const alerts = {
  alerts: [
    { external_ref: REF, source_type: 'librenms', alert_rule: 'Space-on-/-is-90-and-95-in-use', host: 'dc1excalidraw01', severity: 'critical', received_at: '2026-08-03T23:00:15Z' },
    { external_ref: 'tg-liveness-dc1wallos01-1', source_type: 'pve-liveness', alert_rule: 'Device-Down', host: 'dc1wallos01', severity: 'warning', received_at: '2026-08-03T23:00:28Z' },
  ],
  counts: { total: 2992, last_24h: 7 },
};
const trace = { id: REF, ref: REF, title: 'Space-on-/', host: 'dc1excalidraw01', status: 'stopped', conf: 0,
  nodes: [{ t: 'ingest', lb: 'Ingested', pay: 'Space-on-/ · critical · dc1excalidraw01', conf: 0 }] };

async function mount(page) {
  const traceReads = [];
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/ledger') return route.fulfill({ json: ledger });
    if (p === '/v1/alerts') return route.fulfill({ json: alerts });
    if (p.startsWith('/v1/sessions/')) { traceReads.push(p); return route.fulfill({ json: trace }); }
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#logs', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Wait for the terminal rows themselves — the exact condition the row-count check below reads — rather
  // than a fixed boot-settle guess.
  await page.waitForFunction(() => document.querySelectorAll('#view .log-row').length >= 4, null, { timeout: 20000 }).catch(() => {});
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'logs'); if (a) a.click(); });
  await page.waitForFunction(() => document.querySelectorAll('#view .log-row').length >= 4, null, { timeout: 20000 }).catch(() => {});
  return traceReads;
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1600, height: 1100 } }).then(c => c.newPage());
  const errs = []; page.on('pageerror', e => errs.push(String(e)));
  const traceReads = await mount(page);

  const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
  ok(text.length > 300, `#logs rendered only ${text.length} chars — nothing below is meaningful`);

  // ---- 1. the drill target drills: an alert row's click opens its session's recorded walk --------------
  const rows = await page.evaluate(() => document.querySelectorAll('#view .log-row').length);
  ok(rows >= 4, `the designed terminal rows are absent (${rows} .log-row) — THE DEFECT: the live override ` +
    'replaced the design with a dead list, so search/filters/pivots all vanished together');

  const clicked = await page.evaluate(ref => {
    const r = [...document.querySelectorAll('#view .log-row')].find(x => (x.innerText || '').includes(ref));
    if (!r) return false;
    r.click(); return true;
  }, REF);
  ok(clicked, `no clickable row carries the session ref ${REF}`);
  // Wait for the exact navigation AND the fetch the two checks below read. The hash flips SYNCHRONOUSLY
  // inside logPivot() (location.hash = "reasoning" is the first statement), but the session read that
  // populates traceReads is async (liveReasonSelect -> liveReasonLoad) — waiting on the hash alone races
  // the traceReads check, which would then read state a moment too early.
  await page.waitForFunction(() => location.hash === '#reasoning').catch(() => {});
  await waitForNode(() => traceReads.some(p => p.includes(REF)));
  if (clicked) {
    ok((await page.evaluate(() => location.hash)) === '#reasoning',
      'clicking a row with a session ref did not navigate to the recorded walk — the drill target has no drill');
    ok(traceReads.some(p => p.includes(REF)),
      'the pivot navigated but never fetched the session — the walk on screen is not the row\'s session');
  }

  // back to logs for the remaining checks
  await page.evaluate(() => { location.hash = 'logs'; });
  await page.waitForFunction(() => document.querySelectorAll('#view .log-row').length >= 4, null, { timeout: 20000 }).catch(() => {});

  // ---- 2. governance detail is reachable IN FULL, never clipped-and-lost --------------------------------
  const gClicked = await page.evaluate(() => {
    const r = [...document.querySelectorAll('#view .log-row')].find(x => (x.innerText || '').includes('dark-seam'));
    if (!r) return false;
    r.click(); return true;
  });
  ok(gClicked, 'the governance dark-seam row is missing from the terminal');
  // Wait for the drawer to open — the exact state the checks below read.
  await page.waitForFunction(() => document.querySelector('#drawer')?.classList.contains('open')).catch(() => {});
  if (gClicked) {
    const drawer = await page.evaluate(() => {
      const d = document.querySelector('#drawer');
      return { open: !!(d && d.classList.contains('open')), text: d ? d.innerText : '' };
    });
    ok(drawer.open, 'clicking a governance row opened nothing — its clipped detail stays unreachable');
    ok(drawer.text.includes('actuation target'),
      'the drawer does not carry the END of the long detail — the full text is still not reachable, which ' +
      'is the mid-word clip defect wearing a drawer');
    await page.evaluate(() => { const d = document.querySelector('#drawer'); if (d) { const x = d.querySelector('.iconbtn.x'); if (x) x.click(); } });
  }

  // ---- 3. the window is stated, not silent ---------------------------------------------------------------
  ok(/of [\d,]+ recorded/i.test(text),
    'the header does not state the recorded population ("of N recorded") — 90 events silently reads as ' +
    'ALL events against a rail badge of 11,732');
  ok(/UTC/.test(text), 'the timestamp zone is no longer stated');

  // ---- 4. the designed controls exist AND OPERATE on live rows -------------------------------------------
  const hasSearch = await page.evaluate(() => !!document.querySelector('#view .log-search'));
  ok(hasSearch, 'the search input is gone — the designed terminal was discarded, not wired');
  if (hasSearch) {
    await page.fill('#view .log-search', 'dark-seam');
    // Wait for the exact row count the check below reads; .catch lets a non-filtering search box still
    // report its precise row count instead of an opaque timeout.
    await page.waitForFunction(() => document.querySelectorAll('#view .log-row').length === 1).catch(() => {});
    const shown = await page.evaluate(() => document.querySelectorAll('#view .log-row').length);
    ok(shown === 1, `searching "dark-seam" left ${shown} rows, want exactly 1 — the search box does not ` +
      'filter the live rows, so it is scenery');
    await page.fill('#view .log-search', '');
    // Wait for the full row set to return — the same population invariant established earlier in this file.
    await page.waitForFunction(() => document.querySelectorAll('#view .log-row').length >= 4).catch(() => {});
  }

  // a filter chip that can never match the live window must not present as a working control
  const execChip = await page.evaluate(() => {
    const c = document.querySelector('#view .log-fchip.log-k-executor');
    return c ? { disabled: c.disabled, aria: c.getAttribute('aria-disabled') } : null;
  });
  ok(execChip && (execChip.disabled || execChip.aria === 'true'),
    'the "executor" source chip presents as a working filter while no executor events exist in the ' +
    'window — the header overclaim rebuilt as a button');

  // ---- 5. the header no longer promises sources the stream does not carry --------------------------------
  const desc = await page.evaluate(() => document.querySelector('#vDesc')?.textContent || '');
  ok(!/unified event stream across connectors, agent, gates, executor/.test(desc),
    `the view description still promises agent/gate/executor events the stream does not carry: "${desc}"`);

  // ---- 6. columns do not collide ------------------------------------------------------------------------
  const srcOverflow = await page.evaluate(() =>
    [...document.querySelectorAll('#view .log-src')].filter(el => el.scrollWidth > el.clientWidth + 1).length);
  ok(srcOverflow === 0, `${srcOverflow} source cell(s) overflow their column — the GOVERNANCESYSTEM mash`);

  // ---- 7. the terminal uses the viewport ----------------------------------------------------------------
  const bodyH = await page.evaluate(() => document.querySelector('#view .log-body')?.clientHeight || 0);
  ok(bodyH > 540, `the stream body is ${bodyH}px tall in an 1100px viewport — half the page is empty ` +
    'while the stream scrolls in a letterbox');

  // ---- 8. no fixture line ever joins the live scrollback -------------------------------------------------
  await page.waitForTimeout(2500); // past the fixture tail's 1.8s cadence — Class-3 measurement window: intentional
  // fixed wait, MUST NOT become a condition-wait (proves no invented fixture tail line joins the live
  // scrollback over a real window; there is no DOM event for "nothing arrived")
  const late = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
  ok(!/payments-api|demo-w3|actualbudget-probe/.test(late),
    'an INVENTED fixture tail line joined the live scrollback — fiction interleaved with the estate in ' +
    'one stream, the worst place in the console to mix them');

  ok(errs.length === 0, `uncaught page errors: ${errs.join(' | ')}`);
  await page.close();
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('LOGS-TERMINAL E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('LOGS-TERMINAL E2E PASS — rows drill to their session, governance detail opens in full, the window and zone are stated, the designed search/filters operate on live rows, impossible filters are disabled, columns fit, the terminal fills the viewport, and no fixture line joins the live scrollback.');
