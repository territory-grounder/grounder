// manifest-elevate.mjs — the TG-669 step-up path on the world-model review surface, end to end in a browser.
//
// WHY A SEPARATE ORACLE. manifest-review.mjs proves the surface renders and offers only the closed verbs. It
// never clicks one. After TG-669 the three verbs are ADMIN-tier writes: a plain session gets 401, the module
// must drive the SHARED step-up modal (cfgElevateOverlay, the same one config/secret/mode writes use) and then
// re-submit the operator's OWN rationale — not re-prompt, not drop the click, not loop. That behaviour exists
// only in deploy/console/v2/modules/manifest/js.txt and is reachable only through the assembled index.html, so
// a Go test cannot see it and a review of the JS proves nothing about what the browser does. This does.
//
//   1. adopt → 401 ⇒ the step-up modal (#cfgElevate) opens; NO error text claims the write failed, and the
//      backend saw exactly one POST (the refused one).
//   2. the operator steps up (POST /v1/session/elevate 200) ⇒ the SAME verb is re-submitted with the SAME
//      rationale, once, and the ledger note renders.
//   3. a 403 is terminal: the modal must NOT open and the honest refusal text renders.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const RATIONALE = 'mealie is ours and TG may restart it';
const entries = [
  { id: 1, entity_type: 'service', name: 'mealie.service', host: 'dc1mealie01', source: 'declared',
    confidence: 0.85, status: 'draft', materializes: true, allowlist_kind: 'unit',
    last_seen_at: '2026-07-31T12:00:00Z', caller_can_act: true },
];

// mount wires the fake control plane. `state` is mutated by the test to drive the write's answer and to
// record what the browser actually sent.
async function mount(page, state) {
  page.on('dialog', d => d.accept(RATIONALE)); // the rationale prompt: a real answer, typed once
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/manifest') return route.fulfill({ json: { entries, drafts: 1, total: 1, caller_can_act: true } });
    if (p === '/v1/session/elevate' && req.method() === 'POST') {
      state.elevations++;
      state.elevated = true;
      return route.fulfill({ json: { admin_until: '2099-01-01T00:00:00Z' } });
    }
    if (p.startsWith('/v1/manifest/entries/') && req.method() === 'POST') {
      state.posts.push({ path: p, body: req.postData() || '' });
      if (state.writeStatus === 403) return route.fulfill({ status: 403, body: 'cross-origin write refused' });
      if (!state.elevated) return route.fulfill({ status: 401, body: 'unauthenticated' });
      return route.fulfill({ json: { id: 1, name: 'mealie.service', status: 'approved', ledger_seq: 41 } });
    }
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#manifest', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(
    () => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'manifest'),
    null, { timeout: 20000 });
  await page.waitForFunction(
    () => typeof liveState === 'undefined' || !liveState.on || !!liveState.lastRefresh,
    null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'manifest'); if (a) a.click(); });
  await page.waitForFunction(() => {
    const v = document.querySelector('#view');
    if (!v) return false;
    const t = v.innerText || '';
    return t.trim().length > 0 && !/Loading the reviewable world model/i.test(t);
  }, null, { timeout: 20000 });
}

const viewText = (page) => page.evaluate(() => document.querySelector('#view')?.innerText || '');
const clickAdopt = (page) => page.evaluate(() => {
  const b = [...document.querySelectorAll('#view [data-act="adopt"]')][0];
  if (!b) return false;
  b.click();
  return true;
});

const browser = await chromium.launch();
try {
  // 1 + 2. plain session → 401 → the shared step-up modal → elevated → the same verb re-submitted once.
  {
    const state = { elevated: false, elevations: 0, posts: [], writeStatus: 401 };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    ok(await clickAdopt(page), 'adopt: the review surface must offer an enabled adopt control');
    await page.waitForSelector('#cfgElevate', { timeout: 20000 });
    ok(state.posts.length === 1, `401: exactly one refused POST must have gone out before the modal, got ${state.posts.length}`);
    ok(state.posts[0] && state.posts[0].body.includes(RATIONALE), 'the typed rationale must be in the refused POST');
    const beforeText = await viewText(page);
    ok(!/write failed|refused by the control plane/i.test(beforeText), '401: the view must not claim the write failed while the step-up is pending');

    await page.fill('#cfgAdmName', 'root-admin');
    await page.fill('#cfgAdmTok', 'admin-token-for-the-oracle');
    await page.evaluate(() => document.querySelector('#cfgElevForm').requestSubmit());
    await page.waitForFunction(() => !document.querySelector('#cfgElevate'), null, { timeout: 20000 });
    await page.waitForFunction(() => /recorded on the ledger/i.test(document.querySelector('#view')?.innerText || ''), null, { timeout: 20000 });
    ok(state.elevations === 1, `step-up: exactly one elevation call, got ${state.elevations}`);
    ok(state.posts.length === 2, `re-submit: exactly one more POST after the step-up (two in all), got ${state.posts.length}`);
    ok(state.posts.every(p => p.path === '/v1/manifest/entries/1/adopt'), `re-submit: the SAME verb on the SAME entry, got ${JSON.stringify(state.posts.map(p => p.path))}`);
    ok(state.posts[1] && state.posts[1].body.includes(RATIONALE), 're-submit: the operator\'s own rationale, not a re-prompt');
    const text = await viewText(page);
    ok(/adopted mealie\.service/i.test(text), `success: the ledger note must render, got ${JSON.stringify(text.slice(0, 200))}`);
    await page.context().close();
  }
  // 3. a 403 is terminal — no modal, an honest refusal, one POST.
  {
    const state = { elevated: false, elevations: 0, posts: [], writeStatus: 403 };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    ok(await clickAdopt(page), '403: the adopt control must exist');
    await page.waitForFunction(() => /refused by the control plane/i.test(document.querySelector('#view')?.innerText || ''), null, { timeout: 20000 });
    const modal = await page.evaluate(() => !!document.querySelector('#cfgElevate'));
    ok(!modal, '403: the step-up modal must NOT open — elevation cannot fix a terminal refusal');
    ok(state.elevations === 0 && state.posts.length === 1, `403: no elevation and exactly one POST, got elevations=${state.elevations} posts=${state.posts.length}`);
    const text = await viewText(page);
    ok(/cross-origin write refused/.test(text), '403: the server\'s own reason must reach the operator');
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('manifest-elevate FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('manifest-elevate: OK');
