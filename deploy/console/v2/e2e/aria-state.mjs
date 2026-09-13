// STATE THAT IS PAINTED MUST ALSO BE PUBLISHED.
//
// Four controls in this console carried their CURRENT STATE in a CSS class and nowhere else. A sighted
// operator saw which filter was on, which view they were in, and which palette row was selected; assistive
// tech was told none of it. Measured live on the deployed bundle:
//
//   * command palette — 28 result rows, `div.r` with a `.on` class moved by arrow keys. role=null,
//     aria-selected=null, and the input carried no combobox semantics, so the console's only global
//     jump affordance was a text box that silently did nothing an AT user could observe.
//   * facet chips — 39 chips across 10 views, `.facet` → `.facet on`. The filters demonstrably change
//     the rows below them; not one chip carried aria-pressed.
//   * primary nav — 22 `a.navi`, active view by class only, no aria-current.
//   * seal form — the four fields that write a secret into the estate had aria-labels (which fixed the
//     screen-reader half) and label[for] on 0 of 4, so a SIGHTED operator saw four identical boxes the
//     moment typing destroyed the placeholders.
//
// Plus two colour defects the same sweep measured: ::placeholder fell through to the UA default
// rgb(117,117,117) on the seal form and the palette input, and the verdict glyph added to the spine node
// (✓ ◐ ✕ — the colourblind second channel) was unstyled text on a saturated fill at 2.73:1.
//
// This oracle asserts over the CLOSED SET each defect was found in — every result row, every chip on every
// view, every nav item — not over one hand-picked sample. A sampler is how three of these survived two
// earlier sweeps.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// sRGB relative luminance + WCAG contrast, on already-composited rgb() strings.
const lum = (c) => { const f = c.map(v => { v /= 255; return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }); return 0.2126 * f[0] + 0.7152 * f[1] + 0.0722 * f[2]; };
const ratio = (a, b) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
const parse = (s) => (s.match(/[\d.]+/g) || []).slice(0, 3).map(Number);

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(() => (typeof route === 'function' ? route('command') : null));
  await page.waitForFunction(() => typeof views === 'object' && Object.keys(views).length > 5);

  const viewNames = await page.evaluate(() => (typeof views === 'object' ? Object.keys(views) : []));
  check('views are enumerable', viewNames.length > 5, `${viewNames.length} views`);

  // ---- 1. primary nav publishes the current view ----
  const nav = await page.evaluate(() => {
    const out = [];
    for (const a of document.querySelectorAll('a.navi[data-view]')) out.push({ v: a.dataset.view, on: a.classList.contains('on'), cur: a.getAttribute('aria-current') });
    return out;
  });
  check('nav is the full set', nav.length >= 15, `${nav.length} items`);
  const navBad = nav.filter(n => (n.on ? n.cur !== 'page' : n.cur !== null));
  check('every nav item: painted-active <=> aria-current=page', navBad.length === 0, JSON.stringify(navBad.slice(0, 4)));
  check('exactly one nav item is current', nav.filter(n => n.cur === 'page').length === 1, `${nav.filter(n => n.cur === 'page').length}`);

  // ---- 2. facet chips publish pressed state, on EVERY view that has them ----
  let chipTotal = 0; const chipBad = [];
  for (const v of viewNames) {
    await page.evaluate(n => (typeof route === 'function' ? route(n) : null), v);
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    const chips = await page.evaluate(() => Array.from(document.querySelectorAll('.facet')).map(c => ({ t: (c.textContent || '').trim().slice(0, 18), on: c.classList.contains('on'), pressed: c.getAttribute('aria-pressed') })));
    chipTotal += chips.length;
    for (const c of chips) if (c.on ? c.pressed !== 'true' : c.pressed !== 'false') chipBad.push({ v, ...c });
  }
  check('facet chips exist across views', chipTotal >= 20, `${chipTotal} chips`);
  check('every facet chip: painted-on <=> aria-pressed', chipBad.length === 0, `${chipBad.length} bad, e.g. ${JSON.stringify(chipBad.slice(0, 3))}`);

  // ---- 3. the command palette is a real combobox, and selection is published ----
  await page.evaluate(() => (typeof route === 'function' ? route('command') : null));
  await page.waitForFunction(() => !!document.querySelector('#palOpen'));
  await page.evaluate(() => document.querySelector('#palOpen')?.click());
  await page.waitForFunction(() => { const r = document.querySelector('#palRes'); return r && r.children.length > 3; });
  const pal0 = await page.evaluate(() => {
    const i = document.querySelector('#palInput'), r = document.querySelector('#palRes');
    return { role: i?.getAttribute('role'), exp: i?.getAttribute('aria-expanded'), ctrl: i?.getAttribute('aria-controls'), auto: i?.getAttribute('aria-autocomplete'), listRole: r?.getAttribute('role'), n: r ? r.querySelectorAll('[role=option]').length : 0, rows: r ? r.children.length : 0 };
  });
  check('palette input is a combobox', pal0.role === 'combobox' && pal0.exp === 'true' && pal0.ctrl === 'palRes' && pal0.auto === 'list', JSON.stringify(pal0));
  check('palette result list is a listbox', pal0.listRole === 'listbox', String(pal0.listRole));
  check('EVERY palette row is an option', pal0.n > 3 && pal0.n === pal0.rows, `${pal0.n} options / ${pal0.rows} rows`);
  const selBefore = await page.evaluate(() => {
    const r = document.querySelector('#palRes'), i = document.querySelector('#palInput');
    const on = Array.from(r.children).findIndex(c => c.classList.contains('sel'));
    const sel = Array.from(r.children).findIndex(c => c.getAttribute('aria-selected') === 'true');
    return { on, sel, active: i.getAttribute('aria-activedescendant'), activeId: on >= 0 ? r.children[on].id : null };
  });
  check('palette selection is published', selBefore.on >= 0 && selBefore.on === selBefore.sel && selBefore.active === selBefore.activeId, JSON.stringify(selBefore));
  await page.keyboard.press('ArrowDown');
  await page.waitForFunction(prev => { const r = document.querySelector('#palRes'); return r && Array.from(r.children).findIndex(c => c.classList.contains('sel')) === prev + 1; }, selBefore.on);
  const selAfter = await page.evaluate(() => {
    const r = document.querySelector('#palRes'), i = document.querySelector('#palInput');
    const on = Array.from(r.children).findIndex(c => c.classList.contains('sel'));
    const sel = Array.from(r.children).findIndex(c => c.getAttribute('aria-selected') === 'true');
    return { on, sel, active: i.getAttribute('aria-activedescendant'), activeId: on >= 0 ? r.children[on].id : null };
  });
  check('ArrowDown moves the painted row', selAfter.on === selBefore.on + 1, `${selBefore.on} -> ${selAfter.on}`);
  check('ArrowDown moves aria-selected AND aria-activedescendant with it', selAfter.on === selAfter.sel && selAfter.active === selAfter.activeId && selAfter.active !== selBefore.active, JSON.stringify(selAfter));

  // ---- 4. the palette input's own placeholder must be readable ----
  const palPh = await page.evaluate(() => {
    const i = document.querySelector('#palInput'); if (!i) return null;
    const walk = (el) => { let e = el; while (e) { const bg = getComputedStyle(e).backgroundColor; if (bg && !/rgba\(0, 0, 0, 0\)|transparent/.test(bg)) return bg; e = e.parentElement; } return 'rgb(0,0,0)'; };
    return { fg: getComputedStyle(i, '::placeholder').color, bg: walk(i) };
  });
  check('palette placeholder is AA', palPh && ratio(parse(palPh.fg), parse(palPh.bg)) >= 4.5, palPh ? `${palPh.fg} on ${palPh.bg} = ${ratio(parse(palPh.fg), parse(palPh.bg)).toFixed(2)}` : 'no #palInput');
  await page.keyboard.press('Escape');
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  // ---- 5. the seal form: a VISIBLE label per field, and a readable placeholder ----
  // the seal form only exists for a signed-in operator (views.secrets returns a sign-in card otherwise),
  // so put the client in that state — the form is rendered entirely client-side from these three fields.
  await page.evaluate(() => { liveState.on = true; liveState.secretRefs = []; liveState.sealed = []; route('secrets'); });
  await page.waitForFunction(() => { const b = document.querySelector('#view'); return b && b.querySelectorAll('input,textarea').length >= 4; });
  const seal = await page.evaluate(() => {
    const box = document.querySelector('#view');
    const fields = Array.from(box.querySelectorAll('input,textarea')).filter(el => el.getAttribute('aria-label') || el.placeholder);
    const walk = (el) => { let e = el; while (e) { const bg = getComputedStyle(e).backgroundColor; if (bg && !/rgba\(0, 0, 0, 0\)|transparent/.test(bg)) return bg; e = e.parentElement; } return 'rgb(0,0,0)'; };
    return fields.map(f => {
      const lab = f.closest('label');
      // the visible caption must be REAL TEXT that survives typing, not the placeholder
      const visible = lab ? Array.from(lab.querySelectorAll('span,div')).map(s => (s.textContent || '').trim()).filter(Boolean)[0] : (f.labels && f.labels[0] ? f.labels[0].textContent.trim() : '');
      const cs = lab ? getComputedStyle(lab) : null;
      return { name: f.getAttribute('aria-label') || f.placeholder, visible: visible || '', shown: !!(lab && cs.display !== 'none' && cs.visibility !== 'hidden'), ph: getComputedStyle(f, '::placeholder').color, bg: walk(f) };
    });
  });
  check('seal form fields found', seal.length >= 4, `${seal.length}`);
  const noLabel = seal.filter(f => !f.shown || f.visible.length < 3);
  check('EVERY seal field has a visible caption', noLabel.length === 0, JSON.stringify(noLabel.map(f => f.name)));
  const phBad = seal.filter(f => ratio(parse(f.ph), parse(f.bg)) < 4.5);
  check('EVERY seal placeholder is AA', phBad.length === 0, phBad.map(f => `${f.name}: ${f.ph} on ${f.bg} = ${ratio(parse(f.ph), parse(f.bg)).toFixed(2)}`).join(' | '));

  // ---- 6. the verdict glyph is legible in BOTH themes ----
  for (const theme of ['dark', 'light']) {
    await page.evaluate(t => { document.documentElement.setAttribute('data-theme', t); }, theme);
    await page.evaluate(() => (typeof route === 'function' ? route('command') : null));
    await page.waitForFunction(() => document.querySelectorAll('.stage .node').length > 0);
    const glyphs = await page.evaluate(() => Array.from(document.querySelectorAll('.stage .node')).filter(n => (n.textContent || '').trim()).map(n => ({ g: n.textContent.trim(), fg: getComputedStyle(n).color, bg: getComputedStyle(n).backgroundColor })));
    const bad = glyphs.filter(x => ratio(parse(x.fg), parse(x.bg)) < 4.5);
    check(`${theme}: verdict glyph is AA on its own fill`, glyphs.length > 0 && bad.length === 0, glyphs.length === 0 ? 'NO GLYPHS RENDERED — the colourblind second channel is gone' : bad.map(x => `${x.g} ${x.fg}/${x.bg} = ${ratio(parse(x.fg), parse(x.bg)).toFixed(2)}`).slice(0, 3).join(' | '));
  }
} finally { await browser.close(); }

console.log(failed ? `aria-state: ${failed} FAILED` : 'aria-state: all checks passed');
process.exit(failed ? 1 : 0);
