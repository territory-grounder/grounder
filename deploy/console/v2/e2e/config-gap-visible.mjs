// THE CONTROL PLANE'S OWN CONFIGURATION HEALTH WAS FETCHED INTO THE BROWSER AND THEN DISCARDED.
//
// `config:gap-at-boot` is appended to the governance ledger when — and only when — the worker finds a
// configuration gap at startup: carve-outs that do not cover allowlisted guests, an evidence-reader domain
// armed with NO sanctioned principals (every non-TG actor there then reads attributed-suspicious), a PVE TLS
// flag disagreement.
//
// Measured live 2026-07-30: GET /v1/ledger returned 200 entries containing ELEVEN such rows, newest at
// 22:47:04 — and the console renders `entries.slice(-40)`, which held NONE of them. The finding reached the
// spine, reached the browser, and fell off the end of a window sized for recent activity. So no surface could
// tell an operator that their own control plane was misconfigured, while the answer sat in memory.
//
// ★ AND THE BRANCH THIS ORACLE MUST DRIVE. The first version of this file stubbed /v1/sessions with an EMPTY
// list, so views.command fell through to `return withApprovals(wrap)` — and the card, which !757 prepended to
// `wrap`, rendered. Production never takes that branch: the live spine always has rows, so the function early-
// returns `withApprovals(strip)` and the card was discarded on every real load. The oracle passed and the
// feature did not exist. Measured on the deployed console: configGap retained, panel absent.
// So the spine below is POPULATED. Both branches are asserted, and the populated one is the one that matters.
//
// The second property here is the one that is easy to get wrong: the row exists ONLY when there IS a gap, so
// its absence is NOT a clean bill of health. A panel that renders "configuration OK" from a missing row
// repeats the absent-is-not-zero error that once made an empty query read as a healthy estate.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const GAP_REASON = [
  'carve-outs do not cover 2 allowlisted guest(s) [dc1docuseal01 dc1habitica01] — a harness cycle on an uncovered guest escalates to a human',
  'domain "journal" armed with NO sanctioned principals — every non-TG actor there reads attributed-suspicious',
  'domain "awx" armed with NO self-actor and NO sanctioned principals — every actor there reads attributed-suspicious',
].join(' | ');

// A page shaped like the live one: the gap row is EARLY, and 60 later rows push it outside any -40 window.
const mkLedger = (withGap) => {
  const entries = [];
  const t0 = Date.now() - 3 * 3600 * 1000;
  if (withGap) {
    entries.push({ seq: 1, decision: 'config:gap-at-boot', reason: GAP_REASON,
                   action_id: 'boot-1', created_at: new Date(t0).toISOString(), hash: 'h1', prev: '' });
  }
  for (let i = 0; i < 60; i++) {
    entries.push({ seq: 2 + i, decision: 'suppress:escalate', reason: 'routine', action_id: `a${i}`,
                   created_at: new Date(t0 + (i + 1) * 60000).toISOString(), hash: `h${2 + i}`, prev: `h${1 + i}` });
  }
  return { entries };
};

const run = async (withGap) => {
  const browser = await chromium.launch();
  try {
    const page = await (await browser.newContext({ viewport: { width: 1500, height: 1000 } })).newPage();
    await page.route('**/v1/**', async rt => {
      const u = rt.request().url();
      const j = b => rt.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(b) });
      if (u.includes('/v1/whoami')) return j({ source: 'operator:test', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'test' });
      if (u.includes('/v1/events')) return rt.abort();
      if (u.includes('/v1/ledger')) return j(mkLedger(withGap));
      // POPULATED on purpose — see the note above. An empty spine drives a branch production never takes.
      if (u.includes('/v1/sessions')) return j({ total: 3, sessions: [
        { external_ref: 'run-a', band: 'AUTO', verdict: 'match', host: 'dc1h1', action_id: 'a1', op_class: 'restart-service', classified_at: '2026-07-30T00:00:00Z' },
        { external_ref: 'run-b', band: 'POLL_PAUSE', verdict: '', host: 'dc1h2', action_id: 'a2', op_class: 'start-service', classified_at: '2026-07-30T00:01:00Z' },
        { external_ref: 'run-c', band: 'AUTO', verdict: 'deviation', host: 'dc1h3', action_id: 'a3', op_class: 'restart-container', classified_at: '2026-07-30T00:02:00Z' } ] });
      return j({});
    });
    await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } });
    await page.evaluate(async () => { try { await liveAdopt(); } catch (e) {} });
    // liveAdopt() is already fully awaited above — every in-chain fetch and the post-adopt re-render have
    // resolved by the time page.evaluate() returns — so this is margin for the DOM to settle, not a guess
    // at fetch latency.
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    return await page.evaluate(() => {
      route('command');
      const v = document.querySelector('#view');
      const text = v.innerText || '';
      // the window the LEDGER view uses, to prove the row is genuinely outside it
      const inWindow = Array.isArray(liveState.ledger) && liveState.ledger.some(e => /config:gap/.test(e.decision || ''));
      return {
        text,
        // Prove WHICH branch rendered: the audit-spine element is what production returns.
        spinePopulated: Array.isArray(liveState.sessions) && liveState.sessions.length > 0,
        retained: !!liveState.configGap,
        retainedDecision: liveState.configGap ? liveState.configGap.decision : null,
        inLedgerWindow: inWindow,
        bullets: Array.from(v.querySelectorAll('li')).map(li => (li.textContent || '').trim()).filter(t => t.length > 20),
        claimsOK: /configuration (is )?(ok|fine|healthy|clean)/i.test(text),
        saysNotACleanBill: /not a clean bill of health/i.test(text),
      };
    });
  } finally { await browser.close(); }
};

// ---- WITH a gap in the page ----
const withGap = await run(true);
check('the fixture drives the POPULATED-spine branch (the one production takes)', withGap.spinePopulated === true,
  'an empty spine takes the fall-through return, where the old wrap.prepend happened to work — that is how a ' +
  'discarded card shipped green');
check('the gap row is genuinely OUTSIDE the ledger render window', withGap.inLedgerWindow === false,
  'the fixture did not reproduce the live condition — the row must fall outside slice(-40) or this proves nothing');
check('the newest config-gap entry is retained from the FULL page', withGap.retained === true,
  'nothing kept it, so the finding is discarded exactly as it was live');
check('and it is the config-gap decision', withGap.retainedDecision === 'config:gap-at-boot', String(withGap.retainedDecision));
check('#command surfaces a control-plane configuration panel', /CONTROL-PLANE CONFIGURATION/i.test(withGap.text),
  JSON.stringify(withGap.text.slice(0, 200)));
check('it reports the NUMBER of gaps found', /3 configuration gaps/i.test(withGap.text),
  `expected 3 findings from the " | "-joined reason: ${JSON.stringify(withGap.text.slice(0, 240))}`);
check('each finding is rendered separately, not as one wall of prose', withGap.bullets.length === 3,
  `${withGap.bullets.length} findings rendered`);
check('the uncovered-guest finding names the guests', withGap.bullets.some(b => /dc1docuseal01/.test(b)),
  JSON.stringify(withGap.bullets));
check('the armed-but-unsanctioned domain finding is shown', withGap.bullets.some(b => /journal.*sanctioned principals/i.test(b)),
  JSON.stringify(withGap.bullets));
check('it states the report is from the last boot and does not self-correct', /last boot/i.test(withGap.text) && /will not self-correct/i.test(withGap.text),
  JSON.stringify(withGap.text.slice(0, 300)));

// ---- WITHOUT a gap: the absence must NOT be reported as health ----
const noGap = await run(false);
check('with no gap row, nothing is retained', noGap.retained === false, String(noGap.retainedDecision));
check('the panel still appears (silence is stated, not left blank)', /CONTROL-PLANE CONFIGURATION/i.test(noGap.text),
  'an absent panel is indistinguishable from a panel that failed to render');
check('it does NOT claim the configuration is OK', noGap.claimsOK === false,
  'the row is written only when a gap EXISTS, so its absence proves nothing about health');
check('it says explicitly that this is not a clean bill of health', noGap.saysNotACleanBill === true,
  JSON.stringify(noGap.text.slice(0, 300)));
check('and it does not invent findings', noGap.bullets.length === 0, JSON.stringify(noGap.bullets));

console.log(failed ? `config-gap-visible: ${failed} FAILED` : 'config-gap-visible: all checks passed');
process.exit(failed ? 1 : 0);
