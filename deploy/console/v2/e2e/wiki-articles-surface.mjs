/* wiki-articles-surface — the compiled per-host wiki, as an operator meets it.
 *
 * The Articles facet is the read end of the wikicompile lane: the worker compiles one page per host TG has
 * triaged, the grounder serves them at /v1/wiki, and this tab is where they are read. Three things must
 * hold, and each has failed somewhere in this console before:
 *
 *  1. The tab must render ARTICLES, not the default view. wkView's facet dispatch ends in a catch-all
 *     (`else root.append(wkLessons(d))`), so a facet declared without its own arm silently renders LESSONS
 *     under the Articles tab — a tab that appears, is clickable, shows content, and is showing the wrong
 *     thing. That is worse than a blank tab because it looks right.
 *  2. An UNCOMPILED wiki must say so, and must not publish a compile date. `omitempty` does not apply to a
 *     time.Time, so the first version of this field served "0001-01-01T00:00:00Z" — a console reading
 *     "compiled 1 Jan 0001" over a wiki that never ran.
 *  3. A compiled wiki must show its provenance and its true total, never the page size.
 */
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
const failures = [];
const ok = (cond, msg) => { if (!cond) failures.push(msg); };

const EMPTY = {
  alerts: [], sessions: [], actions: [], decisions: [], entries: [], items: [], rules: [], pages: [],
  skills: [], models: [], modules: [], sources: [], resolutions: [], coverage: [], lane_coverage: [],
  refs: [], sealed: [], classes: [], proposals: [], candidates: [], available: false, node_count: 0,
};
const WHOAMI = { source: 'operator:tester', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' };

async function open(page, wiki) {
  await page.route('**/api/**', route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: WHOAMI });
    if (p === '/v1/wiki') return route.fulfill({ json: wiki });
    return route.fulfill({ json: EMPTY });
  });
  await page.goto(`${BASE}/index.html#wiki`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // The Articles facet renders from wkData()/wkLive.idx (modules/wiki/js.txt), NOT liveState.wiki: wiki is
  // wired onto liveAdopt as a fire-and-forget monkey-patch (wkLoadIndex(), see the wiki-module's
  // `liveAdopt = async function(refresh){ const r = await wkPrevLiveAdopt(refresh); ... wkLoadIndex(); ...}`)
  // that starts its OWN /v1/wiki read only AFTER liveAdopt() itself has already resolved. An earlier version
  // of this wait used liveState.lastRefresh, which raced that read — proven flaky (~1 in 2 runs) on
  // rail-badges-never-fixture.mjs's skills/wiki badges, the same race, before this file could show it too.
  // wkLive.idx/wkLive.err are the real completion signal for this data.
  await page.waitForFunction(() => typeof wkLive !== 'undefined' && (wkLive.idx !== null || wkLive.err !== null)).catch(() => {});
  // Select the Articles facet the way an operator does.
  await page.evaluate(() => {
    if (typeof facetState === 'object') facetState.wiki = 'articles';
    if (typeof route === 'function') route('wiki');
  });
  // route('wiki') just re-rendered synchronously from already-loaded data; this is a reflow flush so the
  // innerText read below reflects the just-painted DOM, not a guess at an async settle time that doesn't apply.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
}
const text = page => page.evaluate(() => document.querySelector('#view')?.innerText || '');

const COMPILED = {
  lessons: [{ slug: 'librenms-1', external_ref: 'librenms-1', host: 'h1', alert_rule: 'r', summary: 'a lesson body', resolution: 'x', tags: [] }],
  lesson_total: 1,
  runbooks: [{ slug: 'triage-protocol', title: 'Triage protocol' }],
  skills: [], skills_available: false,
  articles: [
    { slug: 'host-dc1mealie01', title: 'dc1mealie01' },
    { slug: 'host-dc1pve01', title: 'dc1pve01' },
  ],
  article_total: 78,
  articles_compiled_at: '2026-08-01T06:30:00Z',
};
const NEVER_COMPILED = {
  lessons: COMPILED.lessons, lesson_total: 1, runbooks: COMPILED.runbooks,
  skills: [], skills_available: false, articles: [], article_total: 0,
};

const browser = await chromium.launch();
try {
  {
    const page = await browser.newContext({ viewport: { width: 1500, height: 1100 } }).then(c => c.newPage());
    await open(page, COMPILED);
    const t = await text(page);
    ok(/dc1mealie01/.test(t), `the Articles tab must list compiled host pages, got:\n${t.slice(0, 500)}`);
    ok(!/a lesson body/.test(t),
      'the Articles tab rendered the LESSONS view — the facet has no dispatch arm and the catch-all ' +
      'answered for it, which is the failure mode that looks like it works');
    ok(/2 of 78 host pages/.test(t),
      `a bounded list must state the TRUE total, not the page size; got:\n${t.slice(0, 400)}`);
    ok(/2026-08-01 06:30/.test(t),
      'the compile instant must be shown — these pages are derived from live data and their age is the ' +
      'operator\'s only signal of staleness');
    ok(/Nothing here is authored/i.test(t), 'the provenance claim must be on the surface');
    await page.context().close();
  }
  {
    const page = await browser.newContext({ viewport: { width: 1500, height: 1100 } }).then(c => c.newPage());
    await open(page, NEVER_COMPILED);
    const t = await text(page);
    ok(/No articles compiled yet/i.test(t), `an uncompiled wiki must say so, got:\n${t.slice(0, 400)}`);
    ok(!/0001/.test(t),
      'an uncompiled wiki must never publish a compile date — "compiled 1 Jan 0001" is an invented value');
    ok(/absence, not an empty estate/i.test(t),
      'the empty state must distinguish "the compiler has not run" from "TG has seen nothing"');
    await page.context().close();
  }
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('wiki-articles-surface FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('wiki-articles-surface: OK');
