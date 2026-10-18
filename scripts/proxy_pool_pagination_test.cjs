'use strict';

// Independent, dependency-free VM tests for the actual embedded UI event handlers.
// --baseline is a characterization test: the prior UI renders all 55 proxies.
// --root PATH tests a separately extracted source tree without rewriting it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const rootArg = process.argv.indexOf('--root');
const root = rootArg >= 0 ? path.resolve(process.argv[rootArg + 1]) : path.resolve(__dirname, '..');
const baseline = process.argv.includes('--baseline');
const html = fs.readFileSync(path.join(root, 'web/index.html'), 'utf8');
const originalSource = fs.readFileSync(path.join(root, 'web/app.js'), 'utf8');
const locales = Object.fromEntries(['zh', 'en'].map(lang => [lang,
  JSON.parse(fs.readFileSync(path.join(root, 'web/locales', lang + '.json'), 'utf8'))]));
const encode = value => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const response = (data, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => data });

function makeHarness(initialStorage = {}) {
  let document;
  class Element {
    constructor(id = '') {
      Object.assign(this, { id, value: '', checked: false, indeterminate: false, disabled: false,
        dataset: {}, _text: '', _html: null, attributes: {}, listeners: {}, style: {}, isConnected: true });
      const classes = new Set();
      this.classList = { contains: name => classes.has(name), add: name => classes.add(name),
        remove: name => classes.delete(name), toggle: (name, force) => {
          const add = force === undefined ? !classes.has(name) : force;
          if (add) classes.add(name); else classes.delete(name); return add;
        } };
    }
    set textContent(value) { this._text = String(value); this._html = null; }
    get textContent() { return this._text; }
    set innerHTML(value) { this._html = String(value); this.rows = null; }
    get innerHTML() { return this._html === null ? encode(this._text) : this._html; }
    setAttribute(name, value) { this.attributes[name] = String(value); }
    getAttribute(name) { return this.attributes[name]; }
    hasAttribute(name) { return name in this.attributes; }
    addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
    removeEventListener() {}
    querySelectorAll(selector) {
      if (!selector.includes('data-proxy-select')) return [];
      if (!this.rows) this.rows = [...this.innerHTML.matchAll(/<input\b([^>]*\bdata-proxy-select="([^"]+)"[^>]*)>/g)].map(match => {
        const checkbox = new Element();
        checkbox.dataset.proxySelect = match[2];
        checkbox.checked = /\bchecked(?:\s|$)/.test(match[1]);
        checkbox.row = new Element();
        checkbox.closest = selector => selector === 'tr' ? checkbox.row : null;
        return checkbox;
      });
      return this.rows;
    }
    querySelector() { return null; }
    contains(element) { return element === this || (this.rows || []).includes(element); }
    focus() { document.activeElement = this; }
    reportValidity() { return !!this.value; }
  }
  const elements = new Map([...html.matchAll(/\bid="([^"]+)"/g)].map(match => [match[1], new Element(match[1])]));
  const batchButtons = new Map([...html.matchAll(/<button\b[^>]*\bdata-proxy-batch="([^"]+)"[^>]*>/g)].map(match => {
    const button = new Element('batch-' + match[1]); button.dataset.proxyBatch = match[1];
    return [match[1], button];
  }));
  document = { readyState: 'loading', visibilityState: 'visible', activeElement: null,
    getElementById: id => elements.get(id), createElement: () => new Element(),
    querySelectorAll: selector => selector === '[data-proxy-batch]' ? [...batchButtons.values()] : [],
    addEventListener() {}, body: new Element(), documentElement: new Element() };
  const stored = new Map(Object.entries(initialStorage));
  const storage = { getItem: key => stored.get(key) ?? null, removeItem: key => stored.delete(key),
    setItem: (key, value) => stored.set(key, String(value)) };
  const requests = [];
  let handler = async url => { throw new Error('Unexpected API call: ' + url); };
  const context = vm.createContext({ document, localStorage: storage, sessionStorage: storage,
    location: { origin: 'http://local.test' }, navigator: {}, URL, Date, Math, Map, Set, Promise, console,
    setTimeout: () => 1, clearTimeout() {}, requestAnimationFrame: fn => fn(),
    window: { addEventListener() {}, scrollTo() {}, matchMedia: () => ({ matches: false, addEventListener() {} }) },
    fetch: async (url, options = {}) => { requests.push({ url, options }); return handler(url, options); } });
  const source = originalSource.replace(/\}\)\(\);\s*$/, `
    dict.zh = ${JSON.stringify(locales.zh)};
    dict.en = ${JSON.stringify(locales.en)};
    globalThis.ui = { acceptProxyPoolData, renderProxyPool, getFilteredProxies, bindProxyPoolEvents,
      loadProxyPool, runProxyPoolAction, selectedProxies,
      getPage: () => typeof getProxyPoolPage === 'function' ? getProxyPoolPage() : null,
      setLanguage: value => { currentLang = value; } };
  })();`);
  vm.runInContext(source, context, { filename: path.join(root, 'web/app.js') });
  const element = id => { assert.ok(elements.has(id), 'HTML contains #' + id); return elements.get(id); };
  const dispatch = async (id, kind, patch = {}) => {
    const target = typeof id === 'string' ? element(id) : id;
    const event = { target, preventDefault() {}, ...patch };
    assert.ok(target.listeners[kind]?.length, '#' + target.id + ' binds ' + kind);
    for (const listener of target.listeners[kind]) await listener(event);
  };
  const ids = () => [...element('proxyPoolList').innerHTML.matchAll(/\bdata-proxy-select="([^"]+)"/g)].map(match => match[1]);
  const choose = async (id, kind, value) => { element(id).value = value; await dispatch(id, kind); };
  return { ui: context.ui, element, dispatch, choose, ids, stored, requests, batchButtons, document,
    setHandler: fn => { handler = fn; } };
}

function pool(count = 55) {
  return { settings: { enabled: true, blockWithoutProxy: true, checkIntervalSeconds: 60, checkConcurrency: 16 },
    proxies: Array.from({ length: count }, (_, index) => {
      const n = index + 1;
      return { id: 'p' + n, name: 'Proxy ' + String(n).padStart(3, '0'), url: 'http://proxy' + n + '.test:8080',
        enabled: n % 2 === 1, health: n % 2 === 1 ? 'healthy' : 'unhealthy', country: 'United States',
        city: n <= 25 ? 'Alpha' : 'Beta', exitIP: '203.0.113.' + n,
        accountCount: 1, currentConcurrency: 2, currentBytesPerSecond: 1024, totalBytes: 2048 };
    }), inspection: { running: false, total: count, completed: count } };
}

async function run() {
  const h = makeHarness();
  const { ui, element, dispatch, choose, ids } = h;
  ui.bindProxyPoolEvents();
  ui.acceptProxyPoolData(pool());
  if (baseline) {
    assert.equal(ids().length, 55);
    assert.equal(ui.getPage(), null);
    console.log('BASELINE: total=55 rendered=55 pagination=absent first=p1 last=p55 exit=0');
    return;
  }
  const nav = { first: 'proxyPoolFirstPage', prev: 'proxyPoolPreviousPage', next: 'proxyPoolNextPage', last: 'proxyPoolLastPage' };
  const page = () => JSON.parse(JSON.stringify(ui.getPage()));
  const expectRows = (start, end) => assert.deepEqual(ids(), Array.from({ length: Math.max(0, end - start + 1) }, (_, n) => 'p' + (start + n)));
  const jump = async value => choose('proxyPoolPageInput', 'change', String(value));
  const selectAll = async checked => { element('proxyPoolSelectAll').checked = checked; await dispatch('proxyPoolSelectAll', 'change'); };
  const selectOne = async (id, checked) => dispatch('proxyPoolList', 'change', { target: { dataset: { proxySelect: id }, checked } });

  // Contract and accessibility translation coverage; no missing element/key can ship.
  [...originalSource.matchAll(/\$\('(proxyPool[^']*|settingsProxyPool[^']*)'\)/g)].forEach(match => element(match[1]));
  const keys = new Set([...html.matchAll(/data-i18n(?:-placeholder|-title|-aria-label)?="(proxyPool\.[^"]+)"/g)].map(match => match[1]));
  for (const match of originalSource.matchAll(/'((?:proxyPool\.)[A-Za-z]+)'/g)) keys.add(match[1]);
  keys.delete('proxyPool.');
  for (const key of keys) for (const lang of ['zh', 'en']) assert.ok(locales[lang][key], lang + ' translation for ' + key);

  assert.equal(page().page, 1); assert.equal(page().pageSize, 20); assert.equal(page().pageCount, 3);
  assert.equal(page().total, 55); assert.equal(page().start, 1); assert.equal(page().end, 20);
  expectRows(1, 20);
  assert.equal(element(nav.first).disabled, true); assert.equal(element(nav.prev).disabled, true);
  assert.equal(element(nav.next).disabled, false); assert.equal(element(nav.last).disabled, false);
  const summary = element('proxyPoolSummary').innerHTML;
  assert.ok(summary.includes('<strong>55</strong>') && summary.includes('<strong>110</strong>') && summary.includes('55.0 KiB/s'), 'summary aggregates whole pool');

  await selectAll(true);
  assert.equal(ui.selectedProxies.size, 20, 'top checkbox selects current page, not all filtered entries');
  assert.ok(element('proxyPoolList').querySelectorAll('[data-proxy-select]').every(row => row.checked), 'rendered checkboxes reflect selection');
  await dispatch(nav.next, 'click'); expectRows(21, 40);
  assert.equal(page().page, 2); assert.equal(element('proxyPoolSelectAll').checked, false);
  assert.equal(element('proxyPoolSelectAll').indeterminate, false, 'off-page selection does not affect current checkbox');
  assert.equal(element('proxyPoolSelectionCount').textContent, '跨页共选 20 个 · 当前页 0 个');
  await selectOne('p21', true);
  assert.equal(element('proxyPoolSelectAll').indeterminate, true);
  assert.equal(element('proxyPoolList').querySelectorAll('[data-proxy-select]')[0].checked, true);
  await selectAll(true); assert.equal(ui.selectedProxies.size, 40);
  assert.equal(element('proxyPoolSelectAll').checked, true);
  await selectAll(false); assert.equal(ui.selectedProxies.size, 20, 'deselect current page preserves off-page selection');
  await dispatch(nav.first, 'click'); assert.equal(element('proxyPoolSelectAll').checked, true);
  await dispatch('proxyPoolClearSelection', 'click'); assert.equal(ui.selectedProxies.size, 0);
  assert.equal(element('proxyPoolSelectAll').checked, false);
  assert.equal(element('proxyPoolBatchActions').classList.contains('hidden'), true);

  await dispatch(nav.last, 'click'); expectRows(41, 55);
  assert.equal(element(nav.next).disabled, true); assert.equal(element(nav.last).disabled, true);
  await jump('999'); assert.equal(page().page, 3);
  for (const invalid of ['', 'bad', 'NaN', '-10', 'Infinity']) {
    await jump(invalid); assert.equal(page().page, 1, 'invalid page input resets first: ' + invalid);
    assert.ok(!element('proxyPoolPageStatus').textContent.includes('NaN'));
    assert.ok(!element('proxyPoolPageRange').textContent.includes('NaN'));
  }
  await jump('2.9'); assert.equal(page().page, 2, 'fractional input floored');
  element('proxyPoolPageInput').value = '3'; await dispatch('proxyPoolPageInput', 'keydown', { key: 'Enter' });
  assert.equal(page().page, 3, 'Enter applies jump');

  for (const size of [10, 20, 50, 100]) {
    await choose('proxyPoolPageSize', 'change', String(size));
    assert.equal(page().page, 1); assert.equal(page().pageSize, size);
    assert.equal(ids().length, Math.min(size, 55));
    assert.equal(h.stored.get('kiro_proxy_pool_page_size'), String(size));
    const reload = makeHarness(Object.fromEntries(h.stored));
    reload.ui.bindProxyPoolEvents(); reload.ui.acceptProxyPoolData(pool());
    assert.equal(reload.ui.getPage().pageSize, size, 'size restored after reload');
  }
  for (const invalid of ['', '999', '-10', '20x', '20.5', 'NaN', 'Infinity', '<script>']) {
    const corrupt = makeHarness({ kiro_proxy_pool_page_size: invalid });
    corrupt.ui.bindProxyPoolEvents(); corrupt.ui.acceptProxyPoolData(pool());
    assert.equal(corrupt.ui.getPage().pageSize, 20, 'storage size sanitization: ' + invalid);
  }

  await choose('proxyPoolPageSize', 'change', '20');
  await dispatch(nav.next, 'click');
  h.setHandler(async url => { assert.equal(url, '/admin/api/proxy-pool'); return response(pool()); });
  await ui.loadProxyPool(true); expectRows(21, 40); assert.equal(page().page, 2, 'poll does not reset page');
  element('proxyPoolPageInput').value = '3'; element('proxyPoolPageInput').focus();
  await ui.loadProxyPool(true);
  assert.equal(element('proxyPoolPageInput').value, '3', 'poll preserves a focused page-jump draft');
  assert.equal(page().page, 2, 'draft is not applied until explicitly committed');
  h.document.activeElement = null; ui.renderProxyPool();
  assert.equal(element('proxyPoolSummary').innerHTML, summary, 'summary remains whole-pool on another page');
  await choose('proxyPoolSearch', 'input', 'Alpha');
  assert.equal(page().page, 1); assert.equal(page().total, 25); expectRows(1, 20);
  await dispatch(nav.next, 'click'); expectRows(21, 25);
  await choose('proxyPoolFilter', 'change', 'enabled');
  assert.equal(page().page, 1); assert.equal(page().total, 13); assert.equal(ids().length, 13);
  await choose('proxyPoolSearch', 'input', 'no-match');
  assert.equal(page().total, 0); assert.equal(page().start, 0); assert.equal(page().end, 0); assert.equal(ids().length, 0);
  Object.values(nav).forEach(id => assert.equal(element(id).disabled, true));
  assert.equal(element('proxyPoolSelectAll').disabled, true);
  await choose('proxyPoolSearch', 'input', ''); await choose('proxyPoolFilter', 'change', 'all');
  await dispatch(nav.last, 'click');
  await selectAll(true); assert.equal(ui.selectedProxies.size, 15);
  ui.acceptProxyPoolData(pool(40));
  assert.equal(page().page, 2); expectRows(21, 40);
  assert.equal(ui.selectedProxies.size, 0, 'deleted proxies removed from cross-page selection');
  ui.acceptProxyPoolData(pool(0));
  assert.equal(page().page, 1); assert.equal(page().pageCount, 1); assert.equal(page().total, 0);
  Object.values(nav).forEach(id => assert.equal(element(id).disabled, true));

  // A page-sized list must never silently narrow the explicit all-proxy check action.
  ui.acceptProxyPoolData(pool()); await dispatch(nav.next, 'click');
  h.setHandler(async (url, options) => {
    if (url === '/admin/api/proxy-pool/check') {
      assert.deepEqual(JSON.parse(options.body), { ids: [] });
      return response({ ...pool(), inspection: { running: true, completed: 0, total: 55 } }, 202);
    }
    assert.equal(url, '/admin/api/proxy-pool'); return response(pool());
  });
  await dispatch('proxyPoolCheckAllBtn', 'click');
  assert.equal(page().page, 2); expectRows(21, 40);
  await selectOne('p21', true); await dispatch(nav.first, 'click');
  await selectOne('p1', true); await dispatch(nav.next, 'click');
  h.setHandler(async (url, options) => {
    if (url === '/admin/api/proxy-pool/check') {
      assert.deepEqual(JSON.parse(options.body), { ids: ['p21', 'p1'] }, 'batch action retains explicit selections from both pages');
      return response({ ...pool(), inspection: { running: true, completed: 0, total: 2 } }, 202);
    }
    assert.equal(url, '/admin/api/proxy-pool'); return response(pool());
  });
  await dispatch(h.batchButtons.get('check'), 'click');
  assert.equal(ui.selectedProxies.size, 2);
  await dispatch('proxyPoolClearSelection', 'click');
  ui.setLanguage('en'); ui.renderProxyPool();
  assert.equal(element('proxyPoolPageStatus').textContent, 'Page 2 of 3', 'English pagination resolves translations');

  const large = makeHarness();
  large.ui.bindProxyPoolEvents(); large.ui.acceptProxyPoolData(pool(103));
  await large.choose('proxyPoolPageSize', 'change', '100'); await large.dispatch(nav.last, 'click');
  assert.deepEqual(large.ids(), ['p101', 'p102', 'p103']);
  assert.equal(large.ui.getPage().start, 101); assert.equal(large.ui.getPage().end, 103);
  assert.equal(large.element(nav.next).disabled, true);
  console.log('PASS proxy pool pagination: 55/103 fixtures, 20-row default, navigation/jump/boundaries, sizes/persistence/sanitization, filter/search reset, refresh/draft retention, delete clamp, cross-page selection/batch scope, whole-pool summary, locale/DOM contracts, all-proxy check semantics');
  console.log('PAGINATION: total=55 page=2 rows=20 first=p21 last=p40 selected=0 exit=0');
}

run().catch(error => { console.error(error); process.exitCode = 1; });
