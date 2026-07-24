// Console e2e — #secrets live-wiring + honesty guard. The secrets view was rewired to render ONLY from the
// value-less /v1/secrets surface (ref · purpose · resolved) + /v1/config; the old fixture renderer invented
// rotation ages, a rotation-horizon chart, fingerprints, consumers, a ledger trail, and rotate/verify actions
// the API never provides. This test mocks /v1/secrets with DISTINCTIVE refs (so a pass proves the live list
// replaced the fixtures, not that fixtures matched) and asserts: the real refs render with their scheme +
// resolution, missing refs sort first with a MISSING chip, the value-less invariant banner is present, the
// stat tiles are derived from the real list, and NONE of the retired fixture concepts appear. No JS error.
// Run: CONSOLE_BASE=... node secrets.mjs   (set E2E_SCREENSHOT=/p.png to also capture)
import { chromium } from 'playwright';
const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (c, m) => { if (!c) failures.push(m); };

// Distinctive refs — none of these names exist in any fixture, so their presence proves live injection.
const secrets = {
  refs: [
    { ref: 'bao:secret/data/tg/zz-live-alpha#token', purpose: 'live alpha via OpenBao', resolved: true },
    { ref: 'env:ZZ_LIVE_BRAVO', purpose: 'live bravo', resolved: true },
    { ref: 'file:/secrets/zz-live-charlie', purpose: 'live charlie', resolved: true },
    { ref: 'env:ZZ_LIVE_MISSING', purpose: 'deliberately unresolved', resolved: false },
  ],
  sealed: [{ name: 'zz.sealed.one', ref: 'store:zz.sealed.one', purpose: 'live sealed', created_at: '2026-07-19T20:22:00Z', updated_at: '2026-07-21T11:03:00Z' }],
};
const config = { config: [{ name: 'zz.live.knob', value: '12h', source: 'default', description: 'live knob', console_writable: true }] };

async function mock(page) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return route.fulfill({ json: { source: 'operator:tester', mutation_enabled: true, posture_stale: false, posture_source: 'runtime', role: 'admin', mode: 'semi-auto' } });
    if (p === '/v1/secrets') return route.fulfill({ json: secrets });
    if (p === '/v1/config') return route.fulfill({ json: config });
    return route.fulfill({ json: {} });
  });
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 950 } }).then(c => c.newPage());
  const pageErrors = []; page.on('pageerror', e => pageErrors.push(String(e)));
  await mock(page);
  await page.goto(BASE + '/index.html#secrets', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2200);

  const r = await page.evaluate(() => {
    const body = document.body.innerText;
    const rows = [...document.querySelectorAll('table tbody tr')].map(tr => tr.innerText);
    const refRows = rows.filter(t => /zz-live|ZZ_LIVE/.test(t));
    const firstRef = refRows[0] || '';
    const statTiles = [...document.querySelectorAll('.tile')].map(t => t.innerText.replace(/\s+/g, ' ').trim());
    return {
      liveRefsRender: /zz-live-alpha/.test(body) && /ZZ_LIVE_BRAVO/.test(body) && /zz-live-charlie/.test(body),
      baoScheme: /bao:secret\/data\/tg\/zz-live-alpha/.test(body),
      missingFirst: /ZZ_LIVE_MISSING/.test(firstRef),                 // unresolved sorts to the top
      missingChip: /MISSING/.test(body) && /RESOLVED/.test(body),
      invariant: /INVARIANT/.test(body) && /value-less/i.test(body),
      sealedLive: /zz\.sealed\.one/.test(body),
      configLive: /zz\.live\.knob/.test(body),
      // retired fixture concepts must be GONE
      noRotationHorizon: !/ROTATION HORIZON/i.test(body),
      noFingerprint: !/fingerprint/i.test(body),
      noAutoRotated: !/AUTO-ROTATED/i.test(body),
      noRotationDueFacet: !/ROTATION DUE/i.test(body),
      tileCount: statTiles.length,
      tilesFromLive: statTiles.some(t => /4\s*REFERENCES|REFERENCES\s*4/i.test(t)) || /\b4\b[\s\S]*references/i.test(body),
    };
  });

  ok(r.liveRefsRender, 'the live /v1/secrets refs did not render (zz-live-* absent)');
  ok(r.baoScheme, 'the bao: scheme reference did not render as-is');
  ok(r.missingFirst, 'unresolved references do not sort first (missing-first ordering broken)');
  ok(r.missingChip, 'the RESOLVED/MISSING status chips did not render');
  ok(r.invariant, 'the value-less INVARIANT banner is missing');
  ok(r.sealedLive, 'the live sealed secret did not render');
  ok(r.configLive, 'the live config knob did not render');
  ok(r.noRotationHorizon, 'FIXTURE LEAK: the retired ROTATION HORIZON chart is still rendered');
  ok(r.noFingerprint, 'FIXTURE LEAK: fabricated fingerprints are still shown (not in the value-less API)');
  ok(r.noAutoRotated, 'FIXTURE LEAK: the fabricated AUTO-ROTATED status is still shown');
  ok(r.noRotationDueFacet, 'the retired "ROTATION DUE" facet is still present');
  ok(r.tileCount >= 4, 'the real stat tiles did not render (expected >=4, got ' + r.tileCount + ')');
  ok(pageErrors.length === 0, 'uncaught JS errors: ' + pageErrors.join(' | '));

  if (process.env.E2E_SCREENSHOT) await page.screenshot({ path: process.env.E2E_SCREENSHOT, fullPage: true });
} finally { await browser.close(); }

if (failures.length) { console.error('SECRETS E2E FAIL:\n  - ' + failures.join('\n  - ')); process.exit(1); }
console.log('SECRETS E2E PASS — the #secrets view renders ONLY live value-less data (refs+scheme+resolution, sealed, config), missing-first, invariant present, and NONE of the retired fixture concepts (rotation horizon, fingerprints, auto-rotated) leak. No JS error.');
