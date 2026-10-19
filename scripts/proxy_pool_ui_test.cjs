'use strict';

// Dependency-free tests for the embedded admin UI; no server or network access.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'web/index.html'), 'utf8');
const originalSource = fs.readFileSync(path.join(root, 'web/app.js'), 'utf8');
const locales = Object.fromEntries(['zh', 'en'].map(lang => [lang, JSON.parse(fs.readFileSync(path.join(root, 'web/locales', lang + '.json'), 'utf8'))]));
const encode = value => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
let document;
class Element {
  constructor(id = '') {
    this.id = id; this.value = ''; this.checked = false; this.disabled = false; this.dataset = {};
    this._text = ''; this._html = null; this.attributes = {}; this.listeners = {}; this.style = {}; this.isConnected = true;
    const classes = new Set();
    this.classList = {
      contains: name => classes.has(name), add: name => classes.add(name), remove: name => classes.delete(name),
      toggle: (name, force) => { const add = force === undefined ? !classes.has(name) : force; if (add) classes.add(name); else classes.delete(name); return add; }
    };
  }
  set textContent(value) { this._text = String(value); this._html = null; }
  get textContent() { return this._text; }
  set innerHTML(value) { this._html = String(value); }
  get innerHTML() { return this._html === null ? encode(this._text) : this._html; }
  setAttribute(name, value) { this.attributes[name] = value; }
  getAttribute(name) { return this.attributes[name]; }
  hasAttribute(name) { return name in this.attributes; }
  addEventListener(name, fn) { (this.listeners[name] ||= []).push(fn); }
  removeEventListener() {}
  querySelectorAll() { return []; }
  querySelector() { return null; }
  contains(element) { return element === this; }
  focus() { document.activeElement = this; }
  reportValidity() { return !!this.value; }
}
const elements = new Map(Array.from(html.matchAll(/\bid="([^"]+)"/g), match => [match[1], new Element(match[1])]));
document = {
  readyState: 'loading', visibilityState: 'visible', activeElement: null,
  getElementById: id => elements.get(id), createElement: () => new Element(), querySelectorAll: () => [],
  addEventListener() {}, body: new Element(), documentElement: new Element()
};
const storage = { getItem: () => null, removeItem() {}, setItem() {} };
const response = (data, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => data });
const requests = [];
let handler = async (url) => { throw new Error('Unexpected API call: ' + url); };
const context = vm.createContext({
  document, localStorage: storage, sessionStorage: storage, location: { origin: 'http://local.test' },
  navigator: {}, URL, Date, Math, Map, Set, Promise, console,
  setTimeout: () => 1, clearTimeout() {}, requestAnimationFrame: fn => fn(),
  window: { addEventListener() {}, scrollTo() {}, matchMedia: () => ({ matches: false, addEventListener() {} }) },
  fetch: async (url, options = {}) => { requests.push({ url, options }); return handler(url, options); }
});
const source = originalSource.replace(/\}\)\(\);\s*$/, `
  dict.zh = ${JSON.stringify(locales.zh)};
  dict.en = ${JSON.stringify(locales.en)};
  globalThis.ui = { acceptProxyPoolData, renderProxyPool, formatProxyBytes, proxyEndpoint, formatProxyChecked,
    getFilteredProxies, saveProxyPoolSettings, loadProxyPool, mutateProxyPool, bindProxyPoolEvents,
    openProxyPoolEdit, submitProxyPoolEdit, submitProxyPoolImport, runProxyPoolAction, renderAccountProxy,
    selectedProxies, getData: () => proxyPoolData, setLanguage: value => { currentLang = value; } };
})();`);
vm.runInContext(source, context, { filename: 'web/app.js' });
const ui = context.ui;
const element = id => {
  assert.ok(elements.has(id), 'HTML contains #' + id);
  return elements.get(id);
};
const state = (enabled = false) => ({
  settings: { enabled, blockWithoutProxy: true, checkIntervalSeconds: 60, checkConcurrency: 16 },
  proxies: [
    { id: 'proxy-1', name: '<img src=x onerror=bad()>', url: 'http://alice:SECRET@proxy.example:8080', enabled: true, health: 'healthy', exitIP: '203.0.113.1', country: 'United States', region: 'New York', city: 'Albany', accountCount: 2, currentConcurrency: 1, currentBytesPerSecond: 2048, totalBytes: 8192, lastChecked: 1780000000, latencyMS: 125 },
    { id: 'proxy-2', name: 'Tokyo', url: 'socks5://host.test:1080', enabled: false, disabledBy: 'health', health: 'unhealthy', country: 'Japan', city: 'Tokyo', accountCount: 0, currentConcurrency: 0, currentBytesPerSecond: 0, totalBytes: 1024, lastError: '<script>bad()</script>' }
  ], inspection: { running: false, total: 2, completed: 2 }
});
const event = { preventDefault() {} };
async function run() {
  // UI IDs and locale references must remain deployable without a build step.
  const ids = [...originalSource.matchAll(/\$\('(proxyPool[^']*|settingsProxyPool[^']*|settingsOpenProxyPoolBtn)'\)/g)].map(match => match[1]);
  ids.forEach(element);
  const keys = new Set([...html.matchAll(/data-i18n(?:-placeholder|-title|-aria-label)?="(proxyPool\.[^"]+|tabs\.proxyPool)"/g)].map(match => match[1]));
  for (const match of originalSource.matchAll(/'((?:proxyPool\.)[A-Za-z]+)'/g)) keys.add(match[1]);
  keys.delete('proxyPool.');
  for (const key of keys) for (const lang of ['zh', 'en']) assert.ok(locales[lang][key], lang + ' translation for ' + key);
  assert.ok(!/s2ap/i.test(originalSource + html + JSON.stringify(locales)), 'temporary S2AP UI removed');
  ui.bindProxyPoolEvents();
  assert.equal(element('proxyPoolEnabled').disabled, true, 'unknown initial settings are not writable');
  ui.acceptProxyPoolData(state());
  assert.equal(ui.formatProxyBytes(0), '0 B');
  assert.equal(ui.formatProxyBytes(0.5), '1 B');
  assert.equal(ui.formatProxyBytes(2048), '2.0 KiB');
  assert.equal(ui.formatProxyBytes(-1), '0 B');
  assert.equal(ui.proxyEndpoint('http://alice:SECRET@host.test:80'), 'http://host.test');
  const markup = element('proxyPoolList').innerHTML;
  assert.ok(markup.includes('Albany') && markup.includes('203.0.113.1'), 'GeoIP metadata rendered');
  assert.ok(markup.includes('2.0 KiB/s') && markup.includes('8.0 KiB'), 'current and total traffic rendered');
  assert.ok(markup.includes('&lt;img') && markup.includes('&lt;script&gt;'), 'untrusted names and errors escaped');
  assert.ok(!markup.includes('SECRET') && !markup.includes('alice'), 'credentials hidden in list');
  // Disable intention and measured health are distinct. Unknown legacy reasons
  // stay manual, while only health failures advertise automatic recovery.
  const proxyRow = () => element('proxyPoolList').innerHTML.match(/<tr(?: [^>]*)?>[\s\S]*?<\/tr>/g).find(row => row.includes('data-proxy-select="proxy-2"'));
  assert.ok(proxyRow().includes(locales.zh['proxyPool.autoDisabled']), 'automatic fault disable badge');
  assert.ok(proxyRow().includes(locales.zh['proxyPool.unhealthy']), 'health badge remains independent');
  assert.ok(proxyRow().includes('data-proxy-action="enable"'), 'auto-disabled row offers manual enable');
  assert.ok(!proxyRow().includes('data-proxy-action="disable"'), 'auto-disabled row does not offer ambiguous already-disabled toggle');
  for (const lang of ['zh', 'en']) {
    ui.setLanguage(lang);
    for (const [disabledBy, labelKey] of [['health', 'autoDisabled'], ['manual', 'manualDisabled'], ['invalid', 'invalidDisabled'], ['', 'manualDisabled'], ['<svg onload=bad()>', 'manualDisabled']]) {
      const fixture = state();
      Object.assign(fixture.proxies[1], { disabledBy, health: 'healthy' });
      ui.acceptProxyPoolData(fixture);
      assert.ok(proxyRow().includes(locales[lang]['proxyPool.' + labelKey]), lang + ' disable origin ' + disabledBy);
      assert.ok(proxyRow().includes(locales[lang]['proxyPool.healthy']), lang + ' healthy does not hide disabled intention');
      assert.ok(!proxyRow().includes('<svg'), 'disable origin uses trusted translation keys');
      if (disabledBy !== 'health') assert.ok(!proxyRow().includes(locales[lang]['proxyPool.autoDisabled']), 'manual/invalid/unknown never promise auto-recovery');
    }
  }
  ui.setLanguage('zh'); ui.acceptProxyPoolData(state());
  element('proxyPoolSearch').value = 'albany';
  assert.equal(ui.getFilteredProxies().length, 1, 'city search');
  element('proxyPoolSearch').value = '';
  element('proxyPoolFilter').value = 'unbound';
  assert.equal(ui.getFilteredProxies()[0].id, 'proxy-2', 'unbound filter');
  element('proxyPoolSelectAll').checked = true;
  element('proxyPoolSelectAll').listeners.change[0]({ target: element('proxyPoolSelectAll') });
  assert.deepEqual([...ui.selectedProxies], ['proxy-2'], 'select all only affects filtered rows');
  element('proxyPoolFilter').value = 'all';
  ui.setLanguage('en'); ui.renderProxyPool();
  assert.ok(element('proxyPoolList').innerHTML.includes('Healthy'), 'English rendering');
  ui.setLanguage('zh');

  // A GET started before the POST must not revert either copy of the switch.
  let resolveOldGet;
  let enabled = false;
  handler = async (url, options) => {
    if (url === '/admin/api/proxy-pool' && !options.method) return new Promise(resolve => { resolveOldGet = resolve; });
    if (url === '/admin/api/proxy-pool/settings') {
      const patch = JSON.parse(options.body);
      assert.deepEqual(patch, { enabled: true });
      enabled = patch.enabled;
      return response(state(enabled));
    }
    if (url === '/admin/api/accounts') return response([]);
    throw new Error('Unexpected API call: ' + url);
  };
  const oldGet = ui.loadProxyPool(true);
  await ui.saveProxyPoolSettings({ enabled: true });
  resolveOldGet(response(state(false)));
  await oldGet;
  assert.equal(ui.getData().settings.enabled, true, 'stale GET discarded after save');
  assert.equal(element('proxyPoolEnabled').checked, true);
  assert.equal(element('settingsProxyPoolEnabled').checked, true, 'settings and page switch synced');
  handler = async (url) => {
    if (url === '/admin/api/proxy-pool/settings') return response({ error: 'test save failed' }, 500);
    throw new Error('Unexpected API call: ' + url);
  };
  await ui.saveProxyPoolSettings({ enabled: false });
  assert.equal(element('proxyPoolEnabled').checked, true, 'failed save rolls page switch back');
  assert.equal(element('settingsProxyPoolEnabled').checked, true, 'failed save rolls settings switch back');

  // Blank batch-edit fields must not overwrite each proxy's own URL.
  handler = async (url, options) => {
    if (url === '/admin/api/proxy-pool/batch') {
      assert.deepEqual(JSON.parse(options.body), { action: 'edit', ids: ['proxy-1', 'proxy-2'], patch: { scheme: 'socks5', username: 'new-user' } });
      return response(state(true));
    }
    if (url === '/admin/api/accounts') return response([]);
    throw new Error('Unexpected API call: ' + url);
  };
  ui.openProxyPoolEdit(['proxy-1', 'proxy-2']);
  element('proxyPoolEditUsername').value = 'new-user';
  element('proxyPoolEditScheme').value = 'socks5';
  await ui.submitProxyPoolEdit(event);
  assert.equal(element('proxyPoolEditModal').classList.contains('active'), false);

  // Import remains open with line-specific feedback on partial failure.
  handler = async (url, options) => {
    if (url === '/admin/api/proxy-pool/import') {
      assert.deepEqual(JSON.parse(options.body), { text: 'a|http://host.test:8080\ninvalid', defaultScheme: 'http' });
      return response({ ...state(true), imported: 1, duplicates: 0, errors: [{ line: 2, error: 'invalid proxy' }] });
    }
    if (url === '/admin/api/accounts') return response([]);
    throw new Error('Unexpected API call: ' + url);
  };
  element('proxyPoolImportBtn').listeners.click[0]();
  element('proxyPoolImportText').value = 'a|http://host.test:8080\ninvalid';
  element('proxyPoolImportScheme').value = 'http';
  await ui.submitProxyPoolImport(event);
  assert.ok(element('proxyPoolImportError').textContent.includes('2'));
  assert.equal(element('proxyPoolImportModal').classList.contains('active'), true);

  handler = async (url, options) => {
    if (url === '/admin/api/proxy-pool/check') {
      assert.deepEqual(JSON.parse(options.body), { ids: [] });
      return response({ ...state(true), inspection: { running: true, completed: 0, total: 2 } }, 202);
    }
    if (url === '/admin/api/proxy-pool') return response({ ...state(true), inspection: { running: true, completed: 1, total: 2 } });
    throw new Error('Unexpected API call: ' + url);
  };
  await ui.runProxyPoolAction('check', []);
  assert.equal(element('proxyPoolProgress').value, 1);
  assert.equal(element('proxyPoolProgress').max, 2);
  assert.equal(element('proxyPoolProgress').classList.contains('hidden'), false);
  assert.ok(ui.renderAccountProxy({ proxyPoolEnabled: true, proxyBlocked: true }).includes('已阻断'));
  assert.equal(ui.renderAccountProxy({ proxyPoolEnabled: false }), '');
  console.log('PASS proxy pool UI: locale/DOM contracts, escaping, disabled-origin/health badges and recovery semantics in zh/en, metrics, filters, selection, switch sync/stale responses/save rollback, batch edit, partial import, background inspection, blocked accounts');
  console.log('API requests exercised:', requests.length);
}
run().catch(error => { console.error(error); process.exitCode = 1; });
