// Run with: node --test ui_polling_test.cjs
// Exercise the actual embedded UI script with a simulated clock and network.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = Array.from(fs.readFileSync('ui.html', 'utf8').matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi), m => m[1]).join('\n');
const sample = (name, inactive = false) => ({ id: name, name, address: `http://${name}:18081`, inactive });
const agent = (host, id, inactive = false) => ({ id: `${host}-${id}`, name: `Agent ${id}`, address: `http://${host}:18081/${id}`, inactive });
const status = { remaining_seconds: 300, is_running: true, time_up_visible: false, on_behalf_of: [] };
const flush = async () => { for (let i = 0; i < 25; i++) await Promise.resolve(); };

function harness(initial, settings = { pollIntervalSeconds: 1, falsePolling: false }) {
  let now = 0, serial = 0;
  const timers = new Map(), nodes = new Map(), calls = [];
  const node = id => {
    if (!nodes.has(id)) {
      const el = {
        id,
        textContent: '', innerHTML: '', className: '', hidden: false, children: [], dataset: {},
        value: '', disabled: false, attributes: {}, checked: false,
        classList: {
          _set: new Set(),
          add(...tokens) {
            tokens.forEach(t => this._set.add(t));
            el.className = [...this._set].join(' ');
          },
          remove(...tokens) {
            tokens.forEach(t => this._set.delete(t));
            el.className = [...this._set].join(' ');
          },
          toggle(token, force) {
            let res;
            if (force === true) { this._set.add(token); res = true; }
            else if (force === false) { this._set.delete(token); res = false; }
            else if (this._set.has(token)) { this._set.delete(token); res = false; }
            else { this._set.add(token); res = true; }
            el.className = [...this._set].join(' ');
            return res;
          },
          contains(token) { return this._set.has(token); },
        },
        querySelectorAll() { return []; },
        focus() {}, append(){}, replaceChildren(){}, querySelector(){return null;}, insertBefore(){}, appendChild(){},
        setAttribute(name, value) { this.attributes[name] = String(value); },
        getAttribute(name) { return this.attributes[name] ?? null; },
        removeAttribute(name) { delete this.attributes[name]; },
        hasAttribute(name) { return Object.hasOwn(this.attributes, name); },
        close() { this.open = false; }, showModal() { this.open = true; },
        closest(selector) {
          return {
            querySelector(){return null;}, insertBefore(){}, appendChild(){},
            classList: {
              add() {}, remove() {}, toggle() {}, contains() { return false; },
            },
            querySelectorAll: sel => sel === '.timer-extension' ? state.extensions
              : sel === '.quick-add-apply' ? state.applies
              : sel === '.mobile-toggle' ? state.toggles : []
          };
        },
      };
      nodes.set(id, el);
    }
    return nodes.get(id);
  };
  const storage = new Map();
  const sessionStorage = new Map();
  if (settings !== null) storage.set('overlay-timer-ui-settings-v1', JSON.stringify(settings));
  const state = {
    list: initial, stallList: false, slow: new Set(), responses: new Map(), calls, node, storage,
    sessionStorage, toggles: [], extensions: [], applies: [], fetchOverride: null, lastAlert: null,
  };
  function schedule(fn, delay, interval = 0) {
    const d = Math.max(0, Number(delay) || 0);
    const id = ++serial;
    timers.set(id, { fn, at: now + d, interval: Math.max(0, Number(interval) || 0), serial: id });
    return id;
  }
  const localStorageMock = {
    getItem: key => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, String(value)),
    removeItem: key => storage.delete(key),
    clear: () => storage.clear(),
  };
  const sessionStorageMock = {
    getItem: key => sessionStorage.get(key) ?? null,
    setItem: (key, value) => sessionStorage.set(key, String(value)),
    removeItem: key => sessionStorage.delete(key),
    clear: () => sessionStorage.clear(),
  };
  const matchMediaMock = query => ({
    matches: false,
    media: query,
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() { return true; },
  });
  const contextObj = {
    console, AbortController, URL, URLSearchParams,
    performance: { now: () => now },
    localStorage: localStorageMock,
    sessionStorage: sessionStorageMock,
    matchMedia: matchMediaMock,
    alert: msg => { state.lastAlert = String(msg); },
    document: {
      documentElement:{dataset:{}},
      getElementById: node, createElement: tag => node(tag+serial++),
      querySelectorAll: selector => selector === '.mobile-toggle' ? state.toggles
        : selector === '.timer-extension' ? state.extensions
        : selector === '.quick-add-apply' ? state.applies : []
    },
    setTimeout: (fn, delay, ...args) => schedule(() => fn(...args), delay),
    clearTimeout: id => timers.delete(id),
    setInterval: (fn, delay, ...args) => schedule(() => fn(...args), delay, delay),
    clearInterval: id => timers.delete(id),
    fetch(url, options = {}) {
      if (state.fetchOverride) return state.fetchOverride(url, options);
      const urlStr = typeof url === 'string' ? url : url?.toString?.() || '';
      const call = { url: urlStr, options, at: now, pending: true, aborted: false };
      calls.push(call);
      return new Promise((resolve, reject) => {
        call.resolve = (data, statusCode = 200) => {
          call.pending = false;
          const status = typeof statusCode === 'number' ? statusCode : 200;
          resolve({
            ok: status >= 200 && status < 300,
            status,
            headers: { get: name => name.toLowerCase() === 'content-type' ? 'application/json' : null },
            text: async () => typeof data === 'string' ? data : (data === undefined ? '' : JSON.stringify(data)),
            json: async () => typeof data === 'string' ? JSON.parse(data) : data,
          });
        };
        call.fail = error => {
          call.pending = false;
          reject(error instanceof Error ? error : new Error(String(error)));
        };
        options.signal?.addEventListener('abort', () => {
          call.aborted = true;
          call.pending = false;
          reject(new Error('aborted'));
        }, { once: true });
        if (urlStr === '/api/pcs') {
          if (!state.stallList) call.resolve(structuredClone(state.list));
        } else if (!state.slow.has(urlStr)) {
          const response = state.responses.get(urlStr);
          const val = typeof response === 'function' ? response(call, state) : (response !== undefined ? response : status);
          call.resolve(val === null ? null : structuredClone(val));
        }
      });
    },
  };
  contextObj.window = contextObj;
  contextObj.globalThis = contextObj;
  const context = vm.createContext(contextObj);
  state.run = code => vm.runInContext(code, context);
  state.start = () => vm.runInContext(source, context);
  state.advance = async milliseconds => {
    const end = now + Math.max(0, Number(milliseconds) || 0);
    await flush();
    while (true) {
      const next = [...timers]
        .filter(([, t]) => t.at <= end)
        .sort((a, b) => a[1].at !== b[1].at ? a[1].at - b[1].at : a[1].serial - b[1].serial)[0];
      if (!next) break;
      const [id, timer] = next;
      now = timer.at;
      if (timer.interval) timer.at += timer.interval;
      else timers.delete(id);
      timer.fn();
      await flush();
    }
    now = end;
    await flush();
  };
  return state;
}

test('one online PC polls each second while nine offline PCs time out independently', async () => {
  const h = harness(Array.from({ length: 10 }, (_, i) => sample(`pc${i}`)));
  for (let i = 1; i < 10; i++) h.slow.add(`http://pc${i}:18081/status`);
  h.start();
  await h.advance(3500);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://pc0:18081/status').map(c => c.at), [0, 1000, 2000, 3000]);
  for (let i = 1; i < 10; i++) assert.equal(h.calls.filter(c => c.url === `http://pc${i}:18081/status`).length, 1);
  await h.advance(2500);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://pc0:18081/status').map(c => c.at), [0, 1000, 2000, 3000, 4000, 5000, 6000]);
  assert.ok(h.calls.some(c => c.url === 'http://pc1:18081/status' && c.aborted));
});

test('a stalled saved-list refresh does not delay status polling or duplicate list requests', async () => {
  const h = harness([sample('online')]);
  h.start();
  await h.advance(14000);
  h.stallList = true;
  await h.advance(1000);
  h.run('refreshSavedList(); refreshSavedList();');
  await h.advance(5000);
  assert.deepEqual(h.calls.filter(c => c.url === '/api/pcs').map(c => c.at), [0, 15000]);
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 21);
  assert.match(h.node('discoveryStatus').textContent, /Could not refresh saved PCs/);
  h.stallList = false;
  h.list.push(sample('new'));
  await h.advance(10000);
  assert.ok(h.calls.some(c => c.url === 'http://new:18081/status' && c.at === 30000));
});

test('ticks, manual refresh and detail navigation share pending requests', async () => {
  const h = harness([sample('slow'), sample('online')]);
  h.slow.add('http://slow:18081/status');
  h.start();
  await h.advance(0);
  h.run('showDetail("slow"); pollAll(); refreshNow();');
  await h.advance(3000);
  assert.equal(h.calls.filter(c => c.url === 'http://slow:18081/status').length, 1);
  assert.ok(h.calls.some(c => c.url === 'http://online:18081/status' && c.at === 3000));
});

test('list changes stop removed/inactive PCs and replace changed addresses', async () => {
  const h = harness([sample('old'), sample('removed'), sample('disabled')]);
  h.slow.add('http://old:18081/status');
  h.start();
  await h.advance(0);
  const old = h.calls.find(c => c.url === 'http://old:18081/status');
  h.list = [{ ...sample('old'), address: 'http://new-address:18081' }, sample('disabled', true)];
  await h.run('load()');
  await h.advance(2000);
  assert.equal(old.aborted, true);
  assert.equal(h.calls.filter(c => c.url === 'http://removed:18081/status').length, 1);
  assert.equal(h.calls.filter(c => c.url === 'http://disabled:18081/status').length, 1);
  assert.equal(h.calls.filter(c => c.url === 'http://new-address:18081/status').length, 3);
  assert.equal(h.node('ds-old').textContent, 'Running');
});

test('default settings poll every five seconds and leave false polling disabled', async () => {
  const h = harness([sample('pc')], null);
  h.start();
  await h.advance(4000);
  assert.equal(h.node('dt-pc').textContent, '00:05:00');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
  await h.advance(6000);
  assert.deepEqual(h.calls.filter(c => c.url.endsWith('/status')).map(c => c.at), [0, 5000, 10000]);
});

test('settings apply to all schedules, preserve pending requests, and survive reload', async () => {
  const h = harness([sample('online'), sample('slow')], null);
  h.slow.add('http://slow:18081/status');
  h.start();
  await h.advance(1000);
  h.run('openSettings()');
  assert.equal(h.node('pollingDialog').open,true);
  assert.equal(h.node('pollInterval').value,5);
  assert.equal(h.node('falsePollingState').textContent,"Disabled");
  h.node('pollInterval').value='2';
  h.run('saveSettings({preventDefault(){}});toggleFalsePolling()');
  assert.equal(h.node('pollingDialog').open,false);
  assert.equal(h.node('falsePollingState').textContent,"Enabled");
  await h.advance(4000);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://online:18081/status').map(c => c.at), [0, 3000, 5000]);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://slow:18081/status').map(c => c.at), [0, 5000]);
  const saved = JSON.parse(h.storage.get('overlay-timer-ui-settings-v1'));
  assert.deepEqual(saved, { pollIntervalSeconds: 2, falsePolling: true });
  const reloaded = harness([sample('pc')], saved);
  reloaded.start();
  await reloaded.advance(3000);
  assert.deepEqual(reloaded.calls.filter(c => c.url.endsWith('/status')).map(c => c.at), [0, 2000]);
  assert.equal(reloaded.node('dt-pc').textContent, '00:04:59');
});

test('false polling interpolates running timers and agents, preserves paused timers, and resyncs', async () => {
  const h = harness([sample('pc'), agent('pc', '1'), agent('pc', '2')], { pollIntervalSeconds: 5, falsePolling: true });
  h.responses.set('http://pc:18081/status', {
    ...status, remaining_seconds: 2135, on_behalf_of: [
      { ...status, id: '1', name: 'Running agent', remaining_seconds: 20 },
      { ...status, id: '2', name: 'Paused agent', remaining_seconds: 40, is_running: false },
    ]
  });
  h.start();
  await h.advance(0);
  h.run('showDetail("pc")');
  await h.advance(2000);
  assert.equal(h.node('dt-pc').textContent, '00:35:33');
  assert.equal(h.node('time').textContent, '00:35:33');
  assert.equal(h.node('dt-pc-1').textContent, '00:00:18');
  assert.equal(h.node('ds-pc-1').textContent, 'Running');
  assert.equal(h.node('dt-pc-2').textContent, '00:00:40');
  assert.equal(h.node('ds-pc-2').textContent, 'Paused');
  const calls = h.calls.length;
  h.run('falsePollingTick(); falsePollingTick();');
  assert.equal(h.calls.length, calls, 'local display ticks must not send requests');
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 90, is_running: false });
  await h.advance(4000);
  assert.equal(h.node('dt-pc').textContent, '00:01:30');
  assert.equal(h.node('ds-pc').textContent, 'Paused');
  assert.equal(h.node('ds-pc-1').textContent, 'Unavailable');
  assert.equal(h.node('dt-pc-1').textContent, '--:--:--');
});

test('discovered systems have separate cards and controls but one status request per host', async () => {
  const h = harness([sample('pc4'), agent('pc4', '1'), agent('pc4', '20'), sample('pc5'), agent('pc5', '1')]);
  h.responses.set('http://pc4:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '20', remaining_seconds: 20 },
      { ...status, id: '1', remaining_seconds: 91, is_running: false },
    ]
  });
  h.responses.set('http://pc5:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '1', remaining_seconds: 45 },
    ]
  });
  h.start();
  await h.advance(0);
  assert.equal(h.node('activeCount').textContent, '(5/5)');
  assert.equal((h.node('dashboardGrid').innerHTML.match(/<article /g) || []).length, 5);
  assert.equal(h.node('dt-pc4-1').textContent, '00:01:31');
  assert.equal(h.node('dt-pc5-1').textContent, '00:00:45');
  assert.equal(h.node('dt-pc4-20').textContent, '00:00:20');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 2);
  h.run('showDetail("pc4-1")');
  await flush();
  assert.equal(h.node('time').textContent, '00:01:31');
  assert.equal(h.node('overlayControls').hidden, true);
  await h.run('quick("pc4-1", "play")');
  await h.run('callSelected("set", {seconds: 120})');
  const play = h.calls.find(c => c.url === 'http://pc4:18081/1/play');
  const set = h.calls.find(c => c.url === 'http://pc4:18081/1/set');
  assert.equal(play.options.method, 'POST');
  assert.deepEqual(JSON.parse(set.options.body), { seconds: 120 });
  assert.ok(!h.calls.some(c => /\/\d+\/status$/.test(c.url)));
  h.run('showDetail("pc4")');
  assert.equal(h.node('overlayControls').hidden, false);
});

test('shared status polling continues for an active agent with its own timer unchecked', async () => {
  const h = harness([sample('pc', true), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', remaining_seconds: 17 }] });
  h.start();
  await h.advance(2000);
  assert.equal(h.node('dt-pc-1').textContent, '00:00:17');
  assert.deepEqual(h.calls.filter(c => c.url.endsWith('/status')).map(c => c.url), Array(3).fill('http://pc:18081/status'));
  const before = h.calls.length;
  assert.equal(await h.run('command("pc", "play")'), false);
  assert.equal(h.calls.length, before);
});

test('mobile toggles use each system status and route actions to its timer URL', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.toggles.push({ dataset: { pc: 'pc', running: 'false' } }, { dataset: { pc: 'pc-1', running: 'false' } });
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', is_running: false }] });
  h.start();
  await h.advance(0);
  assert.equal(h.toggles[0].textContent, 'Pause');
  assert.equal(h.toggles[1].textContent, 'Play');
  await h.run('toggleTimer(document.querySelectorAll(".mobile-toggle")[1], "pc-1")');
  assert.ok(h.calls.some(c => c.url === 'http://pc:18081/1/play' && c.options.method === 'POST'));
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1' }] });
  await h.advance(1000);
  assert.equal(h.toggles[1].textContent, 'Pause');
  await h.run('toggleTimer(document.querySelectorAll(".mobile-toggle")[1], "pc-1")');
  assert.ok(h.calls.some(c => c.url === 'http://pc:18081/1/pause' && c.options.method === 'POST'));
  assert.ok(!h.calls.some(c => /\/\d+\/status$/.test(c.url)));
});

test('removing one sibling retains a pending shared poll; removing the last cancels it', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.slow.add('http://pc:18081/status');
  h.start();
  await h.advance(0);
  const pending = h.calls.find(c => c.url.endsWith('/status'));
  h.list = [agent('pc', '1')];
  await h.run('load()');
  assert.equal(pending.aborted, false);
  h.run('pollAll(); showDetail("pc-1"); refreshNow();');
  await h.advance(1000);
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
  h.list = [];
  await h.run('load()');
  assert.equal(pending.aborted, true);
  await h.advance(4000);
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
});

test('adding an agent to a known host reuses its sample and missing agents do not borrow the local countdown', async () => {
  const h = harness([sample('pc')]);
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', remaining_seconds: 70 }] });
  h.start();
  await h.advance(0);
  h.list.push(agent('pc', '1'), agent('pc', '2'));
  await h.run('load()');
  assert.equal(h.node('dt-pc-1').textContent, '00:01:10');
  assert.equal(h.node('ds-pc-2').textContent, 'Unavailable');
  assert.equal(h.node('dt-pc-2').textContent, '--:--:--');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', remaining_seconds: -1 }, { ...status, id: '2' }] });
  await h.advance(1000);
  assert.equal(h.node('ds-pc').textContent, 'Running');
  assert.equal(h.node('ds-pc-1').textContent, 'Unavailable');
  assert.equal(h.node('ds-pc-2').textContent, 'Running');
});

test('false polling clamps at zero without inventing TIME UP and disabling restores the real sample', async () => {
  const h = harness([sample('pc')], { pollIntervalSeconds: 30, falsePolling: true });
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 2 });
  h.start();
  await h.advance(4000);
  assert.equal(h.node('dt-pc').textContent, '00:00:00');
  assert.equal(h.node('ds-pc').textContent, 'Running');
  h.run('applySettings({pollIntervalSeconds:30, falsePolling:false})');
  assert.equal(h.node('dt-pc').textContent, '00:00:02');
  await h.advance(2000);
  assert.equal(h.node('dt-pc').textContent, '00:00:02');
});

test('offline timers freeze until a fresh successful response arrives', async () => {
  const h = harness([sample('pc')], { pollIntervalSeconds: 5, falsePolling: true });
  h.start();
  await h.advance(0);
  h.slow.add('http://pc:18081/status');
  await h.advance(9500);
  assert.equal(h.node('ds-pc').textContent, 'Offline');
  const frozen = h.node('dt-pc').textContent;
  await h.advance(2500);
  assert.equal(h.node('dt-pc').textContent, frozen);
  h.slow.clear();
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 120 });
  await h.advance(3000);
  assert.equal(h.node('ds-pc').textContent, 'Running');
  assert.equal(h.node('dt-pc').textContent, '00:02:00');
});

test('invalid settings cannot overwrite the current interval', async () => {
  const h = harness([], null);
  h.start();
  await h.advance(0);
  for (const value of ['0', '-1', '1.5', '301', 'no']) {
    h.node('pollInterval').value = value;
    h.run('saveSettings({preventDefault() {}})');
    assert.match(h.node('settingsMessage').textContent, /whole number/);
    assert.equal(h.run('uiSettings.pollIntervalSeconds'), 5);
  }
});

test('special buttons use each timer labels and send only the name to the owning endpoint', async () => {
  const h = harness([sample('pc'), agent('pc', '1'), agent('pc', '20')]);
  const name = 'Switch "off" & <TV>';
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'Lock PC', short_name: 'LOCK' }],
    on_behalf_of: [
      { ...status, id: '1', special_commands: [{ name, short_name: 'OFF' }] },
      { ...status, id: '20', special_commands: [] },
    ],
  });
  h.start();
  await h.advance(0);
  assert.match(h.node('special-pc').innerHTML, /special-full-name">Lock PC</);
  assert.match(h.node('special-pc').innerHTML, /special-short-name" aria-hidden="true">LOCK</);
  assert.match(h.node('special-pc-1').innerHTML, /Switch &quot;off&quot; &amp; &lt;TV&gt;/);
  assert.match(h.node('special-pc-1').innerHTML, />OFF<\/span>/);
  assert.ok(!h.node('special-pc').innerHTML.includes('OFF'));
  assert.equal(h.node('special-pc-20').hidden, true);
  h.run('showDetail("pc-1")');
  await flush();
  assert.equal(h.node('detailSpecialCommands').innerHTML, h.node('special-pc-1').innerHTML);
  const polls = h.calls.filter(c => c.url.endsWith('/status')).length;
  await h.run(`specialCommand("pc-1", ${JSON.stringify(name)})`);
  await h.run('specialCommand("pc", "Lock PC")');
  const childCall = h.calls.find(c => c.url === 'http://pc:18081/1/special');
  const rootCall = h.calls.find(c => c.url === 'http://pc:18081/special');
  assert.equal(childCall.options.method, 'POST');
  assert.deepEqual(JSON.parse(childCall.options.body), { name });
  assert.deepEqual(JSON.parse(rootCall.options.body), { name: 'Lock PC' });
  assert.equal(h.node('msg').textContent, name + ': requested');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, polls);
  assert.ok(!h.calls.some(c => c.url.endsWith('/identity') || /\/\d+\/status$/.test(c.url)));
});

test('pending special commands disable both views and coalesce repeated clicks without blocking polls', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '1', special_commands: [{ name: 'switch_off', short_name: 'OFF' }] },
    ]
  });
  h.start();
  await h.advance(0);
  h.run('showDetail("pc-1")');
  await flush();
  h.slow.add('http://pc:18081/1/special');
  h.run('specialCommand("pc-1", "switch_off"); specialCommand("pc-1", "switch_off");');
  await h.advance(2000);
  assert.equal(h.calls.filter(c => c.url.endsWith('/special')).length, 1);
  assert.match(h.node('special-pc-1').innerHTML, /disabled/);
  assert.match(h.node('detailSpecialCommands').innerHTML, /disabled/);
  assert.ok(h.calls.some(c => c.url === 'http://pc:18081/status' && c.at === 2000));
  h.calls.find(c => c.url.endsWith('/special')).resolve({ status: 'accepted' });
  await flush();
  assert.ok(!h.node('special-pc-1').innerHTML.includes('disabled'));
  assert.ok(!h.node('detailSpecialCommands').innerHTML.includes('disabled'));
});

test('timer extension buttons target the selected timer, add exact seconds, and preserve paused state', async () => {
  const h = harness([sample('pc'), agent('pc', '1'), agent('pc', '2')]);
  h.responses.set('/api/sessions/extend', { session_found: false });
  const agentStatus = (one, two) => ({
    ...status, remaining_seconds: 300, on_behalf_of: [
      { ...status, id: '1', remaining_seconds: one, is_running: false },
      { ...status, id: '2', remaining_seconds: two, is_running: true },
    ]
  });
  h.responses.set('http://pc:18081/status', agentStatus(60, 75));
  h.start();
  await h.advance(0);
  const markup = h.run('activePCs().map(timerExtensionButtons).join("")');
  assert.equal((markup.match(/class="timer-extension"/g) || []).length, 15);
  for (const label of ['+15 min', '+30 min', '+1 hr']) assert.ok(markup.includes(`>${label}</button>`));

  let expected = 60;
  for (const amount of [900, 1800, 3600]) {
    expected += amount;
    h.responses.set('http://pc:18081/1/tweak', { remaining_seconds: expected });
    h.responses.set('http://pc:18081/status', agentStatus(expected, 75));
    await h.run(`extendTimer("pc-1", ${amount})`);
    const formatted = `${String(Math.floor(expected / 3600)).padStart(2, '0')}:${String(Math.floor((expected % 3600) / 60)).padStart(2, '0')}:${String(expected % 60).padStart(2, '0')}`;
    assert.equal(h.node('dt-pc-1').textContent, formatted);
    assert.equal(h.node('dt-pc-2').textContent, '00:01:15');
    assert.equal(h.node('dt-pc').textContent, '00:05:00');
    assert.equal(h.node('ds-pc-1').textContent, 'Paused');
  }
  const extensions = h.calls.filter(call => call.url === 'http://pc:18081/1/tweak');
  assert.deepEqual(extensions.map(call => JSON.parse(call.options.body)), [900, 1800, 3600].map(amount => ({ amount })));
  assert.equal(h.calls.filter(call => call.url.endsWith('/play')).length, 0);
  assert.equal(h.calls.filter(call => call.url.endsWith('/pause')).length, 0);
});

test('in-flight timer extension clicks are coalesced and update the cached display immediately', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.responses.set('/api/sessions/extend', { session_found: false });
  h.responses.set('http://pc:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '1', remaining_seconds: 60, is_running: false },
    ]
  });
  h.start();
  await h.advance(0);
  h.extensions.push(
    { dataset: { pc: 'pc-1' }, disabled: false },
    { dataset: { pc: 'pc-1' }, disabled: false },
    { dataset: { pc: 'pc-1' }, disabled: false },
  );
  h.slow.add('http://pc:18081/1/tweak');
  h.slow.add('http://pc:18081/status');
  h.run('extendTimer("pc-1", 900); extendTimer("pc-1", 900);');
  await flush();
  const extension = h.calls.filter(call => call.url === 'http://pc:18081/1/tweak');
  assert.equal(extension.length, 1);
  assert.ok(h.extensions.every(button => button.disabled));
  extension[0].resolve({ remaining_seconds: 960 });
  await flush();
  assert.equal(h.node('dt-pc-1').textContent, '00:16:00');
  assert.equal(h.node('ds-pc-1').textContent, 'Paused');
  assert.ok(h.extensions.every(button => button.disabled));
});

test('a billed dashboard extension uses one session update and refreshes the matching system card', async () => {
  const h = harness([sample('pc')]);
  const session = {
    id: 'S-session-a', pc_id: 'pc', pc_address: 'http://pc:18081', status: 'open',
    customer_name: 'Alice', category: 'PC Lite', purchased_minutes: 45,
    additional_minutes: 0, final_amount: 45, amount_paid: 45,
  };
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 960 });
  h.responses.set('/api/sessions/extend', { session_found: true, remaining_seconds: 960, session });
  h.responses.set('/api/sessions?status=open', [session]);
  h.start();
  await h.advance(0);

  await h.run('extendTimer("pc", 900)');

  assert.equal(h.node('dt-pc').textContent, '00:16:00');
  assert.match(h.node('bill-pc').textContent, /INR 45/);
  assert.equal(h.calls.filter(call => call.url === '/api/sessions/extend').length, 1);
  assert.equal(h.calls.filter(call => call.url === 'http://pc:18081/tweak').length, 0);
});

test('special commands work for active agents when the host timer is unchecked and reject cross-timer names', async () => {
  const h = harness([sample('pc', true), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'Lock PC', short_name: 'LOCK' }],
    on_behalf_of: [{ ...status, id: '1', special_commands: [{ name: 'switch_off', short_name: 'OFF' }] }],
  });
  h.start();
  await h.advance(0);
  await h.run('specialCommand("pc", "Lock PC"); specialCommand("pc-1", "Lock PC");');
  assert.equal(h.calls.filter(c => c.url.endsWith('/special')).length, 0);
  await h.run('specialCommand("pc-1", "switch_off")');
  assert.equal(h.calls.filter(c => c.url === 'http://pc:18081/1/special').length, 1);
});

test('special labels refresh from status and clear for missing commands or legacy hosts', async () => {
  const h = harness([sample('pc')]);
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [null, {}, { name: 'Valid command' }],
  });
  h.start();
  await h.advance(0);
  assert.match(h.node('special-pc').innerHTML, /special-short-name" aria-hidden="true">Valid command</);
  h.responses.set('http://pc:18081/status', status);
  await h.advance(1000);
  assert.equal(h.node('special-pc').innerHTML, '');
  assert.equal(h.node('special-pc').hidden, true);
  await h.run('specialCommand("pc", "Valid command")');
  assert.ok(!h.calls.some(c => c.url.endsWith('/special')));
});

test('special request timeouts restore buttons and display an error instead of success', async () => {
  const h = harness([sample('pc')]);
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'switch_off', short_name: 'OFF' }],
  });
  h.start();
  await h.advance(0);
  h.slow.add('http://pc:18081/special');
  const command = h.run('specialCommand("pc", "switch_off")');
  await h.advance(4100);
  await command;
  assert.match(h.node('dm-pc').textContent, /switch_off: aborted/);
  assert.ok(!h.node('special-pc').innerHTML.includes('disabled'));
  assert.equal(h.run('pendingSpecial.size'), 0);
});

test('unchanged status preserves special button elements despite browser HTML normalization', async () => {
  const h = harness([sample('pc')]);
  let markup = '', replacements = 0;
  Object.defineProperty(h.node('special-pc'), 'innerHTML', {
    get: () => markup,
    set(value) { markup = value.replace(/\s+/g, ' '); replacements++; },
  });
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'switch_off', short_name: 'OFF' }],
  });
  h.start();
  await h.advance(0);
  const before = replacements;
  await h.advance(3000);
  assert.equal(replacements, before, 'polling must not replace unchanged buttons and lose keyboard focus');
});

test('start accepts missing customer details and Save updates the same session',async()=>{
 const h=harness([sample("pc"),sample("other")]);
 h.responses.set("/api/pricing",{entries:[{category:"PC Pro",players:1,duration_minutes:15,amount:20}]});
 const started={id:"s1",pc_id:"pc",pc_address:"http://pc:18081",category:"PC Pro",status:"open",players:0,customer_name:"",updated_at:"v1"};
 h.responses.set("/api/sessions",started);
 h.responses.set("/api/sessions?status=open",[]);
 h.start();await h.advance(0);
 assert.equal(h.run("cardCategoryHTML(pcs[0])"),"");
 h.run('billingCatalog.entries.push({category:"Premium",group:"PC Pro",players:1,duration_minutes:15,amount:25})');
 assert.match(h.run("cardCategoryHTML(pcs[0])"),/<select/);
 h.node("cv-pc").value="+30";h.node("cf-pc").value="60";
 h.responses.set("/api/sessions?status=open",[started]);
 await h.run('startPCBillingSession("pc")');
 assert.match(h.node("sf-message-pc").textContent,/again/);
 assert.equal(h.calls.filter(c=>c.url==="/api/sessions" && c.options.method==="POST").length,0);
 await h.run('startPCBillingSession("pc")');
 const call=h.calls.find(c=>c.url==="/api/sessions" && c.options.method==="POST");
 assert.deepEqual(JSON.parse(call.options.body),{pc_id:"pc",category:"PC Pro",customer_name:"",players:1,duration_minutes:30,duration_seconds:1800,allow_incomplete:true});
 h.run('cardSessionInput("pc","customer","Alex");cardSessionInput("pc","players","1");quickFill($("btn"),"pc","+5","60")');
 const saved={...started,customer_name:"Alex",players:1,updated_at:"v2"};
 h.responses.set("/api/sessions/s1/save",saved);
 h.responses.set("/api/sessions?status=open",[saved]);
 await h.run('savePCBillingSession("pc",false)');
 const update=h.calls.find(c=>c.url==="/api/sessions/s1/save");
 assert.deepEqual(JSON.parse(update.options.body),{customer_name:"Alex",players:1,category:"PC Pro",timer_mode:"tweak",seconds:300,manual_price:null,set_manual_price:true,updated_at:"v1"});
 assert.equal(h.calls.filter(c=>c.url==="/api/sessions" && c.options.method==="POST").length,1);
});
test('quick buttons accumulate signed minutes on the selected timer',async()=>{
 const h=harness([sample("pc")]);h.start();await h.advance(0);
 h.node("cv-pc").value="15";h.node("cf-pc").value="60";
 for(const [value,expected] of [["+15","+15"],["+15","+30"],["-5","+25"],["+5","+30"]]){
 h.run('quickFill($("btn"),"pc",'+JSON.stringify(value)+',"60")');assert.equal(h.node("cv-pc").value,expected);}
});
test('consolidated report renders synchronized totals and incomplete device status', async () => {
  const h = harness([]);
  h.responses.set('/api/billing/report?date=2026-02-02', {
    date: '2026-02-02', timezone: 'Asia/Kolkata', complete: false,
    system_count: 2, session_count: 2, unique_customer_count: 1,
    customer_count: 2, billed_amount: 85, collected_amount: 75, revenue: 75, outstanding: 10,
    pcs: [{ device_id: 'PC-01', pc_id: 'pc1', pc_name: 'PC 1', session_count: 2, customer_count: 2, billed_amount: 85, collected_amount: 75, revenue: 75, outstanding: 10 }],
    devices: [{ device_id: 'PC-02', name: 'PC 2', online: false, pending_records: 1, error: 'offline' }],
    records: [{
      customer_name: 'Alice', pc_name: 'PC 1', pc_id: 'pc1', players: 2,
      started_at: '2026-02-02T10:00:00+05:30', expected_end_at: '2026-02-02T10:45:00+05:30',
      purchased_minutes: 45, additional_minutes: 15, amount_paid: 75, final_amount: 85,
      payment_status: 'partial',
    }],
  });

  h.start();
  await h.advance(0);
  await h.run("$('billingReportDate').value = '2026-02-02'; loadBillingReport()");
  assert.equal(h.node('reportSystems').textContent, '2');
  assert.equal(h.node('reportSessions').textContent, '2');
  assert.equal(h.node('reportCustomers').textContent, '1');
  assert.equal(h.node('reportBilled').textContent, 'INR 85');
  assert.equal(h.node('reportRevenue').textContent, 'INR 75');
  assert.match(h.node('billingReportPCs').innerHTML, /PC 1.*INR 85.*INR 75/);
  assert.match(h.node('billingReportDevices').innerHTML, /PC 2.*Offline.*offline/);
  assert.match(h.node('billingReportRecords').innerHTML, /Alice.*2026-02-02.*60 min.*15 min.*INR 85.*INR 75.*INR 10.*partial/);
  assert.match(h.node('billingReportStatus').textContent, /incomplete.*PC 2: offline.*last sync never/);
  await h.run('syncBillingNow()');
  assert.ok(h.calls.some(call => call.url === '/api/billing/sync' && call.options.method === 'POST'));
  assert.ok(!h.node('syncBillingButton').disabled);
});

test('central request checker adds correlation metadata and retains bounded request metrics', async () => {
  const h = harness([sample('pc-a')]);
  h.start();
  await flush();
  h.slow.add('/api/health');
  const promise = h.run('request("/api/health", {}, { deviceId: "PC-4", feature: "health-check" })');
  await flush();
  const call = h.calls.find(c => c.url === '/api/health');
  assert.equal(call.options.headers['X-Device-ID'], 'PC-4');
  assert.match(call.options.headers['X-Request-ID'], /^[A-Za-z0-9._-]+$/);
  call.resolve({ status: 'ok' });
  assert.equal((await promise).status, 'ok');
  const monitor = h.run('globalThis.__requestMonitor');
  assert.ok(monitor.recent.some(item => item.requestId === call.options.headers['X-Request-ID'] &&
    item.deviceId === 'PC-4' && item.feature === 'health-check' && item.ok));
});

test('central request checker rejects unexpected response formats without success metrics', async () => {
  const h = harness([sample('pc-a')]);
  h.start();
  await flush();
  h.fetchOverride = async () => ({
    ok: true, status: 200,
    headers: { get: () => 'text/html' },
    text: async () => '<html>not an API response</html>',
  });
  await assert.rejects(h.run('request("/api/health")'), /Unexpected response content type/);
  const monitor = h.run('globalThis.__requestMonitor');
  assert.ok(monitor.recent.some(item => item.path === '/api/health' && !item.ok));
});

test('central request checker counts HTTP 404 as failed and omits response bodies from diagnostics', async () => {
  const h = harness([]);
  h.start();
  await flush();
  h.fetchOverride = async () => ({
    ok: false, status: 404,
    headers: { get: () => 'text/plain' },
    text: async () => '404 page not found',
  });
  await assert.rejects(h.run('request("/internal/api/local/sessions/extend")'), /404 page not found/);
  const monitor = h.run('globalThis.__requestMonitor');
  const failure = (monitor.recent.findLast
    ? monitor.recent.findLast(item => item.path === '/internal/api/local/sessions/extend')
    : [...monitor.recent].reverse().find(item => item.path === '/internal/api/local/sessions/extend'));
  assert.equal(failure.status, 404);
  assert.equal(failure.ok, false);
  assert.equal(failure.error, 'HTTP 404');
  assert.ok(monitor.totals.notFound >= 1);
});

test('card timer adjustment and unit change execute properly with mock window and DOM', async () => {
  const h = harness([sample('pc-a')]);
  h.responses.set('http://pc-a:18081/tweak', { status: 'success', remaining_seconds: 900 });
  h.start();
  await h.advance(0);

  h.node('cv-pc-a').value = '+10';
  h.node('cf-pc-a').value = '60';
  h.run('cardUnitChanged("pc-a")');
  assert.equal(h.node('dm-pc-a').textContent, '');

  const tweak=h.run('cardTimerDraft("pc-a")');
  assert.equal(tweak.timer_mode,"tweak");
  assert.equal(tweak.seconds,600);
});


test('a bill follows its owner and virtual timer across addresses',async()=>{
  const h=harness([]);h.start();await h.advance(0);
  const same=h.run('sessionMatchesPC');
  const session={device_id:"Desk",pc_id:"old",pc_address:"http://192.0.2.1:18081/1"};
  const alias={owner:"Desk",id:"new",address:"http://198.51.100.2:18081/1"};
  assert.equal(same(session,alias),true);
  assert.equal(same(session,{...alias,owner:"Another desk"}),false);
  assert.equal(same(session,{...alias,address:"http://198.51.100.2:18081/2"}),false);
});

test('same-name hosts share a card and select an address while virtual timers stay distinct', async () => {
  const a = {...sample("first"),name:"Desk"};
  const b = {...sample("second"),name:"desk"};
  const h = harness([a,b,{...sample("link"),name:"Link",address:"http://169.254.42.1:18081"},
    {...agent("first","1"),owner:"Desk"}]);
  h.start();await h.advance(0);
  assert.equal(h.run("pcs.length"),2);
  assert.match(h.node("dashboardGrid").innerHTML, /<select aria-label="Address for Desk"/);
  h.run('choosePCAddress("first", "http://second:18081")');
  assert.equal(h.run("pcs[0].id"),"second");
  assert.equal(h.run("activePCs().length"),2);
});

test('system config exposes the category and keeps on-behalf configuration separate', async () => {
  const h = harness([sample("host"),agent("host","1")]);
  const config = {friendly_name:"Desk", pricing_group:"PC Pro", billing:{enabled:true},
    timer_port:18081, on_behalf_of:[{id:"1",name:"Screen",pricing_group:"Console"}]};
  for (const id of ["host","host-1"]) h.responses.set("/api/pcs/"+id+"/config",{config,full_access:true});
  h.start();await h.advance(0);
  h.run('showDetail("host")');
  await h.advance(0);
  assert.equal(h.node('detail').hidden,false);
  assert.equal(h.node("systemCategory").value,"PC Pro");
  assert.match(h.node("systemConfigTimers").innerHTML,/Details.*button/s);
  assert.match(h.node("systemAdvancedFields").innerHTML,/Timer port/);
  h.run('showDetail("host-1")');
  await h.advance(0);
  assert.equal(h.node("systemCategory").value,"Console");
  assert.match(h.node("systemAdvancedFields").innerHTML,/Show command/);
  assert.doesNotMatch(h.node("systemAdvancedFields").innerHTML,/Timer port/);
});

test('today bill opens a populated editor and sends customer, system, category, time and amounts',async()=>{
  const h=harness([sample("pc"),sample("other")]);
  h.start();await h.advance(0);
  h.run('todayBills=[{id:"bill1",customer_name:"Alex",pc_id:"pc",pc_name:"Desk",category:"PC Pro",players:1,status:"closed",started_at:"2026-11-26T15:00:00+05:30",ended_at:"2026-11-26T15:30:00+05:30",purchased_minutes:30,additional_minutes:0,actual_duration_seconds:1800,final_amount:80,amount_paid:60,updated_at:"version"}];openBillEditor("bill1")');
  assert.equal(h.node("billEditor").open,true);
  assert.equal(h.node("billStarted").value,"2026-11-26T15:00:00");
  h.node("billCustomer").value="Sam";
  h.node("billSystem").value="other";
  h.node("billCategory").value="Console";
  h.node("billPaid").value="90";
  await h.run('saveBill({preventDefault(){}})');
  const payload=JSON.parse(h.calls.find(c=>c.url==="/api/sessions/bill1/edit").options.body);
  assert.equal(payload.customer_name,"Sam");
  assert.equal(payload.pc_id,"other");
  assert.equal(payload.category,"Console");
  assert.equal(payload.started_at,"2026-11-26T15:00:00+05:30");
  assert.equal(payload.amount_paid,90);
});

test('today bills show newest first and offer continuation only on the latest closed bill per system',async()=>{
  const h=harness([sample("pc"),sample("other")]);
  h.start();await h.advance(0);
  const today=h.run("billingTodayDate()");
  const bill=(id,pc,status,time)=>({id,pc_id:pc,pc_address:"http://"+pc+":18081",pc_name:pc,
    status,customer_name:id,created_at:"2026-11-26T"+time+":00+05:30"});
  const latest=bill("latest","pc","closed","12:00");
  h.responses.set("/api/billing/report?date="+today,{records:[
    bill("old","pc","closed","11:00"),latest,bill("other","other","open","12:30")]});
  await h.run("loadToday()");
  const html=h.node("todayBills").innerHTML;
  assert.ok(html.indexOf("other")<html.indexOf("latest")&&html.indexOf("latest")<html.indexOf("old"));
  assert.equal((html.match(/Continue this session/g)||[]).length,2);
  h.node("todayBillsSearch").value="old";
  h.run("renderTodayBills()");
  assert.match(h.node("todayBills").innerHTML,/old/);
  assert.doesNotMatch(h.node("todayBills").innerHTML,/Continue this session|latest/);
  h.node("todayBillsSearch").value="";
  h.responses.set("/api/sessions/latest/continue",{...latest,status:"open"});
  h.responses.set("/api/sessions?status=open",[{...latest,status:"open"}]);
  await h.run('continueBill("latest",$("continue"))');
  assert.ok(h.calls.some(c=>c.url==="/api/sessions/latest/continue"));
  assert.equal(h.node("cv-pc").value,"");
  assert.match(h.node("sf-message-pc").textContent,/press Save/);
});

test('signed adjustments stay visible and apply once per Save',async()=>{
 const h=harness([sample("pc")]);h.start();await h.advance(0);
 const bill={id:"repeat",pc_id:"pc",pc_address:"http://pc:18081",status:"open",
   customer_name:"Alex",players:1,category:"PC Pro",updated_at:"v1",purchased_minutes:20,additional_minutes:0};
 h.run('applyCardSession(pcs[0],'+JSON.stringify(bill)+')');
 h.node("cv-pc").value="-5";h.node("cf-pc").value="60";
 h.run('cardNumberChanged("pc")');
 for(const version of ["v2","v3"]){
   const updated={...bill,updated_at:version};
   h.responses.set("/api/sessions/repeat/save",updated);
   h.responses.set("/api/sessions?status=open",[updated]);
   await h.run('savePCBillingSession("pc",false)');
   assert.equal(h.node("cv-pc").value,"-5");
 }
 assert.deepEqual(h.calls.filter(c=>c.url==="/api/sessions/repeat/save").map(c=>JSON.parse(c.options.body).seconds),[-300,-300]);
});
test('zero is the default start and style controls persist independently',async()=>{
 const h=harness([sample("pc")]);h.start();await h.advance(0);
 assert.match(h.node("dashboardGrid").innerHTML,/id="cv-pc"[^>]+value="0"/);
 h.node("cv-pc").value="0";h.node("cf-pc").value="60";
 h.run('cardSessionInput("pc","customer","Alex")');
 await h.run('startPCBillingSession("pc")');
 const start=h.calls.find(c=>c.url==="/api/sessions"&&c.options.method==="POST");
 assert.equal(JSON.parse(start.options.body).duration_seconds,0);
 h.run('setDashboardStyle("experimental");toggleTimerUnits()');
 assert.deepEqual(JSON.parse(h.storage.get("overlay-timer-dashboard-view-v1")),{style:"experimental",showUnits:true});
 assert.equal(h.node("dashboard").getAttribute("data-style"),"experimental");
});

test('customer name suggestions are loaded from saved session names',async()=>{
 const h=harness([sample("pc")]);
 h.responses.set("/api/customers",["Alice & Bob","Casey"]);
 h.start();await h.advance(0);
 await h.run("loadCustomerNameSuggestions()");
 assert.match(h.node("customerNameSuggestions").innerHTML,/Alice &amp; Bob/);
 assert.match(h.node("customerNameSuggestions").innerHTML,/Casey/);
 assert.match(h.node("sf-customer-pc").innerHTML||h.node("dashboardGrid").innerHTML,/list="customerNameSuggestions"/);
});

test('proxy mode sends host polls and on-behalf commands to same-origin registered-PC routes', async()=>{
 const h=harness([sample("pc")]);h.start();await h.advance(0);
 h.run('proxyEnabled=true');
 await h.run('timerRequest(pcs[0],"host","status",{})');
 await h.run('pcRequest("pc","pause",{method:"POST"})');
 const calls=h.calls.filter(c=>c.url.startsWith("/api/proxy/relay/"));
 assert.deepEqual(calls.map(c=>c.url),["/api/proxy/relay/pc/host/status","/api/proxy/relay/pc/timer/pause"]);
 assert.ok(calls.every(c=>c.options.credentials==="same-origin"));
});
