// A RE-RENDER MUST NOT THROW THE OPERATOR OFF THE PAGE.
//
// #workflows was armed for the keyboard in an earlier change — every run row, stage header and ReAct kid row
// got role, tabindex and Enter/Space activation. That was half the job. Both panes re-render by
// replaceChildren(), which DETACHES the focused node, so measured live on the deployed bundle:
//
//   * 15 of 15 stage toggles moved document.activeElement to <body> on activation. The aria-expanded flip
//     therefore happened on a node the screen reader had already been thrown off: the utterance is silence.
//   * every run activation did the same, AND announced nothing — the entire consequence of the action (the
//     right-hand pane rewriting into 15 stage disclosures) was unspoken.
//   * worst: it happened with NO USER INPUT AT ALL. The 1s liveness tick and the live poll re-render on their
//     own, so an operator who tabbed 34 times to reach a run lost the reading cursor within 20 seconds of
//     arriving, forever, with nothing they did causing it.
//
// A focus-destroying re-render is the same defect as an unfocusable div, arriving on a timer. This oracle
// drives the REAL render entry points (wfRenderList / wfRenderMain — the ones the poll calls), not a
// hand-rolled imitation, and asserts over EVERY toggle in the pane rather than one sample.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { // Reveal the console the way the APP does. Poking #appRoot.hidden used to be enough; since the auth
    // gate became a real modal it also inerts #appRoot, so a bare hidden=false leaves a console that is
    // visible and cannot take focus — which is the correct production behaviour for an unauthenticated
    // page, and the reason this line must go through setGate rather than reach past it.
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    else { const r = document.querySelector('#appRoot'); if (r) r.hidden = false; } route('workflows'); });
  await page.waitForFunction(() => document.querySelectorAll('.wf-list [role=option]').length >= 5 && document.querySelectorAll('.wf-main [role=button][aria-expanded]').length >= 3);

  // ---- 0. the pane is populated and every focusable node carries a stable identity ----
  const census = await page.evaluate(() => {
    const opts = Array.from(document.querySelectorAll('.wf-list [role=option]'));
    const togs = Array.from(document.querySelectorAll('.wf-main [role=button][aria-expanded]'));
    return {
      opts: opts.length, togs: togs.length,
      optNoKey: opts.filter(o => !o.getAttribute('data-wf-key')).length,
      togNoKey: togs.filter(o => !o.getAttribute('data-wf-key')).length,
      dupKeys: (() => { const all = [...opts, ...togs].map(e => e.getAttribute('data-wf-key')); return all.length - new Set(all).size; })(),
    };
  });
  check('run list is populated', census.opts >= 5, `${census.opts} options`);
  check('stage toggles are populated', census.togs >= 3, `${census.togs} toggles`);
  check('EVERY run option has a stable key', census.optNoKey === 0, `${census.optNoKey} without data-wf-key`);
  check('EVERY stage toggle has a stable key', census.togNoKey === 0, `${census.togNoKey} without data-wf-key`);
  check('keys are unique', census.dupKeys === 0, `${census.dupKeys} duplicates`);

  // ---- 1. THE UNPROVOKED CASE: a re-render with no user input must not move focus ----
  // This is the one that mattered most — the poll and the 1s tick both land here.
  const spontaneous = await page.evaluate(() => {
    const list = document.querySelector('.wf-list'), main = document.querySelector('.wf-main');
    const opt = list.querySelectorAll('[role=option]')[3];
    opt.focus();
    const before = document.activeElement.getAttribute('data-wf-key');
    wfRenderList(list, main);            // exactly what the liveness tick / poll calls
    const afterList = document.activeElement === document.body ? 'BODY' : document.activeElement.getAttribute('data-wf-key');
    wfRenderMain(main);                  // the other pane re-rendering must not steal it either
    const afterMain = document.activeElement === document.body ? 'BODY' : document.activeElement.getAttribute('data-wf-key');
    return { before, afterList, afterMain };
  });
  check('focused run survives a list re-render', spontaneous.afterList === spontaneous.before, `${spontaneous.before} -> ${spontaneous.afterList}`);
  check('focused run survives the OTHER pane re-rendering', spontaneous.afterMain === spontaneous.before, `${spontaneous.before} -> ${spontaneous.afterMain}`);

  // ---- 2. same for a focused stage toggle in the detail pane ----
  const detailSurvive = await page.evaluate(() => {
    const main = document.querySelector('.wf-main');
    const t = main.querySelectorAll('[role=button][aria-expanded]')[1];
    t.focus();
    const before = document.activeElement.getAttribute('data-wf-key');
    wfRenderMain(main);
    return { before, after: document.activeElement === document.body ? 'BODY' : document.activeElement.getAttribute('data-wf-key') };
  });
  check('focused stage toggle survives a detail re-render', detailSurvive.after === detailSurvive.before, `${detailSurvive.before} -> ${detailSurvive.after}`);

  // ---- 3. EVERY toggle: real Enter, state flips, focus stays, and the change is announced ----
  const n = await page.evaluate(() => document.querySelectorAll('.wf-main [role=button][aria-expanded]').length);
  let lostFocus = 0, noFlip = 0, silent = 0;
  for (let i = 0; i < n; i++) {
    const r = await page.evaluate(async (idx) => {
      const main = document.querySelector('.wf-main');
      const t = main.querySelectorAll('[role=button][aria-expanded]')[idx];
      if (!t) return null;
      const key = t.getAttribute('data-wf-key'), was = t.getAttribute('aria-expanded');
      document.querySelector('#tgAnnounce').textContent = '';
      t.focus();
      t.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }));
      await new Promise(r2 => setTimeout(r2, 30));
      const now = main.querySelector('[data-wf-key="' + CSS.escape(key) + '"]');
      return {
        focusKey: document.activeElement === document.body ? 'BODY' : document.activeElement.getAttribute('data-wf-key'),
        key, was, nowExp: now ? now.getAttribute('aria-expanded') : null,
        ann: document.querySelector('#tgAnnounce').textContent.trim(),
      };
    }, i);
    if (!r) continue;
    if (r.focusKey !== r.key) lostFocus++;
    if (r.nowExp === r.was) noFlip++;
    if (!/expanded|collapsed/i.test(r.ann)) silent++;
  }
  check('toggles were exercised', n >= 3, `${n}`);
  check(`ALL ${n} toggles keep focus through Enter`, lostFocus === 0, `${lostFocus} lost focus (was 15/15)`);
  check(`ALL ${n} toggles flip aria-expanded`, noFlip === 0, `${noFlip} did not flip`);
  check(`ALL ${n} toggles announce the new state`, silent === 0, `${silent} silent`);

  // ---- 4. activating a run keeps focus AND says what changed ----
  const act = await page.evaluate(async () => {
    const list = document.querySelector('.wf-list');
    const opts = list.querySelectorAll('[role=option]');
    const target = Array.from(opts).find(o => o.getAttribute('aria-selected') !== 'true') || opts[1];
    const key = target.getAttribute('data-wf-key');
    document.querySelector('#tgAnnounce').textContent = '';
    target.focus();
    target.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }));
    await new Promise(r => setTimeout(r, 80));
    return {
      key,
      focusKey: document.activeElement === document.body ? 'BODY' : document.activeElement.getAttribute('data-wf-key'),
      sel: (document.querySelector('.wf-list [aria-selected=true]') || {}).getAttribute?.call(document.querySelector('.wf-list [aria-selected=true]'), 'data-wf-key'),
      ann: document.querySelector('#tgAnnounce').textContent.trim(),
    };
  });
  check('activating a run keeps focus on the run', act.focusKey === act.key, `${act.key} -> ${act.focusKey}`);
  check('activating a run moves aria-selected', act.sel === act.key, `selected=${act.sel} activated=${act.key}`);
  check('activating a run announces the consequence', act.ann.length > 20 && /stage/i.test(act.ann), JSON.stringify(act.ann));

  // ---- 5. the disclosure caret is not SPOKEN ----
  // The state was duplicated into the accessible NAME as a glyph: "agent-loop — …Z ▸" is spoken by
  // NVDA/eSpeak as "black right-pointing small triangle" at the end of every stage header. aria-expanded
  // already carries the fact. This asks CHROME'S OWN accname engine what the name is — an innerText check
  // would be blind to aria-hidden and would report a leak that no screen reader ever hears.
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('DOM.enable'); await cdp.send('Accessibility.enable');
  const { root } = await cdp.send('DOM.getDocument', { depth: -1 });
  const { nodeIds } = await cdp.send('DOM.querySelectorAll', { nodeId: root.nodeId, selector: '.wf-main [role=button][aria-expanded]' });
  const names = [];
  for (const nodeId of nodeIds) {
    const { nodes } = await cdp.send('Accessibility.getPartialAXTree', { nodeId, fetchRelatives: false });
    const self = nodes.find(n => n.nodeId !== undefined && n.name);
    names.push(self && self.name ? String(self.name.value || '') : '');
  }
  const leaked = names.filter(nm => /[\u25b8\u25be\u25b6\u25bc]/.test(nm));
  check('names were computed by the browser', names.filter(Boolean).length >= 3, `${names.filter(Boolean).length}/${nodeIds.length} named`);
  check('no caret glyph reaches the accessible name', leaked.length === 0, `${leaked.length} leak, e.g. ${JSON.stringify(leaked.slice(0, 2))}`);
} finally { await browser.close(); }

console.log(failed ? `workflows-focus: ${failed} FAILED` : 'workflows-focus: all checks passed');
process.exit(failed ? 1 : 0);
