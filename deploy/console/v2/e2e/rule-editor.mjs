// Console e2e — the POLICY rules-as-data EDITOR (TG-104 slice-2). The grounder NEVER writes the ruleset itself
// (spec/015 REQ-1503): this UI reads the live rules, edits them as ordered cards, composes the FULL replacement
// document and POSTs {document, expected_version, rationale} to /api/v1/policy/ruleset, where the worker
// (core/policy.ParseRuleSet the authority) validates, compare-and-swaps, ledgers and applies it. This oracle
// drives the stubbed /api surface and asserts the surface is honest AND — the load-bearing safety property —
// that a read→edit→write round-trip DROPS NOTHING (a dropped match dimension or default is how a deny silently
// becomes an allow). Eight properties, each with a real failure mode behind it:
//   1. ★ IDENTITY ROUND-TRIP (the killing test): load the SHIPPED core/policy/default_ruleset.json, open the
//      editor, Save WITHOUT editing, and assert the posted document is SEMANTICALLY EQUAL to the original —
//      same rules, same order, same verdict/op_class/reversible/min_confidence(0!). Goes RED if any field is
//      dropped. expected_version equals the version the read pinned.
//   1b. ★ SELECTOR-KIND + EVERY-FIELD ROUND-TRIP: a fixture bearing every hyphenated selector kind + argv /
//      territory / band / rate / approve_by / a default-flagged rule; Save unedited; assert each read kind maps
//      back to its UNDERSCORE write key and every param nests under `params`. Goes RED if a kind is mis-mapped
//      (host-glob→host) or a field is dropped.
//   2. EDIT ONE VERDICT: change one rule's verdict, Save; ONLY that field changes and expected_version is pinned.
//   3. ADD / DELETE / MOVE reflected in the posted document ORDER (order is deny-overrides-significant).
//   4. 400 → the backend's VERBATIM validator text is shown, with NO success state.
//   5. 409 → the honest "changed underneath you" state + a Re-read that REFETCHES and resets — NEVER a silent
//      overwrite (exactly one POST).
//   6. 401 → drives the SHARED admin step-up, then completes the write.
//   7. has_default=true → the editor FAILS CLOSED: it refuses to edit/save and fires NO POST.
//   8. has_default ABSENT (a mixed-version / rolling-deploy skew where this console briefly talks to a
//      pre-slice-2 backend that never emitted the field) → the editor FAILS CLOSED on the UNKNOWN: blocked,
//      no cards, no Save, and NO POST — a SAFETY guard must never read an absent field as safe-to-edit.
import { chromium } from 'playwright';
import { readFile } from 'node:fs/promises';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const SHADOW = { mode: 'Shadow', may_auto_actuate: false, requires_human_vote: false, persisted: true,
  posture: 'Shadow — read-only: suggest and record only, never actuate.' };

// ---- the read→write reference maps (the SAME closed sets the editor + core/policy use) --------------------
// projectToRead mirrors cmd/grounder.projectPolicyRule: params flatten to the item top-level, and the estate
// selector renders "<kind>:<pattern>" with the credential.SelectorKind spelling (HYPHENATED host-glob /
// device-class). canonRule normalizes a WRITE-schema rule to its SEMANTIC field set (what ParseRuleSet keeps),
// so "op_class omitted" and "op_class:''" compare equal while a genuinely dropped field does not.
const SEL_KIND_READ = { host: 'host', host_glob: 'host-glob', group: 'group', device_class: 'device-class', resource: 'resource' };
const ESTATE_KEYS = ['host', 'host_glob', 'group', 'device_class', 'resource'];
function projectToRead(rule) {
  const m = rule.match || {}, out = { id: rule.id, verdict: rule.verdict }, rm = {};
  for (const k of ESTATE_KEYS) if (m[k]) { rm.selector = SEL_KIND_READ[k] + ':' + m[k]; break; }
  if (m.op_class) rm.op_class = m.op_class;
  if (m.argv_pattern) rm.argv_pattern = m.argv_pattern;
  if (m.territory) rm.territory = m.territory;
  if (m.reversible != null) rm.reversible = m.reversible;
  if (m.inverse_only != null) rm.inverse_only = m.inverse_only;
  out.match = rm;
  const p = rule.params || {};
  if (p.min_confidence != null) out.min_confidence = p.min_confidence;
  if (p.band_mode) out.band_mode = p.band_mode;
  if (p.rate_limit != null) out.rate_limit = p.rate_limit;
  if (rule.approve_by && rule.approve_by.length) out.approve_by = rule.approve_by.slice();
  if (rule.is_default) out.is_default = true;
  return out;
}
function canonRule(r) {
  const m = r.match || {}, p = r.params || {}, out = { id: r.id, verdict: r.verdict };
  for (const k of ESTATE_KEYS) if (m[k]) out['sel_' + k] = m[k];
  if (m.op_class) out.op_class = m.op_class;
  if (m.argv_pattern) out.argv_pattern = m.argv_pattern;
  if (m.territory) out.territory = m.territory;
  if (m.reversible != null) out.reversible = m.reversible;
  if (m.inverse_only != null) out.inverse_only = m.inverse_only;
  if (p.min_confidence != null) out.min_confidence = p.min_confidence;
  if (p.band_mode) out.band_mode = p.band_mode;
  if (p.rate_limit != null) out.rate_limit = p.rate_limit;
  if (r.approve_by && r.approve_by.length) out.approve_by = r.approve_by.slice();
  if (r.is_default) out.is_default = true;
  return out;
}
const canonDoc = doc => JSON.stringify({ rules: (doc.rules || []).map(canonRule), default: doc.default || null });
const mkReadPage = (rules, { version = 'bundle-v1', has_default = false } = {}) =>
  ({ present: true, rule_count: rules.length, updated_by: 'admin:test', updated_at: '2026-08-12T00:00:00Z',
     version, has_default, rules });

// the SHIPPED canonical ruleset (relative to THIS test file, CWD-independent).
const SHIPPED = JSON.parse(await readFile(new URL('../../../../core/policy/default_ruleset.json', import.meta.url), 'utf8'));

const mkState = () => ({ posts: [], replies: [], elevatePosts: 0, rulesGets: 0, pageErrors: [] });

async function mount(page, { rulesPage, state }) {
  state.rulesPage = rulesPage;
  page.on('pageerror', e => state.pageErrors.push(String(e)));
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    if (p === '/v1/policy/ruleset' && req.method() === 'POST') {
      let body = null; try { body = req.postDataJSON(); } catch (e) {}
      state.posts.push(body);
      const rep = state.replies.length ? state.replies.shift() : null;
      if (rep && rep.status) return route.fulfill({ status: rep.status, body: rep.body || '' });
      const n = (body && body.document && body.document.rules) ? body.document.rules.length : 0;
      return route.fulfill({ json: { version: 'bundle-v-new', rule_count: n, updated_by: 'admin:test', ledger_seq: 42 } });
    }
    if (p === '/v1/session/elevate' && req.method() === 'POST') {
      state.elevatePosts++;
      return route.fulfill({ json: { admin_until: new Date(Date.now() + 15 * 60000).toISOString() } });
    }
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/policy/mode') return route.fulfill({ json: SHADOW });
    if (p === '/v1/policy/rules') { state.rulesGets++; return route.fulfill({ json: state.rulesPage }); }
    if (p === '/v1/policy/graduation') return route.fulfill({ json: { classes: [] } });
    if (p === '/v1/policy/decisions') return route.fulfill({ json: { decisions: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#policy', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'policy'), null, { timeout: 20000 });
  await page.waitForFunction(() => typeof liveState === 'undefined' || !liveState.on || !!liveState.lastRefresh, null, { timeout: 20000 });
  await page.evaluate(() => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === 'policy'); if (a) a.click(); });
  await page.waitForSelector('.pol-editbtn', { timeout: 20000 });
}
async function openEditor(page) { await page.click('.pol-editbtn'); await page.waitForSelector('#polEditDlg', { timeout: 10000 }); }
async function saveWith(page, rationale) {
  await page.waitForSelector('#polEditSave', { timeout: 10000 });
  await page.fill('#polEditRationale', rationale);
  await page.click('#polEditSave');
}
async function elevate(page) {
  await page.waitForSelector('#cfgElevate #cfgAdmName', { timeout: 10000 });
  await page.fill('#cfgAdmName', 'admin');
  await page.fill('#cfgAdmTok', 'break-glass-token');
  await page.click('#cfgElevate button[type=submit]');
}
const bodyText = page => page.evaluate(() => { const b = document.querySelector('#polEditBody'); return b ? b.innerText : ''; });

const browser = await chromium.launch();
try {
  // 1. ★ IDENTITY ROUND-TRIP on the SHIPPED ruleset — Save unedited ⇒ posted document ≡ original (nothing dropped).
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    const readPage = mkReadPage(SHIPPED.rules.map(projectToRead), { version: 'bundle-shipped' });
    await mount(page, { rulesPage: readPage, state });
    await openEditor(page);
    await saveWith(page, 'no-op round-trip — must not mutate the ruleset');
    await page.waitForSelector('#polEditDone', { timeout: 10000 });
    ok(state.posts.length === 1, `identity: exactly one POST, got ${state.posts.length}`);
    const post = state.posts[0] || {};
    ok(post.expected_version === 'bundle-shipped', `identity: expected_version must pin the read version, got ${JSON.stringify(post.expected_version)}`);
    ok(post.document && canonDoc(post.document) === canonDoc(SHIPPED),
      'identity: the posted document must be SEMANTICALLY EQUAL to the shipped ruleset (no field dropped)\n     shipped=' + canonDoc(SHIPPED) + '\n     posted =' + (post.document ? canonDoc(post.document) : 'null'));
    // explicit: the min_confidence:0 floor and reversible:true survive as SET values (falsy-drop is the classic bug).
    const r0 = post.document && post.document.rules && post.document.rules[0];
    ok(r0 && r0.params && r0.params.min_confidence === 0, `identity: min_confidence:0 must survive as a set value, got ${JSON.stringify(r0 && r0.params)}`);
    ok(r0 && r0.match && r0.match.reversible === true, `identity: reversible:true must survive, got ${JSON.stringify(r0 && r0.match)}`);
    ok(state.pageErrors.length === 0, 'identity: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 1b. ★ SELECTOR-KIND + EVERY-FIELD ROUND-TRIP — every hyphenated kind maps back to its underscore write key.
  const RICH_READ = [
    { id: 'host-rule', verdict: 'deny', match: { selector: 'host:librespeed01', argv_pattern: 'rm -rf', reversible: false }, min_confidence: 0.9, band_mode: 'force', rate_limit: 5, approve_by: ['group:sre', 'user:root'] },
    { id: 'glob-rule', verdict: 'approve', match: { selector: 'host-glob:nl*', op_class: 'restart-service', territory: 'prod', reversible: true }, min_confidence: 0.6 },
    { id: 'group-rule', verdict: 'auto', match: { selector: 'group:edge' } },
    { id: 'class-rule', verdict: 'approve', match: { selector: 'device-class:cisco-asa' }, band_mode: 'respect' },
    { id: 'res-rule', verdict: 'deny', match: { selector: 'resource:mealie.service' }, rate_limit: 0 },
    { id: 'the-default', verdict: 'approve', match: {}, min_confidence: 0.5, is_default: true },
  ];
  const RICH_EXPECT = { rules: [
    { id: 'host-rule', verdict: 'deny', match: { host: 'librespeed01', argv_pattern: 'rm -rf', reversible: false }, params: { min_confidence: 0.9, band_mode: 'force', rate_limit: 5 }, approve_by: ['group:sre', 'user:root'] },
    { id: 'glob-rule', verdict: 'approve', match: { host_glob: 'nl*', op_class: 'restart-service', territory: 'prod', reversible: true }, params: { min_confidence: 0.6 } },
    { id: 'group-rule', verdict: 'auto', match: { group: 'edge' } },
    { id: 'class-rule', verdict: 'approve', match: { device_class: 'cisco-asa' }, params: { band_mode: 'respect' } },
    { id: 'res-rule', verdict: 'deny', match: { resource: 'mealie.service' }, params: { rate_limit: 0 } },
    { id: 'the-default', verdict: 'approve', is_default: true, params: { min_confidence: 0.5 } },
  ] };
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(RICH_READ, { version: 'bundle-rich' }), state });
    await openEditor(page);
    await saveWith(page, 'no-op round-trip over every selector kind and field');
    await page.waitForSelector('#polEditDone', { timeout: 10000 });
    const post = state.posts[0] || {};
    ok(post.document && canonDoc(post.document) === canonDoc(RICH_EXPECT),
      'kinds: every hyphenated selector kind must map to its UNDERSCORE write key and every field round-trip\n     expect=' + canonDoc(RICH_EXPECT) + '\n     posted=' + (post.document ? canonDoc(post.document) : 'null'));
    // pin the two hyphenated kinds explicitly (the ones the read renders differently from the write key).
    const byId = {}; (post.document ? post.document.rules : []).forEach(r => { byId[r.id] = r; });
    ok(byId['glob-rule'] && byId['glob-rule'].match && byId['glob-rule'].match.host_glob === 'nl*', 'kinds: host-glob must map to match.host_glob');
    ok(byId['class-rule'] && byId['class-rule'].match && byId['class-rule'].match.device_class === 'cisco-asa', 'kinds: device-class must map to match.device_class');
    ok(byId['res-rule'] && byId['res-rule'].params && byId['res-rule'].params.rate_limit === 0, 'kinds: rate_limit:0 must survive nested under params');
    ok(state.pageErrors.length === 0, 'kinds: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 2. EDIT ONE VERDICT — only that field changes, expected_version pinned.
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(RICH_READ, { version: 'bundle-rich' }), state });
    await openEditor(page);
    await page.waitForSelector('#polEdit_verdict_0', { timeout: 10000 });
    await page.selectOption('#polEdit_verdict_0', 'approve'); // host-rule deny -> approve
    await saveWith(page, 'soften host-rule to approve');
    await page.waitForSelector('#polEditDone', { timeout: 10000 });
    const post = state.posts[0] || {};
    ok(post.expected_version === 'bundle-rich', `edit: expected_version pinned, got ${JSON.stringify(post.expected_version)}`);
    const expect = JSON.parse(JSON.stringify(RICH_EXPECT));
    expect.rules[0].verdict = 'approve';
    ok(post.document && canonDoc(post.document) === canonDoc(expect),
      'edit: ONLY the edited verdict may change; every other field is preserved\n     expect=' + canonDoc(expect) + '\n     posted=' + (post.document ? canonDoc(post.document) : 'null'));
    await page.context().close();
  }

  // 3. ADD / DELETE / MOVE reflected in the posted ORDER (deny-overrides order is semantically significant).
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(SHIPPED.rules.map(projectToRead), { version: 'bundle-shipped' }), state });
    await openEditor(page);
    await page.waitForSelector('#polEditList', { timeout: 10000 });
    const ids0 = SHIPPED.rules.map(r => r.id);
    const last = ids0.length - 1; // count-derived: the SHIPPED set grows over time (e.g. the spec/029 inverse rule)
    // move index 1 up → [1,0,2,...]
    await page.click('.pol-edit-card[data-idx="1"] button[title="move up"]');
    // delete the LAST rule → one shorter
    await page.click(`.pol-edit-card[data-idx="${last}"] button[title="delete rule"]`);
    // add a rule → the new blank lands back at index `last`; fill id + verdict + op_class
    await page.click('#polEditAdd');
    await page.waitForSelector(`#polEdit_id_${last}`, { timeout: 10000 });
    await page.fill(`#polEdit_id_${last}`, 'zeta-added');
    await page.selectOption(`#polEdit_verdict_${last}`, 'auto');
    await page.fill(`#polEdit_op_class_${last}`, 'noop-op');
    await saveWith(page, 'reorder, prune and extend the access-list');
    await page.waitForSelector('#polEditDone', { timeout: 10000 });
    const post = state.posts[0] || {};
    const gotIds = post.document && post.document.rules ? post.document.rules.map(r => r.id) : [];
    const wantIds = [ids0[1], ids0[0], ...ids0.slice(2, last), 'zeta-added'];
    ok(JSON.stringify(gotIds) === JSON.stringify(wantIds), `order: add/delete/move must reflect in posted order\n     want=${JSON.stringify(wantIds)}\n     got =${JSON.stringify(gotIds)}`);
    const added = post.document && post.document.rules && post.document.rules[last];
    ok(added && added.verdict === 'auto' && added.match && added.match.op_class === 'noop-op', `order: the added rule must carry its edited fields, got ${JSON.stringify(added)}`);
    ok(state.pageErrors.length === 0, 'order: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 4. 400 → VERBATIM backend validator text, NO success.
  {
    const state = mkState();
    state.replies = [{ status: 400, body: 'refused: malformed ruleset — policy: malformed rule: rule[0] "x": match specifies no dimension' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(RICH_READ), state });
    await openEditor(page);
    await saveWith(page, 'attempt a write the validator will refuse');
    await page.waitForSelector('#polEditBackErr', { timeout: 10000 });
    const t = await bodyText(page);
    ok(/match specifies no dimension/.test(t), '400: the backend VERBATIM validator message must be shown');
    ok(/WRITE REFUSED/.test(t), '400: an honest refusal state must render');
    ok(!(await page.$('#polEditBody .chip-ok')), '400: NO success chip on a validator refusal');
    ok(!(await page.$('#polEditDone')), '400: no Done/success affordance on a refusal');
    ok(state.posts.length === 1, `400: exactly one POST, got ${state.posts.length}`);
    await page.context().close();
  }

  // 5. 409 → "changed underneath you" + Re-read REFETCHES and resets — never a silent overwrite.
  {
    const state = mkState();
    state.replies = [{ status: 409, body: 'refused: expected_version no longer matches the active ruleset — re-read and retry' }];
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(RICH_READ, { version: 'bundle-rich' }), state });
    await openEditor(page);
    const getsBefore = state.rulesGets;
    await saveWith(page, 'a write that lost the compare-and-swap');
    await page.waitForSelector('#polEditReread', { timeout: 10000 });
    const t = await bodyText(page);
    ok(/changed underneath you/i.test(t), '409: the honest "changed underneath you" state must render');
    ok(!(await page.$('#polEditBody .chip-ok')), '409: NO success chip on a stale CAS');
    ok(!(await page.$('#polEditDone')), '409: no Done/success affordance on a stale CAS');
    // Re-read the moved ruleset (server now returns a different version) — the editor resets from the truth.
    state.rulesPage = mkReadPage(RICH_READ.slice(1), { version: 'bundle-moved' });
    await page.click('#polEditReread');
    await page.waitForSelector('#polEditSave', { timeout: 10000 });
    ok(state.rulesGets > getsBefore, `409: Re-read must REFETCH /v1/policy/rules (gets ${getsBefore}→${state.rulesGets})`);
    ok(state.posts.length === 1, `409: exactly one POST and NO silent overwrite/retry, got ${state.posts.length}`);
    // the reset editor now pins the MOVED baseline — a subsequent Save carries bundle-moved, never the stale one.
    await saveWith(page, 'reapply against the moved baseline');
    await page.waitForSelector('#polEditDone', { timeout: 10000 });
    ok((state.posts[1] || {}).expected_version === 'bundle-moved', `409: after Re-read the CAS baseline must be the moved version, got ${JSON.stringify((state.posts[1] || {}).expected_version)}`);
    ok(state.pageErrors.length === 0, '409: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 6. 401 → drives the SHARED admin step-up, then completes the write.
  {
    const state = mkState();
    state.replies = [{ status: 401, body: 'unauthenticated' }]; // first POST → 401 (elevate); second → 200
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(RICH_READ, { version: 'bundle-rich' }), state });
    await openEditor(page);
    await saveWith(page, 'a write from a not-yet-elevated session');
    await elevate(page);                                    // reuse the config/secret admin step-up (not a new flow)
    await page.waitForSelector('#polEditSave', { timeout: 10000 }); // editor re-opened PRESERVING the edit
    await page.click('#polEditSave');                        // deliberate second Save → POST#2 → 200
    await page.waitForSelector('#polEditDone', { timeout: 10000 });
    ok(state.elevatePosts === 1, `401: the SHARED step-up must be driven exactly once, got ${state.elevatePosts}`);
    ok(state.posts.length === 2, `401: two POSTs (401 then 200), got ${state.posts.length}`);
    ok((state.posts[1] || {}).expected_version === 'bundle-rich', '401: the completed POST must still carry the pinned version');
    ok(/RULESET SAVED/.test(await bodyText(page)), '401: the honest committed outcome must render after elevation');
    ok(state.pageErrors.length === 0, '401: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 7. has_default=true → FAIL CLOSED: the editor refuses to edit/save and fires NO POST.
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    await mount(page, { rulesPage: mkReadPage(RICH_READ, { has_default: true }), state });
    await openEditor(page);
    await page.waitForSelector('#polEditBlockedClose', { timeout: 10000 });
    const t = await bodyText(page);
    ok(/can't be edited here|default/i.test(t), 'default: an honest refusal message naming the default must render');
    ok(!(await page.$('#polEditSave')), 'default: there must be NO Save affordance for an unrepresentable ruleset');
    ok(!(await page.$('.pol-edit-card')), 'default: no editable cards may render for a fail-closed ruleset');
    // defense in depth: even forcing the submit path fires no POST.
    await page.evaluate(() => { try { polEditSubmit(); } catch (e) {} });
    ok(state.posts.length === 0, `default: fail-closed must fire NO POST, got ${state.posts.length}`);
    ok(state.pageErrors.length === 0, 'default: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }

  // 8. has_default ABSENT (field omitted entirely — a pre-slice-2 backend) → FAIL CLOSED on the unknown.
  {
    const state = mkState();
    const page = await (await browser.newContext()).newPage();
    const rp = mkReadPage(RICH_READ);
    delete rp.has_default; // the field the old backend never emitted — the read carries NO has_default at all.
    await mount(page, { rulesPage: rp, state });
    await openEditor(page);
    await page.waitForSelector('#polEditBlockedClose', { timeout: 10000 });
    const t = await bodyText(page);
    ok(/can't confirm|default/i.test(t), 'absent: an honest "can\'t confirm / default" refusal must render');
    ok(!(await page.$('#polEditSave')), 'absent: NO Save affordance when has_default is unknown (fail closed)');
    ok(!(await page.$('.pol-edit-card')), 'absent: no editable cards may render when has_default is unknown');
    // defense in depth: even forcing the submit path fires no POST on an unknown default.
    await page.evaluate(() => { try { polEditSubmit(); } catch (e) {} });
    ok(state.posts.length === 0, `absent: fail-closed on unknown must fire NO POST, got ${state.posts.length}`);
    ok(state.pageErrors.length === 0, 'absent: no uncaught JS errors: ' + state.pageErrors.join(' | '));
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('rule-editor FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('rule-editor: OK');
