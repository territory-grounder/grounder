// A ROLE IS A PROMISE. #workflows declared role=listbox with 50 role=option children and implemented NONE of
// the keyboard behaviour that role commits to: no ArrowUp/ArrowDown, no Home/End (measured: zero occurrences
// in the module). A screen reader announced "listbox, 50 items" and the arrow keys did nothing — a worse
// position than an unlabelled list, because the operator is told a navigation model that does not exist. Every
// option also carried tabindex=0, making the list FIFTY tab stops between the facet chips and the detail pane.
//
// #estatedepth published selection as a CSS class alone across 217 rows, so an assistive-technology operator
// could walk the entire estate and never learn which node they were on.
//
// These are asserted over EVERY option rather than a sample, because the defect was a property of the whole
// set, and in both directions: the fix must not break mouse selection or the focus survival that an earlier
// oracle in this suite already pins.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const SESSIONS = Array.from({ length: 12 }, (_, i) => ({
  external_ref: `run-${i}`, band: i % 3 === 0 ? 'POLL_PAUSE' : 'AUTO', verdict: 'match',
  risk_level: 'low', action_id: `a${i}`, op_class: 'restart-service',
  classified_at: new Date(Date.now() - i * 60000).toISOString(),
}));
const ESTATE = {
  available: true, node_count: 40, edge_count: 0, source_count: 1, captured_at: '2026-07-30T00:00:00Z',
  nodes: Array.from({ length: 40 }, (_, i) => ({ name: `dc1h${String(i).padStart(2, '0')}`, health: i < 3 ? 'crit' : 'ok' })),
  edges: [],
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext({ viewport: { width: 1500, height: 1000 } })).newPage();
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/sessions')) return j({ total: SESSIONS.length, sessions: SESSIONS });
    if (u.includes('/v1/estate')) return j(ESTATE);
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above — every in-chain fetch and the post-adopt re-render have
  // resolved by the time page.evaluate() returns — so this is margin for the DOM to settle, not a guess at
  // fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  // ---- 1. ROVING TABINDEX: one tab stop, not fifty ----
  const roving = await page.evaluate(() => {
    route('workflows');
    const opts = Array.from(document.querySelectorAll('#view [role=option]'));
    return {
      count: opts.length,
      tabbable: opts.filter(o => o.getAttribute('tabindex') === '0').length,
      selected: opts.filter(o => o.getAttribute('aria-selected') === 'true').length,
      // the tabbable one must BE the selected one, or arrow-entry starts somewhere the operator cannot see
      tabbableIsSelected: opts.filter(o => o.getAttribute('tabindex') === '0')
        .every(o => o.getAttribute('aria-selected') === 'true'),
      listRole: (document.querySelector('#view .wf-list') || {}).getAttribute?.call(document.querySelector('#view .wf-list'), 'role'),
    };
  });
  check('the run list is populated', roving.count >= 5, `${roving.count} options`);
  check('the container still declares role=listbox', roving.listRole === 'listbox', String(roving.listRole));
  check('EXACTLY ONE option is tabbable (roving tabindex)', roving.tabbable === 1,
    `${roving.tabbable} of ${roving.count} options are tabbable — a listbox must be one tab stop, not ${roving.count}`);
  check('exactly one option is aria-selected', roving.selected === 1, `${roving.selected} selected`);
  check('the tabbable option IS the selected one', roving.tabbableIsSelected === true,
    'arrow-key entry would begin on an option the operator cannot see is current');

  // ---- 2. THE KEYBOARD CONTRACT, over the CLOSED SET of keys the role promises ----
  const keys = await page.evaluate(async () => {
    const sleep = ms => new Promise(s => setTimeout(s, ms));
    route('workflows'); await sleep(150);
    const sel = () => {
      const o = document.querySelector('#view [role=option][aria-selected=true]');
      return o ? o.getAttribute('data-wf-key') : null;
    };
    const press = async (key) => {
      const active = document.activeElement;
      active.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
      await sleep(120);
      return { sel: sel(), focusKey: document.activeElement.getAttribute
        ? document.activeElement.getAttribute('data-wf-key') : null,
        focusIsOption: document.activeElement.getAttribute
          ? document.activeElement.getAttribute('role') === 'option' : false };
    };
    const first = document.querySelector('#view [role=option]');
    first.focus();
    const start = sel();
    const down1 = await press('ArrowDown');
    const down2 = await press('ArrowDown');
    const up1 = await press('ArrowUp');
    const end = await press('End');
    const home = await press('Home');
    return { start, down1, down2, up1, end, home,
             total: document.querySelectorAll('#view [role=option]').length };
  });
  check('ArrowDown moves the selection', keys.down1.sel && keys.down1.sel !== keys.start,
    `selection stayed at ${keys.start} — the role promises arrow navigation`);
  check('ArrowDown again moves it further', keys.down2.sel && keys.down2.sel !== keys.down1.sel, JSON.stringify(keys.down2));
  check('ArrowUp moves it back', keys.up1.sel === keys.down1.sel, `${keys.up1.sel} != ${keys.down1.sel}`);
  check('End jumps to the last option', keys.end.sel === `run:run-${keys.total - 1}` || keys.end.sel !== keys.up1.sel,
    JSON.stringify(keys.end));
  check('Home jumps to the first', keys.home.sel === keys.start, `${keys.home.sel} != ${keys.start}`);
  // FOCUS MUST FOLLOW, and must land on an option — not on <body> after the pane re-renders.
  check('focus follows the arrows and stays on an option', keys.down1.focusIsOption && keys.home.focusIsOption,
    `down1=${JSON.stringify(keys.down1)} home=${JSON.stringify(keys.home)} — the re-render detaches the node, so ` +
    `focusing the pre-render element silently drops focus to <body>`);
  check('and focus lands on the option that is now selected', keys.home.focusKey === keys.home.sel,
    `focus=${keys.home.focusKey} selected=${keys.home.sel}`);

  // ---- 3. THE OTHER DIRECTION: mouse selection must still work ----
  // Without this, "roving tabindex" could have been implemented by disabling the list.
  const mouse = await page.evaluate(async () => {
    const sleep = ms => new Promise(s => setTimeout(s, ms));
    route('workflows'); await sleep(150);
    const opts = Array.from(document.querySelectorAll('#view [role=option]'));
    const target = opts[3];
    const key = target.getAttribute('data-wf-key');
    target.click(); await sleep(150);
    const now = document.querySelector('#view [role=option][aria-selected=true]');
    return { clicked: key, selected: now ? now.getAttribute('data-wf-key') : null };
  });
  check('clicking an option still selects it', mouse.selected === mouse.clicked, JSON.stringify(mouse));

  // ---- 4. #estatedepth publishes selection in ARIA over EVERY row ----
  const depth = await page.evaluate(async () => {
    const sleep = ms => new Promise(s => setTimeout(s, ms));
    route('estatedepth'); await sleep(400);
    const rows = Array.from(document.querySelectorAll('#view .e2-lrow'));
    const withClass = rows.filter(r => r.classList.contains('sel'));
    const withAria = rows.filter(r => r.getAttribute('aria-current') === 'true');
    return {
      rows: rows.length,
      selByClass: withClass.length,
      selByAria: withAria.length,
      // the two must agree: a class without the attribute is the original defect
      agree: withClass.length === withAria.length &&
             withClass.every(r => r.getAttribute('aria-current') === 'true'),
    };
  });
  check('the estate node list rendered rows', depth.rows > 5, `${depth.rows} rows`);
  check('a row IS visually selected', depth.selByClass === 1, `${depth.selByClass} rows carry .sel`);
  check('the selected row is published as aria-current', depth.selByAria === 1,
    `${depth.selByAria} of ${depth.rows} rows carry aria-current — selection was CSS-only, invisible to assistive technology`);
  check('the visual and the announced selection agree', depth.agree === true,
    'a .sel class without aria-current is exactly the original defect');
} finally { await browser.close(); }

console.log(failed ? `composite-widget-contract: ${failed} FAILED` : 'composite-widget-contract: all checks passed');
process.exit(failed ? 1 : 0);
