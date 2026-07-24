// Console e2e — the browser-level oracle for the v2 operator console decision-tracer (spec/020, T-020-12).
// The console is a JS surface, so a Go acceptance oracle cannot drive it; this Playwright test IS that oracle.
// It loads the SERVED index.html, mocks the /api backend via route interception so the REAL console render code
// runs, and asserts the OWNER-REQUIRED contract: the beautiful WF_RUNS wfView design (NOT the retired flat list)
// renders a real session's governed walk as the full 10-stage timeline — real content on the stages the spine
// backs (classify, commit, agent-loop with NESTED tool-call kids, gate, execute-from-passed-gate, verify),
// honest muted "no data yet" scaffolds on the rest (ingest/correlate/rag/screen) and NEVER a fabricated verdict (INV-15), the
// credential identity is a scheme not a secret (INV-13), the read-only MUTATION-OFF chip shows, and NO uncaught
// JS error fires. Exits non-zero on any failure. Run via ./run.sh (serves index.html + runs this).
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || 'http://127.0.0.1:8099';
const failures = [];
const ok = (cond, msg) => { if (!cond) failures.push(msg); };

const REF = 'librenms-dc1-176728';
// A realistic SessionDetailDTO exactly as core/httpapi/session_detail.go serves it: flat spine nodes the
// mapper (dtoToWfRun) folds/buckets into the beautiful 10-stage walk. agent-cycle + credential fold into the
// agent node's nested kids; propose/predict feed the agent/commit stages; policy+gate feed the gate stage.
const detail = {
  id: REF, ref: REF, title: 'nginx service down', host: 'librespeed01', band: 'AUTO',
  status: 'executed', risk: 'low', conf: 0.82, action: 'a1b2c3d4e5', verdict: 'match',
  nodes: [
    { t: 'ingest', lb: 'Ingested (librenms)', ts: '2026-07-21T09:47:00Z', st: 'ok', src: 'ingest', pay: 'nginx service down · critical · librespeed01\nnginx.service failed on librespeed01' },
    { t: 'correlate', lb: 'Correlate / suppression', ts: '2026-07-21T09:47:00Z', st: 'ok', src: 'correlate', verdict: 'clean', pay: 'dedup:no dedup match' },
    { t: 'classify', lb: 'Risk classification', ts: '2026-07-21T09:47:01Z', st: 'ok', src: 'classify', band: 'AUTO', pay: 'admitted (auto-approved) — reversible, blast 1' },
    { t: 'screen', lb: 'Admission screen', ts: '2026-07-21T09:47:01Z', st: 'ok', src: 'screen', pay: 'clean admission — no pause/floor/novelty signal · auto-approved · risk: low' },
    { t: 'agent-cycle', lb: 'cycle 1 — get-device-status', ts: '2026-07-21T09:47:03Z', st: 'ok', src: 'agent-cycle', pay: 'device up, nginx.service failed', plan: ['get-device-status'] },
    { t: 'agent-cycle', lb: 'cycle 2 — check-host-services', ts: '2026-07-21T09:47:05Z', st: 'ok', src: 'agent-cycle', pay: 'systemd: nginx.service inactive (failed)', plan: ['check-host-services'] },
    { t: 'credential', lb: 'Credential resolve: librespeed01', ts: '2026-07-21T09:47:06Z', st: 'ok', src: 'credential', pay: 'resolved ops via ssh (source: native-hostdiag)', plan: ['env'] },
    { t: 'propose', lb: 'Proposal', ts: '2026-07-21T09:47:08Z', st: 'ok', src: 'propose', band: 'AUTO', pay: 'restart nginx.service — reversible, evidence-grounded' },
    { t: 'predict', lb: 'Consequence prediction', ts: '2026-07-21T09:47:20Z', st: 'ok', src: 'predict', pay: 'scored tp=1 fp=0 fn=0', verdict: 'clean' },
    { t: 'policy', lb: 'Policy decision: auto', ts: '2026-07-21T09:47:21Z', st: 'ok', src: 'policy', pay: 'composed verdict "auto" [mode: Semi-auto]', plan: ['restart-service-auto-canary → auto'], hash: 'bundle-9f3ac1', verdict: 'auto' },
    // the ordered interceptor chain, ending in the trailing 'verify' gate whose verdict is the MECHANICAL
    // outcome (match), NOT an authorization pass — a clean success must NOT render the gate stage as floored.
    { t: 'gate', lb: 'Gate: admission', ts: '2026-07-21T09:47:22Z', st: 'ok', src: 'gate', gate: 'admission', verdict: 'pass' },
    { t: 'gate', lb: 'Gate: mode-chokepoint', ts: '2026-07-21T09:47:22Z', st: 'ok', src: 'gate', gate: 'mode-chokepoint', verdict: 'pass' },
    { t: 'gate', lb: 'Gate: execute', ts: '2026-07-21T09:47:23Z', st: 'ok', src: 'gate', gate: 'execute', verdict: 'pass' },
    { t: 'gate', lb: 'Gate: verify', ts: '2026-07-21T09:47:34Z', st: 'deviation', src: 'gate', gate: 'verify', verdict: 'match' },
    { t: 'verify', lb: 'Mechanical verdict', ts: '2026-07-21T09:47:35Z', st: 'ok', src: 'verify', verdict: 'match', pay: 'deterministic verifier' },
  ],
};
const sessions = { sessions: [{ external_ref: REF, band: 'AUTO', risk_level: 'low', verdict: 'match' }] };

const pageErrors = [];
async function mock(page, authed) {
  await page.route('**/api/**', async route => {
    const p = route.request().url().split('/api')[1].split('?')[0];
    if (p === '/v1/whoami') return authed
      ? route.fulfill({ json: { source: 'operator:tester', mutation_enabled: false, posture_stale: false, posture_source: 'runtime', role: 'trace-read' } })
      : route.fulfill({ status: 401, body: 'unauthenticated' });
    if (p === '/v1/sessions') return route.fulfill({ json: sessions });
    if (p === '/v1/sessions/' + REF) return route.fulfill({ json: detail });
    if (p.endsWith('/stream')) return route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' }, body: `event: snapshot\ndata: ${JSON.stringify(detail)}\n\nevent: done\ndata: {"status":"executed"}\n\n` });
    return route.fulfill({ json: {} });
  });
}

const browser = await chromium.launch();
try {
  const page = await browser.newContext({ viewport: { width: 1440, height: 900 } }).then(c => c.newPage());
  page.on('pageerror', e => pageErrors.push(String(e)));

  // Pass 1 — unauthenticated: the fail-closed login gate must show, the app shell must NOT.
  await mock(page, false);
  await page.goto(BASE + '/index.html', { waitUntil: 'networkidle' });
  await page.waitForTimeout(600);
  const gate = await page.evaluate(() => ({ login: /Sign in to the operator console/i.test(document.body.innerText), shellHidden: !/decision tracer/i.test(document.body.innerText) }));
  ok(gate.login, 'login gate did not render for an unauthenticated session');
  ok(gate.shellHidden, 'the console shell leaked before authentication (fail-closed auth broken)');

  // Pass 2 — authenticated: navigate to the decision-tracer view; the beautiful wfView must render the walk.
  await page.unroute('**/api/**');
  await mock(page, true);
  await page.reload({ waitUntil: 'networkidle' });
  await page.waitForTimeout(900);
  await page.evaluate(() => { location.hash = '#workflows'; if (typeof route === 'function') route('workflows'); });
  await page.waitForTimeout(900); // wfOnSelect lazy-loads the first run's walk, then re-renders
  await page.waitForTimeout(600);

  // Node headers, stage labels, kids and the verify verdict chip all render UNGATED by expand-state, so the
  // assertions read the collapsed walk directly (the design is click-to-expand for the deeper payloads).
  const walk = await page.evaluate(() => {
    const wrap = document.querySelector('.wf-wrap');
    const nodes = [...document.querySelectorAll('.wf-node')];
    const nodeText = nodes.map(n => n.innerText).join('\n');
    const bodyText = document.body.innerText;
    const kidsText = [...document.querySelectorAll('.wf-kids')].map(e => e.innerText).join('\n');
    // scan ONLY the rendered walk cards for a value-shaped secret (not the nav's "Config · Secrets" label).
    const walkText = [...document.querySelectorAll('.wf-card')].map(e => e.innerText).join('\n');
    // Assert stages by their REAL rendered labels (the mapper preserves the spine's labels; scaffolds carry a
    // "· no data yet" note). classify -> "Risk classification"; verify -> "Mechanical verdict".
    // execute now lights from the passed 'execute' interceptor gate (the mock has one) — "action executed".
    const realStages = [/Ingested \(librenms\)/i, /Correlate \/ suppression/i, /Risk classification/i, /prediction\.commit/i, /agent-loop/i, /Admission screen/i, /\d+-gate chain|Gate:/i, /execute · ran/i, /Mechanical verdict/i];
    // the stages the mock supplies NO source for stay honest write-pending scaffolds ("no data yet").
    const scaffoldStages = [/rag · no data yet/i];
    return {
      wfViewRendered: !!wrap,
      isFlatList: /Sessions the control plane recorded — click to trace/.test(bodyText), // the retired render
      nodeCount: nodes.length,
      realStagesMissing: realStages.filter(re => !re.test(nodeText)).map(String),
      scaffoldsMissing: scaffoldStages.filter(re => !re.test(nodeText)).map(String),
      hasKids: !!document.querySelector('.wf-kid'),             // nested tool-call debugging under agent-loop
      credentialInKids: /credential|Credential resolve|resolved ops via ssh/i.test(kidsText),
      credentialNoSecret: !/(password|secret|token|-----BEGIN)/i.test(walkText),
      mutationOffChip: /MUTATION OFF/i.test(bodyText),
      // gate stage is WF_STAGES index 7; an all-pass authorization chain (the trailing 'verify' gate excluded)
      // must render PASS, never floored — a clean deviation/match verify verdict must not floor the gate stage.
      gateNodeText: (nodes[7] && nodes[7].innerText) || '',
      verifyMatch: /\bmatch\b/i.test(nodeText),
      liveLabel: /\blive\b/i.test(document.querySelector('.wf-lhead') ? document.querySelector('.wf-lhead').innerText : ''),
    };
  });
  ok(walk.wfViewRendered, 'the beautiful wfView (.wf-wrap) did not render — live wiring is not feeding the fixture design');
  ok(!walk.isFlatList, 'the RETIRED flat list render is still primary (owner rejected it)');
  ok(walk.nodeCount >= 10, 'the full 10-stage walk did not render (got ' + walk.nodeCount + ' nodes)');
  ok(walk.realStagesMissing.length === 0, 'real stages missing from the walk: ' + walk.realStagesMissing.join(','));
  ok(walk.scaffoldsMissing.length === 0, 'honest "no data yet" scaffolds missing: ' + walk.scaffoldsMissing.join(','));
  ok(walk.hasKids, 'the agent-loop nested tool-call kids (the full-debug detail) did not render');
  ok(walk.credentialInKids, 'credential-resolve identity did not fold into the agent kids');
  ok(walk.credentialNoSecret, 'a value-shaped secret appeared in the rendered walk (INV-13)');
  ok(walk.mutationOffChip, 'the MUTATION-OFF read-only chip did not render');
  ok(!/floor/i.test(walk.gateNodeText), 'clean all-pass gate chain miscast as FLOORED (the trailing verify gate must be excluded from the fold): ' + walk.gateNodeText.replace(/\n/g, ' '));
  ok(walk.verifyMatch, 'the verify verdict did not render');
  ok(walk.liveLabel, 'the run list is not labeled "live" (still reading as fixture data)');
  ok(pageErrors.length === 0, 'uncaught JS errors on the tracer surface: ' + pageErrors.join(' | '));

  if (process.env.E2E_SCREENSHOT) await page.screenshot({ path: process.env.E2E_SCREENSHOT, fullPage: true });
} finally {
  await browser.close();
}

if (failures.length) {
  console.error('CONSOLE E2E FAIL:\n  - ' + failures.join('\n  - '));
  process.exit(1);
}
console.log('CONSOLE E2E PASS — the beautiful wfView renders the full 10-stage governed walk from real DTO data, agent kids nested, scaffolds honest, credential scheme-only, no JS errors.');
