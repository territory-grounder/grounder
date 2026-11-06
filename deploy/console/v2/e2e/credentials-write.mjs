// Console e2e — the CREDENTIALS write half (TG-109, spec/016 REQ-1615/1618). The grounder never syncs or
// writes a rule itself: Sync now POSTs the worker lane (temporal/credentialsync), native-rule add/delete go
// through the worker's ledger-first nativerule lane, and this surface's job is to carry the worker's own
// words to the operator UNCHANGED. This oracle drives the stubbed /api surface (rule-editor.mjs pattern —
// the REAL render code runs; only the network is canned) and pins the honesty properties, each with a real
// failure mode behind it:
//   1. ★ ONE CLICK, ONE POST: Sync now fires exactly one POST /v1/credentials/sources/{id}/sync — a
//      re-render or a retry loop that double-fires would re-hit a third-party credential system.
//   2. ★ THE STARVED SENTENCE RENDERS VERBATIM: a starved source (synced OK, ZERO bindings) reports 0
//      drift and looks converged everywhere else — only the worker's sentence separates it from success,
//      so the console must show summary AND detail unparaphrased.
//   3. PRECEDENCE HONESTY: a published rank renders as the rank; a projection that predates publication
//      (field absent — omitempty, 0 is not a valid rank) renders 'unpublished', never 0-as-rank.
//   4. ★ NATIVE GET 403 → the honest elevated-read state ('elevated read required — the rows carry
//      credential references') with the step-up affordance — never a generic error, never sample rows.
//   5. ★ NATIVE ADD 400 → the backend validator's text VERBATIM, no success state, exactly one POST.
//   6. NATIVE DELETE of a missing row (ErrNoSuchNativeRule → 404) → 'already removed' + a re-read, and
//      SecretRef strings in listed entries render as REFERENCES (INV-13) — no control resolves a value.
//   7. SYNC 502 → 'could not be run … not a verdict on the source'; SYNC 401 → the SHARED admin step-up
//      opens and NOTHING auto-submits (no second POST without a second deliberate click).
//   8. CONFIGURE opens the EXISTING generated module dialog (modCfgCard) for a schema-publishing
//      connector, and renders 'no dialog (inline/native source)' honestly for native-db.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const SOURCES = [
  { source_id: 'openbao', plane: 'machine', last_synced_at: '2026-08-13T10:00:00Z', added: 2, changed: 1, removed: 0,
    drifted: true, covered_targets: 12, outcome: 'ok', precedence: 10 },
  // NO precedence field at all — the pre-0087 projection shape (omitempty). Must render 'unpublished'.
  { source_id: 'native-db', plane: 'machine', last_synced_at: '2026-08-13T09:00:00Z', added: 0, changed: 0, removed: 0,
    drifted: false, covered_targets: 3, outcome: 'ok' },
];

// the worker's REAL starved wording (temporal/credentialsync SyncSourceActivity) — the sentence under test.
const STARVED = { source_id: 'openbao', ok: true, summary: 'synced OK and produced ZERO host bindings',
  detail: 'the pull worked but this source contributes nothing to resolution — check its configured path/prefix and what its account can actually see',
  added: 0, changed: 0, removed: 0, entries: 0, starved: true, elapsed_ms: 412 };

const NATIVE_ROWS = { rules: [
  { id: 5, entry: 'host:db01|postgres|22|ssh|store:db01.key', rationale: 'db01 maintenance', created_by: 'operator:kyriakosp', created_at: '2026-08-13T10:00:00Z' },
], total: 1 };

// a REAL-shaped /v1/modules/schema page: one schema-publishing credsource connector. native-db is (as in
// the binary) absent from modules[] — that absence is what the 'no dialog' honesty renders.
const SCHEMA = { modules: [
  { surface: 'credsource', source_type: 'openbao', title: 'OpenBao / HashiCorp Vault',
    summary: 'Machine-plane credential source over KV v2.',
    fields: [{ name: 'kv_mount', env_key: 'TG_OPENBAO_KV_MOUNT', config_key: 'module.credsource.openbao.kv_mount',
      label: 'KV v2 mount', type: 'text', security: 'ordinary', effect: 'restart' }],
    has_secret: false, test_verb: 'LIST the configured KV v2 mount and prefix — key names only', enabled: true, enabled_known: false },
], undescribed: [] };

const mkState = () => ({ syncPosts: [], syncReplies: [], addPosts: [], addReplies: [], delReqs: [], delReplies: [],
  nativeGets: 0, nativeReply: null, elevatePosts: 0, pageErrors: [] });

async function mount(page, state) {
  page.on('pageerror', e => state.pageErrors.push(String(e)));
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    const syncM = p.match(/^\/v1\/credentials\/sources\/([^/]+)\/sync$/);
    if (syncM && req.method() === 'POST') {
      state.syncPosts.push(syncM[1]);
      const rep = state.syncReplies.length ? state.syncReplies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: rep && rep.json ? rep.json : STARVED });
    }
    if (p === '/v1/credentials/native/rules' && req.method() === 'POST') {
      let body = null; try { body = req.postDataJSON(); } catch (e) {}
      state.addPosts.push(body);
      const rep = state.addReplies.length ? state.addReplies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: { id: 9, ledger_seq: 77 } });
    }
    const delM = p.match(/^\/v1\/credentials\/native\/rules\/([^/]+)$/);
    if (delM && req.method() === 'DELETE') {
      let body = null; try { body = req.postDataJSON(); } catch (e) {}
      state.delReqs.push({ id: delM[1], body });
      const rep = state.delReplies.length ? state.delReplies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: { id: Number(delM[1]), ledger_seq: 78 } });
    }
    if (p === '/v1/credentials/native') {
      state.nativeGets++;
      const rep = state.nativeReply;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      return route.fulfill({ json: rep && rep.json ? rep.json : NATIVE_ROWS });
    }
    if (p === '/v1/session/elevate' && req.method() === 'POST') {
      state.elevatePosts++;
      return route.fulfill({ json: { admin_until: new Date(Date.now() + 15 * 60000).toISOString() } });
    }
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/credentials/sources') return route.fulfill({ json: { sources: SOURCES } });
    if (p === '/v1/credentials/coverage') return route.fulfill({ json: { window_days: 7, by_plane: [], by_source: [], recent_resolved: [], recent_refused: [] } });
    if (p === '/v1/credentials/resolutions') return route.fulfill({ json: { resolutions: [] } });
    if (p === '/v1/modules/schema') return route.fulfill({ json: SCHEMA });
    if (p === '/v1/config') return route.fulfill({ json: { config: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#credentials', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'credentials'), null, { timeout: 20000 });
  await page.waitForFunction(() => typeof liveState === 'undefined' || !liveState.on || !!liveState.lastRefresh, null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'credentials'); if (a) a.click(); });
  await page.waitForSelector('.cred-actbtn', { timeout: 20000 });
}
const viewText = page => page.evaluate(() => { const v = document.querySelector('.main .view'); return v ? v.innerText : ''; });

const browser = await chromium.launch();
try {
  // 1+2+3+4. ★ Sync now: one POST, the starved sentence VERBATIM; precedence honesty; native 403 honesty.
  {
    const state = mkState();
    state.nativeReply = { status: 403, body: 'trace-read role required' };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);

    // 3. precedence: openbao publishes #10; native-db's absent field renders 'unpublished'.
    const t0 = await viewText(page);
    ok(/#10/.test(t0), 'precedence: the published rank (#10) must render on the openbao row');
    ok(/unpublished/.test(t0), "precedence: an absent (pre-0087, omitempty) rank must render 'unpublished'");
    ok(!/#0\b/.test(t0), 'precedence: 0 must NEVER render as a rank');

    // 4. the native section 403 state, before any click.
    ok(/elevated read required — the rows carry credential references/.test(t0),
      'native 403: the honest elevated-read sentence must render');
    ok(await page.$('#credNatElevate') !== null, 'native 403: the step-up affordance must be present');
    ok(!/db01 maintenance/.test(t0), 'native 403: NO rule row may render for a refused read');

    // 1+2. Sync now on openbao → exactly one POST; the starved outcome renders verbatim.
    await page.click('button[aria-label="Sync openbao now"]');
    await page.waitForSelector('.cred-syncrow', { timeout: 10000 });
    await page.waitForFunction(() => /ZERO host bindings/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t1 = await viewText(page);
    ok(state.syncPosts.length === 1, `sync: exactly one POST per click, got ${state.syncPosts.length}`);
    ok(state.syncPosts[0] === 'openbao', `sync: the POST must target the clicked source, got ${JSON.stringify(state.syncPosts)}`);
    ok(/synced OK and produced ZERO host bindings/.test(t1), 'sync: the starved SUMMARY must render verbatim');
    ok(/contributes nothing to resolution — check its configured path\/prefix/.test(t1), 'sync: the starved DETAIL must render verbatim');
    ok(/SYNCED · STARVED/.test(t1), 'sync: a starved run must NOT wear the plain success chip');
    ok(state.pageErrors.length === 0, 'sync/starved: no uncaught JS errors: ' + state.pageErrors.join(' | '));

    // the elevate affordance drives the SHARED step-up modal (not a bespoke one).
    await page.click('#credNatElevate');
    await page.waitForSelector('#cfgElevate', { timeout: 10000 });
    ok(await page.$('#cfgElevate #cfgAdmName') !== null, 'native 403: the SHARED admin step-up modal must open');
    await page.context().close();
  }

  // 5+6. ★ Native rows render references; add 400 renders the validator VERBATIM; delete 404 = 'already removed'.
  {
    const state = mkState();
    state.nativeReply = { json: NATIVE_ROWS };
    state.addReplies = [{ status: 400, body: 'refused: malformed native rule — credential: rule "host:broken|root": need at least kind:pattern|user|port|scheme' }];
    state.delReplies = [{ status: 404, body: 'no such native rule' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    await page.waitForFunction(() => /db01 maintenance/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t0 = await viewText(page);
    ok(/store:db01\.key/.test(t0), 'native rows: the SecretRef renders as its REFERENCE string (INV-13)');

    // add → 400: the validator text verbatim, no success state, exactly one POST.
    await page.fill('#credNatEntry', 'host:broken|root');
    await page.fill('#credNatRationale', 'r');
    await page.click('#credNatAdd');
    await page.waitForFunction(() => /need at least kind:pattern/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t1 = await viewText(page);
    ok(/refused: malformed native rule — credential: rule "host:broken\|root": need at least kind:pattern\|user\|port\|scheme/.test(t1),
      'native add 400: the backend validator text must render VERBATIM');
    ok(!/added — row/.test(t1), 'native add 400: NO success state may render on a refusal');
    ok(state.addPosts.length === 1, `native add: exactly one POST, got ${state.addPosts.length}`);
    ok(state.addPosts[0] && state.addPosts[0].entry === 'host:broken|root' && state.addPosts[0].rationale === 'r',
      'native add: the POST must carry the typed entry + rationale');

    // delete → rationale prompt is mandatory; a 404 (ErrNoSuchNativeRule) renders 'already removed' + re-reads.
    const getsBefore = state.nativeGets;
    await page.click('button[aria-label="Delete rule 5"]');
    await page.waitForSelector('#credNatDelRat', { timeout: 10000 });
    await page.click('.cred-natdel button'); // CONFIRM with an EMPTY rationale → refused client-side, no request
    await page.waitForFunction(() => /a rationale is required/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    ok(state.delReqs.length === 0, 'native delete: an empty rationale must fire NO request');
    await page.fill('#credNatDelRat', 'host retired');
    await page.click('.cred-natdel button');
    await page.waitForFunction(() => /already removed/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    ok(state.delReqs.length === 1 && state.delReqs[0].id === '5', `native delete: exactly one DELETE for row 5, got ${JSON.stringify(state.delReqs)}`);
    ok(state.delReqs[0].body && state.delReqs[0].body.rationale === 'host retired', 'native delete: the rationale must be carried in the body');
    ok(state.nativeGets > getsBefore, `native delete 404: the list must RE-READ (gets ${getsBefore}→${state.nativeGets})`);
    ok(state.pageErrors.length === 0, 'native rows: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 7. Sync 502 → not-a-verdict honesty; sync 401 → the SHARED step-up, nothing auto-submits.
  {
    const state = mkState();
    state.nativeReply = { status: 403, body: 'trace-read role required' };
    state.syncReplies = [{ status: 502, body: 'the sync could not be run' }, { status: 401, body: 'unauthenticated' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);
    await page.click('button[aria-label="Sync openbao now"]');
    await page.waitForFunction(() => /not a verdict on the source/.test(document.querySelector('.main .view').innerText), null, { timeout: 10000 });
    const t0 = await viewText(page);
    ok(/could not be run/.test(t0) && /not a verdict on the source/.test(t0),
      "sync 502: the 'could not be run — not a verdict on the source' honesty must render");
    ok(!/SYNC FAILED/.test(t0), 'sync 502: could-not-run must NOT wear the sync-failed verdict chip');

    await page.click('button[aria-label="Sync openbao now"]');
    await page.waitForSelector('#cfgElevate', { timeout: 10000 });
    ok(state.syncPosts.length === 2, `sync 401: two deliberate clicks ⇒ two POSTs, got ${state.syncPosts.length}`);
    // complete the SHARED step-up; nothing may auto-submit a third POST.
    await page.fill('#cfgAdmName', 'admin');
    await page.fill('#cfgAdmTok', 'break-glass-token');
    await page.click('#cfgElevate button[type=submit]');
    await page.waitForFunction(() => !document.querySelector('#cfgElevate'), null, { timeout: 10000 });
    await page.waitForTimeout(300); // Class-3 measurement window: intentional fixed wait, MUST NOT become a
    // condition-wait (proves completing the SHARED step-up does NOT auto-fire a second sync POST — there is
    // no DOM event for "nothing happened" to wait on instead)
    ok(state.elevatePosts === 1, `sync 401: the SHARED step-up must be driven exactly once, got ${state.elevatePosts}`);
    ok(state.syncPosts.length === 2, `sync 401: elevation must NOT auto-fire a sync (still 2 POSTs), got ${state.syncPosts.length}`);
    ok(state.pageErrors.length === 0, 'sync 502/401: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 8. Configure: the EXISTING generated dialog for a schema-publishing connector; honest absence for native-db.
  {
    const state = mkState();
    state.nativeReply = { status: 403, body: 'trace-read role required' };
    const page = await (await browser.newContext()).newPage();
    await mount(page, state);

    await page.click('button[aria-label="Configure native-db"]');
    await page.waitForSelector('#credCfgDlg', { timeout: 10000 });
    await page.waitForFunction(() => /no dialog \(inline\/native source\)/.test(document.querySelector('#credCfgBody').innerText), null, { timeout: 10000 });
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('#credCfgDlg'), null, { timeout: 10000 });

    await page.click('button[aria-label="Configure openbao"]');
    await page.waitForSelector('#credCfgDlg', { timeout: 10000 });
    await page.waitForFunction(() => /OpenBao \/ HashiCorp Vault/.test(document.querySelector('#credCfgBody').innerText), null, { timeout: 10000 });
    const dlg = await page.evaluate(() => document.querySelector('#credCfgBody').innerText);
    ok(/KV v2 mount/.test(dlg), 'configure: the generated dialog must render the schema fields (modCfgCard, not a bespoke form)');
    ok(/TEST/.test(dlg) || (await page.$('#credCfgBody button')) !== null, 'configure: the dialog must carry the module TEST affordance');
    ok(state.pageErrors.length === 0, 'configure: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('credentials-write FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('credentials-write: OK');
