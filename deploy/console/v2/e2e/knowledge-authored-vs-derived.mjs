// A VIEW THAT SAYS "NEVER HAND-AUTHORED" MUST NOT SHOW HAND-AUTHORED PAGES AS DERIVED.
//
// #knowledge opens with: "Every page below is composed from the estate graph, the incidents that touched an
// entity, and the verifier's verdicts — never hand-authored, so it cannot drift from the running system."
// Three of its pages are literal sentences in modules/knowledge/fixtures.txt, and they are the pages stating
// SAFETY LAW ("Never-auto floor is non-configurable", "A deviation demotes forever"). They were counted into
// the page total and into the rail badge.
//
// That is the one place a stale sentence is indistinguishable from a verified invariant: an operator
// consulting the self-describing wiki for the system's standing rules. The content is useful, so it is kept
// and MARKED — the defect was the claim, the count, and the badge, not the prose.
//
// This oracle asserts over the whole knowledge model rather than one page, because the defect was a category
// being silently absorbed into a total.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// The rail badge is written by liveAdopt, so the whole adopt path is DRIVEN here rather than liveState being
// poked directly. My first draft set liveState by hand and the badge check failed reading "—" — a harness gap
// masquerading as a defect. Driving the real path is what makes the badge assertion mean anything.
const ESTATE = {
  available: true, node_count: 2, edge_count: 1, source_count: 1,
  captured_at: '2026-07-29T20:00:00Z',
  nodes: [{ name: 'dc1h0' }, { name: 'dc1h1' }],
  edges: [{ from: 'dc1h1', to: 'dc1h0', rel: 'runs_on', confidence: 0.9, source: 'pve' }],
};
const SESSIONS = {
  total: 40,
  sessions: [{ external_ref: 'r1', band: 'AUTO', verdict: 'match', risk_level: 'low', action_id: 'a1', classified_at: '2026-07-29T19:00:00Z' }],
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.route('**/v1/**', async rt => {
    const u = rt.request().url();
    const j = (b) => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
    if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
    if (u.includes('/v1/events')) return rt.abort();
    if (u.includes('/v1/estate')) return j(ESTATE);
    if (u.includes('/v1/sessions')) return j(SESSIONS);
    return j({});
  });
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
  await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
  // liveAdopt() is already fully awaited above (estate/sessions are read in its sequential in-chain, so
  // liveState is already settled) — one frame is enough margin for the DOM route('knowledge') call below,
  // not a guess at fetch latency.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));

  const r = await page.evaluate(() => {
    route('knowledge');
    const lead = document.querySelector('#view .know-lead');
    const groupHeads = Array.from(document.querySelectorAll('#view .know-index .lbl, #view .know-index h3, #view .know-index .know-grp'))
      .map(e => (e.textContent || '').trim()).filter(Boolean);
    const model = typeof knowLiveModel === 'function' ? knowLiveModel() : null;
    const badge = (document.querySelector('[data-badge="knowledge"]') || {}).textContent;
    return {
      lead: lead ? lead.innerText : null,
      groupHeads,
      hosts: model ? model.hosts.length : null,
      incidents: model ? model.incidents.length : null,
      rules: model ? model.rules.length : null,
      rulesTagged: model ? model.rules.every(x => x.authored === true) : null,
      badge,
      viewText: document.querySelector('#view').innerText,
    };
  });

  check('the knowledge view rendered', !!r.lead, 'no .know-lead');
  check('the model still carries the authored rules', r.rules > 0, `rules=${r.rules} — the content should be kept, not deleted`);
  check('EVERY authored rule is tagged authored=true', r.rulesTagged === true,
    'an untagged rule is counted as derived again, which is the defect');

  // THE COUNT MUST MATCH THE CLAIM.
  const lead = r.lead || '';
  const derived = (r.hosts || 0) + (r.incidents || 0);
  check('the lead counts DERIVED pages, not the total', lead.includes(String(derived)) && /derived page/i.test(lead),
    `expected ${derived} derived in: ${JSON.stringify(lead.slice(0, 200))}`);
  check('the authored pages are named separately', /authored doctrine/i.test(lead),
    JSON.stringify(lead.slice(0, 240)));
  check('and the lead says they can drift from the code', /can drift|NOT verified/i.test(lead),
    `the whole point of marking them is that a frozen sentence about safety law may no longer be true: ${JSON.stringify(lead.slice(0, 240))}`);

  // THE GROUP HEADING must not present them as derived either.
  const ruleHead = r.groupHeads.find(h => /operational rules/i.test(h)) || '';
  check('the rules group heading says AUTHORED', /authored/i.test(ruleHead),
    `"${ruleHead}" — heads: ${JSON.stringify(r.groupHeads)}`);

  // THE RAIL BADGE is the number an operator glances at; it must not absorb authored prose.
  check('the rail badge counts derived knowledge only', r.badge === String(derived),
    `badge="${r.badge}" want "${derived}" (hosts ${r.hosts} + incidents ${r.incidents}); ` +
    `counting ${r.rules} hand-written sentences as learned knowledge overstates what the estate knows about itself`);

  // And the page itself must still be readable — marking is not hiding.
  check('the authored rules are still reachable on the view', /never-auto floor|deviation demotes/i.test(r.viewText || ''),
    'the content was removed rather than labelled — the doctrine is useful, the false provenance was not');
} finally { await browser.close(); }

console.log(failed ? `knowledge-authored-vs-derived: ${failed} FAILED` : 'knowledge-authored-vs-derived: all checks passed');
process.exit(failed ? 1 : 0);
