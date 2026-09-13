/* failed-read-never-reads-healthy — THE STRUCTURAL ORACLE for one repeated defect.
 *
 * The defect, stated once: the fetch layer distinguishes "this read FAILED" (the field is set to null) from
 * "this read succeeded and returned nothing" ([]). Consumers written as `Array.isArray(x) ? x : []` erase
 * that distinction, and every surface downstream then reports the failure as a healthy zero — in the
 * REASSURING direction, on the lane an operator uses during an incident.
 *
 * It was found in five places on one afternoon (estate host health, the estate-depth rail badge, three of
 * the four #signals tiles, the #logs fixture banner, and the proposals counterfactual headline that started
 * it). That is not five bugs. It is one bug with five faces, and the faces are why the per-symptom oracles
 * missed it: each surface fabricates a DIFFERENT sentence.
 *
 * WHY THIS SUITE IS SHAPED LIKE THIS. The first oracle written against this family named a SYMPTOM — it
 * forbade the string "would have addressed" — and a real mutation survived it by rendering a different
 * reassuring sentence ("No incidents in the last 7 days") instead. So this asserts the RULE:
 *
 *     with a read failed, the surface consuming it must not publish a number about the world.
 *
 * It is driven by a TABLE so that adding a surface without adding a row is the visible failure mode, and
 * the table is covered by a VACUITY FLOOR at the bottom: if an endpoint name drifts, every "failed" case
 * silently degrades into "stubbed something nobody calls, saw no fabricated number, passed". The floor
 * turns that into a named failure instead (precedent: cmd/worker/axis_wiring_test.go:28).
 *
 * RED MUTATION CONTROLS, all executed 2026-08-01 and restored green:
 *   1. estate health reverted to `||"ok"`      -> "1 host chip(s) read OK with /v1/alerts at 503"
 *   2. badge guards the estate read only       -> "badge must read "—" … got "2""
 *   3. signals tile publishes decisions.length -> "tile "approval backlog" reads "0" … at 503"
 *   4. ACTIVE ALERTS panel reports .length     -> "still asserts /no active alerts on this node/"
 *
 * Control 1 SURVIVED the first version of this suite, which asserted only on the badge: the badge guard
 * independently produced "—", so reverting host health changed nothing this oracle looked at. Two consumers
 * of one failed read need two assertions — fixing one does not imply the other, and an oracle that watches
 * the cheaper consumer will certify the more dangerous one for free. Hence the per-chip check.
 */
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
const failures = [];
const ok = (cond, msg) => { if (!cond) failures.push(msg); };

const EMPTY = {
  alerts: [], sessions: [], actions: [], proposals: [], candidates: [], entries: [], decisions: [],
  items: [], rules: [], pages: [], skills: [], models: [], modules: [], sources: [], resolutions: [],
  coverage: [], lane_coverage: [], refs: [], sealed: [], classes: [], available: false, node_count: 0,
};
const WHOAMI = { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' };

/* A REAL estate and REAL alerts, so "healthy" is a conclusion the console could plausibly draw rather than
 * a vacuous one. Two hosts, one of them critical: with alerts readable the badge must say 1. */
const ESTATE = {
  available: true, node_count: 2,
  nodes: [{ name: 'dc1actualbudget01', type: 'lxc' }, { name: 'dc1mealie01', type: 'lxc' }],
  edges: [],
};
/* Timestamps are relative to NOW: the tiles bucket rows over a visible window, so a hardcoded date would
 * quietly stop counting once it aged out and this suite would fail for a reason that is not the defect. */
const ago = mins => new Date(Date.now() - mins * 60000).toISOString();

const ALERTS = {
  alerts: [{ host: 'dc1actualbudget01', severity: 'critical', alert_rule: 'Service up/down', state: 'active', received_at: ago(20), external_ref: 'librenms-1' }],
  counts: { total: 1, last_24h: 1 },
};
const SESSIONS = { sessions: [
  { external_ref: 'librenms-1', host: 'dc1mealie01', band: 'POLL_PAUSE', risk_level: 'medium',
    action_id: 'a1', auto_approved: false, notify_required: true, operator_override: false,
    verdict: '', classified_at: ago(30) },
], total: 1 };
const DECISIONS = { decisions: [{ external_ref: 'TG-a', band: 'POLL_PAUSE' }, { external_ref: 'TG-b', band: 'POLL_PAUSE' }] };
/* Sealed manifests as /v1/actions really shapes them. The mapper dereferences action_id, op_class, params
 * and sealed_at unguarded, so a thin fixture throws INTO the same catch that a 503 hits — which would make
 * the converse case fail for the wrong reason and, worse, make the failure cases pass without proving
 * anything. Fixture realism is load-bearing here, not decoration. */
const ACTIONS = {
  actions: [10, 25, 40].map((m, i) => ({
    action_id: `aaaaaaaa${i}111222233334444`, plan_hash: `bbbbbbbb${i}555566667777`,
    op_class: 'restart-service', params: { unit: 'nginx.service' },
    target: 'dc1mealie01', band: 'POLL_PAUSE', risk_level: 'medium',
    classified: true, predicted: true, approved: false, executed: false, verified: false,
    reversible: true, has_confidence: false, sealed_at: ago(m),
  })),
  counts: { total: 3 },
};

const BODIES = {
  '/v1/estate': ESTATE,
  '/v1/alerts': ALERTS,
  '/v1/decisions': DECISIONS,
  '/v1/actions': ACTIONS,
  '/v1/sessions': SESSIONS,
};

/* Each row: the endpoint to fail, the view that consumes it, and what must not survive the failure. */
const SURFACES = [
  {
    endpoint: '/v1/alerts', view: 'estatedepth',
    what: 'host health AND the not-ok badge are both derived from the alert read',
    badgeMustBe: '—',
    /* The badge and the per-host chips are TWO consumers of the same failed read, and guarding only the
     * badge leaves every host still painted green. Asserted separately because a fix to one does not
     * imply the other — proven: a mutation reverting host health alone survived a badge-only oracle. */
    hostChipsMustBeUnknown: true,
    forbidText: [
      /no active alerts on this node/i,   // a claim about the estate, from a read that 503'd
      /\b0 open\b/i,
    ],
  },
  {
    endpoint: '/v1/sessions', view: 'knowledge',
    what: "each host page lists the incidents that touched it, filtered from the sessions read",
    forbidText: [/a quiet host/i],
  },
  {
    endpoint: '/v1/decisions', view: 'signals', tile: 'approval backlog', liveValue: '2',
    what: 'the approval-backlog tile counts POLL_PAUSE decisions awaiting a vote',
  },
  {
    endpoint: '/v1/actions', view: 'signals', tile: 'sealed actions', liveValue: '3',
    what: 'the sealed-actions tile counts sealed actions in the fetched page',
  },
];

async function mount(page, { fail = [] } = {}) {
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: WHOAMI });
    if (fail.includes(p)) return route.fulfill({ status: 503, json: { error: 'unavailable' } });
    if (BODIES[p]) return route.fulfill({ json: BODIES[p] });
    return route.fulfill({ json: EMPTY });
  });
}
async function open(page, view) {
  await page.goto(`${BASE}/index.html#${view}`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // Every endpoint this table exercises (/v1/alerts, /v1/sessions, /v1/decisions, /v1/actions, /v1/estate)
  // is fetched in-chain inside liveAdopt(); lastRefresh, its last statement, is set only after all of them
  // AND the post-adopt route() re-render for whichever view was landed on.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
}
const badgeText = page => page.evaluate(() =>
  document.querySelector('[data-badge="estatedepth"]')?.textContent?.trim() ?? null);

/* Read ONE tile by its label rather than regexing the page's text blob. The first draft of this suite used
 * proximity matching ("approval backlog" within N characters of a 0) and was wrong twice over: it depended
 * on the character distance to the NEXT tile, and it is the same name-a-symptom mistake that let the
 * original bug through. A tile has a label and a value; assert on those. Returns null when the tile is
 * absent, which the caller must treat as a failure — a renamed tile would otherwise pass vacuously. */
const tileValue = (page, label) => page.evaluate(lbl => {
  const t = [...document.querySelectorAll('.sig-tile')]
    .find(el => (el.querySelector('.lbl')?.textContent || '').trim().toLowerCase() === lbl);
  return t ? (t.querySelector('.sig-tv')?.textContent || '').trim() : null;
}, label);

const browser = await chromium.launch();
try {
  for (const s of SURFACES) {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
    const errs = []; page.on('pageerror', e => errs.push(String(e)));
    await mount(page, { fail: [s.endpoint] });
    await open(page, s.view);

    if (s.tile) {
      const v = await tileValue(page, s.tile);
      ok(v !== null,
        `VACUITY: no .sig-tile labelled ${JSON.stringify(s.tile)} — the selector this case rests on stopped ` +
        `matching, so it proved nothing about ${s.endpoint}`);
      ok(v === null || v === '—',
        `#signals tile ${JSON.stringify(s.tile)} reads ${JSON.stringify(v)} with ${s.endpoint} at 503 — ` +
        `${s.what}. A count derived from a read that never landed is the reassuring direction of the lie; ` +
        `"—" is the only honest value here.`);
    }
    if (s.badgeMustBe !== undefined) {
      const b = await badgeText(page);
      ok(b === s.badgeMustBe,
        `estatedepth badge must read ${JSON.stringify(s.badgeMustBe)} when ${s.endpoint} is at 503, got ` +
        `${JSON.stringify(b)} — a digit asserts a not-ok count over an estate whose health could not be read`);
    }
    if (s.hostChipsMustBeUnknown) {
      const chips = await page.evaluate(() =>
        [...document.querySelectorAll('.e2-chip')].map(e => e.textContent.trim().toUpperCase()));
      ok(chips.length > 0,
        'VACUITY: no .e2-chip elements on #estatedepth — the host health chip selector stopped matching');
      const green = chips.filter(c => c.includes('OK'));
      ok(green.length === 0,
        `${green.length} host chip(s) read OK with ${s.endpoint} at 503 — host health is DERIVED from the ` +
        `alert read, so "OK" here is a health claim about a host nobody could see. Expected UNKNOWN.`);
      ok(chips.some(c => c.includes('UNKNOWN')),
        'with the alert read failed at least one host chip must say UNKNOWN — silence is not health');
    }
    const text = await page.evaluate(() => document.querySelector('#view')?.innerText || '');
    for (const re of (s.forbidText || [])) {
      ok(!re.test(text),
        `#${s.view}: with ${s.endpoint} at 503 the surface still asserts ${re} — ${s.what}`);
    }
    ok(errs.length === 0, `${s.view} (${s.endpoint} failed): uncaught page errors: ${errs.join(' | ')}`);
    await page.context().close();
  }

  /* THE CONVERSE. Without this, a console that renders "—" unconditionally satisfies everything above —
   * honest, useless, and green. Every read succeeds here, so every figure must be a real number. */
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
    await mount(page, { fail: [] });
    await open(page, 'estatedepth');
    const b = await badgeText(page);
    ok(b !== null, 'no [data-badge="estatedepth"] element — the selector this oracle rests on stopped matching');
    ok(/^\d+$/.test(b || ''),
      `with every read succeeding the estate badge must show a real count, got ${JSON.stringify(b)}`);
    ok(b === '1',
      `one of two hosts carries a critical alert, so the not-ok count is 1; got ${JSON.stringify(b)} — ` +
      `the honest-unknown path must not have swallowed a countable answer`);
    await page.context().close();
  }

  /* And the signals tiles must carry their real figures when their reads land — the converse that stops
   * "render — always" from being a passing strategy. */
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
    await mount(page, { fail: [] });
    await open(page, 'signals');
    for (const s of SURFACES.filter(x => x.tile)) {
      const v = await tileValue(page, s.tile);
      ok(v === s.liveValue,
        `with every read answering, #signals tile ${JSON.stringify(s.tile)} must show its real count ` +
        `(${s.liveValue}), got ${JSON.stringify(v)} — the honest-unknown path must not swallow a real answer`);
    }
    await page.context().close();
  }

  /* THE CONVERSE FOR #knowledge, and the assertion that actually kills the original defect: with the
   * sessions read ANSWERING and a host that has an incident, that host's page must SHOW it. The failure
   * case above (no "quiet host" under a 503) passes just as well when the view can never match ANY host —
   * which is precisely the bug that shipped. `host` reaching the DTO is what this proves. */
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
    await mount(page, { fail: [] });
    await open(page, 'knowledge');
    /* Navigate to the HOST'S OWN page and read only its incident block. Asserting on the whole view was
     * useless: #knowledge also renders a GLOBAL, unfiltered incident list, so the ref appears there no
     * matter how the per-host filter behaves — a mutation reverting hostOf to the broken signals lookup
     * passed that version of this check. The per-host list is the thing under test, so select it. */
    await page.evaluate(() => knowGo('host', 'dc1mealie01'));
    // knowGo() re-renders views.knowledge() synchronously in place — no further async work to wait on, just
    // a reflow flush for the DOM to settle before reading it below.
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    const hostPane = await page.evaluate(() => {
      const list = document.querySelector('#view .know-inc-list');
      const empty = [...document.querySelectorAll('#view .know-empty-line')].map(e => e.textContent.trim());
      return { incidents: list ? list.innerText : '', empties: empty };
    });
    ok(/librenms-1/.test(hostPane.incidents),
      'the incident on dc1mealie01 must be listed IN THAT HOST\'S OWN incident block — the sessions ' +
      'DTO carries host and the per-host filter must match on it. Filtering on signals.host (never ' +
      'populated in production) matched nothing for every host, forever, over a spine of 3,202 sessions. ' +
      'Host block was: ' + JSON.stringify(hostPane));
    ok(!hostPane.empties.some(t => /a quiet host/i.test(t)),
      'a host WITH an incident must not be described as quiet');
    await page.context().close();
  }

  /* VACUITY FLOOR. The table is only a test while its endpoints are ones the console actually requests at
   * boot. If a rename drifts them, the failure cases above degrade into "stubbed an endpoint nobody calls,
   * observed no fabricated number, passed" — the quietest way for a suite to stop being one. */
  {
    const page = await browser.newContext({ viewport: { width: 1600, height: 1200 } }).then(c => c.newPage());
    const seen = new Set();
    page.on('request', r => { try { seen.add(new URL(r.url()).pathname); } catch (_) { /* non-URL */ } });
    await mount(page, { fail: [] });
    await open(page, 'signals');
    await open(page, 'estatedepth');
    for (const s of SURFACES) {
      ok([...seen].some(p => p.endsWith(s.endpoint)),
        `VACUITY: ${s.endpoint} is in this suite's table but the console never requested it, so the case ` +
        `for #${s.view} proved nothing. Fix the endpoint, do not delete the row.`);
    }
    await page.context().close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('failed-read-never-reads-healthy FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('failed-read-never-reads-healthy: OK');
