// Screenshot the Command audit spine against a fixed mock, so a design change can be compared
// before/after on identical data rather than against a live table that moves under you.
// Usage: CONSOLE_BASE=... SHOT=/path/out.png node shot-command.mjs
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8137';
const OUT = process.env.SHOT || '/tmp/command-shot.png';

// The real distribution the live console showed: a few decisions among many unclassified alerts.
const sessions = [
  { external_ref: 'librenms-dc1-180872', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:55:46Z' },
  { external_ref: 'librenms-dc1-180873', band: 'POLL_PAUSE', risk_level: 'high', action_id: '522865eb11', verdict: 'match', classified_at: '2026-07-28T15:55:13Z' },
  { external_ref: 'librenms-dc1-180871', band: 'AUTO', risk_level: 'high', action_id: '522865eb11', verdict: 'match', classified_at: '2026-07-28T15:55:00Z' },
  { external_ref: 'librenms-dc1-180874', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:54:34Z' },
  { external_ref: 'tg-liveness-dc1wallos-mylab01', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:53:50Z' },
  { external_ref: 'librenms-dc1-180864', band: 'AUTO', risk_level: 'high', action_id: 'a5357f8c00', verdict: 'match', classified_at: '2026-07-28T15:48:58Z' },
  { external_ref: 'am-NodeMemoryMajorPagesFaults-192.168.85.22', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:47:14Z' },
  { external_ref: 'librenms-dc1-180861', band: 'AUTO', risk_level: 'high', action_id: 'b2a3522800', verdict: 'match', classified_at: '2026-07-28T15:44:35Z' },
  { external_ref: 'librenms-dc1-180848', band: 'POLL_PAUSE', risk_level: 'high', action_id: '47d1d00500', verdict: 'match', classified_at: '2026-07-28T15:35:14Z' },
  { external_ref: 'librenms-dc1-180855', band: 'AUTO_NOTICE', risk_level: 'medium', action_id: 'b16b01bd00', verdict: 'partial', classified_at: '2026-07-28T15:39:52Z' },
  { external_ref: 'librenms-dc1-180852', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:34:32Z' },
  { external_ref: 'am-TargetDown-seaweedfs-filer-client', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:29:49Z' },
  { external_ref: 'librenms-dc1-180850', band: 'POLL_PAUSE', risk_level: 'high', action_id: '47d1d00500', verdict: 'deviation', classified_at: '2026-07-28T15:35:10Z' },
  { external_ref: 'am-KubePodCrashLooping-10.0.6.211', band: '', risk_level: '', verdict: null, classified_at: '2026-07-28T15:19:17Z' },
];

const browser = await chromium.launch();
const page = await browser.newContext({ viewport: { width: 1600, height: 1000 } }).then(c => c.newPage());
await page.route('**/api/**', async route => {
  const p = route.request().url().split('/api')[1].split('?')[0];
  if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mode: 'Semi-auto', may_actuate: true, posture_stale: false, posture_source: 'runtime' } });
  if (p === '/v1/sessions') return route.fulfill({ json: { sessions } });
  if (p === '/v1/alerts') return route.fulfill({ json: { alerts: [], counts: { total: 1586, last_24h: 553 } } });
  if (p === '/v1/actions') return route.fulfill({ json: { actions: [], counts: { total: 109, verified: 95, deviations: 24 } } });
  return route.fulfill({ json: {} });
});
await page.goto(BASE + '/index.html#command', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('#appRoot:not([hidden])', { timeout: 20000 });
// #appRoot unhiding only proves liveAdopt() reached its post-whoami revealConsole() call, not that it
// finished — sessions/alerts/actions (what #command renders) are read later in the same sequential
// in-chain. liveState.lastRefresh is the flag liveAdopt() sets as its last statement, so waiting for it is
// the real "safe to screenshot" signal rather than a guess at how long the mocked reads take.
await page.waitForFunction(() => typeof liveState !== 'undefined' && liveState.lastRefresh != null).catch(() => {});
await page.screenshot({ path: OUT });
await browser.close();
console.log('shot ->', OUT);
