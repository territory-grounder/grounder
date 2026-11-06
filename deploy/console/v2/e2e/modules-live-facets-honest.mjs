// THE #modules FACET CHIPS MUST FILTER THE LIVE DATA, OR NOT BE RENDERED.
//
// The design-preview #modules view offers six facet chips — All / Live / Idle / Dark / Mutate-scope /
// Disabled — and its fixture viewModules() genuinely filters on them (health for live/idle/dark, scope for
// mutate, enabled for off). But the LIVE view is a full override: it renders /v1/capabilities, whose DTO
// (modules.Capability) is only {surface, source_type, capability, enabled}. It carries no `health` and no
// `scope`. Before the fix the live override read facetState NOWHERE, so all six chips re-rendered the same
// rows — dead UI — and four of them (live/idle/dark/mutate) claimed to filter on fields the DTO cannot even
// emit. This oracle asserts the console-honesty bar on the LIVE path:
//   * only the DTO-backed chips are rendered (All, Disabled); the four the DTO cannot back are withdrawn
//   * a facetState left pointing at a withdrawn chip is reset (never filters on an absent dimension)
//   * every rendered chip is a REAL filter: clicking it changes which live rows are shown — no chip is a no-op
//
// RED before the fix: the live override ignored facetState, so FACETS.modules kept all six chips and clicking
// "Disabled" changed nothing (rows for "off" == rows for "all"). GREEN after: two chips, and "Disabled"
// filters to the disabled subset.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// A live registry with a mix of enabled and disabled capabilities across surfaces. 5 declared, 2 enabled
// (librenms, matrix), 3 disabled (webhook, email, ssh) — so "Disabled" is a strict, observable subset.
const CAPS = { capabilities: [
  { surface: 'ingest',    source_type: 'librenms', capability: 'alerts', enabled: true },
  { surface: 'ingest',    source_type: 'webhook',  capability: 'alerts', enabled: false },
  { surface: 'notifier',  source_type: 'matrix',   capability: 'notify', enabled: true },
  { surface: 'notifier',  source_type: 'email',    capability: 'notify', enabled: false },
  { surface: 'actuation', source_type: 'ssh',      capability: 'exec',   enabled: false },
] };
const ALL_ROWS = CAPS.capabilities.length;              // 5
const OFF_ROWS = CAPS.capabilities.filter(c => !c.enabled).length; // 3
const WITHDRAWN = ['live', 'idle', 'dark', 'mutate'];   // the four the DTO cannot emit

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  // Boot adopt fires on http:// — let whoami fail fast so it cannot race our manual live state, and give the
  // config schema an empty (but present) answer so the "Configure a module" section adds no extra rows.
  await page.route('**/api/v1/whoami', r => r.fulfill({ status: 503, contentType: 'application/json', body: '{}' }));
  await page.route('**/api/v1/modules/schema', r => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ modules: [], undescribed: [] }) }));

  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate((caps) => {
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    if (typeof liveState === 'object') { liveState.on = true; liveState.caps = caps; liveState.config = []; }
    // Seed a stale facet pointing at a chip the live DTO cannot back: the honest override must reset it.
    if (typeof facetState === 'object') facetState.modules = 'mutate';
    route('modules');
  }, CAPS);
  // views.modules() kicks off modCfgEnsureSchema() as a fire-and-forget call on every render, guarded so
  // the /v1/modules/schema fetch only actually happens once; modCfgState.schema is set on the same
  // synchronous line that then calls the passed rerender() callback, so observing it (rather than a fixed
  // guess) also guarantees the schema-triggered re-render has already landed.
  await page.waitForFunction(() => typeof modCfgState !== 'undefined' && modCfgState.schema !== null).catch(() => {});

  // ---- read the rendered facet chips, FACETS source, facetState, and the registry rows ----
  const read = () => page.evaluate(() => {
    const chips = Array.from(document.querySelectorAll('#facets button')).map(b => ({
      k: b.getAttribute('data-k'), label: (b.textContent || '').trim() }));
    const rows = Array.from(document.querySelectorAll('#view table.tbl tbody tr'));
    return {
      chips,
      facetsModules: (typeof FACETS !== 'undefined' && Array.isArray(FACETS.modules)) ? FACETS.modules.map(f => f[0]) : null,
      facetStateModules: (typeof facetState !== 'undefined') ? facetState.modules : null,
      rowCount: rows.length,
      rowSig: rows.map(tr => ((tr.querySelector('td') || {}).textContent || '').trim()).sort().join('|'),
      bodyLen: (document.querySelector('#view').innerText || '').length,
      body: document.querySelector('#view').innerText || '',
    };
  });
  const clickChip = async (k) => {
    await page.evaluate((kk) => { const b = document.querySelector(`#facets button[data-k="${kk}"]`); if (b) b.click(); }, k);
    // The chip's onclick sets facetState[name] and calls route(name) synchronously (console.html's facet
    // bar wiring) — a reflow flush is enough margin for the DOM to settle, not a guess at fetch latency
    // that does not apply to this purely synchronous re-render.
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  };

  const base = await read();

  // BLANK-PAGE GUARD: every assertion below reads the live view; if it threw, a "does NOT contain" check
  // passes vacuously. Prove it rendered the real registry first.
  check('the live modules view renders the capability registry (not a blank page)',
    base.bodyLen > 300 && /librenms/i.test(base.body) && /capabilities/i.test(base.body),
    `the view rendered ${base.bodyLen} chars / rows=${base.rowCount}; it likely threw and later checks would pass vacuously`);

  // A. Only the DTO-backed chips are rendered. The four that filter on health/scope are withdrawn.
  const chipKeys = base.chips.map(c => c.k);
  check('exactly the two DTO-backed facet chips are rendered (all, off)',
    JSON.stringify(chipKeys) === JSON.stringify(['all', 'off']),
    `rendered chips = ${JSON.stringify(base.chips)} — the live DTO has no health/scope, so live/idle/dark/mutate cannot be honest chips`);
  check('FACETS.modules (the chip source) is narrowed to the DTO-backed set on the live path',
    JSON.stringify(base.facetsModules) === JSON.stringify(['all', 'off']),
    `FACETS.modules = ${JSON.stringify(base.facetsModules)}`);

  // B. None of the four withdrawn dimensions is rendered as a chip.
  const leaked = base.chips.filter(c => WITHDRAWN.includes(c.k) || /^(live|idle|dark|mutate-scope)$/i.test(c.label));
  check('no chip filters on a field the live DTO cannot emit (health / scope)',
    leaked.length === 0, `these chips have no backing field and must not render: ${JSON.stringify(leaked)}`);

  // C. A facetState left pointing at a withdrawn chip is reset, so the view never filters on an absent dim.
  check('a stale facetState on a withdrawn chip ("mutate") is reset to "all"',
    base.facetStateModules === 'all',
    `facetState.modules = ${base.facetStateModules} — a withdrawn facet must not survive as the active filter`);

  // D. The baseline ("all", after the reset) shows the whole declared fleet.
  check('the "all" facet shows every declared capability row',
    base.rowCount === ALL_ROWS, `rows under "all" = ${base.rowCount}, expected ${ALL_ROWS}`);

  // E. THE KILLING ASSERTION: every rendered chip other than "all" is a REAL filter — clicking it changes
  // which live rows are shown. This is exactly what the pre-fix dead-UI failed: it re-rendered the same rows.
  for (const chip of base.chips) {
    if (chip.k === 'all') continue;
    await clickChip(chip.k);
    const r = await read();
    check(`clicking the "${chip.label}" chip actually filters the live rows (not a no-op)`,
      r.rowSig !== base.rowSig && r.rowCount < base.rowCount,
      `rows under "${chip.k}" = ${r.rowCount} (sig ${r.rowSig || '∅'}) is identical to "all" (${base.rowCount}); the chip changed nothing`);
    if (chip.k === 'off') {
      check('the "Disabled" chip filters to exactly the disabled subset',
        r.rowCount === OFF_ROWS, `rows under "off" = ${r.rowCount}, expected ${OFF_ROWS} disabled`);
    }
  }

  // F. Returning to "all" restores the whole fleet (the filter is reversible, not a one-way mutation).
  await clickChip('all');
  const backToAll = await read();
  check('returning to the "all" facet restores every row',
    backToAll.rowCount === ALL_ROWS, `rows back under "all" = ${backToAll.rowCount}, expected ${ALL_ROWS}`);
} finally {
  await browser.close();
}
console.log(failed ? `FAILED (${failed})` : 'PASS');
process.exit(failed ? 1 : 0);
