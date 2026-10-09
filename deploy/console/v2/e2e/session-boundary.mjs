// WHAT HAPPENS AT THE EDGES OF A SESSION — THE LENS THIS CONSOLE HAD NEVER RUN.
//
// Three defects, all measured on the live deployed bundle, all in the same blind spot: everything works
// while a session is healthy, and nothing was checked for the moments it starts, ends, or is taken away.
//
//   1. THE REFRESH GUARD WAS UNREACHABLE. liveAdopt(refresh) takes a parameter for one reason: a 25s poll
//      must not evict a working session on one transient error. Both module wrappers (wiki, skills)
//      reassigned the global with a ZERO-ARG function, so liveRefresh()'s liveAdopt(true) arrived at the
//      real implementation as liveAdopt(undefined) and EVERY poll tick took the first-load path. The guard
//      was written, shipped, and two wrappers away from the code that needed it.
//   2. THE AUTH GATE WAS NOT A MODAL. #authGate is an opaque position:fixed overlay with no dialog role and
//      nothing inert behind it. On a RE-GATED tab the entire dead console stayed keyboard-reachable
//      underneath — Tab walked through the invisible page and reached the live KILL emergency-stop button.
//   3. SIGNING OUT DID NOT CLOSE THE STREAM. /api/v1/events kept delivering governance posture —
//      mutation_enabled, posture_source, chain seq — to a tab whose session had been destroyed.
//
// The wrapper defect (1) is checked STRUCTURALLY, on the wrapped global as the browser sees it, because a
// behavioural check would need a transient failure to fall in exactly the right tick. Arity is the defect.
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
  // No reveal and no live mock here — section 1 only reads `liveAdopt` (and later `#authGate`, `setBreaker`)
  // as script globals, so the real signal is "boot script has parsed", not a guess at how long that takes.
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5).catch(() => {});

  // ---- 1. every wrapper of liveAdopt forwards its argument ----
  // Read the FINAL global, after all module wiring has reassigned it — that is what liveRefresh calls.
  const adopt = await page.evaluate(() => {
    const src = String(liveAdopt);                     // the FINAL global, after all module wiring
    const body = src.slice(src.indexOf('{'));
    // every reassignment in the bundle is the closed set — the two wrappers today, and any added later
    const reassignments = [...document.documentElement.innerHTML.matchAll(/liveAdopt\s*=\s*async function\s*\(([^)]*)\)/g)].map(m => m[0]);
    return {
      arity: liveAdopt.length,
      declares: /^async function\s*\(\s*refresh\s*\)|^\s*async function\s*\(\s*refresh\s*\)/.test(src) || liveAdopt.length >= 1,
      forwards: /\(\s*refresh\s*\)/.test(body),
      reassignments,
      dropped: reassignments.filter(r => !/refresh/.test(r)),
      src: src.slice(0, 200),
    };
  });
  check('the wrapped liveAdopt still declares its parameter', adopt.arity >= 1,
    `arity ${adopt.arity} — a zero-arg wrapper drops refresh, so EVERY poll tick takes the first-load path. src: ${adopt.src}`);
  check('the outermost wrapper passes refresh through', adopt.forwards, adopt.src);
  check('reassignments were found at all', adopt.reassignments.length >= 2, `${adopt.reassignments.length} — if the wrapping moved, this check proves nothing`);
  check('EVERY liveAdopt reassignment declares refresh', adopt.dropped.length === 0, JSON.stringify(adopt.dropped));

  // ---- 2. the auth gate is a real modal, and a re-gate makes the console unreachable ----
  const gate0 = await page.evaluate(() => {
    const g = document.querySelector('#authGate');
    return { role: g?.getAttribute('role'), modal: g?.getAttribute('aria-modal'), labelled: g?.getAttribute('aria-labelledby'),
             labelExists: !!(g && document.getElementById(g.getAttribute('aria-labelledby') || '')) };
  });
  check('#authGate is a labelled modal dialog', gate0.role === 'dialog' && gate0.modal === 'true' && gate0.labelExists,
    JSON.stringify(gate0));

  const regate = await page.evaluate(() => {
    // reveal, then re-gate the way an expired session does
    revealConsole();
    const appBefore = document.querySelector('#appRoot').hasAttribute('inert');
    liveLoginOverlay();
    const app = document.querySelector('#appRoot');
    const focusables = 'a[href],button:not([disabled]),input,select,textarea,[tabindex]:not([tabindex="-1"]),[role=button],[role=option]';
    const inApp = Array.from(app.querySelectorAll(focusables));
    // `inert` is what actually removes them: verify the browser agrees by checking the attribute AND that
    // the kill button cannot take focus.
    const kill = document.querySelector('.kill') || document.querySelector('#killBtn');
    let killTook = null;
    if (kill) { kill.focus(); killTook = document.activeElement === kill; }
    return { appBefore, inert: app.hasAttribute('inert'), controls: inApp.length, killPresent: !!kill, killTook,
             gateHidden: document.querySelector('#authGate').hidden };
  });
  check('revealing the console clears inert', regate.appBefore === false, `inert was ${regate.appBefore} after revealConsole()`);
  check('the gate is shown on a re-gate', regate.gateHidden === false, String(regate.gateHidden));
  check('the console has controls that WOULD be reachable', regate.controls > 10, `${regate.controls} focusable controls behind the gate`);
  check('a re-gated console is inert', regate.inert === true, 'the dead console stayed keyboard-reachable behind an opaque overlay');
  check('the KILL button cannot take focus behind the gate', regate.killPresent && regate.killTook === false,
    `present=${regate.killPresent} tookFocus=${regate.killTook}`);

  // ---- 3. a re-gate and a sign-out both close every stream ----
  const streams = await page.evaluate(() => {
    const opened = [], closed = [];
    const RealES = window.EventSource;
    class ProbeES {
      constructor(url) { this.url = url; this.readyState = 1; opened.push(url); ProbeES.last = this; }
      addEventListener() {} close() { this.readyState = 2; closed.push(this.url); }
    }
    window.EventSource = ProbeES;
    revealConsole();
    liveState.on = true;
    liveStream();                       // opens /api/v1/events
    const afterOpen = { opened: opened.slice(), closed: closed.slice() };
    liveLoginOverlay();                 // the re-gate path
    const afterGate = { opened: opened.slice(), closed: closed.slice(), on: liveState.on };
    window.EventSource = RealES;
    return { afterOpen, afterGate };
  });
  check('the posture stream opens', streams.afterOpen.opened.some(u => /\/v1\/events/.test(u)), JSON.stringify(streams.afterOpen.opened));
  check('a re-gate CLOSES the posture stream', streams.afterGate.closed.some(u => /\/v1\/events/.test(u)),
    `opened ${JSON.stringify(streams.afterGate.opened)} closed ${JSON.stringify(streams.afterGate.closed)}`);
  check('a re-gate also stops the poll', streams.afterGate.on === false, `liveState.on = ${streams.afterGate.on}`);

  // sign-out must tear down BEFORE the credential is destroyed — assert the ordering, not just the fact
  const order = await page.evaluate(async () => {
    const seq = [];
    const RealES = window.EventSource, realFetch = window.fetch;
    class ProbeES { constructor(u) { this.url = u; } addEventListener() {} close() { seq.push('close:' + this.url); } }
    window.EventSource = ProbeES;
    // The logout POST is left HANGING on purpose: the handler calls location.reload() after awaiting it,
    // which would destroy this execution context before the sequence can be read. Hanging the request also
    // makes the assertion the sharper one — the stream must already be closed while the logout is still in
    // flight, not merely by the time the page has reloaded.
    window.fetch = (u) => { if (String(u).includes('logout')) seq.push('logout'); return new Promise(() => {}); };
    revealConsole(); liveState.on = true; liveStream();
    document.querySelector('#logoutBtn').click();
    await new Promise(r => setTimeout(r, 150));
    window.EventSource = RealES; window.fetch = realFetch;
    return seq;
  });
  const iClose = order.findIndex(x => /^close:.*\/v1\/events/.test(x));
  const iOut = order.indexOf('logout');
  check('sign-out closes the stream', iClose >= 0, JSON.stringify(order));
  check('sign-out closes the stream BEFORE destroying the session', iClose >= 0 && iOut >= 0 && iClose < iOut, JSON.stringify(order));

  // ---- 4. the API surface must not caption a write route as a read ----
  // The Methods column had this defect and it was fixed; the constant simply moved one column right, so
  // seven POST-only routes — including /v1/mode, the actuation-mode chokepoint — read "session-ok · GET".
  const api = await page.evaluate(() => {
    const probe = {
      '/v1/mode': ['post'], '/v1/vote': ['post'], '/v1/sessions': ['get'],
      '/v1/secrets/{name}': ['get', 'post'], '/v1/ingest/librenms': ['post'], '/v1/session': ['post'],
    };
    const out = {};
    for (const [p2, ms] of Object.entries(probe)) out[p2] = apiAuthClass(p2, ms)[0];
    return out;
  });
  check('a POST-only route is never captioned GET', !/GET/.test(api['/v1/mode']) && /POST/.test(api['/v1/mode']), `/v1/mode -> "${api['/v1/mode']}"`);
  check('/v1/vote likewise', !/GET/.test(api['/v1/vote']), `"${api['/v1/vote']}"`);
  check('a GET-only route still says GET', /GET/.test(api['/v1/sessions']), `"${api['/v1/sessions']}"`);
  check('a mixed route names both verbs', /GET/.test(api['/v1/secrets/{name}']) && /POST/.test(api['/v1/secrets/{name}']), `"${api['/v1/secrets/{name}']}"`);
  check('the machine-only and login classes still win', api['/v1/ingest/librenms'] === 'machine-only' && api['/v1/session'] === 'operator-login', JSON.stringify(api));

  // ---- 5. a permanently disabled control says why, and does not blame the gate ----
  const est = await page.evaluate(() => {
    revealConsole();
    // The drain/cordon controls live on the NODE CARD. Steps 3-4 above left this shell flagged live
    // (liveState.on=true) with NO adopted estate graph, and on that state the inspector now — correctly —
    // refuses to lay out the fixture globals, so no card exists to probe. The contract under test here is
    // the card's own titles and note (a design property, identical in both modes), so probe it on the
    // design preview, the state where a card legitimately renders without a live graph.
    liveState.on = false;
    route('estatedepth');
    const btns = Array.from(document.querySelectorAll('#view button.e2-gbtn'));
    const note = (document.querySelector('#view .e2-gnote') || {}).innerText || '';
    return { n: btns.length, unexplained: btns.filter(b => !(b.getAttribute('title') || '').trim()).length,
             blamesGate: btns.filter(b => /gate|mode (chokepoint|withholds)|read-only/i.test(b.getAttribute('title') || '') && !/not gated/i.test(b.getAttribute('title') || '')).length,
             note };
  });
  check('the disabled estate controls are present', est.n >= 2, `${est.n}`);
  check('EVERY disabled control explains itself', est.unexplained === 0, `${est.unexplained} with no title`);
  check('no disabled control blames the mode chokepoint', est.blamesGate === 0, `${est.blamesGate}`);
  check('the note says they are outside the action space', /action space|op-class/i.test(est.note), JSON.stringify(est.note.slice(0, 120)));

  // ---- 6. every timestamp column names its clock ----
  const clocks = await page.evaluate(() => {
    const out = {};
    liveState.on = true;
    // seed each surface so its TABLE actually renders — an empty view has no header row and would let this
    // check pass by finding nothing, which is the shape of an oracle that cannot fail.
    // the ledger and alerts views render from the GLOBALS the live layer replaces, not from liveState —
    // seed what each view actually reads, or the table never renders and this check proves nothing
    liveState.ledger = [{ seq: 1, ts: '2026-07-29 12:00:00', actor: 'system', kind: 'classify', ref: 'r1', action: 'a1', hash: 'h', prev: 'p' }];
    liveState.ledgerState = 'ok';
    LEDGER.length = 0; LEDGER.push(...liveState.ledger);
    liveState.chain = { verified: true, seq: 1 };
    liveState.alerts = [{ src: 'librenms', host: 'h1', rule: 'r', sev: 'warn', ts: '12:00:00', state: 'active', prov: 'single' }];
    liveState.alertsState = 'ok';
    if (typeof ALERTS !== 'undefined') { ALERTS.length = 0; ALERTS.push({ id: 'al-1', src: 'librenms', host: 'h1', rule: 'r', sev: 'warn', ts: '12:00:00', state: 'active', prov: 'single', session: 'r1' }); }
    liveState.sessions = [{ external_ref: 'r1', band: 'AUTO', verdict: 'match', risk_level: 'low', action_id: 'a1', classified_at: '2026-07-29T12:00:00Z' }];
    liveState.sessionTotal = 1;
    for (const v of ['ledger', 'alerts', 'command']) {
      try { route(v); } catch (e) { continue; }
      out[v] = Array.from(document.querySelectorAll('#view table.tbl thead th'))
        .map(th => th.textContent.trim())
        .filter(t => /time|classified|when|ts/i.test(t));
    }
    return out;
  });
  for (const [v, cols] of Object.entries(clocks)) {
    if (!cols.length) continue;
    const bare = cols.filter(c => !/utc|local|z\b|\(/i.test(c));
    check(`#${v}: every time column names its zone`, bare.length === 0, `unlabelled: ${JSON.stringify(bare)}`);
  }
  // A surface that rendered NO table yields no columns and would pass the loop above by finding nothing.
  // Name which surfaces produced evidence, so a silently-empty view is visible rather than counted as clean.
  const withCols = Object.entries(clocks).filter(([, c]) => c.length).map(([v]) => v);
  check('time columns were actually found on the surfaces that carry them', withCols.length >= 2,
    `only ${JSON.stringify(withCols)} produced a time column — ${JSON.stringify(clocks)}`);

  // ---- 6b. and the VALUE is UTC, not the viewer's clock ----
  // A header check alone cannot catch the original defect: relabelling the column "UTC" while still
  // formatting with toLocaleTimeString would pass it. So render a KNOWN instant in a non-UTC browser and
  // read the cell back. Asia/Tokyo is UTC+9 with no DST, so a local render is off by exactly 9 hours.
  const tzCtx = await browser.newContext({ timezoneId: 'Asia/Tokyo' });
  const tzPage = await tzCtx.newPage();
  await tzPage.route('**/v1/**', async r => {
    const u = r.request().url();
    if (u.includes('/v1/whoami')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ source: 'operator:test', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'test' }) });
    if (u.includes('/v1/ledger')) return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ entries: [{ seq: 7, created_at: '2026-07-29T12:00:00Z', decision: 'system:classify', external_ref: 'r1', action_id: 'a1', entry_hash: 'h', prev_hash: 'p' }] }) });
    return r.fulfill({ status: 500, contentType: 'application/json', body: '{}' });
  });
  await tzPage.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await tzPage.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await tzPage.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  const tz = await tzPage.evaluate(() => {
    route('ledger');
    const offsetMin = new Date().getTimezoneOffset();
    const cells = Array.from(document.querySelectorAll('#view table.tbl tbody tr td')).map(td => td.textContent.trim());
    return { offsetMin, cells: cells.slice(0, 3) };
  });
  await tzCtx.close();
  check('the probe browser really is off UTC', tz.offsetMin !== 0, `offset ${tz.offsetMin} min — a UTC browser makes this check vacuous`);
  const rendered = tz.cells.find(c => /\d{2}:\d{2}:\d{2}/.test(c)) || '';
  check('a ledger timestamp renders in UTC, not the viewer clock', /12:00:00/.test(rendered) && !/21:00:00/.test(rendered),
    `created_at 2026-07-29T12:00:00Z rendered as "${rendered}" in Asia/Tokyo (a local render shows 21:00:00)`);
  check('a ledger timestamp carries its date', /2026-07-29/.test(rendered), `"${rendered}" — a bare clock time cannot be compared across days`);
} finally { await browser.close(); }

console.log(failed ? `session-boundary: ${failed} FAILED` : 'session-boundary: all checks passed');
process.exit(failed ? 1 : 0);
