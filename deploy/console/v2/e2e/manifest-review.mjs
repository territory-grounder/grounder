// Console e2e — the MANIFEST view is a REVIEW surface: it renders the granted-vs-proposed diff, offers
// exactly the three closed verbs, honours the server's caller_can_act, and can never author a target.
//
// spec/027 REQ-2703 (T-027-9). Five properties, each with a real failure mode behind it:
//   1. FAIL-CLOSED IS VISIBLE: a 503 from /v1/manifest renders an honest unavailable state, never an
//      empty-but-plausible review queue that would read as "the estate has nothing to adopt".
//   2. NO FABRICATED TARGET: with no data, nothing that looks like an adoptable entry may appear. A fake
//      row on THIS surface is a fake ACTUATION TARGET one adopt click from being granted (INV-15).
//   3. THE REVIEW IS REAL: rows render with provenance, confidence, status, and the server-computed
//      "grants" answer; the diff panel shows what is proposed against what is already granted.
//   4. caller_can_act IS THE SERVER'S: with caller_can_act=false every control is DISABLED (not hidden —
//      hiding leaves a read-only operator wondering where the queue went, enabling promises an action the
//      server will refuse).
//   5. NO CREATE CONTROL: the surface must offer no way to author a manifest entry by hand. That absence
//      is paradigm rule 9 — discovery is the only author of rows; a create control here would rebuild the
//      configuration project this whole plane exists to remove.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const entries = (canAct) => [
  { id: 1, entity_type: 'service', name: 'mealie.service', host: 'dc1mealie01', source: 'declared',
    confidence: 0.85, status: 'draft', materializes: true, allowlist_kind: 'unit',
    last_seen_at: '2026-07-31T12:00:00Z', caller_can_act: canAct },
  { id: 2, entity_type: 'service', name: 'granted.service', host: 'dc1wallos01', source: 'declared',
    confidence: 0.85, status: 'approved', materializes: true, allowlist_kind: 'unit',
    last_seen_at: '2026-07-31T12:01:00Z', caller_can_act: canAct },
  { id: 3, entity_type: 'service', name: 'vanished.service', host: 'dc1wallos01', source: 'declared',
    confidence: 0.85, status: 'retired_candidate_stale', materializes: true, allowlist_kind: 'unit',
    last_seen_at: '2026-07-20T09:00:00Z', caller_can_act: canAct },
  // A site materializes into NO leaf: adopting it grants nothing, and the surface must say so plainly.
  { id: 4, entity_type: 'site', name: 'dc1', host: '', source: 'netbox',
    confidence: 0.90, status: 'draft', materializes: false, allowlist_kind: '',
    last_seen_at: '2026-07-31T12:02:00Z', caller_can_act: canAct },
];

async function mount(page, manifestReply) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/manifest') {
      if (manifestReply === null) return route.fulfill({ status: 503, body: 'manifest surface unavailable' });
      return route.fulfill({ json: manifestReply });
    }
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    if (p === '/v1/estate') return route.fulfill({ json: { available: true, node_count: 1, nodes: [{ name: 'dc1mealie01', type: 'lxc' }], edges: [] } });
    return route.fulfill({ json: {} });
  });
  await page.goto(BASE + '/index.html#manifest', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // DETERMINISTIC WAITS, NOT SLEEPS. This mount used fixed waitForTimeout(1600)/(900) and was FLAKY:
  // commit 599a4e5c produced a green pipeline at 17:47 and a red one at 18:00 on the SAME sha, failing
  // "the honest empty state must render" — the 900ms simply expired before mfLoad()'s fetch+render
  // finished under CI load. A flaky oracle is worse than none here: it teaches re-running until green,
  // which is precisely how "green stops meaning anything". So: wait for the nav to EXIST, then wait for
  // the view to leave its loading state — the real completion signal the module itself publishes.
  await page.waitForFunction(
    () => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === 'manifest'),
    null, { timeout: 20000 });
  /* ...AND WAIT FOR LIVE ADOPTION TO FINISH BEFORE CLICKING. The wait below is for the manifest render,
     but it is not the last render: when the live spine finishes adopting, liveAdopt() calls route() to
     re-render whatever view the operator landed on. That re-render can land AFTER the wait below has
     already passed, so the assertions then read a view that is mid-rebuild — which is why this suite
     failed on a different assertion each run ("the honest empty state must render", then "the view must
     say the surface is unavailable") while passing 3/3 in isolation, where boot is fast enough that adopt
     lands first. Waiting for lastRefresh — the flag liveAdopt sets at the END of adoption — removes the
     race rather than out-waiting it. The `!liveState.on` arm keeps this honest for a non-live shell. */
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

const browser = await chromium.launch();
try {
  // 1 + 2. fail-closed 503 → honest unavailable state, zero fabricated targets.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, null);
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/unavailable|fail-closed/i.test(text), '503: the view must say the surface is unavailable/fail-closed');
    ok(!/mealie\.service|granted\.service/.test(text), '503: no fabricated manifest row may render — a fake row here is a fake actuation target');
    const badge = await page.evaluate(() => document.querySelector('[data-badge="manifest"]')?.textContent || '');
    ok(badge.trim() === '—', `503: the rail badge must stay em-dash (real counts only), got ${JSON.stringify(badge)}`);
    await page.context().close();
  }
  // 3. the review renders: diff panel, provenance, confidence, statuses, and the honest grants answer.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { entries: entries(true), drafts: 2, total: 4, caller_can_act: true });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(text.includes('mealie.service'), 'rows: a drafted unit must render');
    ok(/granted\s*→\s*proposed/i.test(text), 'diff: the granted-vs-proposed panel must render');
    ok(/declared/.test(text) && /0\.85/.test(text), 'rows: provenance and confidence must be visible');
    ok(/still granted/i.test(text), 'rows: a stale entry must say it is STILL GRANTED — a narrowed grant must never be implied');
    ok(/grants nothing/i.test(text), 'rows: an entry that materializes into no leaf must say adopting it grants nothing');
    const badge = await page.evaluate(() => document.querySelector('[data-badge="manifest"]')?.textContent || '');
    ok(badge.trim() === '2', `rows: the badge must show the REAL draft count 2, got ${JSON.stringify(badge)}`);
    // 5. no create control anywhere on the surface.
    const acts = await page.evaluate(() =>
      [...document.querySelectorAll('#view [data-act]')].map(b => b.dataset.act));
    ok(acts.length > 0, 'controls: a review surface must offer its verbs');
    const allowed = new Set(['adopt', 'reject', 'retire']);
    ok(acts.every(a => allowed.has(a)), `no-author: only adopt/reject/retire may exist, found ${JSON.stringify([...new Set(acts)])}`);
    ok(!/\bcreate\b|\badd entry\b|\bnew target\b/i.test(text),
      'no-author: no create/add-entry affordance may appear — discovery is the only author of rows (paradigm rule 9)');
    await page.context().close();
  }
  // 4. caller_can_act=false → controls present but DISABLED, and the reason stated.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { entries: entries(false), drafts: 2, total: 4, caller_can_act: false });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    const states = await page.evaluate(() =>
      [...document.querySelectorAll('#view [data-act]')].map(b => b.disabled));
    ok(states.length > 0, 'read-only: the controls must still be rendered, not hidden');
    ok(states.every(Boolean), `read-only: every control must be DISABLED when the server says caller_can_act=false, got ${JSON.stringify(states)}`);
    ok(/read-only|operator session/i.test(text), 'read-only: the surface must say why the controls are inert');
    await page.context().close();
  }
  // empty spine → honest empty state (a real state, not an error, and not an invented estate).
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { entries: [], drafts: 0, total: 0, caller_can_act: true });
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    ok(/has not drafted anything yet/i.test(text), 'empty: the honest empty state must render');
    ok(!/unavailable/i.test(text), 'empty: an empty spine is not the unavailable state');
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('manifest-review FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('manifest-review: OK');
