// A GOVERNED RUN THAT DEVIATED MUST NOT RENDER LIKE A CLEAN ONE.
//
// #workflows builds every row of the governed-run list with dtoToWfRun(null, row): `detail` is null, so `d`
// is {} and `nodes` is empty. The verdict was resolved from `(vf && vf.verdict) || d.verdict` — and for a
// list row BOTH are always undefined. Every row therefore carried verdict:null no matter what the audit
// spine said, and a run the deterministic verifier marked DEVIATION was visually identical to a matched one
// until an operator clicked into it and the detail walk loaded.
//
// /v1/sessions has emitted the field the whole time. core/httpapi/sessions.go:39:
//
//     // Verdict is the deterministic verifier's outcome for the bound action: match | partial |
//     // deviation, or empty when no verdict exists yet. Never authored here (INV-10).
//     Verdict string `json:"verdict,omitempty"`
//
// Nothing on the console read it. `grep -rn "row.verdict"` over the whole served bundle returned zero hits.
//
// This oracle drives the REAL projection function in the REAL served bundle rather than a copy of its logic:
// a test that re-implemented the precedence would pass over a bundle that had lost it.
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

  const got = await page.evaluate(() => {
    if (typeof dtoToWfRun !== 'function') return { missing: true };
    const row = (ref, verdict) => ({
      external_ref: ref, host: 'web01', alert_rule: 'Device-Down',
      band: 'AUTO', risk_level: 'medium', classified_at: '2026-08-01T10:00:00Z', verdict,
    });
    return {
      // The spine says deviation and there is no detail yet — the list must say deviation.
      deviation: dtoToWfRun(null, row('INC-1', 'deviation')).verdict,
      match: dtoToWfRun(null, row('INC-2', 'match')).verdict,
      partial: dtoToWfRun(null, row('INC-3', 'partial')).verdict,
      // No verdict on the spine ⇒ still none. A list row must never invent an outcome (INV-15).
      absent: dtoToWfRun(null, row('INC-4', '')).verdict,
      undef: dtoToWfRun(null, row('INC-5', undefined)).verdict,
      // Garbage on the row is not a verdict either — only the three real outcomes pass the filter.
      garbage: dtoToWfRun(null, row('INC-6', 'looks-fine')).verdict,
      // The DETAIL is the walked evidence for this run and must outrank the row's summary.
      detailWins: dtoToWfRun({ verdict: 'deviation' }, row('INC-7', 'match')).verdict,
      // …and the row must not overwrite a detail that legitimately has none.
      detailEmptyRowFills: dtoToWfRun({ verdict: '' }, row('INC-8', 'deviation')).verdict,
    };
  });

  check('dtoToWfRun is reachable in the served bundle', !got.missing,
    'the projection function is not global — this oracle would certify nothing');

  if (!got.missing) {
    check('a DEVIATION on the spine reaches the list row', got.deviation === 'deviation',
      `got ${JSON.stringify(got.deviation)} — a run the verifier marked deviation renders like a clean one`);
    check('a MATCH on the spine reaches the list row', got.match === 'match', `got ${JSON.stringify(got.match)}`);
    check('a PARTIAL on the spine reaches the list row', got.partial === 'partial', `got ${JSON.stringify(got.partial)}`);

    // The other direction, and it is not decoration: the fix must not turn "no verdict yet" into an outcome.
    check('an empty spine verdict stays unrendered', got.absent === null,
      `got ${JSON.stringify(got.absent)} — a session with no verdict must render pending, never an invented outcome`);
    check('a missing spine verdict stays unrendered', got.undef === null, `got ${JSON.stringify(got.undef)}`);
    check('an unrecognised verdict string is refused', got.garbage === null,
      `got ${JSON.stringify(got.garbage)} — only match/partial/deviation are real outcomes`);

    check('the fetched detail outranks the row summary', got.detailWins === 'deviation',
      `got ${JSON.stringify(got.detailWins)} — the walked evidence for this run must win`);
    check('the row fills in when the detail carries no verdict', got.detailEmptyRowFills === 'deviation',
      `got ${JSON.stringify(got.detailEmptyRowFills)}`);
  }
} finally {
  await browser.close();
}

console.log(failed ? `FAILED (${failed})` : 'PASS');
process.exit(failed ? 1 : 0);
