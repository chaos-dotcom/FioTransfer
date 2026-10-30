//! Live end-to-end check against a running Portal instance.
//!
//! Skipped unless `PORTAL_LIVE_URL` and `PORTAL_LIVE_TOKEN` are set, so a normal
//! `cargo test` never touches the network. Everything else is the real path:
//! `App::portal_reconcile` -> `portal::agent::reconcile` -> `PortalClient` over
//! HTTP to the live host -> sqlite.
//!
//!   PORTAL_LIVE_URL=http://127.0.0.1:18080 PORTAL_LIVE_TOKEN=… \
//!     cargo test --offline -p fiotransfer --test portal_live -- --nocapture

use std::path::PathBuf;
use std::sync::Arc;

use fiotransfer::app::App;
use fiotransfer::config::Settings;
use fiotransfer::history::Store;
use fiotransfer::portal;
use fiotransfer::xfer::{self, manager as xfer_manager};

fn live() -> Option<(String, String)> {
    match (std::env::var("PORTAL_LIVE_URL"), std::env::var("PORTAL_LIVE_TOKEN")) {
        (Ok(u), Ok(t)) if !u.trim().is_empty() && !t.trim().is_empty() => Some((u, t)),
        _ => {
            eprintln!("skipped: PORTAL_LIVE_URL / PORTAL_LIVE_TOKEN not set");
            None
        }
    }
}

struct World {
    rt: tokio::runtime::Runtime,
    root: PathBuf,
    media: PathBuf,
    app: Arc<App>,
}

fn world(portal_url: &str) -> World {
    let tmp = tempfile::tempdir().expect("temp dir");
    let home = tmp.path().join("home");
    let media = tmp.path().join("media");
    let data = home.join("Library/Application Support/FioTransfer");
    let logs = home.join("Library/Logs/FioTransfer");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::create_dir_all(&logs).unwrap();
    std::fs::create_dir_all(&media).unwrap();
    std::env::set_var("HOME", &home);
    std::env::set_var("FIO_NO_KEYCHAIN", "1");

    let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().expect("runtime");
    let hist = Arc::new(Store::open(&data, &logs).expect("history store"));
    let root = media.join("Live_Work");
    let _g = rt.enter();
    let mut s = Settings::default();
    s.portal_url = portal_url.to_string();
    s.portal_root = root.to_string_lossy().into_owned();
    s.portal_enabled = true;
    let app = App::new(s);
    *app.hist.lock().unwrap() = Some(hist.clone());
    let cell = {
        let a = app.clone();
        move || a.settings()
    };
    let mgr = xfer_manager::new_manager(app.api.clone(), hist.clone(), Box::new(cell));
    let watcher = xfer::watch::Watcher::new(mgr.clone());
    watcher.close(); // these cases assert on the watch list, not on transfers
    *app.wat.lock().unwrap() = Some(watcher);
    *app.mgr.lock().unwrap() = Some(mgr);
    // keep the temp dir alive for the life of the world
    std::mem::forget(tmp);
    World { rt, root, media, app }
}

#[test]
fn live_portal_reconciles_into_watch_folders() {
    let Some((url, token)) = live() else { return };
    let w = world(&url);
    portal::set_token(&token).expect("token stored");

    let _g = w.rt.enter();
    let report = w.rt.block_on(async { w.app.portal_reconcile().await });
    println!("report: {:#?}", report);
    assert!(report.error.is_empty(), "reconcile reported an error: {:?}", report.error);

    let watches = w.app.hist.lock().unwrap().as_ref().unwrap().watches().expect("watches");
    for r in &watches {
        println!("watch  origin={:?} local={} acct={} folder={} label={:?} dir={}", r.origin, r.local_path, r.account_id, r.folder_id, r.dest_label, r.direction);
    }

    let portal_watches: Vec<_> = watches.iter().filter(|r| r.origin.starts_with("portal:")).collect();
    assert_eq!(portal_watches.len(), 1, "exactly one link carries a usable marker");
    let pw = portal_watches[0];
    assert_eq!(PathBuf::from(&pw.local_path), w.root.join("Farbhaus"));
    assert_eq!(pw.account_id, "acct-1");
    assert_eq!(pw.folder_id, "folder-1");
    assert_eq!(pw.direction, "mirror");
    assert!(std::path::Path::new(&pw.local_path).is_dir(), "the mirror folder exists");

    // A hostile marker must never have created a folder, anywhere.
    assert!(!w.media.join("etc").exists());
    assert!(!w.root.join("etc").exists());
    // The account-less link is skipped, so no folder for it either.
    assert!(!w.root.join("Dots").exists());

    // Idempotent: the second run changes nothing.
    let before: Vec<(String, String, String)> =
        watches.iter().map(|r| (r.local_path.clone(), r.account_id.clone(), r.folder_id.clone())).collect();
    let _r2 = w.rt.block_on(async { w.app.portal_reconcile().await });
    let after: Vec<(String, String, String)> = w
        .app
        .hist
        .lock()
        .unwrap()
        .as_ref()
        .unwrap()
        .watches()
        .expect("watches")
        .into_iter()
        .map(|r| (r.local_path, r.account_id, r.folder_id))
        .collect();
    assert_eq!(before, after, "a second reconcile changed the watch list");
}

/// The same host, with a token that is not the right one: the status line must say
/// the token was refused, not print Portal's JSON. Runs against any Portal; skipped
/// unless PORTAL_LIVE_URL is set.
#[test]
fn live_portal_refuses_a_bad_token() {
    let Ok(url) = std::env::var("PORTAL_LIVE_URL") else {
        eprintln!("skipped: PORTAL_LIVE_URL not set");
        return;
    };
    if url.trim().is_empty() {
        return;
    }
    let w = world(&url);
    portal::set_token("definitely-not-the-token").expect("token stored");

    let _g = w.rt.enter();
    let report = w.rt.block_on(async { w.app.portal_reconcile().await });
    println!("report error: {}", report.error);
    assert!(!report.error.is_empty(), "a refused token must be an error, not a quiet success");
    assert!(report.error.contains("refused the token"), "got: {}", report.error);
    assert!(!report.error.contains("\"detail\""), "the JSON wrapper reached the user: {}", report.error);
    assert_eq!(report.ensured, 0, "nothing may be created when the token is refused");

    // And nothing reached the watch list either.
    let watches = w.app.hist.lock().unwrap().as_ref().unwrap().watches().expect("watches");
    assert!(watches.is_empty(), "{} watch(es) created without authorisation", watches.len());
}
