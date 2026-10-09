// WCAG 1.3.1 (header association) and 2.4.2 (page title) for the console's tables.
//
// Measured live on the authenticated console across all 22 views: 151 <th> carried no `scope`, 38 tables had
// no accessible name, and every one of the 22 views served the IDENTICAL document.title.
//
// WHY THIS EXERCISES THE HELPERS RATHER THAN A RENDER. The tables are built from 21 call sites and need live
// API data, which does not exist unauthenticated — a render-based check here would measure zero tables and
// pass vacuously, the failure this suite has already been bitten by. Instead it calls the console's OWN h()
// and nameTables() from the served bundle against real DOM. That is the code the fix changed, so a
// regression in it goes red here; the end-to-end proof that the views actually use them is the live run.
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

  const r = await page.evaluate(() => {
    const out = {};
    out.hExists = typeof h === 'function';
    out.nameExists = typeof nameTables === 'function';
    if (!out.hExists) return out;

    // 1. every <th> h() builds carries scope="col" by default...
    const th = h('th', {}, 'Ref');
    out.defaultScope = th.getAttribute('scope');
    // ...and an EXPLICIT scope still wins (a row header must stay a row header).
    out.explicitScope = h('th', { scope: 'row' }, 'x').getAttribute('scope');
    // ...and no other element is touched.
    out.tdScope = h('td', {}, 'x').getAttribute('scope');

    if (!out.nameExists) return out;
    // 2. nameTables binds a table to the heading it renders under.
    const root = document.createElement('div');
    root.append(h('div', { class: 'sec' }, h('h3', {}, 'Accepted alerts (recent window)')));
    const t1 = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Ref'))));
    root.append(t1);
    // a second heading + table proves it picks the NEAREST preceding heading, not the first
    root.append(h('div', { class: 'sec' }, h('h3', {}, 'Governed ledger')));
    const t2 = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Seq'))));
    root.append(t2);
    // two captioned cards, the #api shape: <div class=wrapcard><div class=cap>…</div><table>
    const card1 = h('div', { class: 'wrapcard' },
      h('div', { class: 'cap' }, '/policy ', '4 routes'),
      h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Path')))));
    const card2 = h('div', { class: 'wrapcard' },
      h('div', { class: 'cap' }, '/secrets ', '3 routes'),
      h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Path')))));
    root.append(card1, card2);

    // a table with NO preceding heading must be left alone, not given a manufactured name
    const bare = document.createElement('div');
    const t3 = h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'X'))));
    bare.append(t3);
    document.body.append(root, bare);
    nameTables(root); nameTables(bare);
    const label = t => { const id = t.getAttribute('aria-labelledby');
                         return id ? (document.getElementById(id) || {}).textContent : null; };
    out.t1 = label(t1); out.t2 = label(t2);
    const capTables = [card1.querySelector('table'), card2.querySelector('table')];
    out.cap = label(capTables[0]);
    out.capDistinct = new Set(capTables.map(label)).size;
    out.t3 = t3.getAttribute('aria-labelledby');
    // 3. an already-named table is never relabelled
    const t4 = h('table', { 'aria-label': 'mine' }); root.append(t4); nameTables(root);
    out.t4 = t4.getAttribute('aria-labelledby');
    document.body.removeChild(root); document.body.removeChild(bare);

    // 4. WCAG 2.4.2 — distinct titles per view
    out.viewCount = typeof VIEW_META === 'object' ? Object.keys(VIEW_META).length : 0;
    out.titles = [];
    if (typeof route === 'function' && out.viewCount) {
      for (const v of Object.keys(VIEW_META)) {
        try { route(v); out.titles.push(document.title); } catch { /* view needs data; title still set first */ }
      }
    }
    return out;
  });

  check('the console exposes h() and nameTables()', r.hExists && r.nameExists,
        `h=${r.hExists} nameTables=${r.nameExists} — nothing below would prove anything`);
  check('h("th") defaults to scope="col"', r.defaultScope === 'col', `got ${JSON.stringify(r.defaultScope)}`);
  check('an EXPLICIT scope still wins', r.explicitScope === 'row', `got ${JSON.stringify(r.explicitScope)}`);
  check('h() adds scope to nothing else', r.tdScope === null, `<td> got scope=${JSON.stringify(r.tdScope)}`);
  check('a table is named by the heading it renders under', r.t1 === 'Accepted alerts (recent window)', `got ${JSON.stringify(r.t1)}`);
  check('a card CAPTION also names its table (the #api regression)', r.cap === '/policy 4 routes', `got ${JSON.stringify(r.cap)}`);
  check('sibling tables under sibling captions get DISTINCT names', r.capDistinct === 2,
        `${r.capDistinct} distinct names across 2 captioned cards — every table having *a* name is not enough if they are all the same one`);
  check('it takes the NEAREST preceding heading', r.t2 === 'Governed ledger', `got ${JSON.stringify(r.t2)} — a table named by a distant heading is worse than unnamed`);
  check('a table with no heading is left unnamed', r.t3 === null, `got ${JSON.stringify(r.t3)} — a manufactured name is a lie`);
  check('an already-named table is not relabelled', r.t4 === null, `got ${JSON.stringify(r.t4)}`);

  const distinct = new Set(r.titles);
  check('every view sets its own document.title', r.titles.length > 0 && distinct.size === r.titles.length,
        `${r.titles.length} views produced ${distinct.size} distinct titles`);
} finally { await browser.close(); }

if (failed) { console.log(`\ntable-semantics: ${failed} FAILED`); process.exit(1); }
console.log('\ntable-semantics: all checks passed');
