
//! User settings persisted under ~/Library/Application Support/FioTransfer.

use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use std::sync::Mutex;

/// One level of a Frame.io path: account, workspace, project or folder.
#[derive(Debug, Clone, Serialize, Deserialize, Default, PartialEq)]
pub struct Crumb {
    pub id: String,
    pub name: String,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub folder_id: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Settings {
    /// Adobe Developer Console "OAuth Single Page App" credential client ID.
    pub client_id: String,
    /// Port for the sign-in redirect (https://localhost:<port>/callback).
    pub redirect_port: i32,
    /// Max parallel HTTP streams across all transfers.
    pub streams: i32,
    /// Download segment size in MiB.
    pub segment_mb: i32,
    /// Default folder for downloads.
    pub download_dir: String,
    /// Write a transfer log next to the media (on by default).
    pub no_folder_logs: bool,
    /// Ask GitHub for new releases (on by default).
    pub no_update_check: bool,
    /// The last release a notification was shown for.
    pub update_notified: String,

    /// Last upload destination, remembered for convenience.
    pub last_account_id: String,
    pub last_folder_id: String,
    pub last_folder_label: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub last_folder_path: Vec<Crumb>,
}

impl Default for Settings {
    fn default() -> Self {
        Settings {
            client_id: String::new(),
            redirect_port: 8765,
            streams: 16,
            segment_mb: 64,
            download_dir: home_dir().join("Downloads").to_string_lossy().into_owned(),
            no_folder_logs: false,
            no_update_check: false,
            update_notified: String::new(),
            last_account_id: String::new(),
            last_folder_id: String::new(),
            last_folder_label: String::new(),
            last_folder_path: Vec::new(),
        }
    }
}

impl Settings {
    pub fn folder_logs(&self) -> bool {
        !self.no_folder_logs
    }

    fn normalize(&mut self) {
        let d = Settings::default();
        if self.redirect_port <= 0 {
            self.redirect_port = d.redirect_port;
        }
        if self.streams <= 0 {
            self.streams = d.streams;
        }
        if self.streams > 128 {
            self.streams = 128;
        }
        if self.segment_mb < 8 {
            self.segment_mb = d.segment_mb;
        }
        if self.segment_mb > 512 {
            self.segment_mb = 512;
        }
        if self.download_dir.is_empty() {
            self.download_dir = d.download_dir;
        }
    }
}

pub fn home_dir() -> PathBuf {
    match std::env::var_os("HOME") {
        Some(h) => PathBuf::from(h),
        None => PathBuf::from("/"),
    }
}

/// ~/Library/Application Support/FioTransfer (created on demand).
pub fn dir() -> PathBuf {
    let dir = home_dir().join("Library").join("Application Support").join("FioTransfer");
    let _ = std::fs::create_dir_all(&dir);
    dir
}

/// Where the human-readable transfer log lives.
pub fn log_dir() -> PathBuf {
    let dir = home_dir().join("Library").join("Logs").join("FioTransfer");
    let _ = std::fs::create_dir_all(&dir);
    dir
}

pub fn path() -> PathBuf {
    dir().join("settings.json")
}

static MU: Mutex<()> = Mutex::new(());

pub fn load() -> Settings {
    let _g = MU.lock().unwrap();
    load_unlocked()
}

fn load_unlocked() -> Settings {
    let mut s = Settings::default();
    if let Ok(b) = std::fs::read(path()) {
        if let Ok(v) = serde_json::from_slice::<Settings>(&b) {
            s = v;
        }
    }
    s.normalize();
    s
}

fn save(s: &Settings) -> std::io::Result<()> {
    let mut s = s.clone();
    s.normalize();
    let b = serde_json::to_vec_pretty(&s).unwrap_or_default();
    let p = path();
    let tmp = PathBuf::from(format!("{}.tmp", p.display()));
    std::fs::write(&tmp, &b)?;
    std::fs::rename(&tmp, &p)
}

/// Loads, mutates and saves settings under one lock, so concurrent updates
/// can't drop each other's changes.
pub fn update<F: FnOnce(&mut Settings)>(f: F) -> std::io::Result<Settings> {
    let _g = MU.lock().unwrap();
    let mut s = load_unlocked();
    f(&mut s);
    s.normalize();
    save(&s)?;
    Ok(s)
}

pub const ERR_NO_CLIENT_ID: &str = "no Adobe Client ID set";
