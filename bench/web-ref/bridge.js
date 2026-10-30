/*
 * The bridge a Wails build installs, and the fixture the egui harness draws.
 *
 * Served as /__bridge.js and injected by render.js in front of app.js, so the
 * front end finds window.go / window.runtime already there when it boots.
 *
 * The fixture is `fixture_state()` from egui-ui/tests/snapshots.rs, translated
 * into the JSON `src/app.rs` would hand this front end. Each block below names
 * the Rust value it comes from. Values the egui fixture does not carry are
 * marked INVENTED and listed in README.md.
 */
(function () {
  'use strict';

  var VERSION = 'dev'; // INVENTED: UiState has no version; the settings screen prints one.

  // ---------- dates ----------
  // egui's HistoryRow carries the words ("Today, 09:41"), not a timestamp; the
  // web front end formats `created_at` itself (fmtDate), so the fixture's three
  // words are turned back into instants, relative to the day the render runs.
  var now = new Date();
  function localDay(offsetDays, hh, mm) {
    return new Date(now.getFullYear(), now.getMonth(), now.getDate() + offsetDays, hh, mm, 0, 0).getTime();
  }
  // "27 Oct, 11:15": a fixed calendar date, in a year that keeps it out of
  // today/yesterday even when the render runs on 26-27 October.
  function october27() {
    var y = now.getFullYear();
    var d = new Date(y, 9, 27, 11, 15, 0, 0).getTime();
    if (localDay(0, 0, 0) - d < 2 * 86400000) d = new Date(y - 1, 9, 27, 11, 15, 0, 0).getTime();
    return d;
  }
  var TODAY_0941 = localDay(0, 9, 41);
  var YESTERDAY_1802 = localDay(-1, 18, 2);
  var OCT27_1115 = october27();

  // ---------- the fixture ----------

  // `let mut upload = job("up-1", "upload", "A007_C012_sh010_v003.mov",
  //                       "running", 8_400_000_000, 3_150_000_000);` and the
  // fields set on it just after.
  var UPLOAD = {
    id: 'up-1',
    kind: 'upload',
    title: 'A007_C012_sh010_v003.mov',
    source: '/Users/dana/Renders/A007_C012_sh010_v003.mov',
    dest: 'Shows / Nightfall / Ep 04 / Renders',
    status: 'running',
    phase: 'Uploading 3 of 8 files',
    error: '',
    total: 8400000000,
    done: 3150000000,
    speed: 92400000,
    eta: 54,
    files_total: 8,
    files_done: 3,
    files_failed: 0,
    files_active: 16,
    started_at: 1793311000000,
    finished_at: 0,
    elapsed: 0,
    avg_speed: 0,
    link: '',
    watch: false,
  };

  // `let mut download = job("dn-1", "download", "Nightfall_Ep04_Dailies",
  //                         "done", 21_500_000_000, 21_500_000_000);`
  var DOWNLOAD = {
    id: 'dn-1',
    kind: 'download',
    title: 'Nightfall_Ep04_Dailies',
    source: 'https://f.io/9f2c1a',
    dest: '/Users/dana/Downloads/Nightfall_Ep04_Dailies',
    status: 'done',
    phase: '',
    error: '',
    total: 21500000000,
    done: 21500000000,
    speed: 0,
    eta: 0,
    files_total: 42,
    files_done: 42,
    files_failed: 0,
    files_active: 0,
    started_at: 1793310400000 - 754000, // INVENTED: derived from `elapsed = 754.0`
    finished_at: 1793310400000,
    elapsed: 754,
    avg_speed: 28500000,
    link: '',
    watch: false,
  };

  // `let files = vec![FileView { .. }, FileView { .. }]`, answered by JobFiles.
  var FILES_UP_1 = [
    {
      name: 'A007_C012_sh010_v003.mov',
      rel_path: 'A007_C012_sh010_v003.mov',
      local_path: '',
      size: 2100000000,
      done: 2100000000,
      status: 'done',
      error: '',
      note: 'Verified',
      view_url: '',
      retries: 0,
    },
    {
      name: 'A007_C012_sh010_v003.mxf',
      rel_path: 'A007_C012_sh010_v003.mxf',
      local_path: '',
      size: 6300000000,
      done: 1050000000,
      status: 'running',
      error: '',
      note: '',
      view_url: '',
      retries: 1,
    },
  ];

  // `let history = vec![HistoryRow { title, kind, status, when, files, size }]`.
  // The web table also has a speed and a duration column; those come from
  // `avg_bps` and `finished_at - started_at`, which the egui rows do not carry.
  var HISTORY = [
    {
      id: 'hist-1',
      title: 'Nightfall_Ep04_Dailies',
      kind: 'download',
      status: 'done',
      created_at: TODAY_0941, // the words "Today, 09:41"
      started_at: TODAY_0941 - 754000,
      finished_at: TODAY_0941,
      files_done: 42,
      files_total: 42,
      total_bytes: 21500000000,
      done_bytes: 21500000000,
      avg_bps: 28500000,
      peak_bps: 0, // INVENTED: the fixture has no peak
      source: 'https://f.io/9f2c1a',
      dest: '/Users/dana/Downloads/Nightfall_Ep04_Dailies',
      link: '',
      share_link: '',
      error: '',
    },
    {
      id: 'hist-2',
      title: 'A006_C003_sh020_v002.mov',
      kind: 'upload',
      status: 'partial',
      created_at: YESTERDAY_1802, // "Yesterday, 18:02"
      started_at: 0,
      finished_at: 0,
      files_done: 7,
      files_total: 8,
      total_bytes: 4100000000, // "4.10 GB"
      done_bytes: 0,
      avg_bps: 0,
      peak_bps: 0,
      source: '/Users/dana/Renders/A006_C003_sh020_v002.mov',
      dest: 'Shows / Nightfall / Ep 04 / Renders',
      link: '',
      share_link: '',
      error: 'the connection dropped',
    },
    {
      id: 'hist-3',
      title: 'Trailer_cutdown_v009.mov',
      kind: 'upload',
      status: 'failed',
      created_at: OCT27_1115, // "27 Oct, 11:15"
      started_at: 0,
      finished_at: 0,
      files_done: 0,
      files_total: 1,
      total_bytes: 820000000, // "820 MB"
      done_bytes: 0,
      avg_bps: 0,
      peak_bps: 0,
      source: '/Users/dana/Renders/Trailer_cutdown_v009.mov',
      dest: 'Shows / Nightfall / Ep 04 / Renders',
      link: '',
      share_link: '',
      error: '',
    },
  ];

  // `let watches = vec![WatchRow { local, label, direction, enabled, pending }]`.
  // The web row adds a name (the folder's own), a state, per-direction counters
  // and a last-transfer instant; `pending` has no web column, so it is carried
  // in `uploaded`, the count the web row prints for an upload watch.
  var WATCHES = [
    {
      id: 'watch-1',
      name: 'Renders', // INVENTED: basename of local_path
      local_path: '/Users/dana/Renders',
      dest_label: 'Shows / Nightfall / Ep 04 / Renders',
      direction: 'upload',
      enabled: true,
      state: 'watching', // derived from `enabled`
      detail: '',
      uploaded: 3, // `pending: 3`
      downloaded: 0,
      failed: 0,
      canceled: 0,
      last_transfer: 0, // INVENTED: nothing to format, the web cell prints a dash
      link: '',
      acct: '',
      folder: '',
      change: 'overwrite',
    },
    {
      id: 'watch-2',
      name: 'Dailies',
      local_path: '/Users/dana/Downloads/Dailies',
      dest_label: 'Shows / Nightfall / Ep 04 / Dailies',
      direction: 'download',
      enabled: false,
      state: 'paused',
      detail: '',
      uploaded: 0,
      downloaded: 0, // `pending: 0`
      failed: 0,
      canceled: 0,
      last_transfer: 0,
      link: '',
      acct: '',
      folder: '',
      change: 'overwrite',
    },
  ];

  // `UiState { logged_in, user, settings, .. }`. `redirect_uri`, `home`,
  // `version`, `open_at_login`, `log_file` and the two ids below are INVENTED:
  // the egui model keeps only the port and the destination label.
  var STATE = {
    logged_in: true,
    user: { name: 'Dana Whitfield', email: 'dana@nightfall.tv' },
    redirect_uri: 'http://127.0.0.1:8765/callback',
    home: '/Users/dana',
    version: VERSION,
    open_at_login: true,
    log_file: '/Users/dana/Library/Logs/FioTransfer/fio.log',
    log_dir: '/Users/dana/Library/Logs/FioTransfer',
    settings: {
      client_id: '9f2c1a4e',
      redirect_port: 8765,
      streams: 16,
      segment_mb: 64,
      download_dir: '/Users/dana/Downloads/FioTransfer',
      no_folder_logs: false,
      no_update_check: false,
      last_folder_label: 'Shows / Nightfall / Ep 04 / Renders',
      last_folder_id: 'fld-ep04-renders', // INVENTED: makes the label the destination chip
      last_account_id: 'acc-nightfall', // INVENTED: only read by the folder picker
    },
  };

  // `Snapshot { jobs, speed, active_streams, stream_limit }`.
  var SNAPSHOT = {
    jobs: [UPLOAD, DOWNLOAD],
    speed: 92400000,
    active_streams: 16,
    stream_limit: 16,
  };

  // `Space()` - the walk `App::refresh_space` (src/app.rs) takes, one row per
  // project, largest first. INVENTED: the egui fixture has no Space screen, so
  // these four projects are made up to `SpaceProject`'s own fields. The sizes
  // are picked to print at every branch of fmtBytes (92.5 GB, 18.4 GB, 6.20 GB,
  // 1.05 GB, 118 GB for the total).
  var SPACE = [
    {
      account_id: 'acc-nightfall',
      account_name: 'Nightfall',
      workspace_id: 'ws-shows',
      workspace_name: 'Shows',
      project_id: 'proj-ep04',
      name: 'Nightfall Ep 04',
      root_folder_id: 'fld-ep04',
      bytes: 92500000000,
      files: 1432,
      walked: true,
      error: '',
    },
    {
      account_id: 'acc-nightfall',
      account_name: 'Nightfall',
      workspace_id: 'ws-shows',
      workspace_name: 'Shows',
      project_id: 'proj-trailer',
      name: 'Trailer',
      root_folder_id: 'fld-trailer',
      bytes: 18400000000,
      files: 96,
      walked: true,
      error: '',
    },
    {
      // INVENTED: a project Frame.io sized itself (`Project.storage`), so the
      // row shows a dash for files and says "from Frame.io".
      account_id: 'acc-nightfall',
      account_name: 'Nightfall',
      workspace_id: 'ws-marketing',
      workspace_name: 'Marketing',
      project_id: 'proj-stills',
      name: 'Still Lives',
      root_folder_id: 'fld-stills',
      bytes: 6200000000,
      files: 0,
      walked: false,
      error: '',
    },
    {
      // INVENTED: a project with nothing in it, so the legend's "0 B · 0 files"
      // row and the arc-less swatch are drawn too.
      account_id: 'acc-nightfall',
      account_name: 'Nightfall',
      workspace_id: 'ws-archive',
      workspace_name: 'Archive',
      project_id: 'proj-archive',
      name: 'Archive 2021',
      root_folder_id: 'fld-archive',
      bytes: 0,
      files: 0,
      walked: true,
      error: '',
    },
    {
      account_id: 'acc-agency',
      account_name: 'Nightfall Agency',
      workspace_id: 'ws-agency',
      workspace_name: 'Agency',
      project_id: 'proj-title',
      name: 'Title Sequence',
      root_folder_id: 'fld-title',
      bytes: 1050000000,
      files: 37,
      walked: true,
      // INVENTED: one row that could not finish, so the warning is drawn.
      error: 'permission denied in fld-title/masters',
    },
  ];

  // The accounts behind those rows: `Account.storage_limit` and
  // `Account.storage_usage` as Frame.io reports them. INVENTED, but each number
  // is a real shape: a limit with room left, `storage_limit: null` for an
  // account with no limit, and an account that reports no usage at all.
  var SPACE_ACCOUNTS = [
    { account_id: 'acc-nightfall', name: 'Nightfall', limit: 500000000000, used: 118000000000 },
    { account_id: 'acc-agency', name: 'Nightfall Agency', limit: null, used: 1050000000 },
    { account_id: 'acc-quiet', name: 'Quiet Account', limit: 2000000000000, used: null },
  ];

  var SPACE_WALKING = false;

  // The egui fixture has no update status at all: idle, nothing to draw.
  var UPDATE = {
    available: false,
    state: 'idle',
    current: VERSION,
    latest: '',
    url: '',
    blocker: 'development build',
    progress: 0,
    checked_at: 0,
    error: '',
  };

  // ---------- the answers ----------
  // Every method app.js can reach with `call(name, ...)`. Anything that is not
  // a fixture question answers the empty/default shape, and anything that is
  // not here at all is recorded in __bridge.unhandled instead of being faked.
  var clone = function (v) { return JSON.parse(JSON.stringify(v)); };

  var ANSWERS = {
    GetState: function () { return STATE; },
    Snapshot: function () { return SNAPSHOT; },
    Watches: function () { return WATCHES; },
    History: function () { return HISTORY; },
    JobFiles: function (id) { return id === 'up-1' ? FILES_UP_1 : []; },
    HistoryFiles: function () { return []; },
    UpdateStatus: function () { return UPDATE; },
    CheckForUpdate: function () { return UPDATE; },
    TakeServiceRequests: function () { return []; },

    // Host actions: nothing to open, nothing to pick, nothing on the clipboard.
    OpenURL: function () { return null; },
    OpenLogs: function () { return null; },
    Reveal: function () { return null; },
    ClipboardText: function () { return ''; },
    PickFiles: function () { return []; },
    PickFolder: function () { return ''; },
    PickWatchFolder: function () { return ''; },
    PickDownloadDir: function () { return ''; },
    ExportHistory: function () { return ''; },
    ShareLink: function () { return ''; },
    InspectLink: function () { return { error: '', files: 0, total_bytes: 0 }; },
    ListAccounts: function () { return []; },
    ListWorkspaces: function () { return []; },
    ListProjects: function () { return []; },
    ListFolders: function () { return []; },
    Search: function () { return []; },
    PathTo: function () { return []; },
    InstallUpdate: function () { return null; },

    // State writes answer the state the write would leave behind.
    SaveSettings: function (s) {
      STATE.settings = Object.assign({}, STATE.settings, s || {});
      return STATE;
    },
    SetUploadDestination: function (account, folder, label) {
      STATE.settings.last_account_id = account;
      STATE.settings.last_folder_id = folder;
      STATE.settings.last_folder_label = label;
      return STATE;
    },
    SetOpenAtLogin: function (v) { STATE.open_at_login = !!v; return STATE; },
    Login: function () { return STATE; },
    Logout: function () { return STATE; },
    Cancel: function () { return null; },
    Retry: function () { return ''; },
    ClearFinished: function () { return null; },
    RequeueCanceled: function () { return null; },
    SetWatchEnabled: function () { return WATCHES; },
    AddWatch: function () { return WATCHES; },
    UpdateWatch: function () { return WATCHES; },
    RemoveWatch: function () { return WATCHES; },
    // ---- Space (the walk `Space()` returns; INVENTED above) ----
    // `{ projects, accounts }`: the rows the chart draws and the accounts the
    // headline counts.
    Space: function () { return { projects: SPACE, accounts: SPACE_ACCOUNTS }; },
    SpaceWalking: function () { return SPACE_WALKING; },
    RefreshSpace: function () {
      SPACE_WALKING = true;
      setTimeout(function () { SPACE_WALKING = false; }, 1200);
      return null;
    },
    // The one destructive call: it drops the row and records the arguments, so
    // the render can prove what was asked for (and that nothing else was).
    DeleteProject: function (acct, project) {
      SPACE = SPACE.filter(function (p) { return !(p.project_id === project && p.account_id === acct); });
      window.__bridge.deleted.push([acct, project]);
      return null;
    },

    StartUpload: function () { return null; },
    StartDownload: function () { return null; },
    CreateFolder: function () { return { id: '', name: '', kind: 'folder', folder_id: '' }; },
  };

  // ---------- the bridge ----------
  var listeners = {};
  var calls = [];
  var unhandled = [];

  function dispatch(name, args) {
    calls.push(name);
    var answer = ANSWERS[name];
    if (answer) return Promise.resolve(clone(answer.apply(null, args)));
    unhandled.push(name);
    return Promise.reject(new Error('bridge: no fixture for App.' + name));
  }

  var App = {};
  Object.keys(ANSWERS).forEach(function (name) {
    App[name] = function () { return dispatch(name, Array.prototype.slice.call(arguments)); };
  });

  // A method app.js calls that this fixture does not answer still gets a
  // promise, so the boot sequence never hangs; the call is recorded.
  var appProxy = new Proxy(App, {
    get: function (target, prop) {
      if (typeof prop !== 'string' || prop in target) return target[prop];
      return function () { return dispatch(prop, Array.prototype.slice.call(arguments)); };
    },
    has: function () { return true; },
  });

  window.go = { main: { App: appProxy } };

  window.runtime = {
    EventsOn: function (name, cb) {
      (listeners[name] = listeners[name] || []).push(cb);
      return function () { window.runtime.EventsOff(name); };
    },
    EventsOnce: function (name, cb) { return window.runtime.EventsOn(name, cb); },
    EventsOff: function (name) { delete listeners[name]; },
    EventsOffAll: function () { listeners = {}; },
    EventsEmit: function (name, payload) { return window.__bridge.emit(name, payload); },
    ClipboardSetText: function (text) { window.__bridge.clipboard = String(text); return Promise.resolve(true); },
    ClipboardGetText: function () { return Promise.resolve(window.__bridge.clipboard || ''); },
    Quit: function () { return null; },
    WindowReload: function () { return null; },
    BrowserOpenURL: function (url) { window.__bridge.opened.push(String(url)); return null; },
  };

  window.__bridge = {
    version: VERSION,
    fixture: { state: STATE, snapshot: SNAPSHOT, watches: WATCHES, history: HISTORY, update: UPDATE },
    calls: calls,
    unhandled: unhandled,
    deleted: [],
    opened: [],
    clipboard: '',
    emit: function (name, payload) {
      var hit = listeners[name] || [];
      hit.forEach(function (cb) { try { cb(payload); } catch (e) { console.error('bridge: listener', name, e); } });
      return hit.length;
    },
    listening: function () { return Object.keys(listeners); },
  };
})();
