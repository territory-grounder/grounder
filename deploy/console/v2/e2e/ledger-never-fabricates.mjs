// A TAMPER-EVIDENT RECORD MUST NEVER INVENT A ROW.
//
// Measured 2026-07-29 on the deployed console with /v1/ledger forced to fail: #ledger rendered the bundle's
// EIGHT design-fixture rows under a heading that reads "Tamper-evident record · SHA-256 chain verified ·
// client-side". Two of those rows carry a real operator's name — one on an `approve`, one on a
// `kill-switch.arm` — for actions that never happened. The caption then computed "showing the latest 8 of
// 1428" from the fixture's own seq, so even the total was fabricated. The live chain was at seq 5850.
//
// The cause was not a missing feature. `LEDGER.length=0; LEDGER.push(...)` ran ONLY inside the success path,
// and only when the live ledger was non-empty, so BOTH a failed read and a genuinely empty ledger fell
// through to the fixture. The catch block's comment claimed the ledger "stays fixture-labeled"; it did not —
// nothing labelled it at all.
//
// This oracle drives the three states an operator can actually land in, using real request interception
// (client-side only — nothing is sent to the server), and asserts that a fabricated row NEVER reaches the
// screen. The strongest assertion here is the last: the fixture's own seq numbers and the real name embedded
// in it must appear NOWHERE on a served console, in any state.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
// Values that exist ONLY in the design fixture. If any of these render on a served console, the console is
// showing invented governance history.
const FIXTURE_SEQS = ['1428', '1427', '1426', '1425', '1424', '1423', '1422', '1421'];
const FIXTURE_NAME = 'A. Operator';

let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const openLedger = async (browser, route) => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  if (route) await page.route('**/v1/**', route);
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  // Reveal the app and drive the ledger view directly: this oracle is about what the view RENDERS for a
  // given adopt outcome, and it sets that outcome explicitly rather than hoping the network produced it.
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  // No positive condition to wait on here (the caller stomps LEDGER/liveState itself, synchronously, right
  // after this returns) — a one-frame flush is the established fallback for a reveal with no render signal.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  return { ctx, page };
};

// ★ THE FIRST VERSION OF THIS ORACLE COULD NOT FAIL, AND THE CONTROLS PROVED IT.
// It emptied LEDGER itself before rendering, so it only ever tested the VIEW's branching. Restoring the old
// `if(liveState.ledger.length)` guard in liveAdopt left it GREEN (control CQ), and deleting the failed-read
// branch entirely left 3 of 4 assertions GREEN (control CP) — because with LEDGER already empty there was no
// fixture left to leak. An oracle that pre-arranges the very state it is checking proves nothing.
// The cases below therefore drive the REAL liveAdopt() against intercepted responses and never touch LEDGER,
// so the fixture is present exactly as it ships and has a genuine opportunity to leak.
const WHOAMI = { source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' };

const adoptWith = async (browser, ledgerResponder) => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.route('**/v1/**', async r => {
    const u = r.request().url();
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
    if (u.includes('/v1/ledger')) return ledgerResponder(r);
    return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' });   // everything else fails; each has its own catch
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above, so LEDGER/liveState are already settled — one frame is
  // enough margin for the DOM route() call below, not a guess at fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  const out = await page.evaluate(() => {
    try { route('ledger'); } catch (e) {}
    return { text: (document.querySelector('#view') || {}).innerText || '',
             ledgerLen: (typeof LEDGER !== 'undefined') ? LEDGER.length : -1,
             state: (typeof liveState === 'object' && liveState) ? liveState.ledgerState : 'no-liveState' };
  });
  await ctx.close();
  return out;
};

const browser = await chromium.launch();
try {
  // --- state: the read FAILED ---
  {
    const { ctx, page } = await openLedger(browser);
    const t = await page.evaluate(() => {
      liveState.attempted = true; liveState.ledgerState = 'failed'; LEDGER.length = 0;
      route('ledger');
      return (document.querySelector('#view') || {}).innerText || '';
    });
    check('FAILED read: says it could not read', /could not be read|could not read/i.test(t), `text: "${t.slice(0, 120)}"`);
    check('FAILED read: renders NO table rows', !(await page.$$eval('#view table tbody tr', r => r.length)).valueOf?.() || (await page.$$eval('#view table tbody tr', r => r.length)) === 0,
          `${await page.$$eval('#view table tbody tr', r => r.length).catch(() => 0)} rows rendered`);
    check('FAILED read: does NOT claim the chain is verified', !/SHA-256 chain verified/i.test(t), 'the verified chip survived a failed read');
    for (const seq of FIXTURE_SEQS.slice(0, 3)) {
      check(`FAILED read: fixture seq ${seq} does not appear`, !t.includes(seq), `"${seq}" rendered`);
    }
    check(`FAILED read: the fixture's real-person name does not appear`, !t.includes(FIXTURE_NAME),
          `"${FIXTURE_NAME}" was rendered against an invented approval`);
    await ctx.close();
  }

  // --- state: the read SUCCEEDED but returned nothing ---
  {
    const { ctx, page } = await openLedger(browser);
    const t = await page.evaluate(() => {
      liveState.attempted = true; liveState.ledgerState = 'ok'; LEDGER.length = 0;
      route('ledger');
      return (document.querySelector('#view') || {}).innerText || '';
    });
    check('EMPTY read: says the ledger is empty, not that it failed', /empty/i.test(t) && !/could not/i.test(t), `text: "${t.slice(0, 120)}"`);
    check('EMPTY read: no fixture rows leak in', !FIXTURE_SEQS.some(q => t.includes(q)) && !t.includes(FIXTURE_NAME), 'fixture content rendered on an empty ledger');
    await ctx.close();
  }

  // --- state: real rows ---
  {
    const { ctx, page } = await openLedger(browser);
    const t = await page.evaluate(() => {
      liveState.attempted = true; liveState.ledgerState = 'ok';
      LEDGER.length = 0;
      LEDGER.push({ seq: 5850, ts: '12:00:00', actor: 'system', kind: 'verdict.match', ref: 's-real',
                    action: 'aaaabbbb', hash: 'aaaa…bbbb', prev: 'cccc…dddd' });
      route('ledger');
      return (document.querySelector('#view') || {}).innerText || '';
    });
    check('REAL rows: the real seq renders', t.includes('5850'), `text: "${t.slice(0, 120)}"`);
    check('REAL rows: no fixture seq renders alongside', !FIXTURE_SEQS.some(q => t.includes(q)), 'fixture rows rendered next to real ones');
    await ctx.close();
  }

  // --- REAL PATH: liveAdopt runs, /v1/ledger FAILS, LEDGER is untouched by this oracle ---
  {
    const r = await adoptWith(browser, rq => rq.fulfill({ status: 500, contentType: 'application/json', body: '{}' }));
    check('REAL adopt + failed /v1/ledger: LEDGER holds no rows', r.ledgerLen === 0,
          `LEDGER has ${r.ledgerLen} rows after a failed read (state=${r.state}) — the design fixture survived`);
    check('REAL adopt + failed /v1/ledger: no fixture seq on screen', !FIXTURE_SEQS.some(q => r.text.includes(q)),
          `fixture rows rendered: "${r.text.slice(0, 140)}"`);
    check('REAL adopt + failed /v1/ledger: no real-person name on screen', !r.text.includes(FIXTURE_NAME),
          `"${FIXTURE_NAME}" rendered against an invented approval`);
  }

  // --- REAL PATH: liveAdopt runs, /v1/ledger succeeds with ZERO entries ---
  {
    const r = await adoptWith(browser, rq => rq.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ entries: [] }) }));
    check('REAL adopt + EMPTY /v1/ledger: LEDGER holds no rows', r.ledgerLen === 0,
          `LEDGER has ${r.ledgerLen} rows after an empty read (state=${r.state}) — the fixture was kept because the real ledger was empty`);
    check('REAL adopt + EMPTY /v1/ledger: no fixture seq on screen', !FIXTURE_SEQS.some(q => r.text.includes(q)),
          `fixture rows rendered: "${r.text.slice(0, 140)}"`);
  }

  // --- REAL PATH: /v1/estate FAILS. The fixture graph must not stand in for the real one. ---
  {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.route('**/v1/**', async r => {
      const u = r.request().url();
      if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
      return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' });
    });
    await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
    await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
    // Same reasoning as adoptWith() above: liveAdopt() is already fully awaited, ESTATE/HOSTS are already
    // settled, this is just a one-frame margin before the DOM route() calls below.
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    const est = await page.evaluate(() => {
      const out = {};
      out.nodes = (typeof ESTATE !== 'undefined' && ESTATE.nodes) ? ESTATE.nodes.length : -1;
      out.hosts = (typeof HOSTS !== 'undefined') ? Object.keys(HOSTS).length : -1;
      try { route('estate'); } catch (e) {}
      out.estateText = (document.querySelector('#view') || {}).innerText || '';
      try { route('estatedepth'); } catch (e) {}
      out.depthText = (document.querySelector('#view') || {}).innerText || '';
      try { route('logs'); } catch (e) {}
      out.logsText = (document.querySelector('#view') || {}).innerText || '';
      return out;
    });
    check('failed /v1/estate: the fixture graph is CLEARED', est.nodes === 0 && est.hosts === 0,
          `ESTATE.nodes=${est.nodes} HOSTS=${est.hosts} — the design graph survived a failed read`);
    check('failed /v1/estate: #estate says it could not read', /could not be read/i.test(est.estateText),
          `"${est.estateText.slice(0, 130)}"`);
    check('failed /v1/estate: #estatedepth does NOT call it an empty estate', /could not be read/i.test(est.depthText),
          `"${est.depthText.slice(0, 130)}"`);
    check('failed reads: #logs does NOT call the failed sources "live"', !/front door are live/i.test(est.logsText),
          `"${est.logsText.slice(0, 160)}"`);
    check('failed reads: #logs says the READ failed, not that the estate is quiet',
          /read failed|could not be read/i.test(est.logsText) && !/nothing has flowed/i.test(est.logsText),
          `"${est.logsText.slice(0, 160)}"`);
    await ctx.close();
  }

  // --- REAL PATH: every read SUCCEEDS but returns nothing. "Empty" must not read as "broken". ---
  {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.route('**/v1/**', async r => {
      const u = r.request().url();
      if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(WHOAMI) });
      if (u.includes('/v1/ledger')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ entries: [] }) });
      if (u.includes('/v1/alerts')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ alerts: [], counts: { total: 0, last_24h: 0 } }) });
      return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' });
    });
    await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
    await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
    // Same reasoning again: liveAdopt() is already fully awaited, one frame of margin before route('logs').
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    const t = await page.evaluate(() => { try { route('logs'); } catch (e) {} return (document.querySelector('#view') || {}).innerText || ''; });
    check('empty-but-successful reads: #logs says nothing has flowed, NOT that the read failed',
          /nothing has flowed/i.test(t) && !/could not be read/i.test(t), `"${t.slice(0, 160)}"`);
    await ctx.close();
  }

  // --- the guard that makes the three above mean something ---
  {
    const { ctx, page } = await openLedger(browser);
    const n = await page.evaluate(() => { liveState.attempted = false; route('ledger'); return LEDGER.length; });
    check('the design fixture still EXISTS (so the checks above were not vacuous)', n >= 8,
          `LEDGER has ${n} rows — if the fixture were simply deleted, every assertion above would pass for the wrong reason`);
    await ctx.close();
  }
} finally { await browser.close(); }

if (failed) { console.log(`\nledger-never-fabricates: ${failed} FAILED`); process.exit(1); }
console.log('\nledger-never-fabricates: all checks passed');
