'use strict';

// Runs the real embedded frontend in a local VM; no server, credentials or network.
// --root permits identical tests against an independently saved source snapshot.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const rootArg = process.argv.indexOf('--root');
const root = rootArg < 0 ? path.resolve(__dirname, '..') : path.resolve(process.argv[rootArg + 1]);
const originalSource = fs.readFileSync(path.join(root, 'web/app.js'), 'utf8');
const locales = Object.fromEntries(['zh', 'en'].map(lang => [lang,
  JSON.parse(fs.readFileSync(path.join(root, 'web/locales', lang + '.json'), 'utf8'))]));
const encode = value => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

function harness() {
  class Element {
    constructor(id = '') {
      this.id = id; this.dataset = {}; this._text = ''; this._html = null;
      this.targets = new Map(); this.style = {};
      const classes = new Set();
      this.classList = { contains: key => classes.has(key), add: key => classes.add(key), remove: key => classes.delete(key),
        toggle: (key, force) => { const add = force === undefined ? !classes.has(key) : force; if (add) classes.add(key); else classes.delete(key); } };
    }
    set textContent(value) { this._text = String(value); this._html = null; }
    get textContent() { return this._text; }
    set innerHTML(value) { this._html = String(value); }
    get innerHTML() { return this._html === null ? encode(this._text) : this._html; }
    querySelectorAll(selector) { return this.targets.get(selector) || []; }
  }
  const elements = new Map(['mainPage', 'tabAccounts', 'accountsList'].map(id => [id, new Element(id)]));
  const document = { readyState: 'loading', visibilityState: 'visible',
    getElementById: id => elements.get(id), createElement: () => new Element(),
    querySelectorAll: () => [], addEventListener() {} };
  const storage = { getItem: () => null, removeItem() {}, setItem() {} };
  let latest = [];
  let responseOK = true;
  let requestCount = 0;
  const context = vm.createContext({ document, localStorage: storage, sessionStorage: storage,
    location: { origin: 'http://local.test' }, navigator: {}, URL, Date, Math, Map, Set, Promise, console,
    setTimeout: () => 1, clearTimeout() {}, window: { addEventListener() {},
      matchMedia: () => ({ matches: false, addEventListener() {} }) },
    fetch: async url => {
      assert.equal(url, '/admin/api/accounts', 'only local accounts mock is used');
      requestCount++;
      return { ok: responseOK, status: responseOK ? 200 : 500, json: async () => latest };
    } });
  const source = originalSource.replace(/\}\)\(\);\s*$/, `
    dict.zh = ${JSON.stringify(locales.zh)};
    dict.en = ${JSON.stringify(locales.en)};
    globalThis.ui = { renderAccountProxy, loadAccountConcurrency, renderAccounts,
      setAccounts: value => { accountsData = value; }, getAccounts: () => accountsData,
      setLanguage: value => { currentLang = value; } };
  })();`);
  vm.runInContext(source, context, { filename: path.join(root, 'web/app.js') });
  const list = elements.get('accountsList');
  const slot = new Element(); slot.dataset.accountId = 'account-1';
  const concurrency = new Element(); concurrency.dataset.accountId = 'account-1';
  list.targets.set('.account-proxy-slot', [slot]);
  list.targets.set('.account-concurrency-value', [concurrency]);
  return { ui: context.ui, list, slot, concurrency,
    respond: (value, ok = true) => { latest = value; responseOK = ok; }, requests: () => requestCount };
}

const fixture = patch => ({ id: 'account-1', enabled: true, banStatus: 'ACTIVE', banReason: '', proxyPoolEnabled: true,
  proxyBlocked: true, proxyId: '', proxyName: '', proxyExitIP: '', ...patch });
const labels = {
  zh: { banned: '账号封禁 · 已解绑', disabled: '账号停用 · 未分配', auth: '认证失效 · 已解绑' },
  en: { banned: 'Account banned · unbound', disabled: 'Account disabled · unassigned', auth: 'Authentication expired · unbound' }
};
const tests = [];
function test(name, callback) { tests.push({ name, callback }); }
function assertState(markup, lang, expected, blocked = false) {
  assert.ok(markup.includes(expected), lang + ' renders ' + expected + ', actual: ' + markup);
  assert.equal(markup.includes('account-proxy-blocked'), blocked, 'proxy-fault color matches actual active proxy failure');
  if (!blocked) assert.ok(!markup.includes(locales[lang]['proxyPool.accountBlocked']), 'inactive/bound account does not claim a proxy fault');
}

for (const lang of ['zh', 'en']) {
  for (const status of ['BANNED', 'SUSPENDED', 'DISABLED', ' banned ', 'suspended', ' disabled ']) {
    test(lang + ' ineligible status ' + JSON.stringify(status) + ' overrides stale blocked/bound flags', () => {
      const h = harness(); h.ui.setLanguage(lang);
      const expected = status.trim().toUpperCase() === 'DISABLED' ? labels[lang].disabled : labels[lang].banned;
      for (const enabled of [false, true]) {
        assertState(h.ui.renderAccountProxy(fixture({ enabled, banStatus: status })), lang, expected);
        assertState(h.ui.renderAccountProxy(fixture({ enabled, banStatus: status, proxyBlocked: false, proxyId: 'stale-proxy' })), lang, expected);
      }
    });
  }
  test(lang + ' ordinary disabled account is unassigned, not proxy-blocked', () => {
    const h = harness(); h.ui.setLanguage(lang);
    for (const banStatus of ['ACTIVE', '', undefined, 'UNKNOWN']) {
      assertState(h.ui.renderAccountProxy(fixture({ enabled: false, banStatus })), lang, labels[lang].disabled);
    }
    for (const enabled of [false, true]) {
      assertState(h.ui.renderAccountProxy(fixture({ enabled, banStatus: 'DISABLED', banReason: 'operator-disabled' })), lang, labels[lang].disabled);
    }
  });
  test(lang + ' explicit invalid-token reason stays distinct from an actual account ban, including polls', async () => {
    const h = harness(); h.ui.setLanguage(lang);
    const banReason = 'Authentication failed - token invalid or expired';
    const invalidToken = fixture({ enabled: false, banStatus: 'BANNED', banReason });
    assertState(h.ui.renderAccountProxy(invalidToken), lang, labels[lang].auth);
    for (const banStatus of ['BANNED', 'SUSPENDED', 'DISABLED', 'ACTIVE']) {
      assertState(h.ui.renderAccountProxy(fixture({ enabled: false, banStatus, banReason })), lang, labels[lang].auth);
    }
    assertState(h.ui.renderAccountProxy(fixture({ enabled: false, banStatus: 'BANNED', banReason: 'Account suspended' })), lang, labels[lang].banned);
    assertState(h.ui.renderAccountProxy(fixture({ banReason })), lang, locales[lang]['proxyPool.accountBlocked'], true);
    h.ui.setAccounts([fixture()]); h.respond([invalidToken]);
    await h.ui.loadAccountConcurrency();
    assertState(h.slot.innerHTML, lang, labels[lang].auth);
    assert.equal(h.ui.getAccounts()[0].banReason, banReason, 'reason survives full redraw');
    h.ui.renderAccounts(); assert.ok(h.list.innerHTML.includes(labels[lang].auth));
    h.respond([{ id: 'account-1', inFlight: 1 }]); await h.ui.loadAccountConcurrency();
    assertState(h.slot.innerHTML, lang, labels[lang].auth);
    h.respond([fixture({ proxyBlocked: false, proxyId: 'proxy-1' })]); await h.ui.loadAccountConcurrency();
    assert.equal(h.ui.getAccounts()[0].banReason, '');
    assertState(h.slot.innerHTML, lang, locales[lang]['proxyPool.bound']);
  });
  test(lang + ' enabled blocked, bound, optional-direct and legacy defaults remain compatible', () => {
    const h = harness(); h.ui.setLanguage(lang);
    assertState(h.ui.renderAccountProxy(fixture()), lang, locales[lang]['proxyPool.accountBlocked'], true);
    for (const banStatus of ['', undefined, 'UNKNOWN']) {
      assertState(h.ui.renderAccountProxy(fixture({ banStatus })), lang, locales[lang]['proxyPool.accountBlocked'], true);
    }
    assertState(h.ui.renderAccountProxy({ proxyPoolEnabled: true, proxyBlocked: true }), lang, locales[lang]['proxyPool.accountBlocked'], true);
    assertState(h.ui.renderAccountProxy(fixture({ proxyBlocked: false })), lang, locales[lang]['proxyPool.accountUnbound']);
    assertState(h.ui.renderAccountProxy(fixture({ proxyBlocked: false, proxyId: 'proxy-1', proxyName: 'London', proxyExitIP: '203.0.113.1' })), lang, 'London · 203.0.113.1');
    assert.equal(h.ui.renderAccountProxy(fixture({ enabled: false, banStatus: 'BANNED', proxyPoolEnabled: false })), '');
    assert.equal(h.ui.renderAccountProxy({}), '');
    const escaped = h.ui.renderAccountProxy(fixture({ proxyBlocked: false, proxyId: 'proxy-1', proxyName: '<svg onload=bad()>', proxyExitIP: '<script>bad()</script>' }));
    assert.ok(escaped.includes('&lt;svg') && escaped.includes('&lt;script&gt;') && !escaped.includes('<svg'), 'bound endpoint metadata remains escaped');
  });
  test(lang + ' polling preserves account eligibility across re-render and updates recovery', async () => {
    const h = harness(); h.ui.setLanguage(lang);
    h.ui.setAccounts([fixture()]);
    h.respond([fixture({ enabled: false, banStatus: 'BANNED', inFlight: 2 })]);
    await h.ui.loadAccountConcurrency();
    assertState(h.slot.innerHTML, lang, labels[lang].banned);
    const account = h.ui.getAccounts()[0];
    assert.equal(account.enabled, false, 'latest enabled flag survives subsequent renders');
    assert.equal(account.banStatus, 'BANNED', 'latest ban status survives subsequent renders');
    assert.equal(h.concurrency.textContent, '2');
    assert.equal(h.concurrency.classList.contains('is-active'), true);
    h.ui.renderAccounts();
    assert.ok(h.list.innerHTML.includes(labels[lang].banned), 'full account render retains polled eligibility');
    h.respond([fixture({ enabled: false, banStatus: 'ACTIVE', proxyBlocked: false, inFlight: 0 })]);
    await h.ui.loadAccountConcurrency();
    assertState(h.slot.innerHTML, lang, labels[lang].disabled);
    h.respond([fixture({ proxyBlocked: false, proxyId: 'proxy-1', proxyName: 'Recovered' })]);
    await h.ui.loadAccountConcurrency();
    assertState(h.slot.innerHTML, lang, 'Recovered');
    assert.equal(account.enabled, true); assert.equal(account.banStatus, 'ACTIVE');
    assert.equal(h.requests(), 3);
  });
  test(lang + ' partial polling payload does not erase known eligibility', async () => {
    const h = harness(); h.ui.setLanguage(lang);
    h.ui.setAccounts([fixture({ enabled: false, banStatus: 'BANNED' })]);
    h.respond([{ id: 'account-1', inFlight: 1, proxyPoolEnabled: true, proxyBlocked: true }]);
    await h.ui.loadAccountConcurrency();
    assertState(h.slot.innerHTML, lang, labels[lang].banned);
    assert.equal(h.ui.getAccounts()[0].enabled, false);
    assert.equal(h.ui.getAccounts()[0].banStatus, 'BANNED');
    h.ui.renderAccounts();
    assert.ok(h.list.innerHTML.includes(labels[lang].banned));
  });
}

test('failed/malformed polls leave existing eligibility and markup unchanged', async () => {
  const h = harness(); const account = fixture({ enabled: false, banStatus: 'BANNED' });
  h.ui.setAccounts([account]); h.slot.innerHTML = 'unchanged';
  h.respond([fixture()], false); await h.ui.loadAccountConcurrency();
  h.respond({ error: 'not an account list' }); await h.ui.loadAccountConcurrency();
  assert.equal(account.enabled, false); assert.equal(account.banStatus, 'BANNED');
  assert.equal(h.slot.innerHTML, 'unchanged');
});

(async () => {
  let failures = 0;
  for (const entry of tests) {
    try { await entry.callback(); console.log('PASS ' + entry.name); }
    catch (error) { failures++; console.error('FAIL ' + entry.name + '\n' + error.message); }
  }
  console.log('account proxy status: passed=' + (tests.length - failures) + ' failed=' + failures + ' total=' + tests.length);
  process.exitCode = failures ? 1 : 0;
})().catch(error => { console.error(error); process.exitCode = 1; });
