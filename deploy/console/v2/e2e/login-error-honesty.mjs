// A CONTROL PLANE THAT IS DOWN MUST NOT READ AS A WRONG PASSWORD.
//
// The login gate mapped EVERY non-429 failure to "unauthenticated — check the operator name and token".
// I hit the consequence myself on 2026-07-29: a transient OpenBao stall at boot made the grounder disable
// browser sessions, which UN-REGISTERS /v1/session — so POST returned 404 while every other route returned
// 401. The gate rendered that 404 as a credential error, and the first minutes of a live outage went into
// re-checking a credential that was never the problem.
//
// The status IS the diagnosis and it was being discarded. On a console that owns the KILL switch and the
// approval queue, "check your password" during an outage is the most expensive wrong sentence available.
//
// This oracle asserts over the CLOSED SET of statuses the login POST can return — not one hand-picked
// sample — because the defect was precisely that one branch answered for all of them.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

// The closed set. `credential` marks the ONLY statuses that may blame the operator's credentials.
const CASES = [
  { status: 401, credential: true, why: 'genuinely unauthenticated' },
  { status: 403, credential: true, why: 'genuinely forbidden' },
  { status: 404, credential: false, why: 'THE OUTAGE: session route not registered' },
  { status: 500, credential: false, why: 'control plane failing' },
  { status: 502, credential: false, why: 'gateway down mid-deploy' },
  { status: 503, credential: false, why: 'control plane unavailable' },
];

const browser = await chromium.launch();
try {
  const page = await (await browser.newContext()).newPage();
  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  // Nothing here needs live data or a reveal — every check below calls loginFailureText() directly or
  // inspects liveLogin's source, both script globals set at parse time. Wait for the exact two the file uses,
  // rather than a guess at how long parsing takes.
  await page.waitForFunction(() => typeof loginFailureText === 'function' && typeof liveLogin === 'function').catch(() => {});

  const fn = await page.evaluate(() => typeof loginFailureText);
  check('loginFailureText is reachable', fn === 'function', `typeof = ${fn}`);
  if (fn !== 'function') { console.log('login-error-honesty: 1 FAILED'); process.exit(1); }

  const texts = await page.evaluate(codes => codes.map(c => ({ status: c, text: loginFailureText(c) })), CASES.map(c => c.status));
  const byStatus = Object.fromEntries(texts.map(t => [t.status, t.text]));

  // Every status must produce a non-empty sentence, and every sentence must be distinguishable from the
  // credential one where the cause is NOT a credential.
  const credentialish = /check the operator name|check your password|wrong password|check the operator name and token/i;
  for (const c of CASES) {
    const t = byStatus[c.status] || '';
    check(`${c.status} produces a message`, t.trim().length > 10, `"${t}"`);
    if (c.credential) {
      check(`${c.status} (${c.why}) DOES blame the credential`, credentialish.test(t), `"${t}"`);
    } else {
      check(`${c.status} (${c.why}) does NOT blame the credential`, !credentialish.test(t),
        `"${t}" — this is the defect: a server-side failure told the operator to re-check a password`);
      check(`${c.status} says it is not the password`, /not your password|not necessarily your password/i.test(t), `"${t}"`);
      check(`${c.status} names the status so a screenshot is diagnosable`, t.includes(String(c.status)), `"${t}"`);
    }
  }

  // The 404 is the one that actually happened: it must name the mechanism and a remedy an operator can act
  // on, because "retry" cannot fix an unregistered route.
  const t404 = byStatus[404] || '';
  check('404 names the unregistered session route', /session route is not registered|not registered/i.test(t404), `"${t404}"`);
  check('404 points at the grounder log line that diagnoses it', /browser sessions disabled/i.test(t404), `"${t404}"`);

  // ★ THE ADVICE MUST FIT THE CAUSE, WHICH IS WHAT SEPARATES THE BRANCHES.
  // Deleting the 5xx branch was a mutation control that would NOT go red: the generic fallback also avoids
  // blaming the credential and also names the status, so everything asserted above still passed and the
  // branch was untested. What ONLY the 5xx branch provides is the right ACTION — a 5xx is transient, so
  // "retry" helps; a 404 is an unregistered route, where retrying is exactly the useless advice that made
  // the original single sentence so costly. Assert the advice, not just the blame.
  for (const st of [500, 502, 503]) {
    check(`${st} advises a retry (it is transient)`, /retry/i.test(byStatus[st] || ''), `"${byStatus[st]}"`);
  }
  check('404 does NOT advise a retry (retrying cannot register a route)', !/\bretry\b/i.test(t404), `"${t404}"`);

  // The distinct-message check: the defect was ONE branch answering for all of them. If any two
  // non-credential statuses share a message with the credential one, the collapse is back.
  const distinct = new Set(CASES.map(c => byStatus[c.status]));
  check('the statuses do not collapse to one message', distinct.size >= 4, `${distinct.size} distinct messages across ${CASES.length} statuses`);

  // And the gate must actually USE it — a correct helper with no call site proves nothing.
  const wired = await page.evaluate(() => String(liveLogin).includes('loginFailureText'));
  check('liveLogin actually calls loginFailureText', wired,
    'the helper is correct and unused — the gate would still print the old sentence');
} finally { await browser.close(); }

console.log(failed ? `login-error-honesty: ${failed} FAILED` : 'login-error-honesty: all checks passed');
process.exit(failed ? 1 : 0);
