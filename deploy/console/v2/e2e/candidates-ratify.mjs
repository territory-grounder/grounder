// Console e2e — the EARNED OP-CLASS ladder. This surface converts recurring evidence into PERMISSION, so
// its failure modes are worse than a wrong number: a fabricated row here is a fabricated CAPABILITY one
// ratify click from being granted, and a prefilled form is a capability the model granted itself while the
// ledger records an operator's name against it.
//
// spec/028 REQ-2813 / REQ-2816 / REQ-2808 (T-028-8). Seven properties, each with a real failure mode:
//   1. FAIL-CLOSED IS VISIBLE: 503 renders an honest unavailable state and the rail badge stays em-dash.
//   2. EMPTY IS A STATE: an empty queue says so; it is not the unavailable state and not an invented one.
//   3. THE QUEUE IS REAL: rows carry recurrence, status, tier, and the auto-barred mark; the badge shows
//      the REAL ratify-ready count.
//   4. caller_can_act IS THE SERVER'S: controls DISABLED (not hidden) when the server says so, with a
//      stated reason — hiding leaves an operator wondering where the queue went.
//   5. THE FIVE QUESTIONS, IN ORDER: the dossier answers 1..5 in sequence (REQ-2816). Out of order, an
//      operator reads "what you must type" before "what it could break".
//   6. THE FORM IS EMPTY BESIDE SCREENED EXHIBITS (REQ-2813): every input starts blank, no input lives
//      inside an exhibit, no control offers to copy the model's suggestion into the form, and the POST
//      body carries no element byte-matching the model's prose. This is the one the module is FOR.
//   7. THE CEILING IS TOLD TRUTHFULLY (REQ-2808): an overlay class that finished the second climb renders
//      as a SENTENCE naming the embed-export MR, never as a fraction that implies a promotion is coming.
//
// DETERMINISTIC WAITS ONLY. TG-233: this suite already produced a same-SHA green-then-red from a fixed
// waitForTimeout. Every wait below is on a completion signal the module itself publishes.
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// The model's own words. The oracle asserts these reach the EXHIBIT and never an input or a POST body.
const MODEL_RATIONALE = 'the haproxy backend flapped; reloading clears the stale socket without dropping sessions';
const MODEL_UNDO = 'systemctl reload haproxy';

const CAND = (over = {}) => Object.assign({
  candidate_key: 'reload-haproxy@dc1',
  op_class: 'reload-haproxy',
  op: 'reload',
  param_names: ['unit'],
  status: 'ratify_ready',
  family: 'service-lifecycle',
  tier: 'low-reversible',
  occurrences: 11,
  hosts: 3,
  first_seen_at: '2026-07-01T09:00:00Z',
  last_seen_at: '2026-07-29T18:30:00Z',
  ledger_seq: 4120,
  auto_barred: false,
}, over);

const QUEUE = (canAct) => ({
  candidates: [
    CAND(),
    CAND({ candidate_key: 'wipe-volume@nlams01', op_class: 'wipe-volume', op: 'wipe', status: 'observed',
           tier: 'irreversible', occurrences: 4, hosts: 1, auto_barred: true, ledger_seq: 4121 }),
  ],
  total: 2,
  caller_can_act: canAct,
});

const DOSSIER = (canAct, over = {}) => Object.assign({
  candidate: CAND(),
  exhibits: [{
    model_verb: 'reload', model_rationale: MODEL_RATIONALE, model_undo_sketch: MODEL_UNDO,
    host: 'dc1haproxy01', target: 'haproxy.service', band: 'AUTO_NOTICE', outcome: 'verified_clean',
    observed_at: '2026-07-29T18:30:00Z', external_ref: 'librenms-dc1-180912',
  }],
  hosts: ['dc1haproxy01', 'dc1wallos01', 'nlams01edge01'],
  ratify_ready: true,
  already_granted: false,
  embedded: false,
  caller_can_act: canAct,
}, over);

const GRADUATION = {
  classes: [
    // held: finished the second climb, not embedded → pinned at the threshold, never "5 / 5 and climbing".
    { op_class: 'reload-haproxy', level: 'auto_notice', clean_run_count: 5, notice_run_count: 5,
      notice_threshold: 5, last_outcome: 'verified_clean', updated_at: '2026-07-29T18:30:00Z',
      embedded: false, ceiling_held: true },
    // genuinely mid-climb, same level and domain — the pair is what makes "held" a distinguishable state.
    { op_class: 'restart-mealie', level: 'auto_notice', clean_run_count: 5, notice_run_count: 2,
      notice_threshold: 5, last_outcome: 'verified_clean', updated_at: '2026-07-29T18:31:00Z',
      embedded: false, ceiling_held: false },
    { op_class: 'restart-service', level: 'auto', clean_run_count: 5, notice_run_count: 5,
      notice_threshold: 5, last_outcome: 'verified_clean', updated_at: '2026-07-29T18:32:00Z',
      embedded: true, ceiling_held: false },
  ],
};

// posted collects every write this surface makes, so the oracle can inspect what an operator's click
// actually put on the wire rather than what the form appeared to contain.
async function mount(page, opts) {
  const posted = [];
  await page.route('**/api/**', async route => {
    const req = route.request();
    const p = req.url().split('/api')[1].split('?')[0];
    if (req.method() === 'POST') {
      let body = {}; try { body = JSON.parse(req.postData() || '{}'); } catch (e) {}
      posted.push({ path: p, body });
      if (opts.writeStatus) return route.fulfill({ status: opts.writeStatus, body: opts.writeBody || '' });
      return route.fulfill({ json: { op_class: 'reload-haproxy', ledger_seq: 4200, entry_hash: 'deadbeefcafe0001' } });
    }
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:t', mode: 'Shadow', may_actuate: false, posture_stale: false, posture_source: 'runtime' } });
    if (p === '/v1/opclass/candidates') {
      if (opts.queue === null) return route.fulfill({ status: 503, body: 'op-class candidate surface unavailable' });
      return route.fulfill({ json: opts.queue });
    }
    if (p.startsWith('/v1/opclass/candidates/')) {
      if (!opts.dossier) return route.fulfill({ status: 503, body: 'dossier unavailable' });
      return route.fulfill({ json: opts.dossier });
    }
    if (p === '/v1/policy/graduation') return route.fulfill({ json: opts.graduation || { classes: [] } });
    if (p === '/v1/sessions') return route.fulfill({ json: { sessions: [], total: 0 } });
    if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 0, verified: 0, deviations: 0 } } });
    if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [] } });
    return route.fulfill({ json: {} });
  });
  const view = opts.view || 'candidates';
  await page.goto(BASE + '/index.html#' + view, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
  // BOOT MUST BE QUIESCENT BEFORE ANYTHING IS GRADED. liveAdopt() ends with route(<current view>) and only
  // THEN stamps liveState.lastRefresh, so that stamp is the live layer's own "I am done" signal. Without
  // waiting on it, the post-adopt rebuild lands after the assertions and puts the view back into its
  // loading state — which is exactly how a green oracle turns red on the same SHA under CI load (TG-233).
  await page.waitForFunction(
    () => typeof liveState !== 'undefined' && !!liveState.lastRefresh, null, { timeout: 20000 });
  // Wait for the rail to EXIST, click the entry, then wait for the view to leave its own loading state.
  await page.waitForFunction(
    v => [...document.querySelectorAll('.navi')].some(x => x.dataset.view === v),
    view, { timeout: 20000 });
  await page.evaluate(v => { const a = [...document.querySelectorAll('.navi')].find(x => x.dataset.view === v); if (a) a.click(); }, view);
  await page.waitForFunction(v => {
    const el = document.querySelector('#view');
    if (!el) return false;
    const t = (el.innerText || '').trim();
    if (!t.length) return false;
    return v === 'candidates'
      ? !/Loading the earned-class queue/i.test(t)
      : !/Connecting to the policy engine/i.test(t);
  }, view, { timeout: 20000 });
  return posted;
}

const viewText = page => page.evaluate(() => document.querySelector('#view')?.innerText || '');

async function openDossier(page) {
  await page.click('#view [data-act="open"]');
  // The module publishes "Loading the dossier…" and then replaces it — wait for the replacement, and for
  // the last of the five questions, so a partially-built dossier can never be graded as a complete one.
  await page.waitForFunction(() => {
    const el = document.querySelector('#view');
    if (!el) return false;
    const t = el.innerText || '';
    return !/Loading the dossier/i.test(t) && document.querySelectorAll('#view .oc-qn').length >= 5;
  }, null, { timeout: 20000 });
}

const browser = await chromium.launch();
try {
  // 1. fail-closed 503 → honest unavailable state; nothing that reads as a grantable capability.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { queue: null });
    const text = await viewText(page);
    ok(/unavailable/i.test(text), '503: the view must say the surface is unavailable');
    ok(!/reload-haproxy|wipe-volume/.test(text), '503: no fabricated candidate may render — a fake row here is a fake capability');
    const badge = await page.evaluate(() => document.querySelector('[data-badge="candidates"]')?.textContent || '');
    ok(badge.trim() === '—', `503: the rail badge must stay em-dash (real counts only), got ${JSON.stringify(badge)}`);
    await page.context().close();
  }

  // 2. empty queue → honest empty state, distinct from unavailable.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { queue: { candidates: [], total: 0, caller_can_act: true } });
    const text = await viewText(page);
    ok(/recurred often enough/i.test(text), 'empty: the honest empty state must render');
    ok(!/unavailable/i.test(text), 'empty: an empty queue is not the unavailable state');
    await page.context().close();
  }

  // 3. the queue is real, and the badge counts only ratify-ready candidates.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { queue: QUEUE(true), dossier: DOSSIER(true) });
    const text = await viewText(page);
    ok(text.includes('reload-haproxy'), 'queue: a ratify-ready candidate must render');
    ok(/11×\s*\/\s*3 host/.test(text), 'queue: recurrence must show occurrences and host spread');
    ok(/RATIFY READY/i.test(text), 'queue: the server-computed status must render');
    ok(/never auto/i.test(text), 'queue: an auto-barred candidate must be marked never-auto on the row');
    const badge = await page.evaluate(() => document.querySelector('[data-badge="candidates"]')?.textContent || '');
    ok(badge.trim() === '1', `queue: the badge must show the REAL ratify-ready count 1 (of 2 rows), got ${JSON.stringify(badge)}`);
    // REQ-2817: ONE console ladder. This surface must not grow a second graduation view beside the
    // existing /v1/policy/graduation one — two ladders is two answers to "has this class earned autonomy".
    const secondLadder = await page.evaluate(() => document.querySelectorAll('#view .pol-grad, #view .pol-grad-bar, #view .pol-held').length);
    ok(secondLadder === 0, `one-ladder: the candidates surface must render no graduation ladder of its own, found ${secondLadder} ladder nodes`);
    await page.context().close();
  }

  // 4. read-only: controls rendered but disabled, and the reason stated.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { queue: QUEUE(false), dossier: DOSSIER(false) });
    ok(/read-only|operator session/i.test(await viewText(page)), 'read-only: the queue must say why the grant controls are inert');
    await openDossier(page);
    const states = await page.evaluate(() =>
      [...document.querySelectorAll('#view [data-act="ratify"], #view [data-act="dismiss"]')].map(b => b.disabled));
    ok(states.length === 2, `read-only: both verbs must still be RENDERED, not hidden, got ${states.length}`);
    ok(states.every(Boolean), `read-only: every verb must be DISABLED when caller_can_act=false, got ${JSON.stringify(states)}`);
    const inputs = await page.evaluate(() => [...document.querySelectorAll('#view .oc-fi')].map(i => i.disabled));
    ok(inputs.length > 0 && inputs.every(Boolean), `read-only: the form fields must be disabled too, got ${JSON.stringify(inputs)}`);
    await page.context().close();
  }

  // 5 + 6. the dossier: five questions in order, and the empty form beside screened exhibits.
  {
    const page = await (await browser.newContext()).newPage();
    const posted = await mount(page, { queue: QUEUE(true), dossier: DOSSIER(true) });
    await openDossier(page);

    // 5. the five questions, in sequence.
    const nums = await page.evaluate(() => [...document.querySelectorAll('#view .oc-qn')].map(x => x.textContent.trim()));
    ok(JSON.stringify(nums) === JSON.stringify(['1', '2', '3', '4', '5']),
      `dossier: the five questions must render in order 1..5, got ${JSON.stringify(nums)}`);
    // …and question 4 must be marked DISPLAY-ONLY. Prediction accuracy is the most natural thing on this
    // page to mistake for ladder credit; graduation counts terminus-confirmed action-lane runs only.
    const q4 = await page.evaluate(() => {
      const n = [...document.querySelectorAll('#view .oc-qn')].find(x => x.textContent.trim() === '4');
      return n ? n.closest('.oc-q').innerText : '';
    });
    ok(/displayed only/i.test(q4) && /never feeds the ladder/i.test(q4),
      `dossier: the prediction section must be marked display-only, got ${JSON.stringify(q4.slice(0, 200))}`);

    // 6a. the model's words are present, quoted, and labelled untrusted.
    const text = await viewText(page);
    ok(text.includes(MODEL_RATIONALE), 'exhibit: the model rationale must be shown to the operator — it is the evidence being read');
    ok(/UNTRUSTED/i.test(text), 'exhibit: model text must be labelled UNTRUSTED');
    const exhibitsScreened = await page.evaluate(() =>
      [...document.querySelectorAll('#view [data-exhibit]')].every(e => e.closest('.oc-ex') !== null));
    ok(exhibitsScreened, 'exhibit: every quoted model string must sit inside a screened exhibit block');

    // 6b. THE FORM IS EMPTY. Not "empty by convention" — no input carries any value at all.
    const values = await page.evaluate(() => [...document.querySelectorAll('#view .oc-fi')].map(i => i.value));
    ok(values.length >= 6, `form: the ratify form must render its fields, got ${values.length}`);
    ok(values.every(v => v === ''), `form: EVERY field must render empty (REQ-2813), got ${JSON.stringify(values)}`);
    const placeholders = await page.evaluate(() => [...document.querySelectorAll('#view .oc-fi')].map(i => i.placeholder || ''));
    ok(placeholders.every(p => !p.includes(MODEL_RATIONALE) && !p.includes(MODEL_UNDO)),
      'form: no placeholder may carry the model\'s text — a placeholder is a prefill an operator can accept by not noticing it');

    // 6c. no input lives inside an exhibit, and no exhibit lives inside the form. The separation is the
    //     requirement: a field rendered inside a quotation is a prefill wearing a citation.
    const noInputInExhibit = await page.evaluate(() => document.querySelectorAll('#view .oc-ex input, #view .oc-ex textarea').length === 0);
    ok(noInputInExhibit, 'separation: no input may render inside an exhibit');
    const noExhibitInForm = await page.evaluate(() => document.querySelectorAll('#view .oc-form .oc-ex').length === 0);
    ok(noExhibitInForm, 'separation: no exhibit may render inside the ratify form');

    // 6d. no affordance offers to move the model's suggestion into the form.
    ok(!/use (the )?suggestion|copy to form|prefill|accept (the )?proposal|apply (the )?model/i.test(text),
      'no-prefill: the surface must offer no control that copies the model\'s suggestion into the form');

    // 6e. what an operator's click actually puts on the wire.
    await page.evaluate(() => {
      const set = (id, v) => { const el = document.getElementById(id); el.value = v; };
      set('ocOp', 'reload'); set('ocFamily', 'service-lifecycle'); set('ocTier', 'low-reversible');
      set('ocParams', 'unit'); set('ocArgv', 'systemctl reload ${unit}');
      set('ocRollback', 'systemctl status ${unit}'); set('ocThreshold', '5');
      set('ocRationale', 'haproxy reloads recur across three hosts and are cheaply reversible');
    });
    await page.click('#view [data-act="ratify"]');
    // The module publishes its own completion signal — the acknowledgement note naming the ratified class.
    // Waiting on THAT (not on a sleep, and not on the node-side array, which the page cannot signal) is
    // what makes the assertions below race-free: the note cannot render before the write resolved.
    await page.waitForFunction(() => /ratified reload-haproxy/i.test(document.querySelector('#view')?.innerText || ''),
      null, { timeout: 20000 });
    ok(posted.length === 1, `ratify: exactly one write must be issued, got ${posted.length}`);
    if (posted.length) {
      const b = posted[0].body;
      ok(/\/ratify$/.test(posted[0].path), `ratify: the write must go to the ratify verb, got ${posted[0].path}`);
      ok(JSON.stringify(b.argv_template) === JSON.stringify(['systemctl', 'reload', '${unit}']),
        `ratify: the argv template must be the OPERATOR's typed vector, got ${JSON.stringify(b.argv_template)}`);
      const all = [].concat(b.argv_template || [], b.rollback_template || []);
      ok(all.every(el => el !== MODEL_RATIONALE && el !== MODEL_UNDO),
        'ratify: no template element may byte-match the model\'s prose — that is what the server tripwire refuses');
      ok((b.params || []).every(p => p.required === true),
        `ratify: every declared param must be REQUIRED — an optional slot renders a different command, ${JSON.stringify(b.params)}`);
      ok(typeof b.rationale === 'string' && b.rationale.length > 0, 'ratify: a rationale must accompany the grant');
    }
    await page.context().close();
  }

  // 6f. the 422 tripwire message reaches the operator VERBATIM. Flattening it to "invalid" sends an
  //     operator back to paste the same string.
  {
    const page = await (await browser.newContext()).newPage();
    const TRIP = 'template element "systemctl reload haproxy" byte-matches model-suggested text — the executed vector must be operator-AUTHORED';
    await mount(page, { queue: QUEUE(true), dossier: DOSSIER(true), writeStatus: 422, writeBody: TRIP });
    await openDossier(page);
    await page.evaluate(() => {
      const set = (id, v) => { const el = document.getElementById(id); el.value = v; };
      set('ocOp', 'reload'); set('ocFamily', 'service-lifecycle'); set('ocTier', 'low-reversible');
      set('ocArgv', 'systemctl reload haproxy'); set('ocRationale', 'pasted');
    });
    await page.click('#view [data-act="ratify"]');
    // Wait for the ERROR BANNER, not for the word "refused" anywhere in the view, and not for the
    // expected message. Scoping matters: the form's own hint says "pasting the model's string is refused
    // by the tripwire", so a /refused/ test over #view matches before the POST has even been issued and
    // the assertions below then grade a page that has not answered yet. Waiting on the banner ELEMENT and
    // asserting its CONTENT separately keeps a flattened message a named failure rather than a timeout.
    await page.waitForSelector('#view .oc-err', { timeout: 20000 }).catch(() => {});
    const text = await page.evaluate(() => document.querySelector('#view .oc-err')?.textContent || '');
    ok(text.includes('byte-matches model-suggested text'), '422: the tripwire\'s own message must reach the operator verbatim');
    ok(text.trim() !== 'refused: the template is inadmissible', `422: the server message must not be flattened to a generic refusal, got ${JSON.stringify(text)}`);
    await page.context().close();
  }

  // 8. A REFRESH MUST NOT EVICT THE OPERATOR. liveAdopt() ends with route(<current view>) and liveRefresh()
  //    calls it on a timer, so this view is rebuilt while an operator is mid-dossier. Rebuilding discards
  //    the dossier and every keystroke typed into the ratify form — on the surface where typing the command
  //    yourself IS the control. This oracle exists because the defect was real: the batch RED run below
  //    reproduced it as an intermittent bounce back to the queue.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { queue: QUEUE(true), dossier: DOSSIER(true) });
    await openDossier(page);
    await page.evaluate(() => { document.getElementById('ocArgv').value = 'systemctl reload ${unit}'; });
    // Drive the exact rebuild the poll drives — same entry point, no sleep involved.
    await page.evaluate(() => route('candidates'));
    const survived = await page.evaluate(() => {
      const el = document.getElementById('ocArgv');
      return { present: !!el, value: el ? el.value : null, questions: document.querySelectorAll('#view .oc-qn').length };
    });
    ok(survived.present && survived.questions === 5,
      `refresh: a re-render must leave the operator on the dossier, got ${JSON.stringify(survived)}`);
    ok(survived.value === 'systemctl reload ${unit}',
      `refresh: a re-render must not discard what the operator typed, got ${JSON.stringify(survived.value)}`);
    // …and the retained subtree must still be the LIVE one: leaving the dossier still works.
    await page.evaluate(() => { const b = [...document.querySelectorAll('#view button')].find(x => /back to the queue/i.test(x.textContent)); if (b) b.click(); });
    await page.waitForFunction(() => document.querySelectorAll('#view [data-act="open"]').length > 0, null, { timeout: 20000 }).catch(() => {});
    const back = await page.evaluate(() => document.querySelectorAll('#view [data-act="open"]').length);
    ok(back > 0, 'refresh: the retained subtree must stay interactive — back-to-the-queue must still work');
    await page.context().close();
  }

  // 7. the held ceiling reads as a sentence, never as a fraction.
  {
    const page = await (await browser.newContext()).newPage();
    await mount(page, { view: 'policy', queue: QUEUE(true), graduation: GRADUATION });
    await page.waitForFunction(() => document.querySelectorAll('#view .pol-grad, #view .pol-held').length > 0, null, { timeout: 20000 });
    const rows = await page.evaluate(() => [...document.querySelectorAll('#view tbody tr')].map(r => r.innerText.replace(/\s+/g, ' ')));
    const held = rows.find(r => r.includes('reload-haproxy'));
    const climbing = rows.find(r => r.includes('restart-mealie'));
    ok(!!held && /earned auto — awaiting an embed-export MR/.test(held),
      `ceiling: a held overlay class must say it earned auto and awaits an embed-export MR, got ${JSON.stringify(held)}`);
    ok(!!held && !/\b5\s*\/\s*5\b/.test(held),
      `ceiling: a held class must NOT render a fraction — that implies a promotion that cannot arrive, got ${JSON.stringify(held)}`);
    ok(!!climbing && /\b2\s*\/\s*5\b/.test(climbing),
      `ceiling: a class genuinely mid-second-climb must still show its progress against the NOTICE threshold, got ${JSON.stringify(climbing)}`);
    ok(!!held && /OVERLAY/.test(held), 'ceiling: the tamper domain must be a column — it is what decides whether the silent rung is reachable');
    ok(rows.some(r => r.includes('restart-service') && /EMBEDDED/.test(r)), 'ceiling: an embedded class must be marked EMBEDDED');
    // REQ-2817: the widened vocabulary appears in the EXISTING graduation view, under its existing caption.
    // Read the caption off the section that actually CONTAINS the rows — a page-wide text match would be
    // satisfied by the policy intro paragraph, which already says "earned autonomy" in passing.
    const section = await page.evaluate(() => {
      const tr = [...document.querySelectorAll('#view tbody tr')].find(r => r.innerText.includes('reload-haproxy'));
      const strip = tr && tr.closest('.pol-strip');
      return { heading: strip ? (strip.querySelector('h3')?.textContent || '') : null, rung: tr ? tr.innerText : '' };
    });
    ok(/auto notice/i.test(section.rung), 'one-ladder: the auto-notice rung must appear in the existing graduation view');
    ok(section.heading === 'Earned autonomy — clean runs toward promotion',
      `one-ladder: the widened rows must sit under the EXISTING ladder's caption, not a new section, got ${JSON.stringify(section.heading)}`);
    await page.context().close();
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error('candidates-ratify FAILURES:\n - ' + failures.join('\n - '));
  process.exit(1);
}
console.log('candidates-ratify: OK');
