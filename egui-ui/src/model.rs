//! What a screen needs in order to draw, in the terms the views speak.
//!
//! The webview front end receives this as JSON from `window.go.main.App[...]`;
//! these are the typed versions of those payloads, so the two front ends can be
//! driven by the same snapshot. Field names match `src/app.rs` and what
//! `frontend/dist/app.js` reads on purpose — a mismatch here is a UI bug.

use serde::Deserialize;

/// Which of the five screens is showing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum View {
    #[default]
    Transfers,
    Space,
    Watch,
    History,
    Settings,
}

impl View {
    pub fn label(&self) -> &'static str {
        match self {
            View::Transfers => "Transfers",
            View::Space => "Space",
            View::Watch => "Watch Folders",
            View::History => "History",
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

// ---------- the JSON the app already produces ----------

#[derive(Debug, Clone, Deserialize, Default, PartialEq)]
pub struct JobView {
    #[serde(default)] pub id: String,
    #[serde(default)] pub kind: String,
    #[serde(default)] pub title: String,
    #[serde(default)] pub source: String,
    #[serde(default)] pub dest: String,
    #[serde(default)] pub status: String,
    #[serde(default)] pub phase: String,
    #[serde(default)] pub error: String,
    #[serde(default)] pub total: i64,
    #[serde(default)] pub done: i64,
    #[serde(default)] pub speed: f64,
    #[serde(default)] pub eta: f64,
    #[serde(default)] pub files_total: i64,
    #[serde(default)] pub files_done: i64,
    #[serde(default)] pub files_failed: i64,
    #[serde(default)] pub files_active: i64,
    #[serde(default)] pub started_at: i64,
    #[serde(default)] pub finished_at: i64,
    #[serde(default)] pub elapsed: f64,
    #[serde(default)] pub avg_speed: f64,
    #[serde(default)] pub link: String,
    #[serde(default)] pub watch: bool,
}

impl JobView {
    pub fn terminal(&self) -> bool {
        matches!(self.status.as_str(), "done" | "partial" | "failed" | "canceled" | "skipped")
    }
    /// 0.0–1.0, or None while the size isn't known yet.
    pub fn progress(&self) -> Option<f32> {
        if self.total <= 0 {
            None
        } else {
            Some((self.done as f32 / self.total as f32).clamp(0.0, 1.0))
        }
    }
    pub fn is_upload(&self) -> bool {
        self.kind == "upload"
    }
}

#[derive(Debug, Clone, Deserialize, Default, PartialEq)]
pub struct FileView {
    #[serde(default)] pub name: String,
    #[serde(default)] pub rel_path: String,
    #[serde(default)] pub local_path: String,
    #[serde(default)] pub size: i64,
    #[serde(default)] pub done: i64,
    #[serde(default)] pub status: String,
    #[serde(default)] pub error: String,
    #[serde(default)] pub note: String,
    #[serde(default)] pub view_url: String,
    #[serde(default)] pub retries: i64,
}

#[derive(Debug, Clone, Deserialize, Default, PartialEq)]
pub struct Snapshot {
    #[serde(default)] pub jobs: Vec<JobView>,
    #[serde(default)] pub speed: f64,
    #[serde(default)] pub active_streams: i64,
    #[serde(default)] pub stream_limit: i64,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Settings {
    #[serde(default)] pub client_id: String,
    #[serde(default)] pub redirect_port: i64,
    #[serde(default)] pub streams: i64,
    #[serde(default)] pub segment_mb: i64,
    #[serde(default)] pub download_dir: String,
    #[serde(default)] pub no_folder_logs: bool,
    #[serde(default)] pub no_update_check: bool,
    #[serde(default)] pub last_folder_label: String,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct User {
    #[serde(default)] pub name: String,
    #[serde(default)] pub email: String,
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

/// What Settings and the update card need to know about a release.
#[derive(Debug, Clone, Default)]
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

/// One project's footprint on Frame.io, as `App::Space` reports it.
///
/// The field names are the payload's, minus `root_folder_id` and
/// `workspace_id`: the screen never names a folder, only the project it would
/// delete. `bytes` is what Frame.io reported for the project, or what the walk
/// added up when it reported nothing; `walked` says which, and `files` is only
/// counted by a walk (0 means "not counted", drawn as a dash). `error` is set
/// when a walk could not finish (permissions, a folder deleted mid-walk).
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
pub struct SpaceRow {
    #[serde(default)] pub account_id: String,
    #[serde(default)] pub account_name: String,
    #[serde(default)] pub workspace_name: String,
    #[serde(default)] pub project_id: String,
    #[serde(default)] pub name: String,
    #[serde(default)] pub bytes: i64,
    #[serde(default)] pub files: i64,
    /// True when `bytes` came from walking the project's folders, false when
    /// Frame.io reported it. The row says which.
    #[serde(default)] pub walked: bool,
    #[serde(default)] pub error: String,
}

/// One account's storage, as `Account.storage_limit` and `Account.storage_usage`
/// report it. `limit: None` is Frame.io's `null`, which means "no limit".
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
pub struct SpaceAccount {
    #[serde(default)] pub account_id: String,
    #[serde(default)] pub name: String,
    #[serde(default)] pub limit: Option<i64>,
    #[serde(default)] pub used: Option<i64>,
}

/// The `Space` payload: the project rows to chart and the per-account headline
/// above them. `App::Space` answers this object, not a bare list of projects.
#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq)]
pub struct SpacePayload {
    #[serde(default)] pub projects: Vec<SpaceRow>,
    #[serde(default)] pub accounts: Vec<SpaceAccount>,
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

#[derive(Debug, Clone, Default)]
pub struct WatchRow {
    pub local: String,
    pub label: String,
    pub direction: String,
    pub enabled: bool,
    pub pending: i64,
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
}
