// Console e2e — the deep-link boot-race regression guard (the "landed directly on #workflows and it stuck on
// 'not reachable'" bug). It mocks /api with the sessions fetch DELAYED (as it is in the real adopt sequence,
// ~13th) and navigates DIRECTLY to /#workflows. It asserts: (a) during the delay the view shows a quiet
// "Loading…" — never the alarming "not reachable"; (b) after adopt the view RE-RENDERS to the real wfView walk
// (proving liveAdopt re-renders whatever view was landed on, not only #command); (c) no JS error. Exits
// non-zero on failure. Run via ./run.sh deeplink (or directly: CONSOLE_BASE=... node deeplink.mjs).
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

const REF = 'librenms-dc1-176728';
const detail = {
  id: REF, ref: REF, title: 'nginx service down', host: 'librespeed01', band: 'AUTO',
  status: 'executed', risk: 'low', conf: 0.82, action: 'a1b2c3', verdict: 'match',
  nodes: [
    { t: 'classify', lb: 'Risk classification', ts: '2026-07-21T09:47:01Z', st: 'ok', src: 'classify', band: 'AUTO', pay: 'admitted' },
    { t: 'agent-cycle', lb: 'cycle 1 — get-device-status', ts: '2026-07-21T09:47:03Z', st: 'ok', src: 'agent-cycle', pay: 'nginx.service failed', plan: ['get-device-status'] },
    { t: 'verify', lb: 'Mechanical verdict', ts: '2026-07-21T09:47:35Z', st: 'ok', src: 'verify', verdict: 'match' },
  ],
};
const sessions = { sessions: [{ external_ref: REF, band: 'AUTO', risk_level: 'low', verdict: 'match' }] };

async function mock(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mutation_enabled: false, posture_stale: false, posture_source: 'runtime', role: 'trace-read' } });
    if (p === '/v1/sessions') { await new Promise(r => setTimeout(r, 900)); return route.fulfill({ json: sessions }); } // DELAY: force the boot race
    if (p === '/v1/sessions/' + REF) return route.fulfill({ json: detail });
    if (p.endsWith('/stream')) return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: `event: snapshot\ndata: ${JSON.stringify(detail)}\n\nevent: done\ndata: {"status":"executed"}\n\n` });
    return route.fulfill({ json: {} });
  });
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await mock(page);

  // Land DIRECTLY on #workflows (the owner's entry) — boot + adopt run with sessions arriving late.
  await page.goto(BASE + '/index.html#workflows', { waitUntil: 'domcontentloaded' });

  // During the sessions delay: quiet loading, never "not reachable".
  await page.waitForTimeout(400);
  const mid = await page.evaluate(() => ({
    notReachable: /isn't reachable|not served|may still be deploying/i.test(document.body.innerText),
    loading: /Loading the recorded sessions/i.test(document.body.innerText),
  }));
  ok(!mid.notReachable, 'mid-boot the view showed the alarming "not reachable" instead of a quiet loading state');

  // After adopt completes: the view MUST re-render to the real wfView walk (the core fix).
  await page.waitForTimeout(1600);
  const done = await page.evaluate(() => ({
    wfView: !!document.querySelector('.wf-wrap'),
    stillNotReachable: /isn't reachable|not served|may still be deploying/i.test(document.body.innerText),
    stillLoading: /Loading the recorded sessions/i.test(document.body.innerText),
    runCount: [...document.querySelectorAll('.wf-run')].length,
    hasRealWalk: /Risk classification|prediction\.commit|Mechanical verdict/i.test(document.body.innerText),
  }));
  ok(done.wfView, 'after adopt the beautiful wfView did not render on the deep-linked view');
  ok(!done.stillNotReachable, 'the view stayed STUCK on "not reachable" after sessions loaded (the deep-link bug)');
  ok(!done.stillLoading, 'the view stayed stuck on "Loading…" — it never re-rendered after adopt');
  ok(done.hasRealWalk, 'the real governed walk did not render after the delayed sessions arrived');
  ok(pageErrors.length === 0, 'uncaught JS errors: ' + pageErrors.join(' | '));
} finally { await browser.close(); }

if (failures.length) { console.error('DEEPLINK E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('DEEPLINK E2E PASS — landing directly on #workflows shows a quiet loading state, then re-renders to the real walk after adopt (no stuck "not reachable").');
