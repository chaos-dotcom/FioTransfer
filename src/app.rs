
//! The object the frontend binds to: every method `window.go.main.App[...]`
//! calls, with the same names, arguments, results and error strings as the Go
//! `App` in app.go.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime};

use futures::future::BoxFuture;
use futures::FutureExt;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tokio_util::sync::CancellationToken;

use crate::api::{self, Client};
use crate::auth::AuthManager;
use crate::config::{self, Crumb, Settings};
use crate::gui::{BridgeHost, Emitter};
use crate::history;
use crate::services::ServiceRequest;
use crate::share;
use crate::update::{self, Release};
use crate::xfer::{self, manager as xfer_manager, DownloadOptions, LinkInfo, Snapshot, Status, UploadOptions};

/// One level of a Frame.io path: account, workspace, project or folder.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Node {
    pub id: String,
    pub name: String,
    pub kind: String, // account | workspace | project | folder
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub folder_id: String,
    /// Search results: the folder's project.
    #[serde(rename = "where", default, skip_serializing_if = "String::is_empty")]
    pub where_: String,
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct State {
    pub version: String,
    pub logged_in: bool,
    pub user: Option<api::User>,
    pub settings: Settings,
    pub redirect_uri: String,
    pub log_dir: String,
    pub log_file: String,
    pub home: String,
    pub open_at_login: bool,
}

impl State {
    // Go's State marshals Settings inline; `settings` above does that.
    fn _unused(&self) {}
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct UpdateStatus {
    pub current: String,
    pub latest: String,
    pub available: bool,
    pub url: String,
    pub state: String, // idle | checking | downloading | restarting
    pub progress: f64,
    pub error: String,
    pub checked_at: i64,
    /// Why this copy can't update itself.
    pub blocker: String,
}

struct UpdaterState {
    st: UpdateStatus,
    rel: Option<Release>,
    app: Option<String>,
    last_emit: Option<Instant>,
}

pub struct App {
    pub settings: Arc<Mutex<Settings>>,
    pub me_self: Mutex<std::sync::Weak<App>>,
    pub auth: Arc<AuthManager>,
    pub api: Arc<Client>,
    pub hist: Mutex<Option<Arc<history::Store>>>,
    pub mgr: Mutex<Option<Arc<xfer::Manager>>>,
    pub wat: Mutex<Option<Arc<xfer::watch::Watcher>>>,
    pub emitter: Mutex<Option<Emitter>>,

    me: Mutex<Option<api::User>>,
    me_fetching: AtomicBool,
    me_tried: Mutex<Option<Instant>>,

    login_lock: tokio::sync::Mutex<()>,
    login_cancel: Mutex<Option<CancellationToken>>,

    names: Mutex<HashMap<String, String>>,
    projects_listed: Mutex<Option<Instant>>,

    open_at_login: AtomicBool,
    started: Mutex<Option<Instant>>,

    /// Last Frame.io storage reading, so the Space view can be opened without
    /// asking Frame.io again.
    space: Mutex<Option<(Instant, SpacePayload)>>,
    space_walking: AtomicBool,
    upd: Mutex<UpdaterState>,
    updating: AtomicBool,
    services_app: AtomicBool,
}

impl App {
    pub fn new(settings: Settings) -> Arc<App> {
        let settings_cell = Arc::new(Mutex::new(settings));
        let auth = {
            let c = settings_cell.clone();
            AuthManager::new(move || c.lock().unwrap().client_id.clone())
        };
        let api = Arc::new(Client::new(auth.clone()));
        Self::assemble(settings_cell, auth, api)
    }

    /// The same `App`, but every API call goes through `api`. A test hands in a
    /// real `Client` whose `base_url` is a local stub, so the destructive paths
    /// can be driven without the network. Test seam only; nothing calls this.
    #[doc(hidden)]
    pub fn with_api(settings: Settings, api: Arc<Client>) -> Arc<App> {
        let auth = api.auth.clone();
        Self::assemble(Arc::new(Mutex::new(settings)), auth, api)
    }

    fn assemble(settings_cell: Arc<Mutex<Settings>>, auth: Arc<AuthManager>, api: Arc<Client>) -> Arc<App> {
        let app = Arc::new(App {
            settings: settings_cell,
            me_self: Mutex::new(std::sync::Weak::new()),
            auth,
            api,
            hist: Mutex::new(None),
            mgr: Mutex::new(None),
            wat: Mutex::new(None),
            emitter: Mutex::new(None),
            me: Mutex::new(None),
            me_fetching: AtomicBool::new(false),
            me_tried: Mutex::new(None),
            login_lock: tokio::sync::Mutex::new(()),
            login_cancel: Mutex::new(None),
            names: Mutex::new(HashMap::new()),
            projects_listed: Mutex::new(None),
            open_at_login: AtomicBool::new(false),
            started: Mutex::new(None),
            space: Mutex::new(None),
            space_walking: AtomicBool::new(false),
            upd: Mutex::new(UpdaterState {
                st: UpdateStatus { current: crate::VERSION.to_string(), state: "idle".into(), ..Default::default() },
                rel: None,
                app: None,
                last_emit: None,
            }),
            updating: AtomicBool::new(false),
            services_app: AtomicBool::new(false),
        });
        *app.me_self.lock().unwrap() = Arc::downgrade(&app);
        let _ = GLOBAL.set(Mutex::new(Arc::downgrade(&app)));
        app
    }

    pub fn settings(&self) -> Settings {
        self.settings.lock().unwrap().clone()
    }

    pub fn mgr(&self) -> Option<Arc<xfer::Manager>> {
        self.mgr.lock().unwrap().clone()
    }

    pub fn hist(&self) -> Option<Arc<history::Store>> {
        self.hist.lock().unwrap().clone()
    }

    fn emit(&self, event: &str, payload: Value) {
        if let Some(e) = self.emitter.lock().unwrap().as_ref() {
            e.emit(event, payload);
        }
    }

    fn state_changed(&self) {
        self.emit("state-changed", json!(null));
    }

    pub fn show_window(&self) {
        if let Some(e) = self.emitter.lock().unwrap().as_ref() {
            e.show();
        }
    }

    fn api_cx(&self) -> CancellationToken {
        CancellationToken::new()
    }

    // ---------- Startup ----------

    /// Opens history, starts the manager and the watcher, and begins the loops
    /// that talk to the menu bar, updates and Finder services.
    pub async fn start(self: &Arc<Self>) {
        let h = match history::Store::open(&config::dir(), &config::log_dir()) {
            Ok(h) => Arc::new(h),
            Err(e) => {
                crate::dialogs::error("FioTransfer", &format!("Could not open transfer history: {}", e));
                std::process::exit(1);
            }
        };
        // Only the app does this: the CLI may run while the app is transferring.
        h.mark_interrupted();
        *self.hist.lock().unwrap() = Some(h.clone());

        let settings = self.settings.lock().unwrap().clone();
        let cell = {
            let a = Arc::clone(self);
            move || a.settings()
        };
        let _ = settings;
        let mgr = xfer_manager::new_manager(self.api.clone(), h.clone(), Box::new(cell));
        {
            let a = Arc::clone(self);
            *mgr.on_update.lock().unwrap() = Some(Arc::new(move |s: Snapshot| {
                a.on_snapshot(s);
            }));
        }
        *self.mgr.lock().unwrap() = Some(mgr.clone());
        let wat = xfer::watch::Watcher::new(mgr.clone());
        *self.wat.lock().unwrap() = Some(wat);
        {
            let me = Arc::clone(self);
            *share::VIEWER.lock().unwrap() = Some(Arc::new(move || me.viewer()));
        }
        {
            let mgr2 = mgr.clone();
            tokio::spawn(async move { crate::xfer::upload::clean_up(mgr2).await });
        }
        *self.started.lock().unwrap() = Some(Instant::now());
        crate::services::start(Arc::clone(self) as Arc<dyn crate::services::ServiceHost>);
        crate::quick::start(Arc::clone(self) as Arc<dyn crate::quick::QuickHost>);
        let me = Arc::clone(self);
        tokio::spawn(async move { me.tray_loop().await });
        self.init_updates();
        self.start_services();
    }

    fn viewer(&self) -> Option<(String, String)> {
        let me = self.cached_user();
        me.map(|u| (u.name, u.email))
    }

    /// Posts a notification for the jobs that ran since the last one.
    fn on_snapshot(&self, s: Snapshot) {
        static BUSY: Mutex<Option<HashMap<String, bool>>> = Mutex::new(None);
        let mut guard = BUSY.lock().unwrap();
        let busy = guard.get_or_insert_with(HashMap::new);
        let mut active = false;
        for j in &s.jobs {
            if matches!(j.status, Status::Running | Status::Resolving | Status::Queued) {
                busy.insert(j.id.clone(), true);
                active = true;
            }
        }
        if !active && !busy.is_empty() {
            self.notify_finished(&s, busy);
            busy.clear();
        }
    }

    fn notify_finished(&self, s: &Snapshot, ran: &HashMap<String, bool>) {
        let (mut ok, mut failed) = (0, 0);
        for j in &s.jobs {
            if !ran.contains_key(&j.id) {
                continue;
            }
            match j.status {
                Status::Failed | Status::Partial => failed += 1,
                Status::Done if !j.watch => ok += 1,
                _ => {}
            }
        }
        if ok + failed == 0 {
            return;
        }
        let msg = if failed > 0 {
            format!("{} transfer(s) with errors", failed)
        } else {
            "Transfers complete".to_string()
        };
        crate::notify::notify("FioTransfer", &msg, Some("Glass"));
    }

    // ---------- Menu bar ----------

    async fn tray_loop(self: Arc<Self>) {
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        let mut shown = String::from("{\"jobs\":[],\"more\":0}");
        let mut n = 0u64;
        loop {
            tick.tick().await;
            if n % 30 == 0 {
                self.open_at_login.store(crate::login::enabled(), Ordering::Relaxed);
            }
            n += 1;
            crate::tray::sync_dock();
            let Some(mgr) = self.mgr() else { continue };
            let (up, down, active) = mgr.activity();
            let mut status = "Idle".to_string();
            let mut title = String::new();
            if active > 0 {
                let mut parts = Vec::new();
                if up > 0.0 {
                    parts.push(format!("↑ {}", speed(up)));
                }
                if down > 0.0 {
                    parts.push(format!("↓ {}", speed(down)));
                }
                title = parts.join("  ");
                status = format!("{} active", active);
                if !title.is_empty() {
                    status.push_str(" · ");
                    status.push_str(&title);
                }
            }
            crate::tray::set(&status, &title, self.open_at_login.load(Ordering::Relaxed), &self.update_menu_title());
            let jobs = if active > 0 { tray_jobs(&mgr.snapshot()) } else { "{\"jobs\":[],\"more\":0}".to_string() };
            if jobs != shown {
                crate::tray::set_jobs(&jobs);
                shown = jobs;
            }
            crate::tray::set_transfers_active(active > 0);
        }
    }

    fn update_menu_title(&self) -> String {
        let g = self.upd.lock().unwrap();
        if !g.st.available {
            return String::new();
        }
        format!("Update to {}…", g.st.latest)
    }

    fn start_services(&self) {
        // Requests that came in while the app was starting.
        if !crate::services::take().is_empty() {
            self.serve_requests();
        }
    }

    fn serve_requests(&self) {
        self.show_window();
        self.emit("service", json!(null));
    }

    // ---------- Updates ----------

    fn init_updates(self: &Arc<Self>) {
        {
            let mut g = self.upd.lock().unwrap();
            g.st.current = crate::VERSION.to_string();
            g.st.state = "idle".into();
        }
        if !update::released(crate::VERSION) {
            self.upd.lock().unwrap().st.blocker = "development build".into();
            return;
        }
        let exe = std::env::current_exe().unwrap_or_default();
        let app = update::bundle_path(&exe);
        match app {
            Ok(app) => {
                self.upd.lock().unwrap().app = Some(app.to_string_lossy().into_owned());
                update::cleanup(&app);
                if let Err(e) = update::can_replace(&app) {
                    self.upd.lock().unwrap().st.blocker = e;
                }
            }
            Err(e) => self.upd.lock().unwrap().st.blocker = e,
        }
        let me = Arc::clone(self);
        tokio::spawn(async move { me.update_loop().await });
    }

    async fn update_loop(self: Arc<Self>) {
        tokio::time::sleep(Duration::from_secs(20)).await;
        loop {
            if !self.settings().no_update_check {
                let _ = self.check_update().await;
            }
            tokio::time::sleep(Duration::from_secs(6 * 3600)).await;
        }
    }

    pub fn update_status(&self) -> UpdateStatus {
        self.upd.lock().unwrap().st.clone()
    }

    pub async fn check_for_update(&self) -> UpdateStatus {
        if !update::released(crate::VERSION) {
            return self.update_status();
        }
        self.check_update().await
    }

    async fn check_update(&self) -> UpdateStatus {
        {
            let mut g = self.upd.lock().unwrap();
            if g.st.state != "idle" {
                return g.st.clone();
            }
            g.st.state = "checking".into();
            g.st.error.clear();
        }
        self.emit_update(true);
        let cx = CancellationToken::new();
        let src = update::Source::new(&format!("FioTransfer/{}", crate::VERSION));
        let res = tokio::time::timeout(Duration::from_secs(30), src.latest(&cx)).await;
        let (rel, err) = match res {
            Ok(Ok(r)) => (r, false),
            _ => (None, true),
        };
        let st = {
            let mut g = self.upd.lock().unwrap();
            g.st.state = "idle".into();
            g.st.checked_at = crate::xfer::now_ms();
            if err {
                g.st.error = "Couldn't check for updates".into();
            }
            if let Some(r) = rel {
                g.st.latest = r.version.clone();
                g.st.url = r.url.clone();
                g.st.available = update::newer(&r.version, crate::VERSION);
                g.rel = Some(r);
            }
            g.st.clone()
        };
        if err {
            if let Some(h) = self.hist() {
                h.log("UPDATE   check failed: could not reach GitHub");
            }
        }
        self.emit_update(true);
        if st.available {
            self.notify_update(&st.latest);
        }
        st
    }

    fn emit_update(&self, force: bool) {
        let st = {
            let mut g = self.upd.lock().unwrap();
            if !force && g.last_emit.map(|t| t.elapsed() < Duration::from_millis(200)).unwrap_or(false) {
                return;
            }
            g.last_emit = Some(Instant::now());
            g.st.clone()
        };
        self.emit("update", serde_json::to_value(&st).unwrap_or(Value::Null));
    }

    fn notify_update(&self, v: &str) {
        if self.settings().update_notified == v {
            return;
        }
        if let Ok(saved) = config::update(|s| s.update_notified = v.to_string()) {
            *self.settings.lock().unwrap() = saved;
        }
        crate::notify::notify("FioTransfer", &format!("Version {} is available", v), None);
    }

    pub async fn install_update(self: &Arc<Self>) -> Result<(), String> {
        let (rel, app, st) = {
            let g = self.upd.lock().unwrap();
            (g.rel.clone(), g.app.clone(), g.st.clone())
        };
        if !st.blocker.is_empty() {
            return Err(st.blocker);
        }
        if st.state == "downloading" || st.state == "restarting" {
            return Ok(());
        }
        let Some(rel) = rel else { return Err("no update available".into()) };
        if !update::newer(&rel.version, crate::VERSION) {
            return Err("no update available".into());
        }
        if self.mgr().map(|m| m.active()).unwrap_or(false) {
            let go = crate::dialogs::question(
                "Transfers in progress",
                "Updating restarts FioTransfer and stops running transfers. You can retry them from History.",
                "Update",
                "Cancel",
            );
            if !go {
                return Ok(());
            }
        }
        {
            let mut g = self.upd.lock().unwrap();
            if g.st.state != "idle" {
                return Ok(());
            }
            g.st.state = "downloading".into();
            g.st.progress = 0.0;
            g.st.error.clear();
        }
        self.emit_update(true);
        let app = app.unwrap_or_default();
        if let Some(h) = self.hist() {
            h.log(&format!("UPDATE   installing {} over {} at {}", rel.version, crate::VERSION, app));
        }
        let me = Arc::clone(self);
        tokio::spawn(async move {
            let fail = |msg: String| {
                let mut g = me.upd.lock().unwrap();
                g.st.state = "idle".into();
                g.st.error = msg.clone();
                drop(g);
                me.emit_update(true);
                if let Some(h) = me.hist() {
                    h.log(&format!("UPDATE   {}", msg));
                }
            };
            let src = update::Source::new(&format!("FioTransfer/{}", crate::VERSION));
            let cx = CancellationToken::new();
            let progress = {
                let me = me.clone();
                Arc::new(move |p: f64| {
                    me.upd.lock().unwrap().st.progress = p;
                    me.emit_update(false);
                })
            };
            let old = match src.install(&cx, &rel, std::path::Path::new(&app), progress.as_ref()).await {
                Ok(o) => o,
                Err(e) => {
                    fail(format!("Update failed: {}", e));
                    return;
                }
            };
            if let Err(e) = update::relaunch(std::process::id(), std::path::Path::new(&app), &old) {
                fail(format!("Installed. Quit and reopen FioTransfer to finish. ({})", e));
                return;
            }
            me.upd.lock().unwrap().st.state = "restarting".into();
            me.emit_update(true);
            me.updating.store(true, Ordering::Relaxed);
            if let Some(e) = me.emitter.lock().unwrap().as_ref() {
                e.quit();
            }
        });
        Ok(())
    }

    // ---------- State ----------

    pub fn get_state(&self) -> State {
        let s = self.settings();
        let st = State {
            version: crate::VERSION.to_string(),
            logged_in: self.auth.logged_in(),
            settings: s.clone(),
            redirect_uri: crate::auth::redirect_uri(s.redirect_port.max(1) as u16),
            log_dir: config::log_dir().to_string_lossy().into_owned(),
            log_file: self.hist().map(|h| h.log_path()).unwrap_or_default(),
            home: config::home_dir().to_string_lossy().into_owned(),
            open_at_login: crate::login::enabled(),
            user: None,
        };
        let mut st = st;
        if st.logged_in {
            st.user = self.cached_user();
        }
        st
    }

    /// The signed-in user without waiting on the network. If it isn't known
    /// yet it is fetched in the background, and the UI is told to refresh when
    /// the answer is in; a dead session is cleared then.
    fn cached_user(&self) -> Option<api::User> {
        let known = self.me.lock().unwrap().clone();
        let fetch = {
            let mut tried = self.me_tried.lock().unwrap();
            let stale = tried.map(|t| t.elapsed() > Duration::from_secs(30)).unwrap_or(true);
            if known.is_none() && !self.me_fetching.load(Ordering::Relaxed) && stale {
                self.me_fetching.store(true, Ordering::Relaxed);
                true
            } else {
                false
            }
        };
        if fetch {
            let me = self.handle();
            tokio::spawn(async move {
                me.fetch_user().await;
                me.emit("state-changed", json!(null));
            });
        }
        known
    }

    /// A handle to this app for background work (the App lives in an `Arc`).
    fn handle(&self) -> Arc<App> {
        self.me_self.lock().unwrap().upgrade().expect("app dropped")
    }

    /// The running app, for the AppKit callbacks that have no other handle.
    pub fn global() -> Option<Arc<App>> {
        GLOBAL.get().and_then(|g| g.lock().unwrap().upgrade())
    }

    async fn fetch_user(&self) -> Option<api::User> {
        let cx = CancellationToken::new();
        let res = tokio::time::timeout(Duration::from_secs(15), self.api.me(&cx)).await;
        self.me_fetching.store(false, Ordering::Relaxed);
        *self.me_tried.lock().unwrap() = Some(Instant::now());
        match res {
            Ok(Ok(u)) => {
                *self.me.lock().unwrap() = Some(u.clone());
                Some(u)
            }
            _ => None,
        }
    }

    fn forget_user(&self) {
        *self.me.lock().unwrap() = None;
        *self.me_tried.lock().unwrap() = None;
    }

    pub fn save_settings(&self, s: Settings) -> Result<State, String> {
        let saved = config::update(|cur| {
            cur.client_id = s.client_id.trim().to_string();
            cur.redirect_port = s.redirect_port;
            cur.streams = s.streams;
            cur.segment_mb = s.segment_mb;
            cur.download_dir = s.download_dir.clone();
            cur.no_folder_logs = s.no_folder_logs;
            cur.no_update_check = s.no_update_check;
        })
        .map_err(|e| e.to_string())?;
        *self.settings.lock().unwrap() = saved.clone();
        if let Some(m) = self.mgr() {
            m.set_streams(saved.streams);
        }
        Ok(self.get_state())
    }

    /// Signs in through the browser. Clicking Sign in again restarts it.
    pub async fn login(self: &Arc<Self>) -> Result<State, String> {
        if let Some(cx) = self.login_cancel.lock().unwrap().take() {
            cx.cancel();
        }
        let cx = CancellationToken::new();
        *self.login_cancel.lock().unwrap() = Some(cx.clone());
        let _guard = self.login_lock.lock().await;
        if cx.is_cancelled() {
            return Err(crate::auth::ERR_LOGIN_CANCELED.to_string());
        }
        let s = self.settings();
        let open = |u: &str| {
            let _ = std::process::Command::new("/usr/bin/open").arg(u).spawn();
        };
        self.auth
            .login(&cx, s.redirect_port.max(1) as u16, open)
            .await
            .map_err(|e| e)?;
        self.forget_user();
        self.show_window();
        if self.fetch_user().await.is_none() {
            return Err("Frame.io rejected the Adobe sign-in (is the Frame.io API added to the Adobe project?)".into());
        }
        Ok(self.get_state())
    }

    pub fn logout(&self) -> State {
        self.auth.logout();
        self.forget_user();
        self.get_state()
    }

    // ---------- Frame.io browsing ----------

    fn remember(&self, ns: &[Node]) {
        let mut g = self.names.lock().unwrap();
        for n in ns {
            g.insert(n.id.clone(), n.name.clone());
        }
    }

    fn known_name(&self, id: &str) -> Option<String> {
        self.names.lock().unwrap().get(id).cloned()
    }

    pub async fn list_accounts(&self) -> Result<Vec<Node>, String> {
        let cx = self.api_cx();
        let accts = self.api.accounts(&cx).await.map_err(|e| e.to_string())?;
        let out: Vec<Node> = accts
            .iter()
            .map(|a| Node { id: a.id.clone(), name: a.display_name.clone(), kind: "account".into(), ..Default::default() })
            .collect();
        self.remember(&out);
        Ok(out)
    }

    pub async fn list_workspaces(&self, acct: &str) -> Result<Vec<Node>, String> {
        let cx = self.api_cx();
        let ws = self.api.workspaces(&cx, acct).await.map_err(|e| e.to_string())?;
        let mut out: Vec<Node> = ws
            .iter()
            .map(|w| Node { id: w.id.clone(), name: w.name.clone(), kind: "workspace".into(), ..Default::default() })
            .collect();
        out.sort_by_key(|n| n.name.to_lowercase());
        self.remember(&out);
        Ok(out)
    }

    pub async fn list_projects(&self, acct: &str, ws: &str) -> Result<Vec<Node>, String> {
        let cx = self.api_cx();
        let ps = self.api.projects(&cx, acct, ws).await.map_err(|e| e.to_string())?;
        let out: Vec<Node> = ps
            .iter()
            .map(|p| Node {
                id: p.id.clone(),
                name: p.name.clone(),
                kind: "project".into(),
                folder_id: p.root_folder_id.clone(),
                where_: String::new(),
            })
            .collect();
        self.remember(&out);
        Ok(out)
    }

    pub async fn list_folders(&self, acct: &str, folder: &str) -> Result<Vec<Node>, String> {
        let cx = self.api_cx();
        let kids = self.api.children(&cx, acct, folder).await.map_err(|e| e.to_string())?;
        Ok(kids
            .iter()
            .filter(|k| k.type_ == "folder")
            .map(|k| Node { id: k.id.clone(), name: k.name.clone(), kind: "folder".into(), folder_id: k.id.clone(), where_: String::new() })
            .collect())
    }

    pub async fn create_folder(&self, acct: &str, parent: &str, name: &str) -> Result<Node, String> {
        let cx = self.api_cx();
        let f = self.api.create_folder(&cx, acct, parent, name.trim()).await.map_err(|e| e.to_string())?;
        Ok(Node { id: f.id.clone(), name: f.name.clone(), kind: "folder".into(), folder_id: f.id, where_: String::new() })
    }

    /// The picker path down to a project or folder.
    pub async fn path_to(&self, acct: &str, kind: &str, id: &str) -> Result<Vec<Node>, String> {
        let cx = self.api_cx();
        let mut folders: Vec<Node> = Vec::new();
        let mut project = id.to_string();
        if kind == "folder" {
            let mut f = self.api.folder(&cx, acct, id).await.map_err(|e| gone(&e))?;
            while !f.parent_id.is_empty() {
                if folders.len() == 64 {
                    return Err("folder is nested too deep".into());
                }
                folders.insert(
                    0,
                    Node { id: f.id.clone(), name: f.name.clone(), kind: "folder".into(), folder_id: f.id.clone(), where_: String::new() },
                );
                let next = f.parent_id.clone();
                // As in the Go app: a failed step up the chain is an error, not a
                // shorter path (the UI would show a folder under the wrong parent).
                f = self.api.folder(&cx, acct, &next).await.map_err(|e| gone(&e))?;
            }
            project = f.project_id.clone();
        }
        let p = self.api.project(&cx, acct, &project).await.map_err(|e| gone(&e))?;
        self.remember(&[Node { id: p.id.clone(), name: p.name.clone(), kind: "project".into(), ..Default::default() }]);
        let acct_name = self.account_name(&cx, acct).await;
        let ws_name = self.workspace_name(&cx, acct, &p.workspace_id).await;
        let mut path = vec![
            Node { id: acct.to_string(), name: acct_name, kind: "account".into(), ..Default::default() },
            Node { id: p.workspace_id.clone(), name: ws_name, kind: "workspace".into(), ..Default::default() },
            Node { id: p.id.clone(), name: p.name.clone(), kind: "project".into(), folder_id: p.root_folder_id.clone(), where_: String::new() },
        ];
        path.extend(folders);
        Ok(path)
    }

    async fn account_name(&self, cx: &CancellationToken, acct: &str) -> String {
        if let Some(n) = self.known_name(acct) {
            return n;
        }
        if let Ok(accts) = self.api.accounts(cx).await {
            let ns: Vec<Node> = accts
                .iter()
                .map(|a| Node { id: a.id.clone(), name: a.display_name.clone(), kind: "account".into(), ..Default::default() })
                .collect();
            self.remember(&ns);
        }
        self.known_name(acct).unwrap_or_else(|| "Account".into())
    }

    async fn workspace_name(&self, cx: &CancellationToken, acct: &str, ws: &str) -> String {
        if let Some(n) = self.known_name(ws) {
            return n;
        }
        if let Ok(list) = self.api.workspaces(cx, acct).await {
            let ns: Vec<Node> = list
                .iter()
                .map(|w| Node { id: w.id.clone(), name: w.name.clone(), kind: "workspace".into(), ..Default::default() })
                .collect();
            self.remember(&ns);
        }
        self.known_name(ws).unwrap_or_else(|| "Workspace".into())
    }

    /// Finds projects and folders by name anywhere in an account.
    pub async fn search(&self, acct: &str, query: &str) -> Result<Vec<Node>, String> {
        let query = query.trim().to_string();
        if acct.is_empty() || query.chars().count() < 2 {
            return Ok(Vec::new());
        }
        let cx = self.api_cx();
        let hits = self.api.search(&cx, acct, &query, 50).await.map_err(|e| e.to_string())?;
        let mut out: Vec<Node> = Vec::new();
        let mut projects: Vec<String> = Vec::new();
        for h in &hits {
            if !h.name_match() {
                continue;
            }
            let r = &h.result;
            match h.type_.as_str() {
                "project_result" => {
                    self.remember(&[Node { id: r.id.clone(), name: r.name.clone(), kind: "project".into(), ..Default::default() }]);
                    out.push(Node {
                        id: r.id.clone(),
                        name: r.name.clone(),
                        kind: "project".into(),
                        folder_id: r.root_folder_id.clone(),
                        where_: String::new(),
                    });
                    projects.push(String::new());
                }
                "folder_result" => {
                    if r.parent_id.is_empty() {
                        continue;
                    }
                    out.push(Node { id: r.id.clone(), name: r.name.clone(), kind: "folder".into(), folder_id: r.id.clone(), where_: String::new() });
                    projects.push(r.project_id.clone());
                }
                _ => {}
            }
        }
        let names = self.project_names(&cx, acct, &projects).await;
        for (i, p) in projects.iter().enumerate() {
            out[i].where_ = names.get(p).cloned().unwrap_or_default();
        }
        Ok(out)
    }

    async fn project_names(&self, cx: &CancellationToken, acct: &str, ids: &[String]) -> HashMap<String, String> {
        let lookup = |me: &App| -> (HashMap<String, String>, bool) {
            let mut out = HashMap::new();
            let mut missing = false;
            let g = me.names.lock().unwrap();
            for id in ids {
                match g.get(id) {
                    Some(n) => {
                        out.insert(id.clone(), n.clone());
                    }
                    None => missing = true,
                }
            }
            let mut listed = me.projects_listed.lock().unwrap();
            if missing {
                if listed.map(|t| t.elapsed() < Duration::from_secs(60)).unwrap_or(false) {
                    missing = false;
                } else {
                    *listed = Some(Instant::now());
                }
            }
            (out, missing)
        };
        let (names, missing) = lookup(self);
        if !missing {
            return names;
        }
        let Ok(ws) = self.api.workspaces(cx, acct).await else { return names };
        for w in ws {
            self.remember(&[Node { id: w.id.clone(), name: w.name.clone(), kind: "workspace".into(), ..Default::default() }]);
            if let Ok(ps) = self.api.projects(cx, acct, &w.id).await {
                let ns: Vec<Node> = ps
                    .iter()
                    .map(|p| Node { id: p.id.clone(), name: p.name.clone(), kind: "project".into(), ..Default::default() })
                    .collect();
                self.remember(&ns);
            }
        }
        lookup(self).0
    }

    pub fn set_upload_destination(&self, acct: &str, folder: &str, label: &str, path: Vec<Crumb>) -> Result<State, String> {
        let saved = config::update(|s| {
            s.last_account_id = acct.to_string();
            s.last_folder_id = folder.to_string();
            s.last_folder_label = label.to_string();
            s.last_folder_path = path.clone();
        })
        .map_err(|e| e.to_string())?;
        *self.settings.lock().unwrap() = saved;
        Ok(self.get_state())
    }

    // ---------- Dialogs ----------

    pub fn pick_files(&self) -> Vec<String> {
        crate::dialogs::open_files("Choose files to upload")
    }

    pub fn pick_folder(&self) -> Option<String> {
        crate::dialogs::open_dir("Choose a folder to upload", "", false)
    }

    pub fn pick_watch_folder(&self) -> Option<String> {
        crate::dialogs::open_dir("Choose a folder to watch", "", true)
    }

    pub fn pick_download_dir(&self, current: &str) -> Option<String> {
        let cur = if current.is_empty() { self.settings().download_dir } else { current.to_string() };
        crate::dialogs::open_dir("Download to…", &cur, true)
    }

    // ---------- Transfers ----------

    pub fn start_upload(&self, paths: Vec<String>) -> Result<String, String> {
        let s = self.settings();
        let Some(m) = self.mgr() else { return Err("transfers aren't running".into()) };
        let j = m.start_upload(UploadOptions {
            paths,
            account_id: s.last_account_id,
            folder_id: s.last_folder_id,
            dest_label: s.last_folder_label,
        })?;
        Ok(j.id.clone())
    }

    pub async fn inspect_link(&self, link: &str, passphrase: &str) -> LinkInfo {
        let Some(m) = self.mgr() else { return LinkInfo::default() };
        let cx = CancellationToken::new();
        let _ = &cx;
        match tokio::time::timeout(Duration::from_secs(90), m.inspect_link(&cx, link, passphrase)).await {
            Ok(li) => li,
            Err(_) => LinkInfo { error: "timed out resolving the link".into(), ..Default::default() },
        }
    }

    pub fn start_download(&self, o: DownloadOptions) -> Result<String, String> {
        let Some(m) = self.mgr() else { return Err("transfers aren't running".into()) };
        let mut o = o;
        if o.dest_dir.is_empty() {
            o.dest_dir = self.settings().download_dir;
        }
        let j = m.start_download(o)?;
        Ok(j.id.clone())
    }

    /// A public Frame.io share link to what a transfer moved, making the share
    /// the first time it's asked for.
    pub async fn share_link(&self, id: &str) -> Result<String, String> {
        let Some(m) = self.mgr() else { return Err("transfers aren't running".into()) };
        let cx = self.api_cx();
        m.share_link(&cx, id).await
    }

    pub fn cancel(&self, id: &str) {
        if let Some(m) = self.mgr() {
            m.cancel(id);
        }
    }

    pub fn clear_finished(&self) {
        if let Some(m) = self.mgr() {
            m.clear_finished();
        }
    }

    pub fn snapshot(&self) -> Snapshot {
        self.mgr().map(|m| m.snapshot()).unwrap_or_default()
    }

    pub fn job_files(&self, id: &str) -> Vec<xfer::FileView> {
        self.mgr().map(|m| m.files(id)).unwrap_or_default()
    }

    pub async fn retry(self: &Arc<Self>, id: &str, passphrase: &str) -> Result<String, String> {
        let Some(m) = self.mgr() else { return Err("transfers aren't running".into()) };
        let j = m.retry(id, passphrase).await?;
        Ok(j.map(|j| j.id.clone()).unwrap_or_default())
    }

    // ---------- History ----------

    pub fn history(&self, query: &str, kind: &str, limit: i64) -> Result<Vec<history::Transfer>, String> {
        let Some(h) = self.hist() else { return Ok(Vec::new()) };
        h.list(query, kind, limit)
    }

    pub fn history_files(&self, id: &str) -> Result<Vec<history::File>, String> {
        let Some(h) = self.hist() else { return Ok(Vec::new()) };
        h.files(id)
    }

    pub fn export_history(&self) -> Result<String, String> {
        let name = format!("FioTransfer history {}.csv", today());
        let Some(mut p) = crate::dialogs::save_file("Export transfer history", &name) else {
            return Ok(String::new());
        };
        if !p.to_lowercase().ends_with(".csv") {
            p.push_str(".csv");
        }
        let Some(h) = self.hist() else { return Err("history isn't open".into()) };
        h.export_csv(std::path::Path::new(&p))?;
        Ok(p)
    }

    // ---------- Finder / browser ----------

    pub fn reveal(&self, path: &str) -> Result<(), String> {
        let mut p = std::path::PathBuf::from(path);
        loop {
            if p.exists() {
                let _ = std::process::Command::new("/usr/bin/open").arg("-R").arg(&p).spawn();
                return Ok(());
            }
            match p.parent() {
                Some(parent) if parent != std::path::Path::new("/") => p = parent.to_path_buf(),
                _ => break,
            }
        }
        Err(format!("{} no longer exists", path))
    }

    pub fn open_url(&self, url: &str) {
        let _ = std::process::Command::new("/usr/bin/open").arg(url).spawn();
    }

    pub fn open_logs(&self) -> Result<(), String> {
        std::process::Command::new("/usr/bin/open")
            .arg(config::log_dir())
            .spawn()
            .map(|_| ())
            .map_err(|e| e.to_string())
    }

    pub fn clipboard_text(&self) -> String {
        crate::dialogs::clipboard_text()
    }

    // ---------- Watch folders ----------

    pub fn watches(&self) -> Vec<xfer::watch::WatchView> {
        self.wat.lock().unwrap().as_ref().map(|w| w.views()).unwrap_or_default()
    }

    pub fn add_watch(&self, local: &str, acct: &str, folder: &str, label: &str, dir: &str, on_change: &str, include_existing: bool) -> Result<(), String> {
        let Some(w) = self.wat.lock().unwrap().clone() else { return Err("watch folders aren't running".into()) };
        w.add(local, acct, folder, label, dir, on_change, include_existing)
    }

    pub fn set_watch_enabled(&self, id: &str, on: bool) {
        if let Some(w) = self.wat.lock().unwrap().as_ref() {
            w.set_enabled(id, on);
        }
    }

    pub fn remove_watch(&self, id: &str) {
        if let Some(w) = self.wat.lock().unwrap().as_ref() {
            w.remove(id);
        }
    }

    pub fn set_open_at_login(&self, on: bool) -> Result<State, String> {
        let err = crate::login::set(on);
        self.open_at_login.store(crate::login::enabled(), Ordering::Relaxed);
        err?;
        Ok(self.get_state())
    }

    pub fn update_watch(&self, id: &str, local: &str, acct: &str, folder: &str, label: &str, dir: &str, on_change: &str, include_existing: bool) -> Result<(), String> {
        let Some(w) = self.wat.lock().unwrap().clone() else { return Err("watch folders aren't running".into()) };
        w.update(id, local, acct, folder, label, dir, on_change, include_existing)
    }

    pub fn requeue_canceled(&self, id: &str) {
        if let Some(w) = self.wat.lock().unwrap().as_ref() {
            w.requeue_canceled(id);
        }
    }

    pub fn take_service_requests(&self) -> Vec<ServiceRequest> {
        crate::services::take()
    }

    // ---------- Shutdown ----------

    pub fn shutdown(&self) {
        if let Some(w) = self.wat.lock().unwrap().take() {
            w.close();
        }
        if let Some(m) = self.mgr.lock().unwrap().take() {
            m.cancel_all();
            std::thread::sleep(Duration::from_millis(300));
        }
        if let Some(h) = self.hist.lock().unwrap().take() {
            h.close();
        }
    }
}

static GLOBAL: once_cell::sync::OnceCell<Mutex<std::sync::Weak<App>>> = once_cell::sync::OnceCell::new();

fn gone<E: std::fmt::Display>(e: &E) -> String {
    let text = e.to_string();
    if text.contains("404") {
        "no longer on Frame.io".to_string()
    } else {
        text
    }
}

fn speed(bps: f64) -> String {
    if bps >= 1e9 {
        format!("{:.1} GB/s", bps / 1e9)
    } else if bps >= 1e6 {
        format!("{:.0} MB/s", bps / 1e6)
    } else {
        format!("{:.0} KB/s", bps / 1e3)
    }
}

fn today() -> String {
    let secs = SystemTime::now().duration_since(SystemTime::UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0) as i64;
    let days = secs / 86_400;
    let (y, m, d) = civil_from_days(days);
    format!("{:04}-{:02}-{:02}", y, m, d)
}

fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = (if mp < 10 { mp + 3 } else { mp - 9 }) as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

#[derive(Serialize)]
struct TrayJob {
    up: bool,
    name: String,
    progress: f64,
    pct: String,
    detail: String,
}

const TRAY_JOBS_MAX: usize = 5;

/// Running transfers as the menu bar menu shows them.
fn tray_jobs(s: &Snapshot) -> String {
    let rank = |st: Status| match st {
        Status::Running => Some(0),
        Status::Resolving => Some(1),
        Status::Queued => Some(2),
        _ => None,
    };
    let mut active: Vec<xfer::JobView> = s.jobs.iter().filter(|j| rank(j.status).is_some()).cloned().collect();
    active.sort_by_key(|j| rank(j.status).unwrap_or(9));
    let mut jobs: Vec<TrayJob> = Vec::new();
    let mut more = 0;
    for (i, j) in active.iter().enumerate() {
        if i == TRAY_JOBS_MAX {
            more = active.len() - i;
            break;
        }
        jobs.push(tray_row(j));
    }
    json!({"jobs": jobs, "more": more}).to_string()
}

fn tray_row(j: &xfer::JobView) -> TrayJob {
    let up = j.kind == "upload";
    let mut t = TrayJob { up, name: j.title.clone(), progress: -1.0, pct: String::new(), detail: String::new() };
    if t.name.is_empty() {
        t.name = if up { "Upload".to_string() } else { "Download".to_string() };
    }
    if j.status == Status::Queued {
        t.detail = "Queued".into();
    } else if j.status == Status::Resolving || j.total <= 0 {
        t.detail = if j.phase.is_empty() { "Preparing".to_string() } else { j.phase.clone() };
    } else {
        t.progress = (j.done as f64 / j.total as f64).min(1.0);
        t.pct = format!("{}%", (t.progress * 100.0) as i64);
        if up && j.done >= j.total {
            t.detail = "Verifying".into();
        } else {
            let mut parts = vec![format!("{} of {}", xfer::fmt_bytes(j.done), xfer::fmt_bytes(j.total))];
            if j.speed > 0.0 {
                parts.push(speed(j.speed));
            }
            if j.eta > 0.0 {
                parts.push(format!("{} left", xfer::fmt_dur(j.eta)));
            }
            t.detail = parts.join(" · ");
        }
    }
    t
}

// ---------- Space: what Frame.io is holding, per project ----------

/// One project's footprint on Frame.io.
#[derive(Debug, Clone, Serialize, Default)]
pub struct SpaceProject {
    pub account_id: String,
    pub account_name: String,
    pub workspace_id: String,
    pub workspace_name: String,
    pub project_id: String,
    pub name: String,
    pub root_folder_id: String,
    pub bytes: i64,
    /// Files the walk counted. 0 means "not counted", which is what a row whose
    /// size Frame.io reported says, so the row shows a dash instead of a zero.
    pub files: i64,
    /// True when the figures came from walking the project's folders, false
    /// when Frame.io reported the size (`Project.storage`) and nothing was
    /// walked. The row says which: a walked figure and a reported one are not
    /// the same measurement, and only a walk counts files. A walk that fails
    /// part way is still `true`, with `error` saying why it is not whole.
    pub walked: bool,
    /// Set when the walk could not finish (permissions, a deleted folder). An
    /// errored row has no figure at all, whatever `walked` says.
    pub error: String,
}

/// One account's storage against its limit, straight from `Account`.
///
/// `limit` is `None` when Frame.io sends `storage_limit: null`, which means "no
/// limit" — the view prints that rather than a limit of zero. `used` is `None`
/// when the account reports no `storage_usage`.
#[derive(Debug, Clone, Serialize, Default)]
pub struct SpaceAccount {
    pub account_id: String,
    pub name: String,
    pub limit: Option<i64>,
    pub used: Option<i64>,
}

/// The Space payload: the rows to chart, and the per-account headline over
/// them. A struct rather than a bare list of projects, because "how much is
/// left" belongs to the account, not to any one project.
#[derive(Debug, Clone, Serialize, Default)]
pub struct SpacePayload {
    pub projects: Vec<SpaceProject>,
    pub accounts: Vec<SpaceAccount>,
}

impl App {
    /// The last reading, if there is one. Cheap: the Space view calls this on
    /// every frame.
    pub fn space(&self) -> SpacePayload {
        self.space.lock().unwrap().as_ref().map(|(_, v)| v.clone()).unwrap_or_default()
    }

    pub fn space_walking(&self) -> bool {
        self.space_walking.load(Ordering::Relaxed)
    }

    /// Reads what Frame.io says each account and each project holds, and walks
    /// a project's folders only when Frame.io does not report its size. Reads
    /// only: nothing here deletes or downloads anything.
    ///
    /// The project list is one request per workspace and `Project.storage`
    /// comes with it, so a refresh that never falls back costs one request per
    /// account and per workspace rather than one per folder.
    pub async fn refresh_space(self: &Arc<Self>) {
        if self.space_walking.swap(true, Ordering::SeqCst) {
            return; // a refresh is already running
        }
        let cx = self.api_cx();
        let mut out: Vec<SpaceProject> = Vec::new();
        let mut quotas: Vec<SpaceAccount> = Vec::new();
        let accounts = match self.api.accounts(&cx).await {
            Ok(a) => a,
            Err(e) => {
                self.space_walking.store(false, Ordering::SeqCst);
                let _ = e;
                return;
            }
        };
        for acct in accounts {
            quotas.push(SpaceAccount {
                account_id: acct.id.clone(),
                name: acct.display_name.clone(),
                limit: acct.storage_limit,
                used: acct.storage_usage,
            });
            let workspaces = self.api.workspaces(&cx, &acct.id).await.unwrap_or_default();
            for ws in workspaces {
                let projects = self.api.projects(&cx, &acct.id, &ws.id).await.unwrap_or_default();
                for p in projects {
                    let mut row = SpaceProject {
                        account_id: acct.id.clone(),
                        account_name: acct.display_name.clone(),
                        workspace_id: ws.id.clone(),
                        workspace_name: ws.name.clone(),
                        project_id: p.id.clone(),
                        name: p.name.clone(),
                        root_folder_id: p.root_folder_id.clone(),
                        ..Default::default()
                    };
                    match p.storage {
                        // Frame.io reported it: take the number, and leave
                        // `files` at 0 ("not counted") rather than walk.
                        Some(bytes) => row.bytes = bytes,
                        // No figure from Frame.io: the walk is the fallback.
                        None => {
                            row.walked = true;
                            match self.walk_folder(&cx, &acct.id, &p.root_folder_id, 0).await {
                                Ok((bytes, files)) => {
                                    row.bytes = bytes;
                                    row.files = files;
                                }
                                Err(e) => row.error = e,
                            }
                        }
                    }
                    out.push(row);
                }
            }
        }
        out.sort_by(|a, b| b.bytes.cmp(&a.bytes));
        *self.space.lock().unwrap() = Some((Instant::now(), SpacePayload { projects: out, accounts: quotas }));
        self.space_walking.store(false, Ordering::SeqCst);
        self.state_changed();
    }

    /// Adds up one folder tree. Depth-capped so a cycle in Frame.io's data can
    /// not spin forever.
    async fn walk_folder(&self, cx: &CancellationToken, acct: &str, folder: &str, depth: usize) -> Result<(i64, i64), String> {
        if depth > 32 || folder.is_empty() {
            return Ok((0, 0));
        }
        let kids = self.api.children(cx, acct, folder).await.map_err(|e| e.to_string())?;
        let (mut bytes, mut files) = (0i64, 0i64);
        for k in kids {
            match k.type_.as_str() {
                "folder" => {
                    let (b, f) = Box::pin(self.walk_folder(cx, acct, &k.id, depth + 1)).await?;
                    bytes += b;
                    files += f;
                }
                "version_stack" => {
                    if let Some(head) = &k.head_version {
                        bytes += head.file_size;
                        files += 1;
                    }
                }
                _ => {
                    bytes += k.file_size;
                    files += 1;
                }
            }
        }
        Ok((bytes, files))
    }

    /// Deletes a project on Frame.io. This is the only destructive call the Space
    /// view makes, and it touches Frame.io alone: no local file is read, moved or
    /// removed, so a copy elsewhere (a `Live_Work` folder, a NAS, a watch folder)
    /// stays exactly as it is.
    pub async fn delete_project(&self, acct: &str, project: &str) -> Result<(), String> {
        let cx = self.api_cx();
        self.api.delete_project(&cx, acct, project).await.map_err(|e| e.to_string())?;
        if let Some(h) = self.hist() {
            h.log(&format!("SPACE    deleted project {} on Frame.io (nothing local was touched)", project));
        }
        if let Some((_, payload)) = self.space.lock().unwrap().as_mut() {
            payload.projects.retain(|p| p.project_id != project);
        }
        self.state_changed();
        Ok(())
    }
}

// ---------- The bridge: window.go.main.App[method] ----------

fn arg(args: &[Value], i: usize) -> Value {
    args.get(i).cloned().unwrap_or(Value::Null)
}

fn s(args: &[Value], i: usize) -> String {
    match args.get(i) {
        Some(Value::String(v)) => v.clone(),
        Some(Value::Null) | None => String::new(),
        Some(v) => v.to_string(),
    }
}

fn b(args: &[Value], i: usize) -> bool {
    args.get(i).and_then(|v| v.as_bool()).unwrap_or(false)
}

fn n(args: &[Value], i: usize) -> i64 {
    args.get(i).and_then(|v| v.as_i64()).unwrap_or(0)
}

fn ok<T: serde::Serialize>(v: T) -> Result<Value, String> {
    serde_json::to_value(v).map_err(|e| e.to_string())
}

impl BridgeHost for App {
    fn startup(&self, emitter: Emitter) {
        *self.emitter.lock().unwrap() = Some(emitter.clone());
        // The menu bar item, as tray_darwin.m builds it.
        let e = emitter.clone();
        crate::tray::init(Arc::new(move |tag: i32| e.menu(tag)));
    }

    fn menu(&self, tag: i32) {
        match tag {
            101 => self.emit("menu", json!("upload-files")),
            102 => self.emit("menu", json!("upload-folder")),
            103 => self.emit("menu", json!("download")),
            104 => {
                let _ = self.open_logs();
            }
            1 => self.show_window(),
            4 => {
                let recent = self.started.lock().unwrap().map(|t| t.elapsed() < Duration::from_secs(3)).unwrap_or(true);
                if !recent {
                    self.show_window();
                }
            }
            2 => {
                let on = !crate::login::enabled();
                let _ = self.set_open_at_login(on);
                self.state_changed();
            }
            5 | 6 => {
                self.show_window();
                self.emit("menu", json!("update"));
                let me = self.handle();
                tokio::spawn(async move {
                    if !me.update_status().available {
                        me.check_for_update().await;
                    }
                });
            }
            7 => {
                let me = self.handle();
                tokio::spawn(async move { me.open_quick_download().await });
            }
            3 => {
                if let Some(e) = self.emitter.lock().unwrap().as_ref() {
                    e.quit();
                }
            }
            _ => {}
        }
    }

    fn before_close(&self) -> bool {
        if self.updating.load(Ordering::Relaxed) {
            return true;
        }
        let Some(m) = self.mgr() else { return true };
        if !m.active() {
            return true;
        }
        crate::dialogs::question("Transfers in progress", "Stop running transfers and quit?", "Quit", "Cancel")
    }

    fn call(&self, method: String, args: Vec<Value>) -> BoxFuture<'static, Result<Value, String>> {
        let me = self.handle();
        async move { me.dispatch(&method, args).await }.boxed()
    }
}

impl App {
    async fn dispatch(self: Arc<Self>, method: &str, args: Vec<Value>) -> Result<Value, String> {
        match method {
            // ---- state ----
            "GetState" => ok(self.get_state()),
            "SaveSettings" => {
                let s: Settings = serde_json::from_value(arg(&args, 0)).unwrap_or_else(|_| self.settings());
                ok(self.save_settings(s)?)
            }
            "Login" => ok(self.login().await?),
            "Logout" => ok(self.logout()),
            "SetOpenAtLogin" => ok(self.set_open_at_login(b(&args, 0))?),
            "CheckForUpdate" => ok(self.check_for_update().await),
            "UpdateStatus" => ok(self.update_status()),
            "InstallUpdate" => {
                self.install_update().await?;
                ok(Value::Null)
            }
            // ---- browsing ----
            "ListAccounts" => ok(self.list_accounts().await?),
            "ListWorkspaces" => ok(self.list_workspaces(&s(&args, 0)).await?),
            "ListProjects" => ok(self.list_projects(&s(&args, 0), &s(&args, 1)).await?),
            "ListFolders" => ok(self.list_folders(&s(&args, 0), &s(&args, 1)).await?),
            "CreateFolder" => ok(self.create_folder(&s(&args, 0), &s(&args, 1), &s(&args, 2)).await?),
            "PathTo" => ok(self.path_to(&s(&args, 0), &s(&args, 1), &s(&args, 2)).await?),
            "Search" => ok(self.search(&s(&args, 0), &s(&args, 1)).await?),
            "SetUploadDestination" => {
                let path: Vec<Crumb> = serde_json::from_value(arg(&args, 3)).unwrap_or_default();
                ok(self.set_upload_destination(&s(&args, 0), &s(&args, 1), &s(&args, 2), path)?)
            }
            // ---- dialogs ----
            "PickFiles" => ok(self.pick_files()),
            "PickFolder" => ok(self.pick_folder()),
            "PickWatchFolder" => ok(self.pick_watch_folder()),
            "PickDownloadDir" => ok(self.pick_download_dir(&s(&args, 0))),
            "ExportHistory" => ok(self.export_history()?),
            // ---- transfers ----
            "StartUpload" => ok(self.start_upload(
                serde_json::from_value::<Vec<String>>(arg(&args, 0)).unwrap_or_default(),
            )?),
            "InspectLink" => ok(self.inspect_link(&s(&args, 0), &s(&args, 1)).await),
            "StartDownload" => {
                let o: DownloadOptions = serde_json::from_value(arg(&args, 0)).unwrap_or_default();
                ok(self.start_download(o)?)
            }
            "ShareLink" => ok(self.share_link(&s(&args, 0)).await?),
            "Cancel" => {
                self.cancel(&s(&args, 0));
                ok(Value::Null)
            }
            "ClearFinished" => {
                self.clear_finished();
                ok(Value::Null)
            }
            "Snapshot" => ok(self.snapshot()),
            "JobFiles" => ok(self.job_files(&s(&args, 0))),
            "Retry" => ok(self.retry(&s(&args, 0), &s(&args, 1)).await?),
            // ---- history ----
            "History" => ok(self.history(&s(&args, 0), &s(&args, 1), n(&args, 2))?),
            "HistoryFiles" => ok(self.history_files(&s(&args, 0))?),
            // ---- finder / browser ----
            "Reveal" => {
                self.reveal(&s(&args, 0))?;
                ok(Value::Null)
            }
            "OpenURL" => {
                self.open_url(&s(&args, 0));
                ok(Value::Null)
            }
            "OpenLogs" => {
                self.open_logs()?;
                ok(Value::Null)
            }
            "ClipboardText" => ok(self.clipboard_text()),
            // ---- watch folders ----
            "Watches" => ok(self.watches()),
            "AddWatch" => {
                self.add_watch(
                    &s(&args, 0),
                    &s(&args, 1),
                    &s(&args, 2),
                    &s(&args, 3),
                    &s(&args, 4),
                    &s(&args, 5),
                    b(&args, 6),
                )?;
                ok(Value::Null)
            }
            "SetWatchEnabled" => {
                self.set_watch_enabled(&s(&args, 0), b(&args, 1));
                ok(Value::Null)
            }
            "RemoveWatch" => {
                self.remove_watch(&s(&args, 0));
                ok(Value::Null)
            }
            "UpdateWatch" => {
                self.update_watch(
                    &s(&args, 0),
                    &s(&args, 1),
                    &s(&args, 2),
                    &s(&args, 3),
                    &s(&args, 4),
                    &s(&args, 5),
                    &s(&args, 6),
                    b(&args, 7),
                )?;
                ok(Value::Null)
            }
            "RequeueCanceled" => {
                self.requeue_canceled(&s(&args, 0));
                ok(Value::Null)
            }
            "TakeServiceRequests" => ok(self.take_service_requests()),
            // ---- space ----
            "Space" => ok(self.space()),
            "SpaceWalking" => ok(self.space_walking()),
            "RefreshSpace" => {
                let me = self.handle();
                tokio::spawn(async move { me.refresh_space().await });
                ok(Value::Null)
            }
            "DeleteProject" => {
                self.delete_project(&s(&args, 0), &s(&args, 1)).await?;
                ok(Value::Null)
            }
            // ---- the bridge's own helpers ----
            "__clipboard_set" => {
                crate::dialogs::clipboard_set_text(&s(&args, 0));
                ok(Value::Null)
            }
            "__quit" => {
                if let Some(e) = self.emitter.lock().unwrap().as_ref() {
                    e.quit();
                }
                ok(Value::Null)
            }
            "__window_show" => {
                self.show_window();
                ok(Value::Null)
            }
            other => Err(format!("no such method: {}", other)),
        }
    }

    /// The menu bar's "Download from Link…": a link and a destination, without
    /// the main window.
    pub async fn open_quick_download(self: &Arc<Self>) {
        let text = self.clipboard_text();
        let link = if share::looks_like_link(&text) { text.trim().to_string() } else { String::new() };
        crate::quick::show(&link, &self.settings().download_dir);
    }

    pub async fn quick_inspect(self: &Arc<Self>, seq: i32, link: String, pass: String) {
        let li = self.inspect_link(&link, &pass).await;
        if li.needs_passphrase {
            crate::quick::info(seq, crate::quick::QUICK_PASS, &li.error, "", "");
        } else if !li.error.is_empty() {
            crate::quick::info(seq, crate::quick::QUICK_ERROR, &li.error, "", "");
        } else {
            let name = if li.name.is_empty() { "Shared files".to_string() } else { li.name.clone() };
            let files = if li.files == 1 { "file" } else { "files" };
            let warn = if li.unavailable > 0 { format!("{} unavailable", li.unavailable) } else { String::new() };
            crate::quick::info(
                seq,
                crate::quick::QUICK_OK,
                &name,
                &format!("{} {} · {}", li.files, files, xfer::fmt_bytes(li.total_bytes)),
                &warn,
            );
        }
    }

    /// The folder chosen in the panel becomes the download folder, as choosing
    /// one in the main window does.
    pub fn quick_dest(&self, dir: &str) {
        if let Ok(saved) = config::update(|s| s.download_dir = dir.to_string()) {
            *self.settings.lock().unwrap() = saved;
            self.state_changed();
        }
    }

    pub fn quick_start(self: &Arc<Self>, link: &str, pass: &str, dir: &str, subfolder: bool) {
        let res = self.start_download(DownloadOptions {
            link: link.to_string(),
            passphrase: pass.to_string(),
            dest_dir: dir.to_string(),
            subfolder,
        });
        crate::quick::started(&res.err().unwrap_or_default());
    }
}

// ---------- Finder services ----------

impl crate::services::ServiceHost for App {
    fn show_window(&self) {
        App::show_window(self);
    }

    fn emit_service(&self) {
        self.emit("service", serde_json::json!(null));
    }
}

// ---------- The download panel ----------

impl crate::quick::QuickHost for App {
    fn inspect(self: Arc<Self>, seq: i32, link: String, pass: String) {
        tokio::spawn(async move { self.quick_inspect(seq, link, pass).await });
    }

    fn dest(self: Arc<Self>, dir: String) {
        self.quick_dest(&dir);
    }

    fn start(self: Arc<Self>, link: String, pass: String, dir: String, subfolder: bool) {
        self.quick_start(&link, &pass, &dir, subfolder);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn payload() -> SpacePayload {
        SpacePayload {
            projects: vec![
                // A row Frame.io sized, and a row the walk sized.
                SpaceProject {
                    project_id: "proj-reported".into(),
                    name: "Nightfall".into(),
                    bytes: 6_200_000_000,
                    files: 0,
                    walked: false,
                    ..Default::default()
                },
                SpaceProject {
                    project_id: "proj-walked".into(),
                    name: "Archive 2024".into(),
                    bytes: 2_100_000_000,
                    files: 212,
                    walked: true,
                    ..Default::default()
                },
            ],
            accounts: vec![
                SpaceAccount {
                    account_id: "acct-limited".into(),
                    name: "Farbhaus".into(),
                    limit: Some(2_000_000_000_000),
                    used: Some(12_400_000_000),
                },
                // `storage_limit: null`: an account with no limit.
                SpaceAccount {
                    account_id: "acct-unlimited".into(),
                    name: "Marketing".into(),
                    limit: None,
                    used: Some(1_050_000_000),
                },
            ],
        }
    }

    /// The payload the two front ends read: a project list and an account list,
    /// with the account's limit and usage as numbers or as null.
    #[test]
    fn the_space_payload_is_projects_and_accounts() {
        let v = serde_json::to_value(payload()).expect("payload serialises");
        let projects = v.get("projects").and_then(|p| p.as_array()).expect("projects is a list");
        assert_eq!(projects.len(), 2);
        assert_eq!(projects[0]["project_id"], "proj-reported");
        assert_eq!(projects[0]["bytes"], 6_200_000_000_i64);
        assert_eq!(projects[0]["walked"], false);
        assert_eq!(projects[1]["walked"], true);
        assert_eq!(projects[1]["files"], 212);

        let accounts = v.get("accounts").and_then(|a| a.as_array()).expect("accounts is a list");
        assert_eq!(accounts.len(), 2);
        assert_eq!(accounts[0]["name"], "Farbhaus");
        assert_eq!(accounts[0]["limit"], 2_000_000_000_000_i64);
        assert_eq!(accounts[0]["used"], 12_400_000_000_i64);
        assert!(accounts[1]["limit"].is_null(), "no limit is null, not 0");
        assert_eq!(accounts[1]["used"], 1_050_000_000_i64);
    }

    /// An account Frame.io says nothing about still gets a row, with both
    /// figures null, so the view can say "not reported" rather than print 0 B.
    #[test]
    fn an_unreported_account_serialises_as_null() {
        let v = serde_json::to_value(SpaceAccount {
            account_id: "acct-quiet".into(),
            name: "Quiet".into(),
            limit: None,
            used: None,
        })
        .expect("account serialises");
        assert!(v["limit"].is_null());
        assert!(v["used"].is_null());
    }

    /// A payload that was never filled is empty in both lists, never a null the
    /// front ends would have to guard against.
    #[test]
    fn an_empty_payload_has_two_empty_lists() {
        let v = serde_json::to_value(SpacePayload::default()).expect("payload serialises");
        assert_eq!(v["projects"], serde_json::json!([]));
        assert_eq!(v["accounts"], serde_json::json!([]));
    }
}
