//! Live end-to-end check against a running Portal instance.
//!
//! Skipped unless the variables each case needs are set, so a normal
//! `cargo test` never touches the network. Everything else is the real path:
//! `App::portal_reconcile` -> `portal::agent::reconcile` -> `PortalClient` over
//! HTTP to the live host -> sqlite.
//!
//!   # with a bearer token (`PORTAL_API_TOKEN` set on that Portal)
//!   PORTAL_LIVE_URL=http://127.0.0.1:18080 PORTAL_LIVE_TOKEN=… \
//!     cargo test --offline -p fiotransfer --test portal_live -- --nocapture
//!
//!   # signing in instead, for a Portal that holds no token
//!   PORTAL_LIVE_URL=http://127.0.0.1:18080 PORTAL_LIVE_EMAIL=… PORTAL_LIVE_PASSWORD=… \
//!     cargo test --offline -p fiotransfer --test portal_live -- --nocapture
//!
//! The password is read from the environment and from nowhere else: a password
//! written into a source file is a password in a repository.

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

/// The admin sign-in's inputs: the same base URL, and the email and password
/// from the environment. Portal's own login is the only place a password is
/// typed, so it is never a constant in this file.
fn live_login() -> Option<(String, String, String)> {
    let url = std::env::var("PORTAL_LIVE_URL").ok().filter(|u| !u.trim().is_empty());
    let email = std::env::var("PORTAL_LIVE_EMAIL").ok().filter(|e| !e.trim().is_empty());
    let password = std::env::var("PORTAL_LIVE_PASSWORD").ok().filter(|p| !p.is_empty());
    match (url, email, password) {
        (Some(u), Some(e), Some(p)) => Some((u, e, p)),
        _ => {
            eprintln!("skipped: PORTAL_LIVE_URL / PORTAL_LIVE_EMAIL / PORTAL_LIVE_PASSWORD not set");
            None
        }
    }
}

/// One live case at a time. `world` sets `HOME` and `FIO_NO_KEYCHAIN` for the
/// whole process, so two cases at once would share — and clobber — the folder
/// the stored credentials live in.
fn one_at_a_time() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

/// Every file under `dir`, as text: what the app wrote, wherever it wrote it.
fn every_document_under(dir: &std::path::Path) -> Vec<(PathBuf, String)> {
    let mut out = Vec::new();
    let mut stack = vec![dir.to_path_buf()];
    while let Some(d) = stack.pop() {
        let Ok(rd) = std::fs::read_dir(&d) else { continue };
        for e in rd.flatten() {
            let p = e.path();
            if p.is_dir() {
                stack.push(p);
            } else if let Ok(b) = std::fs::read(&p) {
                out.push((p.clone(), String::from_utf8_lossy(&b).into_owned()));
            }
        }
    }
    out
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
    let _serial = one_at_a_time();
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
    let _serial = one_at_a_time();
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


// ---------- signing in instead of a bearer token ----------
//
// A Portal deployed by `deploy.sh` holds no `PORTAL_API_TOKEN`, and its `/api`
// endpoints then accept one thing only: the admin's session cookie. These three
// cases are that path, against a live Portal: the sign-in itself, a wrong
// password, and a session that has expired.

/// Sign in with Portal's own login: the cookie is kept as a jar, no password is
/// written anywhere, and the reconcile it authorises makes exactly the watch the
/// bearer-token case makes.
#[test]
fn live_portal_signs_in_and_reconciles_the_same_watch() {
    let _serial = one_at_a_time();
    let Some((url, email, password)) = live_login() else { return };
    let w = world(&url);
    // A stored token wins over a session (that is the precedence), and this case
    // is about the session.
    portal::clear_token();

    let _g = w.rt.enter();
    let st = w
        .rt
        .block_on(async { w.app.portal_sign_in(&email, &password).await })
        .unwrap_or_else(|e| panic!("the sign-in failed: {e}"));
    println!("signed in as {:?} (signed_in={})", st.email, st.signed_in);
    assert!(st.signed_in, "a session is a stored credential");
    assert_eq!(st.email, email, "Portal names the admin it signed in");
    assert_eq!(portal::signed_in_email().as_deref(), Some(email.as_str()));

    // The jar is kept, as JSON, in the app's own data folder (there is no
    // Keychain under FIO_NO_KEYCHAIN), and it names the Portal that issued it.
    let stored = portal::session().expect("a stored session");
    assert_eq!(stored.base, url.trim().trim_end_matches('/'));
    assert!(!stored.is_empty(), "the session cookie is kept");
    assert_eq!(w.app.portal_status().email, email);

    // The password is nowhere the app writes: not in settings.json, not beside
    // it, not in the log folder.
    for (path, text) in every_document_under(&fiotransfer::config::dir()) {
        assert!(!text.contains(&password), "{} carries the password", path.display());
    }
    for (path, text) in every_document_under(&fiotransfer::config::log_dir()) {
        assert!(!text.contains(&password), "{} carries the password", path.display());
    }
    // … and the email, which is not a secret, is a setting so the field can be
    // prefilled next time.
    let saved = std::fs::read_to_string(fiotransfer::config::path()).expect("settings.json");
    assert!(saved.contains(&email), "the email is prefilled from settings: {saved}");

    // The stored session alone is a usable credential: no token is stored.
    assert_eq!(portal::token(), None);
    let c = portal::PortalClient::from_settings(&w.app.settings()).expect("the session is a credential");
    assert_eq!(c.credential(), "session");
    let who = w.rt.block_on(async { c.me().await }).expect("GET /api/auth/me with the session");
    assert_eq!(who.email, email, "me() names the signed-in admin");
    let links = w
        .rt
        .block_on(async { c.upload_links().await })
        .expect("GET /api/upload-links with the session");
    println!("links read with the session: {}", links.len());
    assert!(!links.is_empty(), "a signed-in session reads the links");
    assert!(links.iter().any(|l| l.marker() == "Farbhaus"), "the seeded link is there");

    // And the reconcile it authorises makes the same folder the token does.
    let report = w.rt.block_on(async { w.app.portal_reconcile().await });
    println!("report: {:#?}", report);
    assert!(report.error.is_empty(), "reconcile reported an error: {:?}", report.error);
    let watches = w.app.hist.lock().unwrap().as_ref().unwrap().watches().expect("watches");
    let portal_watches: Vec<_> = watches.iter().filter(|r| r.origin.starts_with("portal:")).collect();
    assert_eq!(portal_watches.len(), 1, "exactly one link carries a usable marker");
    let pw = portal_watches[0];
    assert_eq!(PathBuf::from(&pw.local_path), w.root.join("Farbhaus"));
    assert_eq!(pw.account_id, "acct-1");
    assert_eq!(pw.folder_id, "folder-1");
    assert!(std::path::Path::new(&pw.local_path).is_dir(), "the mirror folder exists");
    assert!(!w.media.join("etc").exists(), "the hostile marker created nothing");

    // Signing out forgets the jar — and nothing else — and the next reconcile
    // says there is no credential instead of calling Portal with none.
    let st = w.app.portal_sign_out();
    assert!(portal::session().is_none(), "the jar is gone");
    assert!(!portal::signed_in());
    assert!(!st.signed_in);
    let rep = w.rt.block_on(async { w.app.portal_reconcile().await });
    assert!(rep.error.contains("token"), "got: {}", rep.error);
}

/// A wrong password: Portal's answer is the message, no cookie is kept, and
/// nothing that could be sent to Portal is stored.
#[test]
fn live_portal_refuses_a_wrong_password_and_keeps_nothing() {
    let _serial = one_at_a_time();
    let Some((url, email, password)) = live_login() else { return };
    let w = world(&url);
    portal::clear_token();
    portal::clear_session();

    let wrong = format!("{password}-and-then-some");
    let _g = w.rt.enter();
    let err = w
        .rt
        .block_on(async { w.app.portal_sign_in(&email, &wrong).await })
        .expect_err("a wrong password must not sign in");
    println!("refused: {err}");
    assert!(err.contains("refused the sign-in"), "got: {err}");
    assert!(err.contains("401"), "got: {err}");
    assert!(!err.contains(&wrong), "the password reached the message: {err}");

    // No cookie, no credential, and the app knows neither.
    assert!(portal::session().is_none(), "a refused sign-in stores no cookie");
    assert!(!portal::signed_in());
    assert!(!w.app.portal_status().signed_in);
    let rep = w.rt.block_on(async { w.app.portal_reconcile().await });
    assert!(rep.error.contains("token"), "got: {}", rep.error);

    // The password is not in settings.json, not beside it, not in a log.
    for dir in [fiotransfer::config::dir(), fiotransfer::config::log_dir()] {
        for (path, text) in every_document_under(&dir) {
            assert!(!text.contains(&wrong), "{} carries the password", path.display());
        }
    }
}

/// A session Portal will not accept says the session expired, and offers the fix
/// — never the token's message, because the fix is not the same one.
#[test]
fn live_portal_says_the_session_expired_not_that_the_token_was_refused() {
    let _serial = one_at_a_time();
    let Some((url, email, _password)) = live_login() else { return };
    let w = world(&url);
    portal::clear_token();

    // Two jars in the shape a real sign-in writes: one holding a cookie Portal
    // never issued, and one emptied — its cookies gone, which is what an expired
    // session looks like on disk.
    let forged = portal::Session {
        base: url.trim().trim_end_matches('/').to_string(),
        email: email.clone(),
        cookies: vec![
            "portal_session=eyJ1c2VyX2lkIjoiMDAwMDAwMDAtMDAwMC0wMDAwLTAwMDAtMDAwMDAwMDAwMDAwIn0.auQbjA.not-a-signature".to_string(),
        ],
    };
    let cases = [
        ("a cookie Portal never issued", forged.clone()),
        ("an emptied jar", portal::Session { cookies: Vec::new(), ..forged.clone() }),
    ];

    let _g = w.rt.enter();
    for (what, jar) in cases {
        portal::save_session(&jar).expect("the jar is stored");
        let c = portal::PortalClient::from_settings(&w.app.settings()).expect("the jar is the credential");
        assert_eq!(c.credential(), "session", "{what}: a session, not a token");

        let rep = w.rt.block_on(async { w.app.portal_reconcile().await });
        println!("{what}: {}", rep.error);
        assert!(rep.error.contains("session expired"), "{what}: {}", rep.error);
        assert!(rep.error.contains("sign in again"), "{what}: {}", rep.error);
        assert!(
            !rep.error.contains("refused the token"),
            "{what}: the token's message is not this one: {}",
            rep.error
        );
        assert_eq!(rep.ensured, 0, "{what}: nothing may be created");
        assert_eq!(rep.links_seen, 0, "{what}: no links were read");
        let watches = w.app.hist.lock().unwrap().as_ref().unwrap().watches().expect("watches");
        assert!(
            watches.iter().all(|r| !r.origin.starts_with("portal:")),
            "{what}: a folder was made without a working credential"
        );
    }
}
