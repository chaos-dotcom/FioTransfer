'use strict';

// ---------- helpers ----------
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const call = (name, ...args) => window.go.main.App[name](...args);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };
const TERMINAL = new Set(['done', 'partial', 'failed', 'canceled', 'interrupted', 'skipped']);

const I = {
  up: '<svg viewBox="0 0 20 20"><path d="M10 15.5V4.5m0 0L5.5 9M10 4.5L14.5 9" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  down: '<svg viewBox="0 0 20 20"><path d="M10 4.5v11m0 0L5.5 11m4.5 4.5l4.5-4.5" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  folder: '<svg viewBox="0 0 20 20"><path d="M3 6.2c0-.7.5-1.2 1.2-1.2h3.4l1.6 1.7h6.6c.7 0 1.2.5 1.2 1.2v6.9c0 .7-.5 1.2-1.2 1.2H4.2c-.7 0-1.2-.5-1.2-1.2z" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>',
  project: '<svg viewBox="0 0 20 20"><rect x="3" y="4" width="14" height="12" rx="2" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M3 8h14" stroke="currentColor" stroke-width="1.4"/></svg>',
  team: '<svg viewBox="0 0 20 20"><circle cx="8" cy="7.5" r="2.6" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M3.5 15.5c.6-2.4 2.3-3.6 4.5-3.6s3.9 1.2 4.5 3.6M13 5.2a2.4 2.4 0 0 1 0 4.6M14.5 11.9c1.3.5 2.1 1.6 2.4 3.6" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>',
  x: '<svg viewBox="0 0 20 20"><path d="M6 6l8 8M14 6l-8 8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>',
  retry: '<svg viewBox="0 0 20 20"><path d="M15.5 10a5.5 5.5 0 1 1-1.6-3.9M15.5 4v3.4h-3.4" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  reveal: '<svg viewBox="0 0 20 20"><path d="M3 6.2c0-.7.5-1.2 1.2-1.2h3.4l1.6 1.7h6.6c.7 0 1.2.5 1.2 1.2v6.9c0 .7-.5 1.2-1.2 1.2H4.2c-.7 0-1.2-.5-1.2-1.2z" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>',
  chevron: '<svg viewBox="0 0 20 20"><path d="M7 8l3 3 3-3" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  chevronUp: '<svg viewBox="0 0 20 20"><path d="M7 12l3-3 3 3" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  open: '<svg viewBox="0 0 20 20"><path d="M8 5H5.5A1.5 1.5 0 0 0 4 6.5v8A1.5 1.5 0 0 0 5.5 16h8a1.5 1.5 0 0 0 1.5-1.5V12M11 4h5v5M16 4l-7 7" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  copy: '<svg viewBox="0 0 20 20"><rect x="7" y="7" width="9" height="9" rx="1.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M13 7V5.5A1.5 1.5 0 0 0 11.5 4h-6A1.5 1.5 0 0 0 4 5.5v6A1.5 1.5 0 0 0 5.5 13H7" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>',
  pencil: '<svg viewBox="0 0 20 20"><path d="M12.8 4.7l2.5 2.5M4 16l.7-3.2 8.6-8.6a1.2 1.2 0 0 1 1.7 0l.8.8a1.2 1.2 0 0 1 0 1.7l-8.6 8.6z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/></svg>',
  external: '<svg viewBox="0 0 20 20" width="12" height="12"><path d="M8 5H5.5A1.5 1.5 0 0 0 4 6.5v8A1.5 1.5 0 0 0 5.5 16h8a1.5 1.5 0 0 0 1.5-1.5V12M11 4h5v5M16 4l-7 7" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
};

function fmtBytes(n) {
  if (!n || n < 0) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0, v = n;
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
  const s = i === 0 ? v.toFixed(0) : v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
  return `${s} ${u[i]}`;
}
const fmtSpeed = (bps) => (!bps || bps < 1 ? '—' : `${fmtBytes(bps)}/s`);
const fmtMbit = (bps) => `${Math.round((bps * 8) / 1e6)} Mbit/s`;
function fmtDur(s) {
  if (!isFinite(s) || s <= 0) return '—';
  s = Math.round(s);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (h) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m) return `${m}m ${String(sec).padStart(2, '0')}s`;
  return `${sec}s`;
}
function fmtDate(ms) {
  if (!ms) return '—';
  const d = new Date(ms), now = new Date();
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = (day(now) - day(d)) / 86400000;
  if (diff === 0) return `Today, ${time}`;
  if (diff === 1) return `Yesterday, ${time}`;
  return `${d.toLocaleDateString([], { month: 'short', day: 'numeric', year: d.getFullYear() !== now.getFullYear() ? 'numeric' : undefined })}, ${time}`;
}
function shortPath(p) {
  const home = S.state?.home;
  if (home && p && (p === home || p.startsWith(home + '/'))) return '~' + p.slice(home.length);
  return p || '';
}
const baseName = (p) => (p || '').replace(/\/+$/, '').split('/').pop() || p;
const shortLink = (l) => (l || '').replace(/^https?:\/\//, '').replace(/\/$/, '');
const initials = (name) => (name || '?').split(/\s+/).filter(Boolean).slice(0, 2).map((w) => w[0].toUpperCase()).join('');

function statusLabel(status, kind) {
  return {
    queued: 'Queued', resolving: 'Preparing', running: kind === 'upload' ? 'Uploading' : kind === 'download' ? 'Downloading' : 'In progress',
    done: 'Completed', partial: 'Completed with errors', failed: 'Failed', canceled: 'Canceled',
    interrupted: 'Interrupted', skipped: 'Skipped',
  }[status] || status;
}
const statusHTML = (status, kind) => `<span class="status ${status}" title="${esc(statusLabel(status, kind))}"><span class="dot"></span><span class="trunc">${statusLabel(status, kind)}</span></span>`;

function toast(msg, kind = 'info', ms = 5000) {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.innerHTML = `<span>${esc(msg)}</span>`;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), ms);
}
const errMsg = (e) => (e && e.message) || String(e);
function copyLink(url) {
  window.runtime.ClipboardSetText(url);
  toast('Link copied', 'ok', 2000);
}

// ---------- state ----------
const S = {
  state: null,
  snap: { jobs: [], speed: 0, active_streams: 0, stream_limit: 16 },
  expanded: new Set(),
  filesFetchedAt: {},
  prevStatus: {},
  view: 'transfers',
  dlDest: '',
  linkInfo: null,
  linkSeq: 0,
  histKind: '',
  histExpanded: new Set(),
  pendingPaths: null,
};

// ---------- boot ----------
async function waitForBindings() {
  for (let i = 0; i < 200; i++) {
    if (window.go?.main?.App && window.runtime) return;
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error('Wails bindings not available');
}

async function init() {
  await waitForBindings();
  bindUI();
  S.state = await call('GetState');
  S.dlDest = S.state.settings.download_dir;
  renderState();
  S.snap = await call('Snapshot');
  renderSnapshot();
  window.runtime.EventsOn('snapshot', (snap) => { S.snap = snap; renderSnapshot(); });
  window.runtime.EventsOn('files-dropped', onDrop);
  window.runtime.EventsOn('menu', onMenu);
  window.runtime.EventsOn('state-changed', async () => { S.state = await call('GetState'); renderState(); });
  loadWatches();
  setInterval(loadWatches, 2000);
}
document.addEventListener('DOMContentLoaded', () => init().catch((e) => toast(errMsg(e), 'err', 15000)));

// ---------- wiring ----------
function bindUI() {
  $$('.nav-item').forEach((b) => b.addEventListener('click', () => showView(b.dataset.view)));
  document.addEventListener('click', (e) => {
    const a = e.target.closest('[data-url]');
    if (a) { e.preventDefault(); call('OpenURL', a.dataset.url); }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!$('#dest-modal').classList.contains('hidden')) closeDest();
    else if (!$('#watch-modal').classList.contains('hidden')) closeWatchEdit();
  });

  const inspectSoon = debounce(inspectLink, 450);
  $('#link').addEventListener('input', () => { S.linkInfo = null; updateDownloadBtn(); inspectSoon(); });
  $('#link').addEventListener('keydown', (e) => { if (e.key === 'Enter') startDownload(); });
  $('#passphrase').addEventListener('input', inspectSoon);
  $('#passphrase').addEventListener('keydown', (e) => { if (e.key === 'Enter') startDownload(); });
  $('#paste-btn').addEventListener('click', async () => {
    const t = (await call('ClipboardText')).trim();
    if (!t) return;
    $('#link').value = t;
    S.linkInfo = null;
    updateDownloadBtn();
    inspectLink();
  });
  $('#dl-dest').addEventListener('click', chooseDownloadDir);
  $('#download-btn').addEventListener('click', startDownload);
  $('#clear-btn').addEventListener('click', () => call('ClearFinished'));

  $('#history-q').addEventListener('input', debounce(loadHistory, 250));
  $$('#history-kind button').forEach((b) => b.addEventListener('click', () => {
    $$('#history-kind button').forEach((x) => x.classList.toggle('active', x === b));
    S.histKind = b.dataset.kind;
    loadHistory();
  }));
  $('#open-logs').addEventListener('click', () => call('OpenLogs'));
  $('#open-logs-2').addEventListener('click', () => call('OpenLogs'));
  $('#export-csv').addEventListener('click', async () => {
    try {
      const p = await call('ExportHistory');
      if (p) toast(`Exported to ${shortPath(p)}`, 'ok');
    } catch (e) { toast(errMsg(e), 'err'); }
  });

  $('#save-client').addEventListener('click', () => saveSettings({ client_id: $('#client-id').value.trim() }, 'Saved'));
  $('#client-id').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('#save-client').click(); });
  $('#streams-range').addEventListener('input', (e) => { $('#streams-val').textContent = e.target.value; });
  $('#streams-range').addEventListener('change', (e) => saveSettings({ streams: +e.target.value }));
  $('#segment').addEventListener('change', (e) => saveSettings({ segment_mb: +e.target.value }));
  $('#open-login').addEventListener('change', async (e) => {
    try { S.state = await call('SetOpenAtLogin', e.target.checked); renderState(); } catch (err) { toast(errMsg(err), 'err'); }
  });
  $('#folder-logs').addEventListener('change', (e) => saveSettings({ no_folder_logs: !e.target.checked }));
  $('#change-default-dir').addEventListener('click', async () => {
    const p = await call('PickDownloadDir', S.state.settings.download_dir);
    if (p) await saveSettings({ download_dir: p });
  });
  $('#copy-redirect').addEventListener('click', (e) => {
    e.preventDefault();
    window.runtime.ClipboardSetText(S.state.redirect_uri);
    toast('Copied', 'ok', 2000);
  });

  $('#watch-add').addEventListener('click', addWatch);
  $('#we-close').addEventListener('click', closeWatchEdit);
  $('#we-cancel').addEventListener('click', closeWatchEdit);
  $('#we-save').addEventListener('click', saveWatchEdit);
  $('#we-local-btn').addEventListener('click', async () => {
    const p = await call('PickFolder');
    if (!p) return;
    WE.local = p;
    renderWatchEdit();
  });
  $('#we-dest-btn').addEventListener('click', () => openDest('pick', (d) => { Object.assign(WE, d); renderWatchEdit(); }));
  $('#we-requeue').addEventListener('click', async () => {
    await call('RequeueCanceled', WE.id);
    WE.canceled = 0;
    renderWatchEdit();
    loadWatches();
  });
  $('#dest-close').addEventListener('click', closeDest);
  $('#dest-cancel').addEventListener('click', closeDest);
  $('#dest-choose').addEventListener('click', chooseDest);
  $('#newfolder-btn').addEventListener('click', newFolder);
  $('#newfolder-name').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') newFolder();
    if (e.key === 'Escape') { e.stopPropagation(); $('#newfolder-name').classList.add('hidden'); }
  });
}

function showView(v) {
  S.view = v;
  $$('.nav-item').forEach((b) => b.classList.toggle('active', b.dataset.view === v));
  $$('.view').forEach((s) => s.classList.toggle('active', s.id === `view-${v}`));
  if (v === 'history') loadHistory();
  if (v === 'watch') loadWatches();
}

function onMenu(what) {
  if (what === 'download') { showView('transfers'); $('#link').focus(); $('#link').select(); return; }
  if (what === 'upload-files') pickFiles();
  if (what === 'upload-folder') pickFolder();
}

// ---------- state rendering ----------
function renderState() {
  const st = S.state, s = st.settings;

  const acc = $('#account');
  acc.classList.remove('clickable');
  acc.onclick = null;
  if (st.logged_in) {
    const u = st.user || { name: 'Signed in', email: '' };
    acc.innerHTML = `<div class="avatar">${esc(initials(u.name))}</div>
      <div class="who"><span class="n">${esc(u.name)}</span><span class="e">${esc(u.email)}</span></div>`;
    acc.classList.add('clickable');
    acc.onclick = () => showView('settings');
  } else {
    acc.innerHTML = `<button class="btn" id="side-signin">Sign in to Frame.io</button>`;
    $('#side-signin').onclick = login;
  }

  renderUploadRow();
  $('#dl-dest-label').textContent = baseName(S.dlDest || s.download_dir);
  $('#dl-dest').title = `Save to ${S.dlDest || s.download_dir}`;

  $('#client-id').value = s.client_id || '';
  $('#redirect-uri').textContent = st.redirect_uri;
  $('#streams-range').value = s.streams;
  $('#streams-val').textContent = s.streams;
  const seg = $('#segment');
  if (![...seg.options].some((o) => o.value === String(s.segment_mb))) seg.insertAdjacentHTML('beforeend', `<option value="${s.segment_mb}">${s.segment_mb} MB</option>`);
  seg.value = String(s.segment_mb);
  $('#default-dir').textContent = shortPath(s.download_dir);
  $('#default-dir').title = s.download_dir;
  $('#folder-logs').checked = !s.no_folder_logs;
  $('#open-login').checked = !!st.open_at_login;
  $('#log-path').textContent = shortPath(st.log_file || st.log_dir);
  $('#about').textContent = `FioTransfer ${st.version}`;

  const row = $('#signin-row');
  if (st.logged_in) {
    row.innerHTML = `<div class="status-text">Signed in${st.user ? ` as <b>${esc(st.user.name)}</b> · ${esc(st.user.email)}` : ''}</div>
      <button class="btn" id="signout-btn">Sign out</button>`;
    $('#signout-btn').onclick = async () => { S.state = await call('Logout'); renderState(); };
    $('#setup-details').open = false;
  } else {
    row.innerHTML = `<div class="status-text">${s.client_id ? 'Not signed in' : 'Not set up'}</div>
      <button class="btn primary" id="signin-btn" ${s.client_id ? '' : 'disabled'}>Sign in with Adobe</button>`;
    $('#signin-btn').onclick = login;
    if (!s.client_id) $('#setup-details').open = true;
  }
}

function renderUploadRow() {
  const st = S.state, s = st.settings, body = $('#upload-body');
  if (!st.logged_in) {
    body.innerHTML = s.client_id
      ? `<div class="notice-line">Not signed in</div><button class="btn primary" id="cta-signin">Sign in</button>`
      : `<div class="notice-line">Not set up</div><button class="btn" id="cta-setup">Set up</button>`;
    if (s.client_id) $('#cta-signin').onclick = login;
    else $('#cta-setup').onclick = () => showView('settings');
    $('#drop-dest').textContent = '';
    return;
  }
  const label = s.last_folder_id ? s.last_folder_label : '';
  const crumbs = label ? label.split(' / ').map(esc).join('<span class="sep">/</span>') : '<span class="ph">Choose destination</span>';
  body.innerHTML = `
    <button class="dest-pick" id="dest-btn" title="${esc(label || 'Choose destination')}">${I.project}<span class="path">${crumbs}</span><span class="chev">${I.chevron}</span></button>
    <button class="btn" id="pick-files">Files…</button>
    <button class="btn" id="pick-folder">Folder…</button>`;
  $('#dest-btn').onclick = () => openDest();
  $('#pick-files').onclick = pickFiles;
  $('#pick-folder').onclick = pickFolder;
  $('#drop-dest').textContent = label;
}

async function saveSettings(patch, okMsg) {
  try {
    S.state = await call('SaveSettings', { ...S.state.settings, ...patch });
    if (patch.download_dir) S.dlDest = patch.download_dir;
    renderState();
    if (okMsg) toast(okMsg, 'ok', 2500);
  } catch (e) { toast(errMsg(e), 'err'); }
}

// Clicking a sign-in button again restarts sign-in (the backend cancels the
// attempt still waiting for the browser); only the latest attempt's result counts.
let loginSeq = 0;
async function login() {
  if (!S.state.settings.client_id) { showView('settings'); $('#client-id').focus(); return; }
  const seq = ++loginSeq;
  $$('#signin-btn, #cta-signin, #side-signin').forEach((b) => { b.textContent = 'Waiting for browser…'; b.title = 'Click to open the sign-in page again'; });
  let st;
  try {
    st = await call('Login');
  } catch (e) {
    if (seq !== loginSeq) return;
    toast(errMsg(e), 'err', 12000);
    st = await call('GetState');
  }
  if (seq !== loginSeq) return;
  S.state = st;
  renderState();
}

// ---------- uploads ----------
async function pickFiles() {
  if (!ensureCanUpload()) return;
  const paths = await call('PickFiles');
  if (paths && paths.length) startUpload(paths);
}
async function pickFolder() {
  if (!ensureCanUpload()) return;
  const p = await call('PickFolder');
  if (p) startUpload([p]);
}
function ensureCanUpload() {
  if (S.state.logged_in) return true;
  showView(S.state.settings.client_id ? 'transfers' : 'settings');
  toast(S.state.settings.client_id ? 'Not signed in' : 'Uploads not set up', 'err');
  return false;
}
function onDrop(paths) {
  if (!paths || !paths.length || !ensureCanUpload()) return;
  showView('transfers');
  startUpload(paths);
}
async function startUpload(paths) {
  if (!S.state.settings.last_folder_id) {
    S.pendingPaths = paths;
    openDest();
    return;
  }
  try {
    await call('StartUpload', paths);
  } catch (e) { toast(errMsg(e), 'err'); }
}

// ---------- downloads ----------
const setPreview = (html) => { $('#link-preview').innerHTML = html; };

async function inspectLink() {
  const link = $('#link').value.trim();
  const pass = $('#passphrase').value;
  const seq = ++S.linkSeq;
  if (!link) {
    setPreview('');
    $('#passphrase').classList.add('hidden');
    updateDownloadBtn();
    return;
  }
  setPreview('<span class="spinner"></span>');
  const li = await call('InspectLink', link, pass);
  if (seq !== S.linkSeq) return;
  S.linkInfo = li;
  if (li.needs_passphrase) {
    $('#passphrase').classList.remove('hidden');
    setPreview(`<span class="warn">${esc(li.error)}</span>`);
    if (!pass) $('#passphrase').focus();
  } else if (li.error) {
    setPreview(`<span class="err">${esc(li.error)}</span>`);
  } else {
    const unavail = li.unavailable ? `<span class="warn">· ${li.unavailable} unavailable</span>` : '';
    setPreview(`<span class="ok-dot"></span><b>${esc(li.name || 'Shared files')}</b><span class="dim">${li.files} file${li.files === 1 ? '' : 's'} · ${fmtBytes(li.total_bytes)}</span>${unavail}`);
  }
  updateDownloadBtn();
}

function updateDownloadBtn() {
  const li = S.linkInfo;
  $('#download-btn').disabled = !$('#link').value.trim() || (li && li.error && !li.needs_passphrase);
}

async function chooseDownloadDir() {
  const p = await call('PickDownloadDir', S.dlDest);
  if (p) await saveSettings({ download_dir: p });
}

async function startDownload() {
  const link = $('#link').value.trim();
  if (!link || $('#download-btn').disabled) return;
  try {
    await call('StartDownload', { link, passphrase: $('#passphrase').value, dest_dir: S.dlDest, subfolder: $('#subfolder').checked });
    $('#link').value = '';
    $('#passphrase').value = '';
    $('#passphrase').classList.add('hidden');
    S.linkInfo = null;
    S.linkSeq++;
    setPreview('');
    updateDownloadBtn();
  } catch (e) { toast(errMsg(e), 'err'); }
}

// ---------- transfers list ----------
function renderSnapshot() {
  const jobs = S.snap.jobs || [];
  let up = 0, down = 0, active = 0;
  for (const j of jobs) {
    if (!TERMINAL.has(j.status)) active++;
    if (j.kind === 'upload') up += j.speed || 0; else down += j.speed || 0;
  }
  $('#speed-up').textContent = fmtSpeed(up);
  $('#speed-down').textContent = fmtSpeed(down);
  $('#speed-up').title = up ? fmtMbit(up) : '';
  $('#speed-down').title = down ? fmtMbit(down) : '';
  $('#streams').textContent = `${S.snap.active_streams} / ${S.snap.stream_limit}`;
  $('#active-count').textContent = active;
  $('#active-count').classList.toggle('hidden', active === 0);

  const wrap = $('#jobs');
  $('#jobs-empty').classList.toggle('hidden', jobs.length > 0);
  $('#jobs-list .list-head').classList.toggle('hidden', jobs.length === 0);
  $('#clear-btn').classList.toggle('hidden', !jobs.some((j) => TERMINAL.has(j.status)));
  const seen = new Set();
  jobs.forEach((j, i) => {
    seen.add(j.id);
    let el = wrap.querySelector(`.row[data-id="${j.id}"]`);
    if (!el) el = createJob(j);
    if (wrap.children[i] !== el) wrap.insertBefore(el, wrap.children[i] || null);
    updateJob(el, j);
    const prev = S.prevStatus[j.id];
    if (prev && !TERMINAL.has(prev) && TERMINAL.has(j.status)) onJobFinished(j);
    S.prevStatus[j.id] = j.status;
  });
  [...wrap.children].forEach((el) => { if (!seen.has(el.dataset.id)) el.remove(); });
  for (const id of Object.keys(S.prevStatus)) {
    if (!seen.has(id)) { delete S.prevStatus[id]; delete S.filesFetchedAt[id]; S.expanded.delete(id); }
  }
}

function onJobFinished(j) {
  if (S.view === 'history') loadHistory();
  if (S.expanded.has(j.id)) refreshFiles(j.id, true);
  if (j.status === 'failed') toast(`${j.kind === 'upload' ? 'Upload' : 'Download'} failed: ${j.error || j.title}`, 'err', 10000);
}

function createJob(j) {
  const el = document.createElement('div');
  el.className = 'row';
  el.dataset.id = j.id;
  el.innerHTML = `
    <div class="row-main jobs-grid">
      <div class="cell-name"><div class="kind-icon">${j.kind === 'upload' ? I.up : I.down}</div><div class="names"><div class="name"></div><div class="sub"></div></div></div>
      <div class="progress"><div class="track"><i></i></div><span class="pct"></span></div>
      <div class="cell r c-size"></div>
      <div class="cell r strong c-speed"></div>
      <div class="c-status"></div>
      <div class="actions">
        <button class="icon-btn a-open" title="Open in Frame.io">${I.open}</button>
        <button class="icon-btn a-copy" title="Copy Frame.io link">${I.copy}</button>
        <button class="icon-btn a-reveal" title="Show in Finder">${I.reveal}</button>
        <button class="icon-btn a-retry" title="Retry">${I.retry}</button>
        <button class="icon-btn a-cancel" title="Cancel">${I.x}</button>
        <button class="icon-btn a-files" title="Show files">${I.chevron}</button>
      </div>
    </div>
    <div class="error-line hidden"></div>
    <div class="row-detail hidden"><div class="files"></div></div>`;
  $('.a-cancel', el).onclick = () => call('Cancel', j.id);
  $('.a-open', el).onclick = () => el.dataset.link && call('OpenURL', el.dataset.link);
  $('.a-copy', el).onclick = () => el.dataset.link && copyLink(el.dataset.link);
  $('.a-retry', el).onclick = () => retry(j.id);
  $('.a-reveal', el).onclick = () => call('Reveal', el.dataset.dest || '').catch((e) => toast(errMsg(e), 'err'));
  $('.a-files', el).onclick = () => toggleFiles(j.id, el);
  return el;
}

function setHTML(el, html) { if (el.dataset.h !== html) { el.innerHTML = html; el.dataset.h = html; } }

function updateJob(el, j) {
  const terminal = TERMINAL.has(j.status);
  el.className = `row ${j.kind} ${j.status}`;
  el.dataset.dest = j.dest;
  el.dataset.link = j.link || '';
  $('.name', el).textContent = j.title || (j.kind === 'upload' ? 'Upload' : 'Download');
  const sub = j.kind === 'upload' ? `To ${j.dest}` : `${shortLink(j.source)} → ${shortPath(j.dest)}`;
  $('.sub', el).textContent = j.phase && !terminal ? j.phase : sub;
  $('.sub', el).title = sub;

  const pct = j.total > 0 ? Math.min(100, (100 * j.done) / j.total) : (j.status === 'done' ? 100 : 0);
  const preparing = j.status === 'resolving' || j.status === 'queued';
  $('.track', el).classList.toggle('indet', preparing);
  $('.track i', el).style.width = `${pct}%`;
  $('.pct', el).textContent = preparing ? '' : `${Math.floor(pct)}%`;

  setHTML($('.c-size', el), j.total ? (terminal ? fmtBytes(j.total) : `${fmtBytes(j.done)} <span style="color:var(--text-4)">/</span> ${fmtBytes(j.total)}`) : '—');
  const speed = !terminal ? (j.speed > 0 ? fmtSpeed(j.speed) : '—') : (j.avg_speed > 0 ? fmtSpeed(j.avg_speed) : '—');
  $('.c-speed', el).textContent = speed;
  $('.c-speed', el).title = terminal ? 'Average speed' : (j.eta > 0 ? `${fmtDur(j.eta)} remaining` : '');
  $('.c-speed', el).style.color = terminal ? 'var(--text-2)' : '';

  let status = statusLabel(j.status, j.kind);
  if (!terminal && j.eta > 0 && !preparing) status = `${fmtDur(j.eta)} left`;
  if (j.status === 'partial') status = `${j.files_failed} of ${j.files_total} failed`;
  if (j.status === 'done') status = `${j.files_total} file${j.files_total === 1 ? '' : 's'} · ${fmtDur(j.elapsed)}`;
  setHTML($('.c-status', el), `<span class="status ${j.status}" title="${esc(status)}"><span class="dot"></span><span class="trunc">${esc(status)}</span></span>`);

  const err = $('.error-line', el);
  const showErr = j.status === 'failed' && j.error;
  err.classList.toggle('hidden', !showErr);
  if (showErr) { err.textContent = j.error; err.title = j.error; }

  $('.a-cancel', el).classList.toggle('hidden', terminal);
  $('.a-open', el).classList.toggle('hidden', !j.link);
  $('.a-copy', el).classList.toggle('hidden', !j.link);
  $('.a-retry', el).classList.toggle('hidden', !(terminal && j.status !== 'done'));
  $('.a-reveal', el).classList.toggle('hidden', j.kind !== 'download' || !j.dest);
  $('.a-files', el).classList.toggle('hidden', j.files_total === 0);
  setHTML($('.a-files', el), S.expanded.has(j.id) ? I.chevronUp : I.chevron);
  if (S.expanded.has(j.id) && !terminal) refreshFiles(j.id);
}

function toggleFiles(id, el) {
  const open = !S.expanded.has(id);
  if (open) S.expanded.add(id); else S.expanded.delete(id);
  $('.row-detail', el).classList.toggle('hidden', !open);
  if (open) refreshFiles(id, true);
  const j = (S.snap.jobs || []).find((x) => x.id === id);
  if (j) updateJob(el, j);
}

function fileRows(files, kind) {
  const order = { running: 0, failed: 1, queued: 2, canceled: 3, done: 4, skipped: 4 };
  const sorted = files.map((f, i) => ({ ...f, i })).sort((a, b) => (order[a.status] ?? 5) - (order[b.status] ?? 5) || a.i - b.i);
  const shown = sorted.slice(0, 400);
  const html = shown.map((f) => {
    const done = f.done ?? (f.status === 'done' ? f.size : 0);
    const pct = f.size > 0 ? Math.min(100, (100 * done) / f.size) : (f.status === 'done' ? 100 : 0);
    let st = statusLabel(f.status, kind);
    if (f.status === 'failed') st = f.error || 'Failed';
    else if (f.status === 'skipped') st = f.note || 'Skipped';
    else if (f.status === 'done' && f.note) st = f.note;
    if (f.retries > 0 && !TERMINAL.has(f.status)) st += ` · ${f.retries} retr${f.retries === 1 ? 'y' : 'ies'}`;
    const open = f.view_url ? `<a data-url="${esc(f.view_url)}" title="Open in Frame.io">${I.external}</a>` : '';
    const reveal = f.local_path && TERMINAL.has(f.status) && f.status !== 'failed' ? `<a class="reveal" data-path="${esc(f.local_path)}" title="Show in Finder">${I.external}</a>` : '';
    const bar = f.status === 'failed' ? 'background:var(--error)' : (f.status === 'done' || f.status === 'skipped') ? 'background:var(--text-4)' : '';
    return `<div class="frow"><div class="fn" title="${esc(f.rel_path)}"><span>${esc(f.rel_path)}</span>${open || reveal}</div>
      <div class="fs">${fmtBytes(f.size)}</div>
      <div class="track"><i style="width:${pct}%;${bar}"></i></div>
      <div class="fst ${f.status}" title="${esc(st)}">${esc(st)}</div></div>`;
  }).join('');
  const more = files.length > shown.length ? `<div class="files-more">+ ${files.length - shown.length} more files</div>` : '';
  return html + more;
}

function bindReveal(root) {
  $$('a.reveal', root).forEach((a) => a.addEventListener('click', (e) => {
    e.preventDefault();
    call('Reveal', a.dataset.path).catch((err) => toast(errMsg(err), 'err'));
  }));
}

async function refreshFiles(id, force) {
  const now = Date.now();
  if (!force && now - (S.filesFetchedAt[id] || 0) < 900) return;
  S.filesFetchedAt[id] = now;
  const files = (await call('JobFiles', id)) || [];
  const box = $(`.row[data-id="${id}"] .files`);
  if (!box) return;
  const kind = ((S.snap.jobs || []).find((x) => x.id === id) || {}).kind || '';
  box.innerHTML = fileRows(files, kind);
  bindReveal(box);
}

async function retry(id) {
  try {
    const newId = await call('Retry', id, '');
    if (!newId) { toast('Its watch folder will upload these files again', 'ok'); loadWatches(); return; }
    showView('transfers');
  } catch (e) { toast(errMsg(e), 'err'); }
}

// ---------- destination picker ----------
const D = { stack: [], mode: 'upload', watchPath: '' };

function openDest(mode = 'upload', onPick = null) {
  D.mode = mode;
  D.onPick = onPick;
  $('#dest-existing-wrap').classList.toggle('hidden', mode !== 'watch');
  $('#dest-existing').checked = false;
  $('#dest-modal .modal-title').textContent = mode === 'watch' ? `${baseName(D.watchPath)} → Frame.io` : 'Upload to';
  $('#dest-choose').textContent = mode === 'watch' ? 'Watch' : mode === 'pick' ? 'Choose' : 'Upload here';
  $('#dest-modal').classList.remove('hidden');
  D.stack = [];
  $('#newfolder-name').classList.add('hidden');
  loadLevel(true);
}
function closeDest() {
  $('#dest-modal').classList.add('hidden');
  if (D.mode !== 'pick') S.pendingPaths = null;
}
const destTop = () => D.stack[D.stack.length - 1];

async function loadLevel(autoSkip) {
  const list = $('#dest-list');
  renderCrumbs();
  const t = destTop();
  const inFolder = !!(t && (t.kind === 'project' || t.kind === 'folder'));
  $('#dest-choose').disabled = !inFolder;
  $('#newfolder-btn').disabled = !inFolder;
  list.innerHTML = '<div class="dest-msg"><span class="spinner"></span></div>';
  let items;
  try {
    if (!t) items = await call('ListAccounts');
    else if (t.kind === 'account') items = await call('ListWorkspaces', t.id);
    else if (t.kind === 'workspace') items = await call('ListProjects', D.stack[0].id, t.id);
    else items = await call('ListFolders', D.stack[0].id, t.folder_id);
  } catch (e) {
    S.state = await call('GetState');
    renderState();
    if (!S.state.logged_in) { closeDest(); toast('Signed out — sign in again', 'err'); return; }
    list.innerHTML = `<div class="dest-msg" style="color:var(--error-text)">${esc(errMsg(e))}</div>`;
    return;
  }
  items = items || [];
  if (autoSkip && items.length === 1 && (!t || t.kind === 'account')) {
    D.stack.push(items[0]);
    return loadLevel(true);
  }
  if (!items.length) {
    list.innerHTML = `<div class="dest-msg">${inFolder ? 'No folders' : 'Empty'}</div>`;
    return;
  }
  const ico = { account: I.team, workspace: I.team, project: I.project, folder: I.folder };
  list.innerHTML = items.map((it, i) => `<div class="dest-item" data-i="${i}">${ico[it.kind]}<span class="nm">${esc(it.name)}</span><span class="chev">›</span></div>`).join('');
  $$('.dest-item', list).forEach((row) => row.addEventListener('click', () => {
    D.stack.push(items[+row.dataset.i]);
    loadLevel(true);
  }));
}

function renderCrumbs() {
  const c = $('#dest-crumbs');
  const parts = [{ name: 'Frame.io' }, ...D.stack];
  c.innerHTML = parts.map((p, i) => `${i ? '<span>/</span>' : ''}<button data-i="${i}" class="${i === parts.length - 1 ? 'cur' : ''}">${esc(p.name)}</button>`).join('');
  $$('button', c).forEach((b) => b.addEventListener('click', () => {
    D.stack = D.stack.slice(0, +b.dataset.i);
    loadLevel(false);
  }));
}

function destLabel() {
  const pi = D.stack.findIndex((x) => x.kind === 'project');
  return D.stack.slice(pi).map((x) => x.name).join(' / ');
}

async function chooseDest() {
  const t = destTop();
  if (!t) return;
  if (D.mode === 'pick') {
    $('#dest-modal').classList.add('hidden');
    if (D.onPick) D.onPick({ acct: D.stack[0].id, folder: t.folder_id, label: destLabel() });
    return;
  }
  if (D.mode === 'watch') {
    try {
      await call('AddWatch', D.watchPath, D.stack[0].id, t.folder_id, destLabel(), $('#dest-existing').checked);
      $('#dest-modal').classList.add('hidden');
      showView('watch');
    } catch (e) { toast(errMsg(e), 'err'); }
    return;
  }
  try {
    S.state = await call('SetUploadDestination', D.stack[0].id, t.folder_id, destLabel());
    renderState();
    const pending = S.pendingPaths;
    $('#dest-modal').classList.add('hidden');
    S.pendingPaths = null;
    if (pending) startUpload(pending);
  } catch (e) { toast(errMsg(e), 'err'); }
}

async function newFolder() {
  const input = $('#newfolder-name');
  if (input.classList.contains('hidden')) {
    input.classList.remove('hidden');
    input.value = '';
    input.focus();
    return;
  }
  const name = input.value.trim();
  if (!name) return input.focus();
  try {
    const f = await call('CreateFolder', D.stack[0].id, destTop().folder_id, name);
    input.classList.add('hidden');
    D.stack.push(f);
    loadLevel(false);
  } catch (e) { toast(errMsg(e), 'err'); }
}

// ---------- history ----------
async function loadHistory() {
  let rows;
  try { rows = (await call('History', $('#history-q').value, S.histKind, 500)) || []; } catch (e) { return toast(errMsg(e), 'err'); }
  const active = new Set((S.snap.jobs || []).filter((j) => !TERMINAL.has(j.status)).map((j) => j.id));
  $('#history-empty').classList.toggle('hidden', rows.length > 0);
  $('#view-history .list-head').classList.toggle('hidden', rows.length === 0);
  const body = $('#history-body');
  body.innerHTML = rows.map((t) => {
    const status = active.has(t.id) ? 'running' : t.status;
    const dur = t.finished_at && t.started_at ? (t.finished_at - t.started_at) / 1000 : 0;
    const sub = t.kind === 'upload' ? `To ${t.dest}` : `${shortLink(t.source)} → ${shortPath(t.dest)}`;
    const canRetry = !active.has(t.id) && ['failed', 'partial', 'canceled', 'interrupted'].includes(t.status);
    const open = S.histExpanded.has(t.id);
    return `<div class="row ${status}" data-id="${t.id}">
      <div class="row-main hist-grid clickable-row">
        <div class="cell-name"><div class="kind-icon">${t.kind === 'upload' ? I.up : I.down}</div><div class="names"><div class="name" title="${esc(t.title)}">${esc(t.title)}</div><div class="sub" title="${esc(sub)}">${esc(sub)}</div></div></div>
        <div class="cell" title="${esc(new Date(t.created_at).toLocaleString())}">${fmtDate(t.created_at)}</div>
        <div class="cell r h-files" title="${t.files_done} of ${t.files_total} files">${t.files_done}/${t.files_total}</div>
        <div class="cell r h-size">${fmtBytes(t.total_bytes)}</div>
        <div class="cell r h-speed" title="${t.avg_bps ? fmtMbit(t.avg_bps) : ''}">${fmtSpeed(t.avg_bps)}</div>
        <div class="cell r h-dur">${fmtDur(dur)}</div>
        <div>${statusHTML(status, t.kind)}</div>
        <div class="actions">
          ${t.link ? `<button class="icon-btn h-open" title="Open in Frame.io">${I.open}</button>` : ''}
          ${canRetry ? `<button class="icon-btn h-retry" title="Retry">${I.retry}</button>` : ''}
          ${t.kind === 'download' ? `<button class="icon-btn h-reveal" title="Show in Finder">${I.reveal}</button>` : ''}
        </div>
      </div>
      ${open ? '<div class="row-detail"><div class="hist-detail"></div></div>' : ''}
    </div>`;
  }).join('');
  const byId = Object.fromEntries(rows.map((r) => [r.id, r]));
  $$('.row', body).forEach((row) => {
    const t = byId[row.dataset.id];
    $('.row-main', row).addEventListener('click', (e) => {
      if (e.target.closest('button')) return;
      if (S.histExpanded.has(t.id)) S.histExpanded.delete(t.id); else S.histExpanded.add(t.id);
      loadHistory();
    });
    const r = $('.h-retry', row); if (r) r.onclick = () => retry(t.id);
    const o = $('.h-open', row); if (o) o.onclick = () => call('OpenURL', t.link);
    const v = $('.h-reveal', row); if (v) v.onclick = () => call('Reveal', t.dest).catch((e) => toast(errMsg(e), 'err'));
    if (S.histExpanded.has(t.id)) renderHistoryDetails(t, $('.hist-detail', row));
  });
}

async function renderHistoryDetails(t, box) {
  const files = (await call('HistoryFiles', t.id)) || [];
  const meta = [
    [t.kind === 'upload' ? 'From' : 'Link', t.source],
    [t.kind === 'upload' ? 'To' : 'Saved to', t.dest],
    ['Started', t.started_at ? new Date(t.started_at).toLocaleString() : '—'],
    ['Finished', t.finished_at ? new Date(t.finished_at).toLocaleString() : '—'],
    ['Transferred', `${fmtBytes(t.done_bytes)} of ${fmtBytes(t.total_bytes)}`],
    ['Speed', t.avg_bps ? `${fmtSpeed(t.avg_bps)} average (${fmtMbit(t.avg_bps)})${t.peak_bps ? ` · ${fmtSpeed(t.peak_bps)} peak` : ''}` : '—'],
    ['Transfer ID', t.id],
  ];
  if (t.link) meta.splice(2, 0, ['Frame.io', t.link]);
  if (t.error) meta.push(['Error', t.error]);
  box.innerHTML = `<div class="meta">${meta.map(([k, v]) => `<span class="k">${esc(k)}</span>${k === 'Frame.io' ? `<span class="v"><a data-url="${esc(v)}">${esc(v)}</a> · <a class="copy-link" data-link="${esc(v)}">Copy</a></span>` : `<span class="v selectable">${esc(v)}</span>`}`).join('')}</div>
    <div class="files">${files.length ? fileRows(files, t.kind) : '<div class="files-more">No files</div>'}</div>`;
  bindReveal(box);
  $$('a.copy-link', box).forEach((a) => a.addEventListener('click', (e) => { e.preventDefault(); copyLink(a.dataset.link); }));
}

// ---------- watch folders ----------
const W = { confirm: null };

async function addWatch() {
  if (!ensureCanUpload()) return;
  const p = await call('PickFolder');
  if (!p) return;
  D.watchPath = p;
  openDest('watch');
}

const WATCH_STATUS = { watching: 'Watching', paused: 'Paused', uploading: 'Uploading', settling: 'Copying', error: 'Error' };

async function loadWatches() {
  let list;
  try { list = (await call('Watches')) || []; } catch { return; }
  const active = list.filter((w) => w.state === 'uploading').length;
  $('#watch-count').textContent = list.filter((w) => w.enabled).length;
  $('#watch-count').classList.toggle('hidden', !list.some((w) => w.enabled));
  if (S.view !== 'watch') return;
  $('#watch-empty').classList.toggle('hidden', list.length > 0);
  $('#view-watch .list-head').classList.toggle('hidden', list.length === 0);
  const pause = '<svg viewBox="0 0 20 20"><path d="M7.5 5v10M12.5 5v10" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>';
  const play = '<svg viewBox="0 0 20 20"><path d="M7 5.5v9l7-4.5z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/></svg>';
  const trash = '<svg viewBox="0 0 20 20"><path d="M4.5 6h11M8 6V4.5h4V6M6 6l.7 9.5h6.6L14 6" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  $('#watch-body').innerHTML = list.map((w) => {
    const label = WATCH_STATUS[w.state] + (w.detail ? ` · ${w.detail}` : '');
    const conf = W.confirm === w.id;
    return `<div class="row ${w.enabled ? '' : 'paused'}" data-id="${w.id}">
      <div class="row-main watch-grid">
        <div class="cell-name"><div class="kind-icon">${I.folder}</div><div class="names"><div class="name">${esc(w.name)}</div><div class="sub" title="${esc(w.local_path)}">${esc(shortPath(w.local_path))}</div></div></div>
        <div class="cell" title="${esc(w.dest_label)}">${esc(w.dest_label)}</div>
        <div><span class="status ${w.state}" title="${esc(label)}"><span class="dot"></span><span class="trunc">${esc(label)}</span></span></div>
        <div class="cell r w-uploaded" title="${w.uploaded} uploaded${w.failed ? ` · ${w.failed} failed` : ''}${w.canceled ? ` · ${w.canceled} canceled` : ''}">${w.uploaded}${w.failed ? ` <span style="color:var(--error-text)">· ${w.failed} failed</span>` : ''}${w.canceled ? ` <span class="muted-note">· ${w.canceled} canceled</span>` : ''}</div>
        <div class="cell w-last">${w.last_upload ? fmtDate(w.last_upload) : '—'}</div>
        <div class="actions">
          <button class="icon-btn w-toggle" title="${w.enabled ? 'Pause' : 'Resume'}">${w.enabled ? pause : play}</button>
          <button class="icon-btn w-edit" title="Edit">${I.pencil}</button>
          ${w.link ? `<button class="icon-btn w-open" title="Open in Frame.io">${I.open}</button>` : ''}
          <button class="icon-btn w-reveal" title="Show in Finder">${I.reveal}</button>
          <button class="icon-btn w-remove ${conf ? 'confirm' : ''}" title="Remove">${conf ? 'Remove' : trash}</button>
        </div>
      </div>
    </div>`;
  }).join('');
  const byId = Object.fromEntries(list.map((w) => [w.id, w]));
  $$('#watch-body .row').forEach((row) => {
    const w = byId[row.dataset.id];
    $('.w-toggle', row).onclick = async () => { await call('SetWatchEnabled', w.id, !w.enabled); loadWatches(); };
    $('.w-reveal', row).onclick = () => call('Reveal', w.local_path).catch((e) => toast(errMsg(e), 'err'));
    $('.w-edit', row).onclick = () => openWatchEdit(w);
    const wo = $('.w-open', row); if (wo) wo.onclick = () => call('OpenURL', w.link);
    $('.w-remove', row).onclick = async () => {
      if (W.confirm !== w.id) { W.confirm = w.id; loadWatches(); setTimeout(() => { if (W.confirm === w.id) { W.confirm = null; loadWatches(); } }, 3000); return; }
      W.confirm = null;
      await call('RemoveWatch', w.id);
      loadWatches();
    };
  });
}

// ---------- watch folder editor ----------
const WE = {};

function openWatchEdit(w) {
  Object.assign(WE, { id: w.id, local: w.local_path, origLocal: w.local_path, acct: w.account_id, folder: w.folder_id, label: w.dest_label, canceled: w.canceled });
  $('#we-existing').checked = false;
  renderWatchEdit();
  $('#watch-modal').classList.remove('hidden');
}

function renderWatchEdit() {
  $('#we-local').textContent = shortPath(WE.local);
  $('#we-local').title = WE.local;
  $('#we-dest').textContent = WE.label;
  $('#we-dest').title = WE.label;
  $('#we-existing-row').classList.toggle('hidden', WE.local === WE.origLocal);
  $('#we-canceled-row').classList.toggle('hidden', !WE.canceled);
  $('#we-canceled').textContent = `${WE.canceled} file${WE.canceled === 1 ? '' : 's'}`;
}

function closeWatchEdit() { $('#watch-modal').classList.add('hidden'); }

async function saveWatchEdit() {
  try {
    await call('UpdateWatch', WE.id, WE.local, WE.acct, WE.folder, WE.label, $('#we-existing').checked);
    closeWatchEdit();
    loadWatches();
  } catch (e) { toast(errMsg(e), 'err'); }
}
