//! What a screen needs in order to draw, in the terms the views speak.
//!
//! The data types are the core's, not copies: a `JobView`, a `FileView`, a
//! `Snapshot`, the `Settings` and the signed-in `User` are what
//! `fiotransfer_core::xfer` and `fiotransfer_core::config` serialise, and the
//! same structs the app's own methods answer with. There is one model now — a
//! mismatch between the two front ends' idea of a job is not possible.
//!
//! One exception, and it is a build one: `fiotransfer-core` cannot compile for
//! `wasm32` (tokio, rustls, rusqlite), so the browser capture gets the same five
//! shapes from [`crate::model_browser`] instead. The re-export below is the whole
//! of the difference; everything else in this file is one file for both targets.
//!
//! What stays here is what only a screen has an opinion about: which of the five
//! screens is showing, the toast stack, the composer's text, and [`Request`] —
//! what a screen asks the app to do.

use serde::Deserialize;

// Two sets of data shapes, one per target. The native build draws what the
// engine produces, so it uses the core's own structs — there is one model, not a
// copy that can drift. The browser capture (`src/wasm.rs`) has no engine to ask:
// `fiotransfer-core` is tokio, rustls and rusqlite underneath and does not build
// for `wasm32`, so it keeps the same five shapes in `model_browser.rs`, field for
// field.
#[cfg(not(target_arch = "wasm32"))]
pub use fiotransfer_core::api::User;
#[cfg(not(target_arch = "wasm32"))]
pub use fiotransfer_core::config::Settings;
#[cfg(not(target_arch = "wasm32"))]
pub use fiotransfer_core::xfer::{FileView, JobView, Snapshot, Status};

#[cfg(target_arch = "wasm32")]
pub use crate::model_browser::*;

/// Which of the five screens is showing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum View {
    #[default]
    Transfers,
    Watch,
    History,
    Space,
    Settings,
}

impl View {
    pub fn label(&self) -> &'static str {
        match self {
            View::Transfers => "Transfers",
            View::Watch => "Watch Folders",
            View::History => "History",
            View::Space => "Space",
            View::Settings => "Settings",
        }
    }
    /// `View::ALL` is the sidebar's order, which is `index.html`'s: Space sits
    /// between History and Settings.
    pub const ALL: [View; 5] = [
        View::Transfers,
        View::Watch,
        View::History,
        View::Space,
        View::Settings,
    ];
}

// ---------- what a screen asks for ----------

/// One thing a screen asked the app to do this frame.
///
/// A view is a pure renderer over [`UiState`]: it never opens a panel, never
/// talks to Frame.io and never touches a file. It pushes a `Request` instead,
/// and the shell (`crate::shell`) hands it to the real `App` — the same call the
/// webview front end made through `window.go.main.App[...]`, named here in Rust.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Request {
    /// A file or folder panel; the answer comes back as new state.
    PickFiles,
    PickFolder,
    PickWatchFolder,
    PickDownloadDir,

    /// Upload these paths to the remembered destination (`StartUpload`).
    StartUpload(Vec<String>),
    /// What `InspectLink` says about a typed link; the answer fills
    /// `UiState::link_info`.
    InspectLink { link: String, passphrase: String },
    /// The composer's Download button (`StartDownload`).
    StartDownload { link: String, passphrase: String, dest_dir: String, subfolder: bool },
    Cancel(String),
    Retry(String),
    /// `ClearFinished`.
    ClearFinished,
    /// The job's public share link; the shell copies it and says so (`ShareLink`).
    ShareLink(String),
    /// Show a path in the file manager (`Reveal`).
    Reveal(String),

    /// Ask the history store again (`History(query, kind, 500)`).
    History { query: String, kind: String },
    /// Write the history to a CSV the user picks (`ExportHistory`).
    ExportCsv,
    /// Open the log folder (`OpenLogs`).
    OpenLogs,

    /// Paste the clipboard into the composer's link field (`ClipboardText`,
    /// then `InspectLink`).
    Paste,
    /// Open a URL in the browser (`OpenURL`).
    OpenUrl(String),
    /// Put text on the clipboard.
    CopyText(String),

    /// `SaveSettings` (and `SetUploadDestination` when the destination is what
    /// changed).
    SaveSettings,
    SetOpenAtLogin(bool),
    Login,
    Logout,
    /// `CheckForUpdate` / `InstallUpdate`.
    CheckUpdate,
    InstallUpdate,

    /// Ask for the watch list again (`Watches`).
    Watches,
    /// Pick a folder, then a destination, and watch it (`AddWatch`).
    AddWatch,
    SetWatchEnabled { id: String, on: bool },
    RemoveWatch(String),
    RequeueCanceled(String),

    /// Walk every account, workspace and project again (`RefreshSpace`).
    RefreshSpace,
    /// Delete one project on Frame.io (`DeleteProject(account, project)`).
    /// `name` is for the toast; the two ids are what the call takes.
    DeleteProject { account_id: String, project_id: String, name: String },

    /// Settings' Portal section: the server URL, already checked for its scheme
    /// by the row that commits it (`App::set_portal`).
    SetPortalUrl(String),
    /// The local folder Portal's markers are mirrored under
    /// (`Settings::portal_root`).
    SetPortalRoot(String),
    /// Store the bearer token in the keychain. The token is a [`Secret`], so no
    /// `{:?}` of this enum can print it, and it is never a setting.
    SetPortalToken(Secret),
    /// Forget the stored bearer token.
    ForgetPortalToken,
    /// Sign in to Portal itself with the admin's email and password
    /// (`App::portal_sign_in`). The password is a [`Secret`], so no `{:?}` of
    /// this enum can print it, and it is never a setting: the app posts it,
    /// keeps the cookie Portal answers with and hands the new status back.
    SignInPortal { email: String, password: Secret },
    /// Forget the stored session cookie (`App::portal_sign_out`). A bearer
    /// token is a different credential under a different key, and stays.
    SignOutPortal,
    /// Turn Portal's background poller on or off (`Settings::portal_enabled`).
    SetPortalEnabled(bool),
    /// Reconcile the watch folders with Portal now (`App::portal_reconcile`).
    PortalReconcile,

    /// Read everything again (the first frame, and after an event).
    Refresh,
    /// The user closed the window (the shell asks before quitting).
    Quit,
}

// ---------- everything a screen draws from ----------

#[derive(Debug, Clone, Default)]
pub struct UiState {
    pub view: View,
    pub logged_in: bool,
    pub user: Option<User>,
    pub settings: Settings,
    pub snapshot: Snapshot,
    pub history: Vec<HistoryRow>,
    pub watches: Vec<WatchRow>,
    /// What the last Frame.io storage reading found, one row per project.
    pub space: Vec<SpaceRow>,
    /// One row per account: how much it uses against its limit, for the Space
    /// screen's headline. Empty until `Space` has answered.
    pub space_accounts: Vec<SpaceAccount>,
    /// True while `RefreshSpace` is walking: the Space screen says so instead of
    /// pretending the figures are current.
    pub space_walking: bool,
    pub toasts: Vec<Toast>,
    /// Frame.io destination the composer will upload to.
    pub destination: String,
    /// The running build's version, for Settings' "Version" row.
    pub version: String,
    /// Whether the app is set to start at login (Settings' checkbox).
    pub open_at_login: bool,
    /// Where the app keeps its log, for "App log" + "Show in Finder".
    pub log_dir: String,
    /// The user's home, so paths can be shown the way `shortPath` does (`~/Movies`).
    pub home: String,
    /// Update state, for the "Check now" row and the sidebar's update card.
    pub update: UpdateState,
    /// What the user has typed in the composer's link field.
    pub link: String,
    /// The Settings screen's Portal section: the last status, the fields being
    /// typed, and the reason a field's last commit was refused.
    pub portal: PortalUi,
    /// The composer's passphrase field, shown only when a link needs one.
    pub passphrase: String,
    /// "New folder" under the link field (`#subfolder`).
    pub new_folder: bool,
    /// The destination chosen for this download, overriding `settings.download_dir`.
    pub download_dir: String,
    /// What `InspectLink` said about the typed link: the preview line, the
    /// passphrase prompt and whether Download is live all come from it.
    pub link_info: Option<LinkInfo>,
    /// The crumbs of the remembered upload destination, outermost first.
    pub dest_path: Vec<Crumb>,
    /// The expanded job whose files are listed, and its files.
    pub expanded: Option<String>,
    pub files: Vec<FileView>,
    /// What the screens asked the app to do since the last frame; the shell
    /// drains it before drawing.
    pub requests: Vec<Request>,
}

#[derive(Debug, Clone, Default)]
pub struct HistoryRow {
    pub title: String,
    pub kind: String,
    pub status: String,
    pub when: String,
    pub files: String,
    pub size: String,
}

/// One level of a Frame.io path: account, workspace, project or folder.
#[derive(Debug, Clone, Default)]
pub struct Crumb {
    pub kind: String,
    pub name: String,
}

/// The `InspectLink` payload the composer's preview is built from.
#[derive(Debug, Clone, Default)]
pub struct LinkInfo {
    pub name: String,
    pub files: i64,
    pub total_bytes: i64,
    pub unavailable: i64,
    pub error: String,
    pub needs_passphrase: bool,
}

impl LinkInfo {
    /// The app's answer, as the composer wants it (`fiotransfer_core::xfer::LinkInfo`).
    ///
    /// Native only: the browser capture has no app to ask, so it has no such
    /// conversion and no `fiotransfer_core` to name.
    #[cfg(not(target_arch = "wasm32"))]
    pub fn from_core(li: fiotransfer_core::xfer::LinkInfo) -> LinkInfo {
        LinkInfo {
            name: li.name,
            files: li.files,
            total_bytes: li.total_bytes,
            unavailable: li.unavailable,
            error: li.error,
            needs_passphrase: li.needs_passphrase,
        }
    }
}

/// What Settings and the update card need to know about a release.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct UpdateState {
    pub current: String,
    pub latest: String,
    pub available: bool,
    /// idle | checking | downloading | restarting
    pub state: String,
    pub progress: f32,
    pub error: String,
    /// Why this copy cannot update itself ("development build", …).
    pub blocker: String,
}

impl UpdateState {
    /// The app's `UpdateStatus`, as the views want it.
    #[cfg(not(target_arch = "wasm32"))]
    pub fn from_core(st: &fiotransfer_core::app::UpdateStatus) -> UpdateState {
        UpdateState {
            current: st.current.clone(),
            latest: st.latest.clone(),
            available: st.available,
            state: st.state.clone(),
            progress: st.progress as f32,
            error: st.error.clone(),
            blocker: st.blocker.clone(),
        }
    }
}

/// What the Settings screen's Portal section knows: the last status the app
/// gave (`App::portal_status`), the fields the user is typing in, and the reason
/// a commit was refused. Neither secret is here: the access token is only a
/// draft the field masks plus a [`Secret`] on its way to the keychain, and the
/// sign-in password is only a masked draft the shell empties, plus the [`Secret`]
/// one request carries.
#[derive(Debug, Clone, Default)]
pub struct PortalUi {
    pub status: PortalView,
    /// The URL being typed, or `None` while the field shows
    /// `settings.portal_url`. A refused value stays here with its reason under
    /// the field, so the stored setting is never the one that was rejected.
    pub url_draft: Option<String>,
    /// The local folder being typed, or `None` while the field shows
    /// `settings.portal_root`.
    pub root_draft: Option<String>,
    /// The token being typed. It never becomes a setting: Save hands it to the
    /// keychain and the shell empties this.
    pub token_draft: String,
    /// Why the last commit of the URL / the folder was refused.
    pub url_error: Option<String>,
    pub root_error: Option<String>,
    /// The admin email being typed into "Sign in instead". Empty means "show
    /// `settings.portal_email`", which is the address the last sign-in used.
    /// An email is not a secret, so this one is a plain draft and may stay
    /// after a failure.
    pub email_draft: String,
    /// The password being typed. Cleared by the shell the moment the sign-in
    /// answers, whichever way it went: nothing here keeps a password, not even
    /// a refused one.
    pub password_draft: String,
    /// Why Portal refused the last sign-in, in the app's own words.
    pub signin_error: Option<String>,
    /// A sign-in is in flight: the button waits rather than posting twice.
    pub signing_in: bool,
}

/// A secret on its way out of a Settings field: the Portal bearer token (to the
/// keychain) or the password of a Portal sign-in (to `App::portal_sign_in` and
/// nowhere else).
///
/// `Debug` prints nothing of it: a secret must not be logged, and a request enum
/// that holds one would otherwise print it the first time somebody wrote
/// `{:?}` on a `Request`. The one place it is read is the shell's call.
#[derive(Clone, PartialEq, Eq)]
pub struct Secret(String);

impl Secret {
    pub fn new(secret: impl Into<String>) -> Secret {
        Secret(secret.into())
    }

    /// The secret itself.
    pub fn expose(&self) -> &str {
        &self.0
    }

    /// Whether there is a token to store at all.
    pub fn is_empty(&self) -> bool {
        self.0.trim().is_empty()
    }
}

impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Secret(…)")
    }
}

/// What Portal's status line needs, as `App::portal_status` reports it.
///
/// A copy, not the core's `PortalStatus`: the browser capture has no core to ask
/// (the same reason [`UpdateState`] exists), and `now_ms` is added so the
/// relative time is a pure function of the status rather than of the clock at
/// draw time.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct PortalView {
    /// The background poller is on (`Settings::portal_enabled`).
    pub enabled: bool,
    /// A credential is stored — a bearer token or a signed-in session. The
    /// credential itself is never part of a status.
    pub signed_in: bool,
    /// The admin the stored session belongs to. Empty for a bearer token, which
    /// names no account, and empty when nobody is signed in.
    pub email: String,
    pub url: String,
    /// The local root, resolved to an absolute path.
    pub root: String,
    /// A reconcile is running right now.
    pub running: bool,
    /// When the last reconcile ran (unix ms); 0 means it has never run.
    pub last_at: i64,
    /// The last reconcile's numbers.
    pub links_seen: usize,
    pub ensured: usize,
    pub removed: usize,
    pub skipped: usize,
    /// Why the last reconcile did nothing, when it did nothing.
    pub error: String,
    /// When this reading was taken (unix ms), so "2 min ago" is decided by the
    /// shell that read the status rather than by the frame that draws it.
    pub now_ms: i64,
}

impl PortalView {
    /// A URL and a credential are both set: the state the reconcile button needs.
    /// A session counts as much as a bearer token.
    pub fn configured(&self) -> bool {
        !self.url.trim().is_empty() && self.signed_in
    }

    /// The app's `PortalStatus`, as the screen wants it.
    #[cfg(not(target_arch = "wasm32"))]
    pub fn from_core(st: &fiotransfer_core::portal::agent::PortalStatus) -> PortalView {
        PortalView {
            enabled: st.enabled,
            signed_in: st.signed_in,
            email: st.email.clone(),
            url: st.url.clone(),
            root: st.root.clone(),
            running: st.running,
            last_at: st.last_at,
            links_seen: st.links_seen,
            ensured: st.ensured,
            removed: st.removed,
            skipped: st.skipped,
            error: st.error.clone(),
            now_ms: 0,
        }
    }
}

/// One project's footprint on Frame.io, as `App::Space` reports it.
///
/// The field names are the payload's, minus `root_folder_id` and
/// `workspace_id`: the screen never names a folder, only the project it would
/// delete. `bytes` is what Frame.io reported for the project, or what the walk
/// added up when it reported nothing; `walked` says which, and `files` is only
/// counted by a walk (0 means "not counted", drawn as a dash). `error` is set
/// when a walk could not finish (permissions, a folder deleted mid-walk).
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct SpaceRow {
    pub account_id: String,
    pub account_name: String,
    pub workspace_name: String,
    pub project_id: String,
    pub name: String,
    pub bytes: i64,
    pub files: i64,
    /// True when `bytes` came from walking the project's folders, false when
    /// Frame.io reported it. The row says which.
    pub walked: bool,
    pub error: String,
}

/// One account's storage, as `Account.storage_limit` and `Account.storage_usage`
/// report it. `limit: None` is Frame.io's `null`, which means "no limit".
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct SpaceAccount {
    pub account_id: String,
    pub name: String,
    pub limit: Option<i64>,
    pub used: Option<i64>,
}

/// The `Space` payload: the project rows to chart and the per-account headline
/// above them. `App::Space` answers this object, not a bare list of projects.
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct SpacePayload {
    pub projects: Vec<SpaceRow>,
    pub accounts: Vec<SpaceAccount>,
}

impl SpaceRow {
    /// This project's part of `total`, 0.0–1.0. A total of zero is no share
    /// rather than a divide by zero.
    pub fn share(&self, total: i64) -> f32 {
        if total <= 0 || self.bytes <= 0 {
            0.0
        } else {
            (self.bytes as f64 / total as f64) as f32
        }
    }

    /// True when the last walk could not finish, so the figures are not whole.
    pub fn incomplete(&self) -> bool {
        !self.error.trim().is_empty()
    }
}

/// The core's `SpaceProject`, as this screen wants it: the two fields the screen
/// never names (`workspace_id`, `root_folder_id`) are dropped.
///
/// Native only: the browser capture is given its rows by `src/fixture.rs`.
#[cfg(not(target_arch = "wasm32"))]
impl From<fiotransfer_core::app::SpaceProject> for SpaceRow {
    fn from(p: fiotransfer_core::app::SpaceProject) -> SpaceRow {
        SpaceRow {
            account_id: p.account_id,
            account_name: p.account_name,
            workspace_name: p.workspace_name,
            project_id: p.project_id,
            name: p.name,
            bytes: p.bytes,
            files: p.files,
            walked: p.walked,
            error: p.error,
        }
    }
}

#[cfg(not(target_arch = "wasm32"))]
impl From<fiotransfer_core::app::SpaceAccount> for SpaceAccount {
    fn from(a: fiotransfer_core::app::SpaceAccount) -> SpaceAccount {
        SpaceAccount {
            account_id: a.account_id,
            name: a.name,
            limit: a.limit,
            used: a.used,
        }
    }
}

#[cfg(not(target_arch = "wasm32"))]
impl From<fiotransfer_core::app::SpacePayload> for SpacePayload {
    fn from(p: fiotransfer_core::app::SpacePayload) -> SpacePayload {
        SpacePayload {
            projects: p.projects.into_iter().map(SpaceRow::from).collect(),
            accounts: p.accounts.into_iter().map(SpaceAccount::from).collect(),
        }
    }
}

/// One row of the Watch Folders screen, from the app's `WatchView`.
#[derive(Debug, Clone, Default)]
pub struct WatchRow {
    /// The app's watch id (`SetWatchEnabled`, `RemoveWatch`, …).
    pub id: String,
    pub local: String,
    pub label: String,
    pub direction: String,
    pub enabled: bool,
    pub pending: i64,
}

impl WatchRow {
    /// The app's `WatchView`, as the Watch Folders screen wants it.
    ///
    /// Native only, for the same reason as [`LinkInfo::from_core`]: the browser
    /// capture is given its rows by `src/fixture.rs`.
    #[cfg(not(target_arch = "wasm32"))]
    pub fn from_core(w: &fiotransfer_core::xfer::watch::WatchView) -> WatchRow {
        WatchRow {
            id: w.id.clone(),
            local: w.local_path.clone(),
            label: if w.dest_label.is_empty() { w.name.clone() } else { w.dest_label.clone() },
            direction: w.direction.clone(),
            enabled: w.enabled,
            pending: w.pending,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ToastKind {
    Ok,
    Err,
    Info,
}

#[derive(Debug, Clone)]
pub struct Toast {
    pub text: String,
    pub kind: ToastKind,
    pub seconds: f32,
}

impl UiState {
    /// Take a `Space` payload: the rows the chart draws and the accounts the
    /// headline names. Both lists are replaced together, so a refresh that
    /// finds no projects does not leave last week's accounts on screen.
    pub fn apply_space(&mut self, payload: SpacePayload) {
        self.space = payload.projects;
        self.space_accounts = payload.accounts;
    }

    pub fn active_jobs(&self) -> usize {
        self.snapshot.jobs.iter().filter(|j| !j.terminal()).count()
    }
    pub fn push_toast(&mut self, text: impl Into<String>, kind: ToastKind) {
        self.toasts.push(Toast { text: text.into(), kind, seconds: 3.5 });
    }
    /// Asks the shell for something.
    pub fn ask(&mut self, request: Request) {
        self.requests.push(request);
    }
}

/// Bytes as the webview front end words them (`fmtBytes` in app.js).
pub fn fmt_bytes(n: i64) -> String {
    if n <= 0 {
        return "0 B".into();
    }
    let units = ["B", "KB", "MB", "GB", "TB", "PB"];
    let mut v = n as f64;
    let mut i = 0;
    while v >= 1000.0 && i < units.len() - 1 {
        v /= 1000.0;
        i += 1;
    }
    if i == 0 || v >= 100.0 {
        format!("{:.0} {}", v, units[i])
    } else if v >= 10.0 {
        format!("{:.1} {}", v, units[i])
    } else {
        format!("{:.2} {}", v, units[i])
    }
}

/// Seconds as the webview front end words them (`fmtDur` in app.js).
pub fn fmt_dur(sec: f64) -> String {
    let s = sec.round() as i64;
    let (h, m) = (s / 3600, s % 3600 / 60);
    if h > 0 {
        format!("{}h {:02}m", h, m)
    } else if m > 0 {
        format!("{}m {:02}s", m, s % 60)
    } else {
        format!("{}s", s)
    }
}

pub fn fmt_speed(bps: f64) -> String {
    if bps <= 0.0 {
        "—".to_string()
    } else {
        format!("{}/s", fmt_bytes(bps as i64))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_snapshot_from_the_app_parses() {
        let json = r#"{"jobs":[{"id":"a","kind":"upload","title":"clip.mov","status":"running",
            "total":100,"done":25,"speed":10.0,"files_done":1,"files_total":4}],
            "speed":10.0,"active_streams":3,"stream_limit":16}"#;
        let s: Snapshot = serde_json::from_str(json).expect("snapshot parses");
        assert_eq!(s.jobs.len(), 1);
        assert_eq!(s.jobs[0].progress(), Some(0.25));
        assert!(s.jobs[0].is_upload());
        assert!(!s.jobs[0].terminal());
        assert_eq!(s.stream_limit, 16);
    }

    #[test]
    fn byte_and_duration_words_match_the_webview_ui() {
        assert_eq!(fmt_bytes(0), "0 B");
        assert_eq!(fmt_bytes(999), "999 B");
        assert_eq!(fmt_bytes(1000), "1.00 KB");
        assert_eq!(fmt_bytes(1_500_000), "1.50 MB");
        assert_eq!(fmt_dur(65.0), "1m 05s");
        assert_eq!(fmt_speed(0.0), "—");
    }

    #[test]
    fn a_space_row_from_the_app_parses_and_knows_its_share() {
        // The payload `App::Space` returns, with the two fields the screen does
        // not need (`workspace_id`, `root_folder_id`) still in it.
        let json = r#"{"account_id":"acct-1","account_name":"Farbhaus",
            "workspace_id":"ws-1","workspace_name":"Shows","project_id":"proj-1",
            "name":"Nightfall","root_folder_id":"root-1","bytes":750,"files":3,
            "error":""}"#;
        let row: SpaceRow = serde_json::from_str(json).expect("space row parses");
        assert_eq!(row.account_name, "Farbhaus");
        assert_eq!(row.workspace_name, "Shows");
        assert_eq!(row.project_id, "proj-1");
        assert_eq!(row.name, "Nightfall");
        assert_eq!((row.bytes, row.files), (750, 3));
        assert!(!row.incomplete());
        assert_eq!(row.share(3000), 0.25);
        // A zero total is no share, not a divide by zero.
        assert_eq!(row.share(0), 0.0);
        assert_eq!(SpaceRow { bytes: 0, ..row.clone() }.share(3000), 0.0);

        let failed = SpaceRow {
            error: "403 forbidden".into(),
            ..row
        };
        assert!(failed.incomplete());
    }

    /// The `Space` payload is `{ projects, accounts }`, and the account figures
    /// are nullable: `storage_limit: null` is "no limit", not a limit of 0.
    #[test]
    fn the_space_payload_parses_projects_and_accounts() {
        let json = r#"{
            "projects":[
                {"account_id":"acct-1","project_id":"proj-1","name":"Nightfall",
                 "bytes":750,"files":3,"walked":false,"error":""}
            ],
            "accounts":[
                {"account_id":"acct-1","name":"Farbhaus","limit":2000,"used":750},
                {"account_id":"acct-2","name":"Archive","limit":null,"used":640},
                {"account_id":"acct-3","name":"Quiet","limit":500,"used":null}
            ]
        }"#;
        let payload: SpacePayload = serde_json::from_str(json).expect("space payload parses");
        assert_eq!(payload.projects.len(), 1);
        assert_eq!(payload.projects[0].name, "Nightfall");
        assert!(!payload.projects[0].walked, "a reported size was not walked");
        assert_eq!(payload.accounts.len(), 3);
        assert_eq!(payload.accounts[0].limit, Some(2000));
        assert_eq!(payload.accounts[1].limit, None, "null is no limit");
        assert_eq!(payload.accounts[1].used, Some(640));
        assert_eq!(payload.accounts[2].used, None, "a null usage is not reported");

        let mut state = UiState::default();
        state.apply_space(payload.clone());
        assert_eq!(state.space.len(), 1);
        assert_eq!(state.space_accounts.len(), 3);

        // A bare list is not this payload. serde reads a sequence into a struct
        // as positional fields, so an empty one is the empty payload rather than
        // an error — no rows, no accounts, nothing drawn.
        let bare: SpacePayload = serde_json::from_str("[]").expect("an empty sequence parses");
        assert!(bare.projects.is_empty() && bare.accounts.is_empty());
        // Both lists may be absent, and then both are empty.
        let empty: SpacePayload = serde_json::from_str("{}").expect("an empty payload parses");
        assert!(empty.projects.is_empty() && empty.accounts.is_empty());
    }

    /// The core's own payload converts into the rows the screen draws, minus the
    /// two fields the screen never names.
    #[cfg(not(target_arch = "wasm32"))]
    #[test]
    fn the_cores_space_payload_becomes_the_screens_rows() {
        let core = fiotransfer_core::app::SpacePayload {
            projects: vec![fiotransfer_core::app::SpaceProject {
                account_id: "acct-1".into(),
                account_name: "Farbhaus".into(),
                workspace_id: "ws-1".into(),
                workspace_name: "Shows".into(),
                project_id: "proj-1".into(),
                name: "Nightfall".into(),
                root_folder_id: "root-1".into(),
                bytes: 750,
                files: 3,
                walked: true,
                error: String::new(),
            }],
            accounts: vec![fiotransfer_core::app::SpaceAccount {
                account_id: "acct-1".into(),
                name: "Farbhaus".into(),
                limit: None,
                used: Some(750),
            }],
        };
        let ours: SpacePayload = core.into();
        assert_eq!(ours.projects.len(), 1);
        assert_eq!(ours.projects[0].project_id, "proj-1");
        assert_eq!(ours.projects[0].bytes, 750);
        assert!(ours.projects[0].walked);
        assert_eq!(ours.accounts[0].limit, None);
        assert_eq!(ours.accounts[0].used, Some(750));
        // The two dropped fields have no home on this side.
        assert_eq!(ours.projects[0].workspace_name, "Shows");
        assert_eq!(ours.projects[0].name, "Nightfall");
        assert_eq!(ours.projects[0].files, 3);
    }
}
