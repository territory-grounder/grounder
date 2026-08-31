// EVERY VIEW RENDERS — the smoke gate this console did not have.
//
// WHY IT EXISTS, precisely. On 2026-08-02 one line in views.modules threw
// (`liveState.config.keys` on an Array resolves to Array.prototype.keys — a truthy FUNCTION — so a
// `||[]` fallback never fired and .forEach died). The Modules view rendered its header and NOTHING else
// for a full day, on the live deployment, while 61 Playwright oracles stayed green. It was found by a
// human asking for a screenshot.
//
// The 61 suites test what a view SAYS. Not one of them asserted a view renders AT ALL, and that gap is
// self-concealing: a substring assertion like "the page does not claim X" passes perfectly on an empty
// page. So a render failure could only ever be caught by looking.
//
// This walks EVERY registered view and asserts the three things a broken view cannot fake:
//   1. no uncaught JS error while routing to it,
//   2. the main region actually produced content (a floor, not a shape),
//   3. no raw undefined/NaN/[object Object] leaked into the operator's text.
//
// It is deliberately shallow. Depth belongs in the per-view suites; this one exists so that NO view can
// be blank without something going red, including views nobody has written a suite for yet.
//
// NON-VACUITY. Three guards, because a smoke test that silently tests nothing is worse than none:
//   - the view list is read from the page's own `views` registry and must exceed a floor;
//   - routing must actually change the rendered title, or navigation is not happening and every
//     subsequent "it rendered" is measuring the same first screen;
//   - the state is populated by the console's OWN loader (liveAdopt), never hand-set. This one is
//     load-bearing and cost a real iteration: the first version assigned liveState fields by hand, so
//     liveState.config stayed undefined, `undefined && …` short-circuited, and the suite PASSED with the
//     2026-08-02 bug reintroduced — I had rebuilt the exact blind spot I was gating against. A smoke test
//     whose state does not come from the real loader is testing a program that does not exist.

import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;

// The floor a view must clear. Views legitimately differ in size with no data; this catches "nothing at
// all", not "less than I expected".
const MIN_CHARS = 120;
const MIN_VIEWS = 15;

let fail = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) fail++;
};

// REPRESENTATIVE payloads, not empty ones. This distinction cost a second iteration and is the whole
// reason the suite can fail: with `modules: []` the Modules view takes its `if(!mods.length)` early exit
// and never reaches the rendering code, so the 2026-08-02 bug sat two lines below a branch the fixture
// never entered — and the suite passed with the bug reintroduced. An empty fleet does not exercise a
// render path; it exercises the empty state. Every surface a view iterates gets at least one row.
const PAYLOAD = {
  capabilities: { capabilities: [{ surface: 'notifier', source_type: 'matrix', capability: 'notify', enabled: true }] },
  config: { config: [{ name: 'module.notifier.matrix.homeserver', value: 'https://m.example.test', source: 'console' }] },
  'modules/schema': {
    modules: [{
      surface: 'notifier', source_type: 'matrix', title: 'Matrix', summary: 'governance notices',
      enabled: true, enabled_known: true, has_secret: true, test_verb: 'send a test notice',
      fields: [{ name: 'homeserver', env_key: 'TG_MATRIX_HOMESERVER', label: 'Homeserver URL',
        help: 'base URL', type: 'url', security: 'ordinary', effect: 'restart', required: true,
        config_key: 'module.notifier.matrix.homeserver' }],
    }],
    undescribed: [{ package: 'modules/model/ollama', reason: 'provider identity only' }],
  },
  sessions: { sessions: [] },
  alerts: { alerts: [] },
  estate: { nodes: [], edges: [] },
  ledger: { entries: [] },
  decisions: { decisions: [] },
};

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1100 } });
  const page = await ctx.newPage();

  await page.route('**/api/v1/**', r => {
    const path = new URL(r.request().url()).pathname.replace(/^\/api\/v1\//, '');
    for (const [k, v] of Object.entries(PAYLOAD)) {
      if (path === k || path.startsWith(k + '/')) {
        return r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(v) });
      }
    }
    return r.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });

  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => {
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    if (typeof liveState === 'object') liveState.on = true;
  });
  // THE CONSOLE'S OWN LOADER fills liveState from the (stubbed) API — the same code path a served console
  // runs. Hand-setting these fields is what made the first draft of this suite vacuous.
  await page.evaluate(async () => { if (typeof liveAdopt === 'function') await liveAdopt(true); });
  // liveAdopt(true) is already fully awaited above — every in-chain fetch and the post-adopt re-render have
  // resolved by the time page.evaluate() returns — so this is margin for the DOM to settle, not a guess at
  // fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  const loaded = await page.evaluate(() => ({
    config: Array.isArray(liveState.config),
    caps: !!liveState.caps,
  }));
  check('the console\'s own loader populated live state (not hand-set fixtures)',
    loaded.config,
    `liveState.config is not an array after liveAdopt (${JSON.stringify(loaded)}) — the loader did not run, ` +
    'so every view below is rendering against undefined state and a shape bug cannot bite');
  if (!loaded.config) throw new Error('liveAdopt did not populate state — refusing to report a vacuous pass');

  const names = await page.evaluate(() => (typeof views === 'object' && views ? Object.keys(views) : []));
  check(`the view registry is readable and non-trivial (${names.length} views)`,
    names.length >= MIN_VIEWS,
    `read ${names.length} views from the page's own registry, expected >= ${MIN_VIEWS} — the enumeration ` +
    'is broken, so this suite would walk nothing and pass');
  if (names.length < MIN_VIEWS) throw new Error('view enumeration failed — refusing to report a vacuous pass');

  const titles = new Set();
  const blank = [];
  const threw = [];
  const leaked = [];

  for (const name of names) {
    const errs = [];
    const onErr = e => errs.push(String(e).split('\n')[0].slice(0, 160));
    page.on('pageerror', onErr);

    await page.evaluate(n => { if (typeof route === 'function') route(n); }, name);
    // Wait for the EXACT condition the blank-view check below reads, rather than a fixed settle guess. Most
    // views finish this synchronously inside route() itself; a few (modules/skills/wiki) additionally kick
    // off a fire-and-forget fetch (schema / skLoadList / wkLoadIndex) that repaints once it lands — this
    // covers both without needing to special-case which view is which. A view that never clears the floor
    // (a real regression) falls through to the precise assertions below instead of hanging the whole suite
    // for the default 30s per view.
    await page.waitForFunction(min => {
      const main = document.querySelector('main') || document.body;
      return (main.innerText || '').trim().length >= min;
    }, MIN_CHARS, { timeout: 5000 }).catch(() => {});

    const r = await page.evaluate(() => {
      const main = document.querySelector('main') || document.body;
      return {
        title: (document.querySelector('#vTitle')?.textContent || '').trim(),
        chars: (main.innerText || '').trim().length,
        leak: (main.innerText || '').match(/\bundefined\b|\bNaN\b|\[object Object\]/) ? true : false,
      };
    });
    page.off('pageerror', onErr);

    titles.add(r.title);
    if (errs.length) threw.push(`${name}: ${errs[0]}`);
    if (r.chars < MIN_CHARS) blank.push(`${name} (${r.chars} chars)`);
    if (r.leak) leaked.push(name);
  }

  check('routing actually navigates (each view renders its own title)',
    titles.size >= Math.min(names.length, MIN_VIEWS),
    `${names.length} views produced only ${titles.size} distinct titles — navigation is not happening, so ` +
    'every "it rendered" below is measuring the same screen');

  check('no view throws while rendering',
    threw.length === 0,
    `${threw.length} view(s) threw — this is the exact shape of the 2026-08-02 blank Modules page:\n      ` +
    threw.join('\n      '));

  check(`every view renders content (>= ${MIN_CHARS} chars)`,
    blank.length === 0,
    `${blank.length} view(s) rendered essentially nothing: ${blank.join(', ')}`);

  check('no raw undefined / NaN / [object Object] reaches the operator',
    leaked.length === 0,
    `leaked in: ${leaked.join(', ')}`);

  console.log(`\nevery-view-renders: walked ${names.length} views`);
} finally {
  await browser.close();
}

if (fail) { console.log('every-view-renders: FAIL'); process.exit(1); }
console.log('every-view-renders: PASS');
