/* knowledge-points-at-compiled — the deferred follow-up from the wiki design, now that the pages exist.
 *
 * #knowledge's host pane filters a 200-row ESTATE-WIDE session window per host. Across 78 hosts it can
 * only ever show a slice, which is why it apologises for itself ("Drawn from the newest 200 sessions
 * across the whole estate"). That apology was CORRECT and was deliberately left in place while nothing
 * better existed.
 *
 * The wiki compiler now produces a `host-<name>` page from a PER-HOST read with no shared window. When
 * one exists, the honest move is to point at it rather than apologise; when one does not, the apology is
 * still the truth and must stay.
 *
 * The third case is the one that matters most and is easiest to get wrong: an UNREADABLE wiki index must
 * fall back to the apology, never render as "no compiled page exists". `liveState.wiki` is null on
 * failure and never {}, so absence of an index is not evidence of absence of pages.
 */
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const EMPTY = {
  alerts: [], sessions: [], actions: [], decisions: [], entries: [], items: [], rules: [], pages: [],
  skills: [], models: [], modules: [], sources: [], resolutions: [], coverage: [], lane_coverage: [],
  refs: [], sealed: [], classes: [], proposals: [], candidates: [], available: false, node_count: 0,
};
const WHOAMI = { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' };
const HOST = 'dc1mealie01';
const ESTATE = { available: true, node_count: 1, nodes: [{ name: HOST, type: 'lxc' }], edges: [] };
/* A FULL window — 200 rows, the API's sessionsPageLimit. The apology only renders when the window is
 * actually full, which is correct: a window that truncated nothing has nothing to apologise for. An
 * earlier version of this fixture carried ONE session, so the apology never rendered and two cases
 * "failed" for a reason that had nothing to do with the code under test. */
const SESSIONS = {
  sessions: Array.from({ length: 200 }, (_, i) => ({
    external_ref: 'librenms-' + i,
    // Only the first is on the host under test; the rest are the estate-wide noise that makes the
    // window a window.
    host: i === 0 ? HOST : 'dc1other' + String(i).padStart(3, '0'),
    band: 'POLL_PAUSE', risk_level: 'medium', action_id: 'a' + i,
    auto_approved: false, notify_required: true, operator_override: false, verdict: '',
    classified_at: new Date(Date.now() - (i + 1) * 60000).toISOString(),
  })),
  total: 3202,
};
const WIKI_WITH = {
  lessons: [], lesson_total: 0, runbooks: [], skills: [], skills_available: false,
  articles: [{ slug: 'host-' + HOST, title: HOST }], article_total: 1,
  articles_compiled_at: '2026-08-01T06:30:00Z',
};
const WIKI_WITHOUT = { lessons: [], lesson_total: 0, runbooks: [], skills: [], skills_available: false, articles: [], article_total: 0 };

async function open(page, wiki) {
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: WHOAMI });
    if (p === '/v1/estate') return route.fulfill({ json: ESTATE });
    if (p === '/v1/sessions') return route.fulfill({ json: SESSIONS });
    if (p === '/v1/wiki') {
      return wiki === null ? route.fulfill({ status: 503, json: {} }) : route.fulfill({ json: wiki });
    }
    return route.fulfill({ json: EMPTY });
  });
  await page.goto(`${BASE}/index.html#knowledge`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // liveState.wiki / liveState.sessions / liveState.estate — everything knowHostPage() reads — are all
  // fetched IN-CHAIN inside liveAdopt() (unlike the skills/wiki VIEWS' own wkLive.idx, which is a
  // fire-and-forget loader), so lastRefresh, set as liveAdopt()'s last statement, correctly covers all
  // three: after the post-adopt route() re-render, never before.
  await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
  await page.evaluate(h => knowGo('host', h), HOST);
  // knowGo() re-renders views.knowledge() synchronously in place — no further async work to wait on, just
  // a reflow flush for the DOM to settle before reading it below.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
  // Return the incident BLOCK, not the whole view: "compiled page" appears elsewhere on this surface, so
  // a loose text match over #view certified a broken link once already — the lookup was reading a field
  // the host object does not carry, the feature was silently off, and the assertion passed anyway.
  return page.evaluate(() => {
    const blocks = [...document.querySelectorAll('#view .know-block')];
    const b = blocks.find(x => /incidents seen here/i.test(x.textContent || ''));
    return b ? b.innerText : '';
  });
}

const browser = await chromium.launch();
try {
  {
    const page = await browser.newContext({ viewport: { width: 1500, height: 1100 } }).then(c => c.newPage());
    const t = await open(page, WIKI_WITH);
    ok(/compiled page/i.test(t) && /the complete per-host record/i.test(t),
      `with a compiled host page served, the pane must POINT AT IT instead of apologising for its window; got:\n${t.slice(0, 600)}`);
    ok(!/this host may have older ones/i.test(t),
      'the windowed apology must be replaced once a complete per-host page exists — keeping both says the ' +
      'record is incomplete while linking to the complete one');
    await page.context().close();
  }
  {
    const page = await browser.newContext({ viewport: { width: 1500, height: 1100 } }).then(c => c.newPage());
    const t = await open(page, WIKI_WITHOUT);
    ok(/this host may have older ones/i.test(t),
      'with NO compiled page, the windowed apology is still the truth and must stay');
    ok(!/compiled page/i.test(t), 'it must not link to a page that does not exist');
    await page.context().close();
  }
  {
    // THE CASE THAT MATTERS MOST. A failed index read must fall back to the apology, never render as
    // "no compiled page exists" — absence of an index is not evidence of absence of pages.
    const page = await browser.newContext({ viewport: { width: 1500, height: 1100 } }).then(c => c.newPage());
    const t = await open(page, null);
    ok(/this host may have older ones/i.test(t),
      'an UNREADABLE wiki index must fall back to the windowed apology, which remains true');
    ok(/could not be read/i.test(t) && /is unknown/i.test(t),
      'a FAILED index read must be visibly different from an index that was read and holds nothing. ' +
      'Collapsing them makes an unreachable wiki render exactly like one that has never run — and this ' +
      'assertion exists because an earlier version DID collapse them while a comment claimed otherwise, ' +
      'so the mutation restoring the collapse passed cleanly. A property asserted in prose and enforced ' +
      'nowhere is not a property.');
    ok(!/The compiled page/i.test(t), 'a failed index read must not claim a compiled page');
    await page.context().close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('knowledge-points-at-compiled FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('knowledge-points-at-compiled: OK');
