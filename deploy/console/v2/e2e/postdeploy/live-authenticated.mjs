// LIVE, AUTHENTICATED — the half no stubbed oracle can reach.
//
// The other 60 oracles grade the deployed BYTES, and 42 of them intercept /v1/** — INCLUDING /v1/whoami,
// the call that flips liveState.on. That proves the rendering path against synthetic rows and cannot
// prove the console renders THIS control plane's real data. Every one of them passes on a page that would
// show an operator nothing.
//
// This one does the other half: a real operator session, zero interception, against the TLS origin the
// deployment actually serves. It is the difference between "the code is right" and "it works here".
//
// RUN IT:
//   curl -c cookies -X POST -H "X-TG-Operator: <name>" -H "Authorization: Bearer <operator token>" \
//        -H "Origin: <base>" <base>/api/v1/session
//   TG_SESSION=$(awk -F'\t' '$6=="tg_session"{print $7}' cookies | tail -1) \
//   CONSOLE_BASE=<base> node deploy/console/v2/e2e/live-authenticated.mjs
//
// THE SESSION COOKIE IS Secure, so the base MUST be https. Pointed at a plain-http origin the login
// returns 200, the browser discards the cookie, and the console silently stays on the unauthenticated
// preview shell — which is exactly what an earlier run of this check mistook for a broken console.
//
// It is NOT part of the default suite: it needs a credential and a reachable deployment, and a gate that
// cannot run everywhere becomes a gate people disable. Run it after a deploy.
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE;
const COOKIE = process.env.TG_SESSION;
let failed = 0;
const check = (n, ok, d) => { console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${n}${ok ? '' : ' — ' + d}`); if (!ok) failed++; };

const browser = await chromium.launch({ args: ['--ignore-certificate-errors'] });
try {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
  await ctx.addCookies([{ name: 'tg_session', value: COOKIE, domain: new URL(BASE).hostname, path: '/', secure: true, httpOnly: true, sameSite: 'Strict' }]);
  const page = await ctx.newPage();
  const apiCalls = [];
  const errors = [];
  page.on('response', r => { const u = r.url(); if (u.includes('/api/v1/')) apiCalls.push(`${r.status()} ${u.split('/api')[1].split('?')[0]}`); });
  page.on('pageerror', e => errors.push(String(e)));

  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(6000);

  const live = await page.evaluate(() => (typeof liveState === 'object' ? liveState.on : null));
  check('the console authenticates and enters the LIVE shell', live === true,
    `liveState.on = ${live} — the page is still the unauthenticated preview`);

  const ok200 = apiCalls.filter(c => c.startsWith('200')).length;
  const un401 = apiCalls.filter(c => c.startsWith('401')).length;
  check('real /v1 reads succeed against this control plane', ok200 > 3 && un401 === 0,
    `${ok200} x 200, ${un401} x 401 — ${apiCalls.slice(0, 8).join(' | ')}`);

  const body = await page.evaluate(() => document.body.innerText);
  // The fixture banner must be ABSENT on a genuinely live shell — this is claim (c) against real data.
  const banner = await page.evaluate(() => {
    const b = document.getElementById('fixtureBanner');
    return !b ? 'absent' : (b.hidden || getComputedStyle(b).display === 'none' ? 'hidden' : 'VISIBLE');
  });
  check('the global fixture banner is not showing on a real live shell', banner !== 'VISIBLE', `banner=${banner}`);
  check('no "fixture · representative" marker survives on live data', !/fixture · representative/i.test(body),
    'a fixture marker is rendered beside real rows');

  // Claim (d) against the REAL DTO. Asserted on CONTENT, not on a DOM shape: the sessions view does not
  // use a <table>, and an earlier version of this check asserted `table.tbl tbody tr` and "failed" against
  // a view that was rendering correctly. An oracle that names a symptom passes every bug wearing a
  // different one — and fails every healthy page wearing a different one too.
  await page.evaluate(() => route('sessions'));
  await page.waitForTimeout(2500);
  const sess = await page.evaluate(() => (document.querySelector('#appRoot') || document.body).innerText);
  const realIds = (sess.match(/librenms-[a-z-]+-\d{6}/g) || []).length;
  check('the sessions view renders REAL session identifiers from the server dto', realIds > 0,
    `no server-shaped session id rendered; ${sess.length} chars of view text`);
  // And where the operator's role is insufficient, it must SAY so rather than render an empty view.
  const roleHonest = !/does not carry the trace-read role/.test(sess) || /Sign in again/.test(sess);
  check('an insufficient role is stated, not rendered as emptiness', roleHonest,
    'the view degraded silently instead of naming the missing role');

  // Claim (b) against real data: estatedepth must state absences, never draw a synthetic series.
  await page.evaluate(() => route('estatedepth'));
  await page.waitForTimeout(2000);
  const ed = await page.evaluate(() => document.body.innerText);
  check('#estatedepth states live absences on real data', /not drawn|not rendered/i.test(ed) && !/synthetic/i.test(ed),
    'it drew something synthetic, or stated no absence');

  check('no page errors across the authenticated load', errors.length === 0, errors.slice(0, 2).join(' | '));
  console.log(`\n  [${apiCalls.length} api calls, ${ok200} ok, ${un401} unauthenticated]`);
} finally {
  await browser.close();
}
console.log(failed ? `FAILED (${failed})` : 'PASS');
process.exit(failed ? 1 : 0);
