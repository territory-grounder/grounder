// WCAG 1.4.3 (AA: 4.5:1 for text below 18.66px) over the console's COLOUR SYSTEM, in BOTH themes.
//
// ★ WHY THIS READS THE STYLESHEET INSTEAD OF THE RENDERED PAGE.
// The defect that produced this file was found by scanning the live authenticated console: 60 failing text
// nodes in dark, 106 in light. That number was a FLOOR, not the answer — a rendered scan only sees the view
// that happened to load. Deriving the set from the token table instead found three more failing light tokens
// (--partial, --notice, --territory) and one in DARK (--deviation at 4.39:1) that no rendered scan reached,
// because no deviation text was on screen at the time. A scan of one view is a sample; the token table is
// the closed enumeration. The token set here is therefore derived from the stylesheet — every `color:var(--X)`
// in any rule — never hand-listed. Hand-listing is exactly what let --ink4 sit at 2.40:1 across 33 rules
// while the comment beside it read "--ink4 is non-text-only (3.1:1)": a rule written down and never checked.
//
// PASS 1 — ink on a surface. Each text token vs the WORST of --g0/--g1/--g2/--g3s (the darkest in light
// theme, the lightest in dark), so passing here means passing on any card in the app.
//
// PASS 2 — INVERTED INK: a chip that paints a token background and puts text on it. This pairing spans
// rules (`.seg span.on` sets the ink, `.seg span.on[data-s="enforce"]` sets the background), so no rule-local
// check can see it; it is measured on a REAL ELEMENT built from the selector and read back through the
// cascade. Two things make it honest rather than noisy:
//   - it only counts elements the stylesheet TREATS AS TEXT (some matching rule sets color/font/letter-
//     spacing/text-transform). Without that filter it injected text into decorative dots, squares and
//     progress fills — .log-sq, .wf-dot, .grnd-fbar-fill — and reported 120 "failures" for text that does
//     not exist. An oracle that cries wolf is one you learn to ignore, which is worse than no oracle.
//   - it fails loudly if it synthesises ZERO chips. It did exactly that at first (`if (r.cssRules)` is
//     truthy for the EMPTY list every CSSStyleRule exposes for nesting, so every plain rule was treated as
//     a group and skipped — 28 rules of ~1900 walked). Without the zero guard that read as a clean pass.
//
// Pass 2 caught five real defects, all one shape — A LITERAL INK HARD-CODED AGAINST A TOKEN BACKGROUND,
// which cannot follow when the token moves: #fff on --map (3.24:1) and #04140a on --auto, #fff on
// --deviation (3.40:1), #2a0e10 on --pause, and #04181a on --accent in .btn.primary (3.13:1 — a real button).
// Three were pre-existing; two were introduced BY the pass-1 fix in this same change, when darkening --auto
// and --accent for AA moved the ground under an ink that could not follow. That is the argument for the
// tokens: --g0/--g1 inverts with the theme, so the ink tracks its own background.
//
// Needs no authentication and no data — it is a property of the palette, which is why it runs in CI while
// the live console cannot.
import { chromium } from 'playwright';

// run.sh passes CONSOLE_BASE, not a port. Reading a different variable would have silently measured
// whatever happened to be listening on the default port — the failure serve.mjs exists to prevent.
const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
const URL = `${BASE}/index.html`;
const SURFACES = ['g0', 'g1', 'g2', 'g3s'];
const AA = 4.5;

const fail = [];
const browser = await chromium.launch();
try {
  for (const theme of ['dark', 'light']) {
    const page = await (await browser.newContext()).newPage();
    await page.goto(URL, { waitUntil: 'domcontentloaded' });
    await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);

    const res = await page.evaluate(surfaces => {
      const lum = c => {
        const f = c.map(v => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
        return 0.2126 * f[0] + 0.7152 * f[1] + 0.0722 * f[2];
      };
      const hex = h => {
        h = h.trim().replace('#', '');
        if (h.length === 3) h = h.split('').map(c => c + c).join('');
        return [0, 2, 4].map(i => parseInt(h.slice(i, i + 2), 16));
      };
      const rgb = c => {
        const m = String(c).match(/rgba?\(([\d.]+),\s*([\d.]+),\s*([\d.]+)(?:,\s*([\d.]+))?\)/);
        return m ? [+m[1], +m[2], +m[3], m[4] === undefined ? 1 : +m[4]] : null;
      };
      const ratio = (a, b) => {
        const [l1, l2] = [lum(a), lum(b)];
        return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
      };
      // Collect every style rule once. Recurse ONLY into rules that actually have children.
      const rules = [];
      for (const sheet of document.styleSheets) {
        let rs; try { rs = sheet.cssRules; } catch { continue; }
        const walk = list => { for (const r of list) {
          if (r.cssRules && r.cssRules.length) walk(r.cssRules);
          if (r.style && r.selectorText) rules.push(r);
        } };
        walk(rs);
      }

      // ---- pass 1: tokens used as ink on a surface ----
      const used = new Set();
      for (const r of rules) {
        const c = r.style.getPropertyValue('color');
        const m = c && c.match(/var\(\s*--([a-z0-9-]+)/i);
        if (m) used.add(m[1]);
      }
      const cs = getComputedStyle(document.documentElement);
      const val = t => (cs.getPropertyValue('--' + t) || '').trim();
      // ★ THE SURFACE SET MUST INCLUDE THE TINTS THAT ACTUALLY CARRY TEXT — but ONLY for the inks that can
      // land on them. Live post-deploy, two elements still failed AA while this oracle passed: .wk-item-s
      // sits inside .wk-item.active, whose background is rgba(14,155,166,.08) over --g0 — composited
      // rgb(211,227,231), DARKER than any surface token. --ink4 measured 4.77 here and 4.42 in the browser.
      //
      // SCOPE, STATED: a tint is included only if some rule actually PAINTS it (background:var(--X-dim));
      // three do, and they carry real text (.wk-item.active holds --ink4, .wk-md blockquote holds --ink2,
      // .sk-btn.sel holds --accent). And only the GENERAL-PURPOSE inks are required to clear AA on them,
      // because those are the ones that appear inside arbitrary components. Requiring every semantic colour
      // on every tint is the cartesian product, not the world: it flagged --deviation on --territory-dim,
      // a pairing that does not exist, and 12 such phantoms at once. An oracle that cries wolf gets ignored.
      const plain = surfaces.map(s => ({ name: s, v: val(s) })).filter(s => /^#[0-9a-f]{6}$/i.test(s.v));
      const TINT_INKS = ['ink', 'ink2', 'ink3', 'ink4', 'accent'];
      /* ★ A PAIRED INK IS NOT A GENERAL-PURPOSE INK, AND CHECKING IT AGAINST PAGE SURFACES IS THE SAME
         CARTESIAN-PRODUCT ERROR THIS FILE ALREADY FIXED ONCE. --on-solid exists for ONE job: text painted
         on a saturated accent FILL (the verdict glyphs). It is #05080a dark / #ffffff light, and against
         --g0 it is 1.04:1 BY CONSTRUCTION — a true reading of a pairing that does not exist.
         So it is measured against the fills it is actually paired with, which is a STRONGER assertion than
         the default: all three pairings must clear AA, a partner that stops resolving fails loudly rather
         than shrinking the check to nothing, and pass 2 independently re-measures the rendered glyph on its
         real element. Dropping the token from the census would have been the weakening; this is not that. */
      const PAIRED = { 'on-solid': ['match', 'partial', 'deviation'] };
      const painted = new Set();
      for (const r of rules) {
        const m = (r.cssText || '').match(/background(?:-color)?\s*:\s*var\(\s*--([a-z0-9-]+)/i);
        if (m && /^#[0-9a-f]{8}$/i.test(val(m[1]))) painted.add(m[1]);
      }
      const tintSurf = [];
      for (const t of painted) {
        const v = val(t);
        const [tr, tg, tb] = hex(v.slice(0, 7));
        const a = parseInt(v.slice(7, 9), 16) / 255;
        for (const b of plain) {
          const [br, bg2, bb] = hex(b.v);
          const mix = [tr * a + br * (1 - a), tg * a + bg2 * (1 - a), tb * a + bb * (1 - a)].map(Math.round);
          tintSurf.push({ name: `${t}-over-${b.name}`,
                          v: '#' + mix.map(x => x.toString(16).padStart(2, '0')).join('') });
        }
      }
      if (!tintSurf.length) throw new Error('no painted tints found — the tint derivation is broken, not the palette');
      const surf = plain;
      const tokens = [];
      for (const t of [...used].sort()) {
        const v = val(t);
        if (!/^#[0-9a-f]{6}$/i.test(v)) continue;   // alpha / derived values are not plain ink
        if (surfaces.includes(t)) continue;         // a SURFACE used as `color` is inverted ink — pass 2 owns it
        let worst = { r: Infinity, on: null };
        const against = PAIRED[t]
          ? PAIRED[t].map(p => ({ name: p, v: val(p) })).filter(x => /^#[0-9a-f]{6}$/i.test(x.v))
          : TINT_INKS.includes(t) ? surf.concat(tintSurf) : surf;
        if (PAIRED[t] && against.length !== PAIRED[t].length) {
          tokens.push({ token: t, value: v, ratio: 0, on: 'MISSING PARTNER' });
          continue;
        }
        for (const s of against) {
          const r = ratio(hex(v), hex(s.v));
          if (r < worst.r) worst = { r: +r.toFixed(2), on: s.name };
        }
        tokens.push({ token: t, value: v, ratio: worst.r, on: worst.on });
      }

      // ---- pass 2: inverted ink, on real elements ----
      const TEXTPROP = ['color', 'font', 'font-size', 'font-family', 'font-weight', 'letter-spacing', 'text-transform'];
      const build = sel => {
        const parts = sel.trim().split(/\s+/);
        let root = null, cur = null;
        for (const part of parts) {
          const tag = part.match(/^[a-z][a-z0-9]*/i);
          const el = document.createElement(tag ? tag[0] : 'div');
          for (const c of part.match(/\.[-\w]+/g) || []) el.classList.add(c.slice(1));
          for (const a of part.match(/\[[^\]]+\]/g) || []) {
            const m = a.match(/\[([-\w]+)(?:[~|^$*]?=["']?([^"'\]]*)["']?)?\]/);
            if (m) el.setAttribute(m[1], m[2] ?? '');
          }
          if (!root) { root = el; cur = el; } else { cur.appendChild(el); cur = el; }
        }
        if (cur) cur.textContent = 'Xg';
        return { root, leaf: cur };
      };
      const chips = [];
      for (const r of rules) {
        if (!/background(-color)?\s*:\s*var\(\s*--/.test(r.cssText || '')) continue;
        for (const sel of r.selectorText.split(',')) {
          if (/[:>+~]/.test(sel)) continue;                    // pseudo/combinator shapes are not synthesisable
          let b; try { b = build(sel); } catch { continue; }
          if (!b.root || !b.leaf) continue;
          document.body.appendChild(b.root);
          // Does the stylesheet treat this element as TEXT? If nothing sets a text property on it, it is
          // decoration (a dot, a bar, a stripe) and the text this harness injected is imaginary.
          let isText = false;
          for (const rr of rules) {
            let hit = false; try { hit = b.leaf.matches(rr.selectorText); } catch { continue; }
            if (!hit) continue;
            if (TEXTPROP.some(p => rr.style.getPropertyValue(p))) { isText = true; break; }
          }
          const lcs = getComputedStyle(b.leaf);
          // Read the STRINGS out now: getComputedStyle is a live view and every property reads back as ""
          // once the element is detached. Storing `lcs.color` for the report printed "( on , needs 4.5:1)"
          // on a genuine failure — the ratio was right and the evidence was blank.
          const fgs = lcs.color, bgs = lcs.backgroundColor;
          const f = rgb(fgs), bg = rgb(bgs);
          document.body.removeChild(b.root);
          if (!isText || !f || !bg || bg[3] < 0.9) continue;
          chips.push({ sel: sel.trim(), fg: fgs, bg: bgs,
                       ratio: +ratio(f.slice(0, 3), bg.slice(0, 3)).toFixed(2) });
        }
      }
      return { tokens, surfaces: surf.map(s => s.name), chips };
    }, SURFACES);

    if (res.tokens.length < 8) fail.push(`${theme}: only ${res.tokens.length} text tokens discovered — the stylesheet walk is broken, not the palette`);
    if (!res.chips.length) fail.push(`${theme}: pass 2 synthesised ZERO text chips — the builder is broken, so it proves nothing`);
    for (const t of res.tokens) if (t.ratio < AA) fail.push(`${theme}: --${t.token} ${t.value} is ${t.ratio}:1 on --${t.on} (needs ${AA}:1)`);
    for (const c of res.chips) if (c.ratio < AA) fail.push(`${theme}: ink on chip \`${c.sel}\` is ${c.ratio}:1 (${c.fg} on ${c.bg}, needs ${AA}:1)`);
    console.log(`  ${theme}: ${res.tokens.length} ink tokens (worst ${Math.min(...res.tokens.map(t => t.ratio))}:1) · ` +
                `${res.chips.length} text chips (worst ${Math.min(...res.chips.map(c => c.ratio))}:1)`);
  }
} finally { await browser.close(); }

if (fail.length) { console.error('FAIL contrast-tokens:\n  ' + fail.join('\n  ')); process.exit(1); }
console.log('PASS contrast-tokens');
