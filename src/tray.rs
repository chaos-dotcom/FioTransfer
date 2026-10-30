//! The menu bar item.
//!
//! `mac/tray_darwin.m` is the AppKit side, unchanged from the Go app: it makes
//! the status item, the menu, the transfer rows drawn from JSON
//! (`set_jobs`) and the app menu's "Check for Updates…" item. This module is
//! its Go counterpart, `tray_darwin.go`: it keeps the handler the app installs
//! with `init` and answers the menu's actions. The C calls are
//! `fiotransfer_sys::tray`'s, including the `goTrayAction` entry point the shim
//! calls back.
//!
//! Threading: every C entry point in `tray_darwin.m` hops to the main queue
//! itself (`dispatch_async(dispatch_get_main_queue(), ^{ … })`), so all the
//! functions here may be called from any thread, tokio workers included.
//! `goTrayAction` arrives on the main thread; the Go app ran the handler on a
//! goroutine, here it runs inline on the main thread, so a handler must not
//! block — the app's handler posts a menu event into the window's event loop
//! and returns.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

/// Open FioTransfer (the main window).
pub const TRAY_OPEN: i32 = 1;
/// Turn "Open at Login" on or off.
pub const TRAY_LOGIN: i32 = 2;
/// Quit FioTransfer.
pub const TRAY_QUIT: i32 = 3;
/// The app was re-opened while hidden (double-click in Finder).
pub const TRAY_REOPEN: i32 = 4;
/// Install the release the update item names.
pub const TRAY_UPDATE: i32 = 5;
/// Check for Updates… (the menu bar menu and the app menu).
pub const TRAY_CHECK: i32 = 6;
/// Download from Link… (the quick download panel).
pub const TRAY_LINK: i32 = 7;

/// The app's menu handler: a tag in, nothing out.
pub type Handler = Arc<dyn Fn(i32) + Send + Sync>;

/// Set once by `init`.
static HANDLER: Mutex<Option<Handler>> = Mutex::new(None);

/// Set once, so a menu that was never connected to the app says so instead of
/// looking broken.
static WARNED: AtomicBool = AtomicBool::new(false);

fn lock() -> std::sync::MutexGuard<'static, Option<Handler>> {
    HANDLER.lock().unwrap_or_else(|e| e.into_inner())
}

/// Installs the menu's action handler and creates the status item. Call it once,
/// after the app has started.
pub fn init(handler: Handler) {
    *lock() = Some(handler);
    fiotransfer_sys::tray::set_tray_action(tray_action);
    fiotransfer_sys::tray::tray_init();
}

/// Called from tray_darwin.m when a menu item is chosen (through
/// `fiotransfer_sys::tray::goTrayAction`, which C links against).
fn tray_action(tag: i32) {
    let handler = lock().clone();
    match handler {
        Some(h) => h(tag),
        None => {
            if !WARNED.swap(true, Ordering::Relaxed) {
                eprintln!("FioTransfer: the menu bar item has no handler (tray::init was not called)");
            }
        }
    }
}

/// Updates the menu's status line, the text next to the icon and the update
/// item (hidden when `update` is empty, with Check for Updates… shown instead).
pub fn set(status: &str, title: &str, login: bool, update: &str) {
    fiotransfer_sys::tray::tray_set(status, title, login, update);
}

/// Shows running transfers in the menu (JSON from the app's tray jobs).
pub fn set_jobs(json: &str) {
    fiotransfer_sys::tray::tray_jobs(json);
}

/// Shows the Dock icon only while a window is on screen.
pub fn sync_dock() {
    fiotransfer_sys::tray::sync_dock();
}

/// Brings the app forward.
pub fn activate() {
    fiotransfer_sys::tray::activate();
}

/// While transfers run, tell macOS this is user-requested work: no App Nap
/// throttling and no idle sleep, even when the window is hidden.
pub fn set_transfers_active(on: bool) {
    fiotransfer_sys::tray::set_active(on);
}

/// The text next to the icon has no NULs and comes back unharmed.
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cstr_keeps_text_and_drops_nuls() {
        // The conversion every shim argument goes through (fiotransfer_sys).
        use fiotransfer_sys::cstr;
        assert_eq!(cstr("↑ 1.2 MB/s  ↓ 3.4 MB/s").to_str().expect("utf8"), "↑ 1.2 MB/s  ↓ 3.4 MB/s");
        assert_eq!(cstr("a\0b").to_str().expect("utf8"), "a b");
    }

    #[test]
    fn tags_match_tray_darwin_m() {
        // The tags in tray_darwin.m's fioAdd()/fioAppMenuAdd() calls.
        assert_eq!((TRAY_OPEN, TRAY_LOGIN, TRAY_QUIT, TRAY_REOPEN), (1, 2, 3, 4));
        assert_eq!((TRAY_UPDATE, TRAY_CHECK, TRAY_LINK), (5, 6, 7));
    }
}
