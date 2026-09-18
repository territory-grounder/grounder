// THE MODULE CONFIGURATION DIALOG MUST BE REAL, NOT ANOTHER PREVIEW.
//
// This drawer already existed once as a design preview: every control present, every control disabled,
// "gated — design preview, no connector runtime". A complete form that could save nothing. That is the
// same defect class as a lane that is wired and never called — it looks like the feature is there.
//
// So this oracle asserts the properties that distinguish a working dialog from a convincing one:
//   * the fields are GENERATED from /v1/modules/schema, not hand-written
//   * Save actually posts, and reports the field's declared EFFECT rather than a bare "saved"
//   * the secret field is write-only and never renders a value
//   * Test discloses what it will do BEFORE it is pressed
//   * undescribed modules are listed, so the page cannot imply completeness
import { chromium } from 'playwright';

const BASE = process.env.CONSOLE_BASE || `http://127.0.0.1:${process.env.CONSOLE_E2E_PORT || '8137'}`;
let failed = 0;
const check = (name, ok, detail) => {
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' — ' + detail}`);
  if (!ok) failed++;
};

const SCHEMA = {
  modules: [{
    // enabled:false + enabled_known:false is the shape the API actually returns for every worker-resident
    // connector. It must NOT render as "not configured" — that is the claim that told an operator a
    // working Matrix notifier was switched off.
    surface: 'notifier', source_type: 'matrix', title: 'Matrix', summary: 'Governance notices.',
    enabled: false, enabled_known: false, has_secret: true, test_verb: 'post a test message to the approvals room',
    fields: [
      { name: 'homeserver', env_key: 'TG_MATRIX_HOMESERVER', config_key: 'module.notifier.matrix.homeserver',
        label: 'Homeserver URL', help: 'Base URL.', type: 'url', security: 'ordinary', effect: 'restart', required: true },
      { name: 'approvers', env_key: 'TG_MATRIX_APPROVERS', config_key: 'module.notifier.matrix.approvers',
        label: 'Allowed responders', help: 'Who may vote.', type: 'idlist', security: 'authority', effect: 'live', required: true },
      { name: 'token_ref', env_key: 'TG_MATRIX_TOKEN_REF', label: 'Access-token reference',
        type: 'secret_ref', security: 'ordinary', effect: 'readonly' },
      { name: 'token', label: 'Bot access token', type: 'secret_value', security: 'secret', effect: 'live' },
    ],
  }],
  // The reason is part of the contract. A bare name cannot distinguish a dialog somebody still owes from
  // a module that has nothing to configure, and the second entry here is deliberately the bare-string
  // shape a control plane predating the reason field would send — the console must still name it.
  undescribed: [
    { package: 'modules/model/ollama', reason: 'provider identity + default model ids only; the server address is LiteLLM\'s config, not TG\'s' },
    'modules/cmdb/netbox',
  ],
};

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  const posted = [];
  await page.route('**/api/v1/modules/schema', r => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(SCHEMA) }));
  await page.route('**/api/v1/config/**', r => { posted.push(r.request().url()); return r.fulfill({ status: 200, contentType: 'application/json', body: '{"ledger_seq":42}' }); });
  await page.route('**/api/v1/modules/*/*/secret', r => { posted.push(r.request().url()); return r.fulfill({ status: 200, contentType: 'application/json', body: '{"kv_path":"secret/data/tg/modules/notifier/matrix","field":"token"}' }); });
  await page.route('**/api/v1/modules/*/*/test', r => { posted.push(r.request().url()); return r.fulfill({ status: 200, contentType: 'application/json', body: '{"ok":true,"summary":"test message delivered to !room:example","elapsed_ms":230}' }); });

  await page.goto(`${BASE}/index.html`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => {
    if (typeof setGate === 'function') { setGate(false); const r = document.querySelector('#appRoot'); if (r) r.hidden = false; }
    if (typeof liveState === 'object') {
      liveState.on = true;
      liveState.caps = { capabilities: [{ surface: 'notifier', source_type: 'matrix', enabled: true }] };
      // liveState.config AS THE LIVE REFRESH LEAVES IT: an ARRAY (it is `page.config||[]` from the
      // {config:[...]} envelope). This suite drives the view directly and never runs that refresh, so
      // stubbing the route would never fire — the state has to be set here or the fixture is a different
      // program from production. Setting it is what makes the blank-page guard below able to fail: with
      // `liveState.config` undefined, the `.keys` bug short-circuited harmlessly and this suite stayed
      // green while the live Modules view rendered nothing at all (2026-08-02 → 2026-08-03).
      liveState.config = [
        { name: 'module.notifier.matrix.homeserver', value: 'https://matrix.example.test', source: 'console' },
        { name: 'session.ttl', value: '12h', source: 'env' },
      ];
    }
    route('modules');
  });
  // Wait for a schema-generated field to actually be on the page — the fields are fetched from
  // /v1/modules/schema and rendered asynchronously, so this is the real precondition for every assertion
  // below rather than a guess at how long that fetch + render takes.
  const schemaRendered = () => document.body.innerText.includes('Homeserver URL');
  await page.waitForFunction(schemaRendered).catch(() => {});
  await page.evaluate(() => { if (typeof render === 'function') render(); });
  await page.waitForFunction(schemaRendered).catch(() => {});

  const body = await page.evaluate(() => document.body.innerText);

  // THE BLANK-PAGE GUARD (2026-08-03). Every assertion below reads `body`; if the view throws while
  // rendering, body is nearly empty and a substring check like "does it NOT say X" passes vacuously.
  // Assert the view rendered at all, and name the module cards, BEFORE anything reads its text.
  check('the modules view renders its cards (not a blank page)',
    body.length > 400 && body.includes('Matrix'),
    `the Modules view rendered ${body.length} chars and no module card — it threw while rendering, and ` +
    'every content assertion after this one would pass vacuously on an empty page');

  check('fields are generated from the schema', body.includes('Homeserver URL') && body.includes('Allowed responders'),
    'the schema fields were not rendered — the form is not generated');

  // THE HONESTY LABELS. A `restart` field must say so; claiming otherwise is the Save-button lie.
  check('a restart-scoped field says it needs a restart', /next restart/i.test(body),
    'a boot-read field did not disclose that a save is inert until restart');
  check('a live field says it takes effect immediately', /takes effect immediately/i.test(body),
    'a per-use field did not disclose that a save lands now');

  // AUTHORITY marking: the approver set moves the trust boundary and must not look like a text box.
  check('the approver set is marked as authority', /authority/i.test(body),
    'the field that decides who may approve is rendered as ordinary text');

  // The secret must never render a value, and must say it cannot be read back.
  check('the secret field is write-only', /never read back|write-only/i.test(body),
    'the secret field does not disclose that it cannot be read back');
  const secretInputType = await page.evaluate(() =>
    (Array.from(document.querySelectorAll('input')).find(i => (i.getAttribute('aria-label')||'').includes('Bot access token'))||{}).type);
  check('the secret input is masked', secretInputType === 'password', `input type is ${secretInputType}`);

  // TEST discloses its verb BEFORE the press — an unlabelled TEST button is not consent.
  check('Test discloses what it will do', body.includes('post a test message to the approvals room'),
    'the Test button does not say what pressing it does');

  // A module this process cannot observe must not be reported as switched off. Compared case-folded:
  // the label is CSS-uppercased, so innerText carries the rendered casing, not the source string.
  const bodyLower = body.toLowerCase();
  check('unobservable state is not rendered as "not configured"', !bodyLower.includes('not configured'),
    'a worker-resident module the API cannot see is claimed to be disabled');
  check('unobservable state says so', bodyLower.includes('state not reported here'),
    'the absence of knowledge is not distinguished from a negative answer');

  // Undescribed modules are named, so the page cannot imply completeness.
  check('undescribed modules are listed', body.includes('modules/model/ollama'),
    'modules with no schema are omitted — the page implies it shows the whole fleet');

  // AND EACH SAYS WHY. Without the reason an operator cannot tell a dialog somebody owes from a module
  // that has nothing to configure, so a finished surface reads as permanently outstanding work.
  check('an undescribed module states its reason', body.includes("the server address is LiteLLM's config"),
    'the reason is not rendered — the list cannot be shrunk by inspection');
  check('no [object Object] leaked into the list', !body.includes('[object Object]'),
    'a structured entry was rendered by string coercion');
  // The older bare-string shape must still name its package rather than blanking the row.
  check('a legacy bare-string entry still names its package', body.includes('modules/cmdb/netbox'),
    'a control plane predating the reason field would have its packages silently dropped');

  // AND THE CONTROLS ACTUALLY POST. This is the assertion the preview version would fail.
  await page.evaluate(() => {
    const btns = Array.from(document.querySelectorAll('button'));
    const save = btns.find(b => b.textContent.trim() === 'SAVE');
    const store = btns.find(b => b.textContent.trim() === 'STORE');
    const test = btns.find(b => b.textContent.trim() === 'TEST');
    const inputs = Array.from(document.querySelectorAll('input'));
    const anyText = inputs.find(i => i.type !== 'password');
    if (anyText) anyText.value = 'https://matrix.example';
    const sec = inputs.find(i => i.type === 'password');
    if (sec) sec.value = 'a-new-token';
    if (save) save.click();
    if (store) store.click();
    if (test) test.click();
  });
  // SAVE/STORE/TEST all post in the same synchronous click batch above and their mocked responses fulfill
  // immediately with no added latency, so waiting for STORE's own observable completion signal (the secret
  // input clearing once its post round-trips — the exact state the last check below reads) is a reliable
  // proxy for the whole batch having landed, not just an arbitrary settle guess.
  await page.waitForFunction(() => {
    const sec = Array.from(document.querySelectorAll('input')).find(i => i.type === 'password');
    return sec && !sec.value;
  }).catch(() => {});
  check('SAVE posts to the config route', posted.some(u => u.includes('/api/v1/config/')),
    'the Save button changed nothing — this is the design-preview failure it replaced');
  check('STORE posts to the secret route', posted.some(u => u.includes('/secret')),
    'the credential field did not post');
  check('TEST posts to the test route', posted.some(u => u.includes('/test')),
    'the Test button did not exercise anything');

  // The secret input must be CLEARED after a successful store: the console cannot read it back and must
  // not appear to be holding it.
  const secretAfter = await page.evaluate(() =>
    (Array.from(document.querySelectorAll('input')).find(i => i.type === 'password')||{}).value);
  check('the secret input is cleared after storing', !secretAfter,
    'the console still holds the credential in a field it can never re-read');
} finally {
  await browser.close();
}
console.log(failed ? `FAILED (${failed})` : 'PASS');
process.exit(failed ? 1 : 0);
