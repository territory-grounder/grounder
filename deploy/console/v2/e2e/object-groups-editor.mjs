// Console e2e — the OBJECT GROUPS editor (TG-481, spec/016 REQ per cmd/grounder/object_group.go +
// core/httpapi/object_groups.go). The API (GET /v1/estate/groups, POST /v1/estate/groups/entries,
// DELETE /v1/estate/groups/entries/{id}) shipped with no console UI — this oracle drives the stubbed
// /api surface (the credentials-write.mjs pattern — the REAL render code runs; only the network is
// canned) and pins the honesty properties this view exists to guarantee, each with a real failure mode
// behind it:
//   1. ★ REAL DATA RENDERS, NOT A FIXTURE: this module ships none (see modules/objectgroups/fixtures.txt)
//      — the only way a group name or pattern reaches the screen is the live GET, so asserting the
//      mocked values render VERBATIM is proof the wiring is real, not coincidental.
//   2. ★ GET 403 → the honest elevated-read state ('elevated read required — object-group membership
//      reveals actuation-policy structure') with the step-up affordance — never a generic error, never
//      sample rows (mirrors credentials/native exactly; GET /v1/estate/groups is AuthTraceRead too).
//   3. ★ ADD validates client-side (name + >=1 pattern + rationale) before ever posting; a 400 renders the
//      backend validator's text VERBATIM, no success state; a valid submit posts the EXACT payload shape
//      {name, patterns:[...], precedence:"union", rationale} — precedence is fixed, never free-typed,
//      because validPrecedence() accepts only "union" today (temporal/objectgroup/objectgroup.go).
//   4. DELETE requires a rationale (taking a group back is a decision, not a cleanup) — an empty CONFIRM
//      fires no request; a 404 (ErrNoSuchObjectGroup) renders 'already removed' + a re-read.
//   5. A 401 on a write drives the SHARED admin step-up modal (#cfgElevate), never a bespoke one.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// Distinctive "-e2e" markers so a match can ONLY have come from this mock — this view has no fixture to
// coincidentally agree with.
const GROUPS_PAGE = { groups: [
  { id: 3, name: 'gpu-fleet-e2e-marker', patterns: ['dc1gpu*-e2e', '*.gpu.e2e.test'], precedence: 'union',
    created_by: 'operator:kyriakosp', created_at: '2026-08-18T10:00:00Z' },
  { id: 4, name: 'edge-routers-e2e', patterns: ['*fw01-e2e'], precedence: 'union',
    created_by: 'operator:kyriakosp', created_at: '2026-08-17T09:00:00Z' },
], total: 2 };

const mkState = () => ({ listGets: 0, listReply: null, addPosts: [], addReplies: [], delReqs: [], delReplies: [],
  elevatePosts: 0, pageErrors: [] });

async function mount(page, state) {
  page.on('pageerror', e => state.pageErrors.push(String(e)));
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    if (p === '/v1/estate/groups' && req.method() === 'GET') {
      state.listGets++;
      const rep = state.listReply;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: (rep && rep.json) ? rep.json : GROUPS_PAGE });
    }
    if (p === '/v1/estate/groups/entries' && req.method() === 'POST') {
      let body = null; try { body = req.postDataJSON(); } catch (e) {}
      state.addPosts.push(body);
      const rep = state.addReplies.length ? state.addReplies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: { id: 9, ledger_seq: 77 } });
    }
    const delM = p.match(/^\/v1\/estate\/groups\/entries\/([^/]+)$/);
    if (delM && req.method() === 'DELETE') {
      let body = null; try { body = req.postDataJSON(); } catch (e) {}
      state.delReqs.push({ id: delM[1], body });
      const rep = state.delReplies.length ? state.delReplies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: { id: Number(delM[1]), ledger_seq: 78 } });
    }
    if (p === '/v1/session/elevate' && req.method() === 'POST') {
      state.elevatePosts++;
      return route.fulfill({ json: { admin_until: new Date(Date.now() + 15 * 60000).toISOString() } });
    }
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#objectgroups', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'objectgroups'), null, { timeout: 20000 });
  await page.waitForFunction(() => typeof liveState === 'undefined' || !liveState.on || !!liveState.lastRefresh, null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'objectgroups'); if (a) a.click(); });
}
const viewText = page => page.evaluate(() => { const v = document.querySelector('.main .view'); return v ? v.innerText : ''; });

const browser = await chromium.launch();
try {
  // 2. ★ GET 403 → the honest elevated-read state, never sample rows; the affordance opens the SHARED step-up.
  {
    const state = mkState();
    state.listReply = { status: 403, body: 'trace-read role required' };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    await page.waitForFunction(() => /Elevated read required/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t0 = await viewText(page);
    ok(/elevated read required — object-group membership reveals actuation-policy structure/.test(t0),
      '403: the honest elevated-read sentence must render');
    ok(await page.$('#ogElevate') !== null, '403: the step-up affordance must be present');
    ok(!/gpu-fleet-e2e-marker/.test(t0), '403: NO group row may render for a refused read');
    ok(state.listGets === 1, `403: exactly one GET, got ${state.listGets}`);

    await page.click('#ogElevate');
    await page.waitForSelector('#cfgElevate', { timeout: 10000 });
    ok(await page.$('#cfgElevate #cfgAdmName') !== null, '403: the SHARED admin step-up modal must open (not a bespoke one)');
    ok(state.pageErrors.length === 0, '403: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 1. ★ real data renders (this module ships no fixture) + full DELETE flow.
  {
    const state = mkState();
    state.listReply = { json: GROUPS_PAGE };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    await page.waitForFunction(() => /gpu-fleet-e2e-marker/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t0 = await viewText(page);
    ok(/gpu-fleet-e2e-marker/.test(t0) && /edge-routers-e2e/.test(t0), 'list: both mocked group names must render');
    ok(/dc1gpu\*-e2e/.test(t0) && /\*\.gpu\.e2e\.test/.test(t0), 'list: every mocked host-glob pattern must render VERBATIM');
    ok(/operator:kyriakosp/.test(t0), 'list: the creator must render');
    ok(/union/.test(t0), 'list: the precedence must render');
    ok(!/undefined/.test(t0) && !/NaN/.test(t0) && !/\[object Object\]/.test(t0), 'list: no raw undefined/NaN/[object Object] leaked');

    // delete: an empty CONFIRM fires no request; a rationale-bearing CONFIRM fires exactly one DELETE.
    await page.click('button[aria-label="Delete group gpu-fleet-e2e-marker"]');
    await page.waitForSelector('#ogDelRat', { timeout: 10000 });
    await page.click('.og-deldiv button:has-text("CONFIRM DELETE")');
    await page.waitForFunction(() => /a rationale is required/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    ok(state.delReqs.length === 0, 'delete: an empty rationale must fire NO request');

    const getsBefore = state.listGets;
    await page.fill('#ogDelRat', 'gpu fleet decommissioned');
    await page.click('.og-deldiv button:has-text("CONFIRM DELETE")');
    await page.waitForFunction(() => /removed — row 3/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    ok(state.delReqs.length === 1 && state.delReqs[0].id === '3', `delete: exactly one DELETE for row 3, got ${JSON.stringify(state.delReqs)}`);
    ok(state.delReqs[0].body && state.delReqs[0].body.rationale === 'gpu fleet decommissioned', 'delete: the rationale must be carried in the body');
    ok(state.listGets > getsBefore, `delete: the list must RE-READ after a successful delete (gets ${getsBefore}→${state.listGets})`);

    // delete of a SECOND row, now simulating the row already having been removed elsewhere (404).
    const getsBefore2 = state.listGets;
    state.delReplies = [{ status: 404, body: 'no such object group' }];
    await page.click('button[aria-label="Delete group edge-routers-e2e"]');
    await page.waitForSelector('#ogDelRat', { timeout: 10000 });
    await page.fill('#ogDelRat', 'stale row');
    await page.click('.og-deldiv button:has-text("CONFIRM DELETE")');
    await page.waitForFunction(() => /already removed/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    ok(state.delReqs.length === 2 && state.delReqs[1].id === '4', `delete 404: exactly one DELETE for row 4, got ${JSON.stringify(state.delReqs)}`);
    ok(state.listGets > getsBefore2, `delete 404: the list must RE-READ too (gets ${getsBefore2}→${state.listGets})`);
    ok(state.pageErrors.length === 0, 'list/delete: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 3. ★ ADD: client-side refusal fires no POST; a 400 renders the validator VERBATIM; a valid submit
  //    posts the EXACT payload shape, comma-split and trimmed, with precedence fixed to "union".
  {
    const state = mkState();
    state.listReply = { json: GROUPS_PAGE };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    await page.waitForFunction(() => /gpu-fleet-e2e-marker/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });

    // empty form: refused client-side, no request.
    await page.click('#ogAdd');
    await page.waitForFunction(() => /name, at least one pattern, and a rationale are all required/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    ok(state.addPosts.length === 0, 'add: an empty form must fire NO POST');

    // a filled form, multi-pattern, comma+space separated — the backend refuses it (400), text VERBATIM.
    state.addReplies = [{ status: 400, body: 'refused: object group refused: objectgroup: invalid object-group: at least one non-empty host-glob pattern required' }];
    await page.fill('#ogName', 'test-group-e2e');
    await page.fill('#ogPatterns', 'dc1test*-e2e, *.test.e2e.example');
    await page.fill('#ogRationale', 'exercising the 400 path');
    await page.click('#ogAdd');
    await page.waitForFunction(() => /at least one non-empty host-glob pattern required/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t1 = await viewText(page);
    ok(/refused: object group refused: objectgroup: invalid object-group: at least one non-empty host-glob pattern required/.test(t1),
      'add 400: the backend validator text must render VERBATIM');
    ok(!/added — row/.test(t1), 'add 400: NO success state may render on a refusal');
    ok(state.addPosts.length === 1, `add: exactly one POST for the 400 case, got ${state.addPosts.length}`);
    ok(state.addPosts[0] && state.addPosts[0].name === 'test-group-e2e', 'add: the POST must carry the typed name');
    ok(state.addPosts[0] && JSON.stringify(state.addPosts[0].patterns) === JSON.stringify(['dc1test*-e2e', '*.test.e2e.example']),
      `add: patterns must be comma-split and trimmed, got ${JSON.stringify(state.addPosts[0] && state.addPosts[0].patterns)}`);
    ok(state.addPosts[0] && state.addPosts[0].precedence === 'union', 'add: precedence must always be the fixed value "union", never free-typed');
    ok(state.addPosts[0] && state.addPosts[0].rationale === 'exercising the 400 path', 'add: the POST must carry the typed rationale');

    // the form survives the refusal (nothing is cleared) — retry, now the mock accepts it (default reply).
    const getsBefore = state.listGets;
    await page.click('#ogAdd');
    await page.waitForFunction(() => /added — row 9/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t2 = await viewText(page);
    ok(/added — row 9 · ledger seq 77/.test(t2), 'add: a successful write must render the committed row id + ledger seq');
    ok(state.addPosts.length === 2, `add: the retry is a SECOND deliberate POST, got ${state.addPosts.length}`);
    ok(state.listGets > getsBefore, `add: the list must RE-READ after a successful add (gets ${getsBefore}→${state.listGets})`);
    ok(state.pageErrors.length === 0, 'add: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 5. A 401 on a write drives the SHARED admin step-up modal, never a bespoke one.
  {
    const state = mkState();
    state.listReply = { json: GROUPS_PAGE };
    state.addReplies = [{ status: 401, body: 'unauthenticated' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    await page.waitForFunction(() => /gpu-fleet-e2e-marker/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    await page.fill('#ogName', 'needs-elevation-e2e');
    await page.fill('#ogPatterns', 'dc1needs-e2e*');
    await page.fill('#ogRationale', 'exercising the 401 path');
    await page.click('#ogAdd');
    await page.waitForSelector('#cfgElevate', { timeout: 10000 });
    ok(await page.$('#cfgElevate #cfgAdmName') !== null, '401: the SHARED admin step-up modal must open');
    ok(state.addPosts.length === 1, `401: exactly one deliberate POST before the step-up, got ${state.addPosts.length}`);
    ok(state.pageErrors.length === 0, '401: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('object-groups-editor FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('object-groups-editor: OK');
