//! What a screen needs in order to draw, in the terms the views speak.
//!
//! The webview front end receives this as JSON from `window.go.main.App[...]`;
//! these are the typed versions of those payloads, so the two front ends can be
//! driven by the same snapshot. Field names match `src/app.rs` and what
//! `frontend/dist/app.js` reads on purpose — a mismatch here is a UI bug.

use serde::Deserialize;

/// Which of the four screens is showing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum View {
    #[default]
    Transfers,
    Watch,
    History,
    Settings,
}

impl View {
    pub fn label(&self) -> &'static str {
        match self {
            View::Transfers => "Transfers",
            View::Watch => "Watch Folders",
            View::History => "History",
            View::Settings => "Settings",
        }
    }
    pub const ALL: [View; 4] = [View::Transfers, View::Watch, View::History, View::Settings];
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
}
