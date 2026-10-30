#!/usr/bin/env node
/*
 * render.js - screenshot `frontend/dist` the way the egui harness screenshots
 * the egui shell, so the two can be compared by pixels.
 *
 *   node render.js [--out DIR] [--dist DIR] [--chromium PATH] [--settle MS]
 *                  [--no-expand] [--no-toasts] [--no-events] [--keep-profile]
 *
 * What it does:
 *   1. serves frontend/dist over http on 127.0.0.1, injecting /__bridge.js in
 *      front of app.js (dist itself is never modified);
 *   2. launches Chromium headless and drives it over the DevTools protocol
 *      (no npm dependencies: Node's own WebSocket client and fetch);
 *   3. sets a 1180x780 viewport at deviceScaleFactor 1, navigates, waits for
 *      the first paint of the fixture, then clicks each nav button and saves
 *      <out>/<view>.png;
 *   4. verifies each PNG is 1180x780, not blank, and different from the other
 *      three, and prints the DOM text of a few fixture values.
 */

'use strict';

const http = require('node:http');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const zlib = require('node:zlib');
const crypto = require('node:crypto');
const { spawn } = require('node:child_process');

// ---------- arguments ----------

function argValue(name, fallback) {
  const i = process.argv.indexOf(name);
  if (i < 0) return fallback;
  const v = process.argv[i + 1];
  if (v === undefined || v.startsWith('--')) throw new Error(name + ' needs a value');
  return v;
}

const HERE = __dirname;
const REPO = path.resolve(HERE, '..', '..');
const OPTS = {
  width: 1180,
  height: 780,
  out: argValue('--out', '/tmp/web-shots'),
  dist: path.resolve(argValue('--dist', path.join(REPO, 'frontend', 'dist'))),
  bridge: path.join(HERE, 'bridge.js'),
  chromium: argValue('--chromium', '/Applications/Chromium.app/Contents/MacOS/Chromium'),
  settle: Number(argValue('--settle', '600')),
  bootTimeout: Number(argValue('--boot-timeout', '30000')),
  expand: !process.argv.includes('--no-expand'),
  toasts: !process.argv.includes('--no-toasts'),
  events: !process.argv.includes('--no-events'),
  keepProfile: process.argv.includes('--keep-profile'),
  views: ['transfers', 'watch', 'history', 'settings', 'space'],
};

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.ttf': 'font/ttf',
  '.ico': 'image/x-icon',
};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (...a) => console.log(...a);

// ---------- the static server ----------

// The injected script goes in front of app.js and nowhere else: app.js is a
// classic script at the end of <body>, so the bridge is defined before it runs.
function injectBridge(html) {
  const tag = '<script src="app.js"></script>';
  if (!html.includes(tag)) {
    throw new Error('index.html has no ' + tag + ' - the injection point moved');
  }
  const injected = '<script src="/__bridge.js"></script>\n<script src="app.js"></script>';
  return html.replace(tag, injected);
}

function startServer(distDir, bridgePath) {
  const server = http.createServer((req, res) => {
    let urlPath;
    try {
      urlPath = decodeURIComponent(new URL(req.url, 'http://127.0.0.1').pathname);
    } catch {
      res.writeHead(400).end('bad url');
      return;
    }
    if (urlPath === '/favicon.ico') {
      // Chromium asks for one; a 204 keeps the page console clean.
      res.writeHead(204).end();
      return;
    }
    if (urlPath === '/__bridge.js') {
      fs.readFile(bridgePath, (err, buf) => {
        if (err) return res.writeHead(500).end(String(err));
        res.writeHead(200, { 'content-type': MIME['.js'], 'cache-control': 'no-store' }).end(buf);
      });
      return;
    }
    if (urlPath === '/' || urlPath === '/index.html') {
      fs.readFile(path.join(distDir, 'index.html'), 'utf8', (err, html) => {
        if (err) return res.writeHead(500).end(String(err));
        let body;
        try {
          body = injectBridge(html);
        } catch (e) {
          return res.writeHead(500).end(String(e.message));
        }
        res.writeHead(200, { 'content-type': MIME['.html'], 'cache-control': 'no-store' }).end(body);
      });
      return;
    }
    const file = path.resolve(distDir, '.' + urlPath);
    if (!file.startsWith(distDir + path.sep)) {
      res.writeHead(403).end('outside dist');
      return;
    }
    fs.readFile(file, (err, buf) => {
      if (err) return res.writeHead(404).end('not found');
      res
        .writeHead(200, {
          'content-type': MIME[path.extname(file)] || 'application/octet-stream',
          'cache-control': 'no-store',
        })
        .end(buf);
    });
  });
  return new Promise((resolve, reject) => {
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({
        url: 'http://127.0.0.1:' + port + '/',
        port,
        close: () =>
          new Promise((r) => {
            server.closeAllConnections?.();
            server.close(r);
          }),
      });
    });
  });
}

// ---------- chromium ----------

async function launchChromium(bin, profileDir, width, height) {
  const args = [
    '--headless=new',
    '--remote-debugging-port=0',
    '--user-data-dir=' + profileDir,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-extensions',
    '--disable-background-networking',
    '--disable-component-update',
    '--disable-sync',
    '--metrics-recording-only',
    '--force-color-profile=srgb',
    '--force-device-scale-factor=1',
    '--hide-scrollbars',
    '--lang=en-GB',
    '--window-size=' + width + ',' + height,
    '--autoplay-policy=no-user-gesture-required',
    'about:blank',
  ];
  const child = spawn(bin, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', (d) => {
    stderr += d.toString();
  });
  child.on('exit', (code) => {
    child.exitCode = code;
  });

  const activePort = path.join(profileDir, 'DevToolsActivePort');
  const deadline = Date.now() + 20000;
  let port = 0;
  while (Date.now() < deadline) {
    try {
      const text = await fsp.readFile(activePort, 'utf8');
      port = Number(text.split('\n')[0]);
      if (port) break;
    } catch {
      /* not written yet */
    }
    await sleep(100);
  }
  if (!port) {
    child.kill('SIGKILL');
    throw new Error('Chromium did not open a DevTools port.\n' + stderr.slice(-2000));
  }

  let list = [];
  const listDeadline = Date.now() + 10000;
  while (Date.now() < listDeadline) {
    try {
      const res = await fetch('http://127.0.0.1:' + port + '/json/list');
      list = await res.json();
      if (list.some((t) => t.type === 'page')) break;
    } catch {
      /* browser is still starting */
    }
    await sleep(100);
  }
  const page = list.find((t) => t.type === 'page');
  if (!page) {
    child.kill('SIGKILL');
    throw new Error('Chromium has no page target.\n' + stderr.slice(-2000));
  }
  return { child, port, wsUrl: page.webSocketDebuggerUrl, stderr: () => stderr };
}

// ---------- a minimal DevTools-protocol client ----------

class Cdp {
  constructor(url) {
    this.url = url;
    this.id = 0;
    this.pending = new Map();
    this.handlers = new Map();
    this.waiters = [];
    this.ws = null;
  }

  async open() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => {
      this.ws.addEventListener('open', resolve, { once: true });
      this.ws.addEventListener('error', (e) => reject(new Error('websocket: ' + (e.message || 'error'))), {
        once: true,
      });
    });
    this.ws.addEventListener('message', (ev) => this._onMessage(String(ev.data)));
  }

  _onMessage(text) {
    let msg;
    try {
      msg = JSON.parse(text);
    } catch {
      return;
    }
    if (msg.id !== undefined) {
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new Error(msg.error.message + ' (' + (msg.error.data || '') + ')'));
      else p.resolve(msg.result || {});
      return;
    }
    const method = msg.method;
    const hits = this.waiters.filter((w) => w.method === method);
    for (const w of hits) {
      if (!w.once || !w.done) {
        w.done = true;
        w.resolve(msg.params || {});
      }
    }
    this.waiters = this.waiters.filter((w) => !(w.once && w.done));
    const registered = this.handlers.get(method);
    if (registered) for (const h of registered) h(msg.params || {});
  }

  on(method, cb) {
    if (!this.handlers.has(method)) this.handlers.set(method, []);
    this.handlers.get(method).push(cb);
  }

  once(method, timeoutMs = 30000) {
    return new Promise((resolve, reject) => {
      const w = { method, once: true, done: false };
      // The timer is cleared when the event lands and never holds the process
      // open on its own (`unref`): the render must exit as soon as it is done.
      const timer = setTimeout(() => {
        if (w.done) return;
        w.done = true;
        this.waiters = this.waiters.filter((x) => x !== w);
        reject(new Error('timed out waiting for ' + method));
      }, timeoutMs);
      timer.unref?.();
      w.resolve = (params) => {
        clearTimeout(timer);
        resolve(params);
      };
      w.reject = (e) => {
        clearTimeout(timer);
        reject(e);
      };
      this.waiters.push(w);
    });
  }

  send(method, params = {}) {
    const id = ++this.id;
    const payload = JSON.stringify({ id, method, params });
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      if (!this.ws || this.ws.readyState !== 1) {
        this.pending.delete(id);
        reject(new Error('websocket is not open (' + method + ')'));
        return;
      }
      this.ws.send(payload);
    });
  }

  // Runtime.evaluate, returning the value by value; awaitPromise by default.
  async evaluate(expression, { awaitPromise = true } = {}) {
    const res = await this.send('Runtime.evaluate', {
      expression,
      awaitPromise,
      returnByValue: true,
      userGesture: true,
    });
    if (res.exceptionDetails) {
      const d = res.exceptionDetails;
      throw new Error('evaluate failed: ' + (d.exception?.description || d.text));
    }
    return res.result?.value;
  }

  async waitFor(expression, timeoutMs, pollMs = 50) {
    const deadline = Date.now() + timeoutMs;
    let last;
    while (Date.now() < deadline) {
      try {
        last = await this.evaluate(expression, { awaitPromise: false });
        if (last) return last;
      } catch (e) {
        last = String(e.message || e);
      }
      await sleep(pollMs);
    }
    throw new Error('timed out waiting for: ' + expression + ' (last: ' + JSON.stringify(last) + ')');
  }

  close() {
    try {
      this.ws?.close();
    } catch {
      /* already gone */
    }
  }
}

// ---------- PNG reading, for the checks ----------

function decodePng(buf) {
  if (buf.readUInt32BE(0) !== 0x89504e47) throw new Error('not a PNG');
  let off = 8;
  let ihdr = null;
  const idat = [];
  while (off + 8 <= buf.length) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString('ascii', off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    if (type === 'IHDR') {
      ihdr = {
        width: data.readUInt32BE(0),
        height: data.readUInt32BE(4),
        bitDepth: data[8],
        colorType: data[9],
        interlace: data[12],
      };
    } else if (type === 'IDAT') {
      idat.push(data);
    } else if (type === 'IEND') {
      break;
    }
    off += 12 + len;
  }
  if (!ihdr) throw new Error('PNG has no IHDR');
  if (ihdr.bitDepth !== 8) throw new Error('PNG bit depth ' + ihdr.bitDepth + ' is not 8');
  if (ihdr.interlace !== 0) throw new Error('interlaced PNG');
  const channels = { 0: 1, 2: 3, 4: 2, 6: 4 }[ihdr.colorType];
  if (!channels) throw new Error('PNG colour type ' + ihdr.colorType + ' is not supported');

  const raw = zlib.inflateSync(Buffer.concat(idat));
  const bpp = channels;
  const stride = ihdr.width * bpp;
  const out = Buffer.alloc(stride * ihdr.height);
  let pos = 0;
  for (let y = 0; y < ihdr.height; y++) {
    const filter = raw[pos++];
    const line = raw.subarray(pos, pos + stride);
    pos += stride;
    const cur = out.subarray(y * stride, (y + 1) * stride);
    const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null;
    for (let x = 0; x < stride; x++) {
      const a = x >= bpp ? cur[x - bpp] : 0;
      const b = prev ? prev[x] : 0;
      const c = prev && x >= bpp ? prev[x - bpp] : 0;
      let v = line[x];
      switch (filter) {
        case 0:
          break;
        case 1:
          v += a;
          break;
        case 2:
          v += b;
          break;
        case 3:
          v += (a + b) >> 1;
          break;
        case 4: {
          const p = a + b - c;
          const pa = Math.abs(p - a);
          const pb = Math.abs(p - b);
          const pc = Math.abs(p - c);
          v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
          break;
        }
        default:
          throw new Error('PNG filter ' + filter);
      }
      cur[x] = v & 0xff;
    }
  }
  return { width: ihdr.width, height: ihdr.height, channels, pixels: out };
}

// How many pixels of two same-size images differ, as a fraction.
function diffFraction(a, b) {
  if (a.width !== b.width || a.height !== b.height) return 1;
  const n = a.width * a.height;
  let different = 0;
  for (let i = 0; i < n; i++) {
    const o = i * a.channels;
    if (
      a.pixels[o] !== b.pixels[o] ||
      a.pixels[o + 1] !== b.pixels[o + 1] ||
      a.pixels[o + 2] !== b.pixels[o + 2]
    ) {
      different++;
    }
  }
  return different / n;
}

// Distinct colours on a coarse grid, as a cheap "is anything drawn" measure.
function distinctColors(img, step = 7) {
  const seen = new Set();
  for (let y = 0; y < img.height; y += step) {
    for (let x = 0; x < img.width; x += step) {
      const o = (y * img.width + x) * img.channels;
      seen.add((img.pixels[o] << 16) | (img.pixels[o + 1] << 8) | img.pixels[o + 2]);
    }
  }
  return seen.size;
}

// ---------- the render ----------

async function main() {
  const failures = [];
  const check = (ok, message) => {
    if (!ok) failures.push(message);
    log((ok ? 'ok   ' : 'FAIL ') + message);
  };

  for (const p of [OPTS.dist, OPTS.bridge]) {
    if (!fs.existsSync(p)) throw new Error('missing ' + p);
  }
  if (!fs.existsSync(OPTS.chromium)) throw new Error('missing Chromium at ' + OPTS.chromium);
  await fsp.mkdir(OPTS.out, { recursive: true });

  const server = await startServer(OPTS.dist, OPTS.bridge);
  const profileDir = await fsp.mkdtemp(path.join(os.tmpdir(), 'web-ref-profile-'));
  log('server   ' + server.url + '  (dist: ' + OPTS.dist + ')');

  let chromium;
  let cdp;
  const cleanup = async () => {
    try {
      cdp?.close();
    } catch {}
    try {
      chromium?.child.kill('SIGTERM');
    } catch {}
    await sleep(200);
    try {
      if (chromium?.child && chromium.child.exitCode === null) chromium.child.kill('SIGKILL');
    } catch {}
    try {
      await server.close();
    } catch {}
    if (!OPTS.keepProfile) {
      await fsp.rm(profileDir, { recursive: true, force: true }).catch(() => {});
    }
  };

  try {
    chromium = await launchChromium(OPTS.chromium, profileDir, OPTS.width, OPTS.height);
    log('chromium pid ' + chromium.child.pid + ', devtools port ' + chromium.port);

    cdp = new Cdp(chromium.wsUrl);
    await cdp.open();

    const consoleLines = [];
    const exceptions = [];
    cdp.on('Runtime.consoleAPICalled', (p) => {
      consoleLines.push(
        p.type + ': ' + (p.args || []).map((a) => a.value ?? a.description ?? a.type).join(' '),
      );
    });
    cdp.on('Runtime.exceptionThrown', (p) => {
      exceptions.push(p.exceptionDetails?.exception?.description || p.exceptionDetails?.text || 'exception');
    });
    cdp.on('Log.entryAdded', (p) => {
      const e = p.entry || {};
      if (e.level === 'error') consoleLines.push('log ' + e.level + ': ' + e.text + ' ' + (e.url || ''));
    });

    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Log.enable');
    await cdp.send('Emulation.setDeviceMetricsOverride', {
      width: OPTS.width,
      height: OPTS.height,
      deviceScaleFactor: 1,
      mobile: false,
      screenWidth: OPTS.width,
      screenHeight: OPTS.height,
      screenOrientation: { type: 'landscapePrimary', angle: 90 },
    });
    await cdp.send('Emulation.setLocaleOverride', { locale: 'en-GB' }).catch(() => {});
    await cdp.send('Emulation.setTimezoneOverride', { timezoneId: 'Europe/London' }).catch(() => {});

    const loaded = cdp.once('Page.loadEventFired', 30000);
    await cdp.send('Page.navigate', { url: server.url });
    await loaded;
    log('navigated ' + server.url);

    // The first paint of the fixture: the running job's row exists and carries
    // the fixture's title.
    await cdp.waitFor(
      "document.querySelector('#jobs .row[data-id=\"up-1\"] .name') && document.querySelector('#jobs .row[data-id=\"up-1\"] .name').textContent",
      OPTS.bootTimeout,
    );
    // Inter, the stylesheet's face, is a webfont: wait for it before measuring.
    await cdp.evaluate('document.fonts.ready.then(() => true)');

    // The event handlers app.js registered, and the methods it asked for.
    const bridgeState = await cdp.evaluate(
      '({ calls: window.__bridge.calls, unhandled: window.__bridge.unhandled, listening: window.__bridge.listening() })',
    );
    log('boot calls: ' + [...new Set(bridgeState.calls)].join(', '));
    log('events registered: ' + bridgeState.listening.join(', '));
    check(bridgeState.unhandled.length === 0, 'the bridge answered every method the front end called');
    if (bridgeState.unhandled.length) log('unhandled: ' + bridgeState.unhandled.join(', '));

    // Emit the two events the front end listens for, so the event path is
    // exercised with the same payload rather than left untested.
    if (OPTS.events) {
      const delivered = await cdp.evaluate(`(() => {
        const snap = window.__bridge.emit('snapshot', window.__bridge.fixture.snapshot);
        const state = window.__bridge.emit('state-changed', null);
        return { snap, state };
      })()`);
      log('emitted snapshot (listeners: ' + delivered.snap + ') and state-changed (listeners: ' + delivered.state + ')');
      await sleep(150);
    }

    // The egui fixture opens job `up-1` and shows its two files; the web front
    // end opens a row on a click, so click it.
    if (OPTS.expand) {
      const opened = await cdp.evaluate(`(() => {
        const row = document.querySelector('#jobs .row[data-id="up-1"]');
        if (!row) return 'no row';
        const btn = row.querySelector('.a-files');
        if (!btn || btn.classList.contains('hidden')) return 'no file button';
        btn.click();
        return true;
      })()`);
      if (opened === true) {
        await cdp.waitFor("document.querySelectorAll('#jobs .row[data-id=\"up-1\"] .frow').length", 5000);
        log('expanded up-1, files drawn: ' + (await cdp.evaluate("document.querySelectorAll('#jobs .row[data-id=\"up-1\"] .frow').length")));
      } else {
        log('could not expand up-1: ' + opened);
      }
    }

    // The egui fixture opens with two toasts. The web front end has no fixture
    // channel for them, so they go through the page's own toast().
    if (OPTS.toasts) {
      const toasts = await cdp.evaluate(`(() => {
        if (typeof window.toast !== 'function') return 'no toast()';
        window.toast('Share link copied', 'ok', 600000);
        window.toast('Upload failed: A006_C003_sh020_v002.mov \\u2014 the connection dropped', 'err', 600000);
        return document.querySelectorAll('#toasts .toast').length;
      })()`);
      log('toasts drawn: ' + toasts);
    }

    await sleep(OPTS.settle);

    const shots = [];
    for (const view of OPTS.views) {
      const clicked = await cdp.evaluate(`(() => {
        const b = document.querySelector('[data-view="${view}"]');
        if (!b) return 'no nav button';
        b.click();
        return b.classList.contains('active');
      })()`);
      if (clicked !== true) throw new Error('nav button ' + view + ' did not take: ' + clicked);
      await cdp.waitFor(`document.querySelector('#view-${view}').classList.contains('active')`, 5000);
      await sleep(OPTS.settle);

      const shot = await cdp.send('Page.captureScreenshot', {
        format: 'png',
        fromSurface: true,
        captureBeyondViewport: false,
      });
      const file = path.join(OPTS.out, view + '.png');
      const buf = Buffer.from(shot.data, 'base64');
      await fsp.writeFile(file, buf);
      shots.push({ view, file, buf });
      log('shot     ' + file + '  ' + buf.length + ' bytes');
    }

    // ---- the DOM values a person would read off the screen ----
    await cdp.evaluate("document.querySelector('[data-view=\"transfers\"]').click()");
    await sleep(200);
    const dom = await cdp.evaluate(`(() => {
      const text = (sel) => { const e = document.querySelector(sel); return e ? e.textContent.trim() : null; };
      const row = (id) => document.querySelector('#jobs .row[data-id="' + id + '"]');
      const cell = (id, sel) => { const r = row(id); const e = r && r.querySelector(sel); return e ? e.textContent.trim() : null; };
      return {
        'jobs/up-1/name': cell('up-1', '.name'),
        'jobs/up-1/sub': cell('up-1', '.sub'),
        'jobs/up-1/pct': cell('up-1', '.pct'),
        'jobs/up-1/size': cell('up-1', '.c-size'),
        'jobs/up-1/speed': cell('up-1', '.c-speed'),
        'jobs/up-1/status': cell('up-1', '.c-status'),
        'jobs/up-1/files': [...document.querySelectorAll('#jobs .row[data-id="up-1"] .frow .fn')].map((e) => e.textContent.trim()).join(' | '),
        'jobs/dn-1/name': cell('dn-1', '.name'),
        'jobs/dn-1/size': cell('dn-1', '.c-size'),
        'jobs/dn-1/speed': cell('dn-1', '.c-speed'),
        'jobs/dn-1/status': cell('dn-1', '.c-status'),
        'sidebar/speed-up': text('#speed-up'),
        'sidebar/speed-down': text('#speed-down'),
        'sidebar/streams': text('#streams'),
        'sidebar/active': text('#active-count'),
        'sidebar/user': text('#account .who .n'),
        'sidebar/email': text('#account .who .e'),
        'sidebar/watches': text('#watch-count'),
        'settings/client-id': document.querySelector('#client-id') ? document.querySelector('#client-id').value : null,
        'settings/streams': text('#streams-val'),
        'settings/segment': document.querySelector('#segment') ? document.querySelector('#segment').value : null,
        'settings/default-dir': text('#default-dir'),
        'settings/redirect': text('#redirect-uri'),
        'settings/version': text('#about'),
      };
    })()`);

    const expected = {
      'jobs/up-1/name': 'A007_C012_sh010_v003.mov',
      'jobs/up-1/sub': 'Uploading 3 of 8 files',
      'jobs/up-1/size': '3.15 GB / 8.40 GB',
      'jobs/up-1/speed': '92.4 MB/s',
      'jobs/up-1/status': '54s left',
      'jobs/dn-1/name': 'Nightfall_Ep04_Dailies',
      'jobs/dn-1/status': '42 files · 12m 34s',
      'sidebar/speed-up': '92.4 MB/s',
      'sidebar/streams': '16 / 16',
      'sidebar/active': '1',
      'sidebar/user': 'Dana Whitfield',
      'settings/client-id': '9f2c1a4e',
      'settings/streams': '16',
      'settings/segment': '64',
    };

    log('');
    log('DOM values (transfers screen, then the settings screen\'s controls):');
    for (const [k, v] of Object.entries(dom)) {
      const want = expected[k];
      const mark = want === undefined ? '     ' : want === v ? 'ok   ' : 'FAIL ';
      if (want !== undefined && want !== v) failures.push('DOM ' + k + ' is ' + JSON.stringify(v) + ', expected ' + JSON.stringify(want));
      log('  ' + mark + k.padEnd(24) + JSON.stringify(v) + (want !== undefined && want !== v ? '   want ' + JSON.stringify(want) : ''));
    }
    log('');

    // The watch and history screens are filled by their own calls; read one
    // value from each so a silently empty list cannot pass.
    await cdp.evaluate("document.querySelector('[data-view=\"watch\"]').click()");
    await sleep(300);
    const watchRow = await cdp.evaluate(
      "document.querySelectorAll('#watch-body .row').length + ' rows, first: ' + (document.querySelector('#watch-body .row .name')||{}).textContent",
    );
    await cdp.evaluate("document.querySelector('[data-view=\"history\"]').click()");
    await sleep(300);
    const histRow = await cdp.evaluate(
      "document.querySelectorAll('#history-body .row').length + ' rows, first: ' + (document.querySelector('#history-body .row .name')||{}).textContent + ' ' + (document.querySelector('#history-body .row .h-files')||{}).textContent",
    );
    log('watch    ' + watchRow);
    log('history  ' + histRow);
    check(/^2 rows, first: Renders$/.test(watchRow), 'the watch screen lists the fixture\'s two watches');
    // The Space screen: the donut, the legend, the walking state, and the
    // delete dialog, which is the only destructive path in the app.
    await cdp.evaluate("document.querySelector('[data-view=\"space\"]').click()");
    await sleep(300);
    const space = await cdp.evaluate(`(() => {
      const rows = [...document.querySelectorAll('#space-legend .row')];
      const arcs = [...document.querySelectorAll('#space-chart .slice')];
      const last = rows[rows.length - 1];
      return {
        rows: rows.length,
        first: rows[0] ? rows[0].title : '',
        last: last ? last.title : '',
        arcs: arcs.length,
        fills: arcs.map((a) => a.getAttribute('fill')),
        total: (document.querySelector('#space-total') || {}).textContent || '',
        warn: [...document.querySelectorAll('#space-legend .space-warn')].map((e) => e.getAttribute('title')),
        svg: !!document.querySelector('#space-chart svg'),
        quota: [...document.querySelectorAll('#space-quota .space-quota-line')].map((e) => e.textContent.trim()),
        quotaShown: !document.querySelector('#space-quota').classList.contains('hidden'),
        subs: rows.map((r) => r.querySelector('.sub').textContent.trim()),
        files: rows.map((r) => r.querySelectorAll('.cell')[1].textContent.trim()),
      };
    })()`);
    log('space    ' + JSON.stringify(space));
    check(space.rows === 5, 'the space legend lists every project');
    check(space.first === 'Nightfall Ep 04 — 92.5 GB — 1432 files — walked', 'the legend reads name — size — files — source, largest first');
    check(space.last === 'Archive 2021 — 0 B — 0 files — walked', 'a project that holds nothing still has a line');
    check(space.arcs === 4 && new Set(space.fills).size === 4, 'one arc per project that holds something, four distinct colours');
    check(space.fills[0] === '#5b53ff', 'the largest project is drawn in the brand colour');
    check(space.total === '118 GB · 5 projects', 'the header carries the total and the project count');
    check(space.warn.length === 1 && /permission denied/.test(space.warn[0]), 'a walk that failed is marked, with its reason');
    // The row says where its figure came from, and a row Frame.io sized shows a
    // dash for files instead of a 0 that would mean "no files".
    check(space.subs[0] === 'Shows · Nightfall · walked', 'a walked row says it was walked (' + space.subs[0] + ')');
    check(space.subs[2] === 'Marketing · Nightfall · from Frame.io', 'a row Frame.io sized says so (' + space.subs[2] + ')');
    check(space.files[0] === '1432' && space.files[2] === '—', 'only a walk prints a file count (' + space.files.join(', ') + ')');
    // The headline: used / limit / left per account, and the two cases Frame.io
    // answers in words rather than numbers.
    check(space.quotaShown, 'the space screen has a headline');
    check(
      space.quota[0] === 'Nightfall · 118 GB of 500 GB used · 382 GB left',
      'a limit and a usage read used of limit, and what is left (' + space.quota[0] + ')',
    );
    check(
      space.quota[1] === 'Nightfall Agency · 1.05 GB used · no limit',
      'storage_limit: null reads "no limit", not a limit of 0 B (' + space.quota[1] + ')',
    );
    check(
      space.quota[2] === 'Quiet Account · 2.00 TB limit · usage not reported',
      'an account that reports no usage says so (' + space.quota[2] + ')',
    );

    // Where the pieces landed: the chart left of the legend, the arcs inside the
    // SVG box, the hole in the middle of the ring, and no sideways scrolling.
    const geom = await cdp.evaluate(`(() => {
      const box = (el) => {
        const b = el.getBoundingClientRect();
        return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), right: Math.round(b.right), bottom: Math.round(b.bottom) };
      };
      const svg = document.querySelector('#space-chart svg');
      const vb = svg.viewBox.baseVal;
      const view = document.querySelector('#view-space');
      const arcs = [...svg.querySelectorAll('path.slice')];
      const arcBoxes = arcs.map((a) => {
        const b = a.getBBox();
        return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), d: a.getAttribute('d').length };
      });
      const textBoxes = [...svg.querySelectorAll('text')].map((t) => {
        const b = t.getBBox();
        return { s: t.textContent, x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) };
      });
      const sr = svg.getBoundingClientRect();
      const centre = document.elementFromPoint(sr.x + sr.width / 2, sr.y + sr.height / 2);
      const del = document.querySelector('#space-legend .space-del');
      const legend = document.querySelector('.space-legend').getBoundingClientRect();
      const row = document.querySelector('#space-legend .row').getBoundingClientRect();
      return {
        chart: box(document.querySelector('#space-chart')),
        legend: box(document.querySelector('.space-legend')),
        quota: box(document.querySelector('#space-quota')),
        body: box(document.querySelector('#space-body')),
        head: box(document.querySelector('#view-space .page-head')),
        svg: box(svg),
        viewBox: { w: vb.width, h: vb.height },
        overflow: view.scrollWidth - view.clientWidth,
        arcBoxes,
        textBoxes,
        centre: centre ? centre.tagName + (centre.getAttribute('class') ? '.' + centre.getAttribute('class') : '') : null,
        rowH: Math.round(row.height),
        del: box(del),
        legendRight: Math.round(legend.right),
        legendBottom: Math.round(legend.bottom),
        viewBottom: Math.round(view.getBoundingClientRect().bottom),
      };
    })()`);
    log('geometry ' + JSON.stringify(geom));
    const radius = (p, cx, cy) => Math.hypot(Math.max(Math.abs(p.x - cx), Math.abs(p.x + p.w - cx)), Math.max(Math.abs(p.y - cy), Math.abs(p.y + p.h - cy)));
    check(geom.chart.right <= geom.legend.x, 'the chart sits left of the legend, not over it');
    check(geom.head.bottom <= geom.quota.y + 1, 'the headline sits under the page head');
    check(geom.quota.bottom <= geom.body.y, 'the headline sits above the chart and the legend');
    check(geom.overflow === 0, 'the space screen does not scroll sideways (' + geom.overflow + ' px)');
    check(
      geom.arcBoxes.length === 4 && geom.arcBoxes.every((b) => b.x >= 0 && b.y >= 0 && b.x + b.w <= geom.viewBox.w && b.y + b.h <= geom.viewBox.h),
      'every arc stays inside the 240x240 SVG box',
    );
    check(geom.centre !== null && !/slice/.test(geom.centre) && geom.centre !== 'path', 'the middle of the ring is a hole, not an arc (' + geom.centre + ')');
    check(
      geom.textBoxes.length === 2 && geom.textBoxes.every((t) => radius(t, 120, 120) < 72),
      'the total and its label sit inside the ring\'s hole (72 px radius)',
    );
    check(geom.del.right <= geom.legendRight - 4 && geom.del.w >= 48, 'the Delete button fits inside the legend card');
    check(geom.legendBottom <= geom.viewBottom, 'the whole legend is on screen at 1180x780 (' + geom.legendBottom + ' of ' + geom.viewBottom + ')');

    // The walking state, driven through the screen's own render: SpaceWalking is
    // the flag, so the state has to read right while it is true.
    const walking = await cdp.evaluate(`(() => {
      const keep = { rows: SP.rows, walked: SP.walked };
      SP.rows = [];
      SP.walking = true;
      renderSpace();
      const out = {
        status: !document.querySelector('#space-status').classList.contains('hidden'),
        statusText: document.querySelector('#space-status').textContent.trim(),
        spinner: !!document.querySelector('#space-status .spinner'),
        refreshDisabled: document.querySelector('#space-refresh').disabled,
        empty: !document.querySelector('#space-empty').classList.contains('hidden'),
        title: document.querySelector('#space-empty-title').textContent.trim(),
        body: !document.querySelector('#space-body').classList.contains('hidden'),
        quota: !document.querySelector('#space-quota').classList.contains('hidden'),
        quotaLines: document.querySelectorAll('#space-quota .space-quota-line').length,
      };
      SP.walking = false;
      SP.walked = true;
      renderSpace();
      const empty = {
        title: document.querySelector('#space-empty-title').textContent.trim(),
        button: document.querySelector('#space-empty-btn').textContent.trim(),
        hiddenButton: document.querySelector('#space-empty-btn').classList.contains('hidden'),
        body: !document.querySelector('#space-body').classList.contains('hidden'),
      };
      SP.rows = keep.rows;
      SP.walked = keep.walked;
      renderSpace();
      return { out, empty };
    })()`);
    log('walking  ' + JSON.stringify(walking.out));
    log('empty    ' + JSON.stringify(walking.empty));
    check(walking.out.status && walking.out.spinner, 'a walk in progress shows the walking line and a spinner');
    check(/^Walking Frame\.io…/.test(walking.out.statusText), 'the walking line says what it is doing');
    check(walking.out.refreshDisabled, 'Refresh is disabled while a walk is in progress');
    check(walking.out.empty && walking.out.title === 'Walking Frame.io…' && !walking.out.body, 'an empty screen while walking says so');
    // The account's own figures are not part of the walk: the headline stays up.
    check(walking.out.quota && walking.out.quotaLines === 3, 'the headline stays while a walk runs (' + walking.out.quotaLines + ' lines)');
    check(walking.empty.title === 'No projects' && !walking.empty.body, 'a finished walk with nothing in it says so');
    check(walking.empty.button === 'Refresh' && !walking.empty.hiddenButton, 'the empty screen offers Refresh');

    // One project: the arc is a whole ring, which is two half arcs in one path -
    // an arc that ends where it starts has no direction to draw.
    const single = await cdp.evaluate(`(() => {
      const keep = SP.rows;
      SP.rows = [{
        account_id: 'acc-one', account_name: 'One', workspace_id: 'ws-one', workspace_name: 'Work',
        project_id: 'proj-only', name: 'Only project', root_folder_id: 'fld-only',
        bytes: 4000000000, files: 3, walked: true, error: '',
      }];
      renderSpace();
      const paths = [...document.querySelectorAll('#space-chart .slice')];
      const boxes = paths.map((p) => { const b = p.getBBox(); return [Math.round(b.x), Math.round(b.y), Math.round(b.width), Math.round(b.height)]; });
      const out = {
        paths: paths.length,
        d: paths.map((p) => p.getAttribute('d')).join(''),
        subpaths: (paths[0]?.getAttribute('d').match(/M/g) || []).length,
        boxes,
        total: document.querySelector('#space-total').textContent,
        rows: document.querySelectorAll('#space-legend .row').length,
      };
      SP.rows = keep;
      renderSpace();
      return out;
    })()`);
    log('one      ' + JSON.stringify({ paths: single.paths, subpaths: single.subpaths, d: single.d, boxes: single.boxes, total: single.total }));
    check(single.paths === 1 && single.subpaths === 2, 'a single project draws one arc of two halves');
    check(single.boxes[0][2] >= 200 && single.boxes[0][3] >= 200, 'that arc spans the whole ring');
    check(single.total === '4.00 GB · 1 project' && single.rows === 1, 'one project reads "1 project", not "1 projects"');

    const del = await cdp.evaluate(`(async () => {
      const openLast = () => {
        const buttons = document.querySelectorAll('#space-legend .space-del');
        buttons[buttons.length - 1].click();
        return document.querySelector('#space-confirm');
      };
      const rowsNow = () => document.querySelectorAll('#space-legend .row').length;
      const m = openLast();
      const shown = {
        open: !m.classList.contains('hidden'),
        name: document.querySelector('#sc-name').textContent,
        size: document.querySelector('#sc-size').textContent,
        where: document.querySelector('#sc-where').textContent,
        warn: document.querySelector('#sc-warn').textContent.trim(),
        focus: document.activeElement.id,
        delFocused: document.activeElement === document.querySelector('#sc-del'),
      };
      document.querySelector('#sc-cancel').click();
      const afterCancel = rowsNow();
      openLast();
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      const afterEscape = { rows: rowsNow(), open: !m.classList.contains('hidden'), deleted: window.__bridge.deleted.length };
      openLast();
      document.querySelector('#sc-del').click();
      await new Promise((r) => setTimeout(r, 250));
      return {
        shown,
        afterCancel,
        afterEscape,
        deletedAfterCancel: afterEscape.deleted,
        rowsAfter: rowsNow(),
        closed: m.classList.contains('hidden'),
        deleted: window.__bridge.deleted,
        total: (document.querySelector('#space-total') || {}).textContent,
        quota: document.querySelectorAll('#space-quota .space-quota-line').length,
      };
    })()`);
    log('delete   ' + JSON.stringify(del));
    check(del.shown.open, 'the delete dialog opens');
    check(del.shown.name === 'Archive 2021', 'the delete dialog names the project');
    check(del.shown.size === '0 B · 0 files', 'the delete dialog names the size and the file count');
    check(del.quota === 3, 'the delete dialog leaves the headline alone (' + del.quota + ' lines)');
    check(del.shown.where === 'Archive · Nightfall', 'the delete dialog names where the project lives');
    check(
      /deletes the project on Frame.io only. Nothing on this Mac is touched/i.test(del.shown.warn),
      'the delete dialog says the delete is on Frame.io only',
    );
    check(del.shown.focus === 'sc-cancel' && !del.shown.delFocused, 'Cancel holds the focus, so Enter cancels');
    check(del.afterCancel === 5 && del.deletedAfterCancel === 0, 'Cancel deletes nothing');
    check(del.afterEscape.rows === 5 && !del.afterEscape.open && del.afterEscape.deleted === 0, 'Escape closes the dialog and deletes nothing');
    check(del.rowsAfter === 4 && del.closed, 'the confirmed delete removes the row and closes the dialog');
    check(
      JSON.stringify(del.deleted) === JSON.stringify([['acc-nightfall', 'proj-archive']]),
      'the confirmed delete asked for that project, on that account, and nothing else',
    );
    log('');

    // ---- the pixels ----
    const decoded = shots.map((s) => {
      const img = decodePng(s.buf);
      const hash = crypto.createHash('sha256').update(s.buf).digest('hex').slice(0, 12);
      return { ...s, img, hash, colors: distinctColors(img) };
    });

    log('files:');
    for (const d of decoded) {
      const size = d.buf.length;
      log('  ' + path.basename(d.file).padEnd(14) + String(size).padStart(8) + ' bytes  ' + d.img.width + 'x' + d.img.height + '  sha ' + d.hash + '  colours(sampled) ' + d.colors);
      check(d.img.width === OPTS.width && d.img.height === OPTS.height, path.basename(d.file) + ' is ' + d.img.width + 'x' + d.img.height);
      check(size > 2000, path.basename(d.file) + ' is not an empty file (' + size + ' bytes)');
      check(d.colors > 64, path.basename(d.file) + ' has drawn content (' + d.colors + ' distinct colours sampled)');
    }

    log('');
    log('pairwise difference (fraction of pixels):');
    for (let i = 0; i < decoded.length; i++) {
      for (let j = i + 1; j < decoded.length; j++) {
        const f = diffFraction(decoded[i].img, decoded[j].img);
        log('  ' + decoded[i].view.padEnd(10) + ' vs ' + decoded[j].view.padEnd(10) + ' ' + (f * 100).toFixed(2) + ' %');
        check(f > 0.001, decoded[i].view + ' and ' + decoded[j].view + ' differ');
      }
    }

    if (consoleLines.length) {
      log('');
      log('page console:');
      for (const l of consoleLines) log('  ' + l);
    }
    if (exceptions.length) {
      log('');
      log('page exceptions:');
      for (const e of exceptions) log('  ' + e);
      failures.push(exceptions.length + ' uncaught exception(s) in the page');
    }

    log('');
    for (const d of decoded) log('size ' + path.basename(d.file) + ': ' + d.buf.length + ' bytes');
  } finally {
    await cleanup();
  }

  log('');
  if (failures.length) {
    log('FAILED: ' + failures.length + ' check(s)');
    for (const f of failures) log('  - ' + f);
    process.exitCode = 1;
  } else {
    log('all checks passed; ' + OPTS.views.length + ' screenshots in ' + OPTS.out);
  }
}

main().catch(async (e) => {
  console.error('render failed: ' + (e && e.stack ? e.stack : e));
  process.exitCode = 1;
});
