'use strict';

// Real frontend functions in a dependency-free VM; all account APIs are mocked.
// --root PATH runs the same assertions against a sealed independent snapshot.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const rootIndex = process.argv.indexOf('--root');
const root = rootIndex < 0 ? path.resolve(__dirname, '..') : path.resolve(process.argv[rootIndex + 1]);
const source = fs.readFileSync(path.join(root, 'web/app.js'), 'utf8');
const css = fs.readFileSync(path.join(root, 'web/styles.css'), 'utf8');
const locales = Object.fromEntries(['zh', 'en'].map(lang => [lang,
  JSON.parse(fs.readFileSync(path.join(root, 'web/locales', lang + '.json'), 'utf8'))]));
const encode = value => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

function harness(lang = 'zh') {
  class Element {
    constructor(id = '') {
      this.id = id; this.dataset = {}; this._text = ''; this._html = null; this.targets = new Map(); this.style = {};
      this.details = { open: false }; this.writes = 0;
      const classes = new Set();
      this.classList = { contains: key => classes.has(key), add: key => classes.add(key), remove: key => classes.delete(key),
        toggle: (key, force) => { if (force) classes.add(key); else classes.delete(key); } };
    }
    set textContent(value) { this._text = String(value); this._html = null; }
    get textContent() { return this._text; }
    set innerHTML(value) { this._html = String(value); this.details = { open: false }; this.writes++; }
    get innerHTML() { return this._html === null ? encode(this._text) : this._html; }
    querySelectorAll(selector) { return this.targets.get(selector) || []; }
    querySelector(selector) { return selector === 'details' ? this.details : null; }
  }
  const elements = new Map(['mainPage', 'tabAccounts', 'accountsList'].map(id => [id, new Element(id)]));
  const document = { readyState: 'loading', visibilityState: 'visible',
    getElementById: id => elements.get(id), createElement: () => new Element(), querySelectorAll: () => [], addEventListener() {} };
  const storage = { getItem: () => null, removeItem() {}, setItem() {} };
  let latest = [], ok = true, requests = 0;
  const context = vm.createContext({ document, localStorage: storage, sessionStorage: storage,
    location: { origin: 'http://local.test' }, navigator: {}, URL, Date, Math, Map, Set, Promise, console,
    setTimeout: () => 1, clearTimeout() {}, window: { addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }) },
    fetch: async url => { assert.equal(url, '/admin/api/accounts'); requests++; return { ok, json: async () => latest }; } });
  vm.runInContext(source.replace(/\}\)\(\);\s*$/, `
    dict.zh = ${JSON.stringify(locales.zh)}; dict.en = ${JSON.stringify(locales.en)}; currentLang = ${JSON.stringify(lang)};
    globalThis.ui = {
      renderQuota: typeof renderAccountQuota === 'function' ? renderAccountQuota : () => '',
      formatUSD: typeof formatQuotaUSD === 'function' ? formatQuotaUSD : () => null,
      loadAccountConcurrency, renderAccounts, renderAccountProxy,
      setAccounts: value => { accountsData = value; }, getAccounts: () => accountsData
    };
  })();`), context, { filename: path.join(root, 'web/app.js') });
  const list = elements.get('accountsList'), quota = new Element(), proxy = new Element();
  quota.dataset.accountId = proxy.dataset.accountId = 'account-1';
  list.targets.set('.account-quota-slot', [quota]); list.targets.set('.account-proxy-slot', [proxy]);
  return { ui: context.ui, list, quota, proxy, respond: (value, valid = true) => { latest = value; ok = valid; }, requests: () => requests };
}
const estimate = patch => ({ currency: 'USD', spentUSD: 12.3456, spentComplete: true, estimatedTotalUSD: 250.789,
  estimateStatus: 'available', sampleCount: 4, lastSampleAt: '2026-10-19T12:00:00.123456789Z', trackingSince: '2026-10-18T12:00:00Z',
  unpricedRequests: 0, creditUsed: 20, creditLimit: 500, priceAsOf: '2026-10-19', ...patch });
const account = patch => ({ id: 'account-1', enabled: true, banStatus: 'ACTIVE', proxyPoolEnabled: true,
  proxyId: 'proxy-1', proxyName: 'Healthy', proxyBlocked: false, quotaEstimate: estimate(), ...patch });
const expectLabels = { zh: ['预估额度（本周期）', '已跑额度（累计）'], en: ['Estimated quota (current cycle)', 'Spent quota (all time)'] };
const tests = [];
function test(name, fn) { tests.push({ name, fn }); }
function values(markup) { return [...markup.matchAll(/<dd\b[^>]*>([\s\S]*?)<\/dd>/g)].slice(0, 2).map(match => match[1]); }

test('USD formatting keeps normal cents, up to four small-amount decimals and truthful tiny amounts', () => {
  const { ui } = harness();
  for (const [input, output] of [[0, '$0.00'], [-0, '$0.00'], [1, '$1.00'], [12.3456, '$12.35'], [0.1, '$0.10'], [0.0012, '$0.0012'], [0.0001, '$0.0001'], [0.00000001, '<$0.0001'], [12345.67, '$12,345.67']]) {
    assert.equal(ui.formatUSD(input), output, 'USD ' + input);
  }
});
test('missing, negative, nonfinite, coerced and unsafe money never become fabricated USD', () => {
  const { ui } = harness();
  for (const input of [null, undefined, '', '7.1', false, {}, [], NaN, Infinity, -Infinity, -1, 1e100]) assert.equal(ui.formatUSD(input), null, String(input));
});

for (const lang of ['zh', 'en']) {
  test(lang + ' renders two distinct scopes, real known amounts and sample details', () => {
    const h = harness(lang), markup = h.ui.renderQuota(account());
    for (const label of expectLabels[lang]) assert.ok(markup.includes(label), label);
    assert.ok(markup.includes('$250.79') && markup.includes('$12.35'));
    assert.ok(markup.includes('<details') && markup.includes('<summary'), 'touch/keyboard-accessible accounting details');
    assert.ok(markup.includes('2026-10-19') && markup.includes('500'), 'pricing date and credit snapshot available');
    const terms = lang === 'zh' ? ['下游', '缓存', '膨胀', '官方 API', 'Kiro', '工具', '时间加权', '积分差分', '同号其他客户端', 'AWS 积分更新滞后']
      : ['downstream', 'cache', 'inflation', 'official API', 'Kiro', 'tool', 'time-weighted', 'credit deltas', 'other clients on the same account', 'delayed AWS credit updates'];
    for (const term of terms) assert.ok(markup.toLowerCase().includes(term.toLowerCase()), 'accounting explanation: ' + term);
    assert.ok(!markup.includes('account-proxy-blocked'));
  });
  test(lang + ' null estimate shows explicit state rather than $0', () => {
    const h = harness(lang);
    const states = ['collecting', 'no_credit_snapshot', 'insufficient_credit_delta', 'unknown_price', 'storage_error', 'unexpected-future-status'];
    for (const estimateStatus of states) {
      const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ estimateStatus, estimatedTotalUSD: null }) }));
      assert.ok(!values(markup)[0]?.includes('$'), estimateStatus + ' does not fabricate an estimate');
      assert.ok(!markup.includes(estimateStatus), 'raw status is never a display string');
      assert.ok(values(markup)[0]?.replace(/<[^>]*>/g, '').trim(), 'unknown value has a readable state');
    }
    assert.ok(h.ui.renderQuota(account({ quotaEstimate: estimate({ estimateStatus: 'unknown_price', estimatedTotalUSD: null }) })).includes(lang === 'zh' ? '价格待补齐' : 'Pricing incomplete'));
  });
  test(lang + ' a fresh import without a ledger does not invent $0 lifetime spending', () => {
    const h = harness(lang);
    for (const quotaEstimate of [undefined, null, {}, estimate({ spentUSD: 0, spentComplete: false, estimatedTotalUSD: null,
      estimateStatus: 'collecting', sampleCount: 0, trackingSince: 0, lastSampleAt: 0 })]) {
      const markup = h.ui.renderQuota(account({ quotaEstimate }));
      assert.ok(!markup.includes('$0'), 'fresh account is not zero-priced history');
      assert.equal(values(markup).length, 2);
    }
    for (const trackingSince of ['0001-01-01T00:00:00Z', '2026-10-19T12:00:00Z']) {
      const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ spentUSD: 0, spentComplete: true, estimatedTotalUSD: null,
        estimateStatus: 'awaiting_usage', sampleCount: 0, trackingSince, lastSampleAt: null }) }));
      assert.ok(!markup.includes('$0'), 'actual no-ledger snapshot status does not fabricate historical zero');
    }
  });
  test(lang + ' a real zero with tracking is displayed; partial known USD is explicitly qualified', () => {
    const h = harness(lang);
    const zero = h.ui.renderQuota(account({ quotaEstimate: estimate({ spentUSD: 0 }) }));
    assert.ok(values(zero)[1].includes('$0.00'));
    const partial = h.ui.renderQuota(account({ quotaEstimate: estimate({ spentComplete: false, unpricedRequests: 2 }) }));
    assert.ok(values(partial)[1].includes('$12.35'));
    assert.ok(values(partial)[1].includes(lang === 'zh' ? '部分' : 'partial'));
  });
  test(lang + ' stale estimates remain visibly stale; unknown currency is not called USD', () => {
    const h = harness(lang);
    const stale = h.ui.renderQuota(account({ quotaEstimate: estimate({ estimateStatus: 'stale' }) }));
    assert.ok(values(stale)[0].includes('$250.79'));
    assert.ok(values(stale)[0].includes(lang === 'zh' ? '已过期' : 'stale'));
    const unsupported = h.ui.renderQuota(account({ quotaEstimate: estimate({ currency: 'EUR' }) }));
    assert.ok(!unsupported.includes('$250') && !unsupported.includes('$12'), 'unsupported currency does not silently relabel prices');
  });
  test(lang + ' actual ledger states have distinct translated meanings and never raw enum values', () => {
    const h = harness(lang);
    const states = ['awaiting_usage', 'awaiting_credit_snapshot', 'awaiting_credit_delta', 'credit_epoch_changed',
      'incomplete_cost_interval', 'unattributed_credit_usage', 'load_error', 'reset', 'ready'];
    for (const estimateStatus of states) {
      const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ estimateStatus, estimatedTotalUSD: null }) }));
      assert.ok(values(markup)[0]?.replace(/<[^>]*>/g, '').trim());
      assert.ok(!markup.includes(estimateStatus), 'enum not leaked: ' + estimateStatus);
      assert.ok(!values(markup)[0].includes('$'));
    }
    const missingPrice = h.ui.renderQuota(account({ quotaEstimate: estimate({ estimateStatus: 'incomplete_cost_interval',
      unpricedRequests: 1, spentUSD: 0, spentComplete: false, estimatedTotalUSD: null }) }));
    assert.ok(values(missingPrice).every(value => value.includes(lang === 'zh' ? '价格待补齐' : 'Pricing incomplete')));
    const ready = h.ui.renderQuota(account({ quotaEstimate: estimate({ estimateStatus: 'ready' }) }));
    assert.ok(values(ready)[0].includes('$250.79'), 'ready alias remains compatible');
  });
  test(lang + ' RFC3339 nanoseconds and Unix seconds are accepted; zero/invalid dates are not tracking', () => {
    const h = harness(lang);
    for (const trackingSince of ['2026-10-19T12:00:00.123456789Z', 1792400000]) {
      const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ spentUSD: 0, trackingSince }) }));
      assert.ok(values(markup)[1].includes('$0.00'), 'real zero after valid tracking');
    }
    for (const trackingSince of ['0001-01-01T00:00:00Z', '1970-01-01T00:00:00Z', 0, null, '', 'invalid-date', '19']) {
      const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ spentUSD: 0, trackingSince, lastSampleAt: trackingSince }) }));
      assert.ok(!values(markup)[1].includes('$0'), 'invalid tracking does not establish a ledger');
      assert.ok(!markup.includes('Invalid Date'));
    }
  });
  test(lang + ' persistence errors are visibly disclosed without echoing raw backend errors', () => {
    const h = harness(lang);
    const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ persistenceError: '<script>RAW_STORAGE_SECRET</script>' }) }));
    assert.ok(markup.includes(lang === 'zh' ? '记录待持久化' : 'Ledger persistence pending'));
    assert.ok(!markup.includes('RAW_STORAGE_SECRET'));
  });
  test(lang + ' untrusted status/date/count/money is escaped or rejected with no invalid numeric artifacts', () => {
    const h = harness(lang);
    for (const invalid of [null, NaN, Infinity, -Infinity, -2, '19', '<script>bad()</script>', {}, 1e308]) {
      const markup = h.ui.renderQuota(account({ quotaEstimate: estimate({ spentUSD: invalid, estimatedTotalUSD: invalid,
        estimateStatus: '<svg onload=bad()>', priceAsOf: '"><script>bad()</script>', trackingSince: invalid,
        lastSampleAt: invalid, sampleCount: invalid, unpricedRequests: invalid, creditUsed: invalid, creditLimit: invalid }) }));
      assert.ok(!/<script>|<svg|NaN|Infinity|Invalid Date/.test(markup), 'malicious and invalid values remain inert');
      assert.ok(!markup.includes('onload='), 'untrusted status never enters attributes');
    }
  });
  test(lang + ' full account rendering contains quota slots and retains the proxy status fix', () => {
    const h = harness(lang); h.ui.setAccounts([account({ enabled: false, banStatus: 'BANNED', proxyId: '', proxyBlocked: true })]);
    h.ui.renderAccounts();
    assert.ok(h.list.innerHTML.includes('account-quota-slot') && h.list.innerHTML.includes('data-account-id="account-1"'));
    assert.ok(h.list.innerHTML.includes(expectLabels[lang][0]));
    assert.ok(h.list.innerHTML.includes(lang === 'zh' ? '账号封禁 · 已解绑' : 'Account banned · unbound'));
    assert.ok(!h.list.innerHTML.includes('account-proxy-blocked'));
  });
  test(lang + ' polling updates USD, preserves omitted fields and keeps expanded details open', async () => {
    const h = harness(lang), initial = account(); h.ui.setAccounts([initial]);
    h.quota.innerHTML = h.ui.renderQuota(initial); h.quota.details.open = true;
    h.respond([account({ quotaEstimate: estimate({ spentUSD: 20, estimatedTotalUSD: 300 }) })]);
    await h.ui.loadAccountConcurrency();
    assert.ok(h.quota.innerHTML.includes('$20.00') && h.quota.innerHTML.includes('$300.00'));
    assert.equal(h.quota.details.open, true, 'background refresh preserves user-open details');
    assert.equal(initial.quotaEstimate.spentUSD, 20, 'future renders use new quota');
    const writes = h.quota.writes;
    h.respond([{ id: 'account-1', inFlight: 3 }]); await h.ui.loadAccountConcurrency();
    assert.equal(initial.quotaEstimate.spentUSD, 20); assert.equal(h.quota.writes, writes, 'unchanged labels are not rewritten');
    h.ui.renderAccounts(); assert.ok(h.list.innerHTML.includes('$300.00'));
    h.respond([{ id: 'account-1', quotaEstimate: null }]); await h.ui.loadAccountConcurrency();
    assert.equal(initial.quotaEstimate, null, 'explicit null clears stale estimate');
    assert.ok(!h.quota.innerHTML.includes('$20.00'));
  });
}
test('all quota locale keys exist in both languages and responsive styling does not widen the old statistics grid', () => {
  const keys = [...source.matchAll(/'(accounts\.quota[A-Za-z]+)'/g)].map(match => match[1]);
  assert.ok(keys.length >= 10, 'explicit localized quota states and explanations');
  for (const key of keys) for (const lang of ['zh', 'en']) assert.ok(locales[lang][key], lang + ' ' + key);
  assert.match(css, /\.account-quota-values\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\)/);
  assert.match(css, /@media\s*\(max-width:\s*420px\)\s*\{[\s\S]*?\.account-quota-values\s*\{[^}]*grid-template-columns:\s*1fr/);
  assert.match(css, /\.account-quota-item\s*\{[^}]*min-width:\s*0/);
  assert.match(css, /\.account-stats\s*\{[^}]*grid-template-columns:\s*repeat\(5,/);
});

(async () => {
  let failed = 0;
  for (const entry of tests) {
    try { await entry.fn(); console.log('PASS ' + entry.name); }
    catch (error) { failed++; console.error('FAIL ' + entry.name + '\n' + error.message); }
  }
  console.log('account quota UI: passed=' + (tests.length - failed) + ' failed=' + failed + ' total=' + tests.length);
  process.exitCode = failed ? 1 : 0;
})().catch(error => { console.error(error); process.exitCode = 1; });
