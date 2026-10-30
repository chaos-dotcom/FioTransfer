//! The menu bar item.
//!
//! `mac/tray_darwin.m` is the AppKit side, unchanged from the Go app: it makes
//! the status item, the menu, the transfer rows drawn from JSON
//! (`set_jobs`) and the app menu's "Check for Updates…" item. This module is
//! its Go counterpart, `tray_darwin.go`: it declares the C functions that file
//! provides, exports `goTrayAction` back to it and keeps the handler the app
//! installs with `init`.
//!
//! Threading: every C entry point in `tray_darwin.m` hops to the main queue
//! itself (`dispatch_async(dispatch_get_main_queue(), ^{ … })`), so all the
//! functions here may be called from any thread, tokio workers included.
//! `goTrayAction` arrives on the main thread; the Go app ran the handler on a
//! goroutine, here it runs inline on the main thread, so a handler must not
//! block — the app's handler posts a menu event into the window's event loop
//! and returns.

#![allow(unsafe_code)] // calls the menu bar shims in mac/tray_darwin.m

use std::ffi::{c_char, c_int, CString};
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

extern "C" {
    fn fioTrayInit();
    fn fioTraySet(status: *const c_char, title: *const c_char, login: c_int, update: *const c_char);
    fn fioTrayJobs(json: *const c_char);
    fn fioSyncDock();
    fn fioActivate();
    fn fioSetActive(on: c_int);
}

/// The app's menu handler: a tag in, nothing out.
pub type Handler = Arc<dyn Fn(i32) + Send + Sync>;

/// Set once by `init`.
static HANDLER: Mutex<Option<Handler>> = Mutex::new(None);

/// Set once, so a menu that was never connected to the app says so instead of
/// looking broken.
static WARNED: AtomicBool = AtomicBool::new(false);

/// A C string for one of the arguments. C can't take an interior NUL, so those
/// become spaces instead of cutting the string short.
fn cstr(s: &str) -> CString {
    match CString::new(s) {
        Ok(c) => c,
        Err(_) => {
            let cleaned: String = s.chars().map(|c| if c == '\0' { ' ' } else { c }).collect();
            CString::new(cleaned).unwrap_or_else(|_| CString::new("").expect("empty"))
        }
    }
}

fn lock() -> std::sync::MutexGuard<'static, Option<Handler>> {
    HANDLER.lock().unwrap_or_else(|e| e.into_inner())
}

/// Installs the menu's action handler and creates the status item. Call it once,
/// after the app has started.
pub fn init(handler: Handler) {
    *lock() = Some(handler);
    unsafe { fioTrayInit() };
}

/// Called from tray_darwin.m when a menu item is chosen.
#[no_mangle]
#[allow(non_snake_case)]
pub extern "C" fn goTrayAction(tag: i32) {
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
    let (cs, ct, cu) = (cstr(status), cstr(title), cstr(update));
    unsafe { fioTraySet(cs.as_ptr(), ct.as_ptr(), i32::from(login), cu.as_ptr()) };
}

/// Shows running transfers in the menu (JSON from the app's tray jobs).
pub fn set_jobs(json: &str) {
    let c = cstr(json);
    unsafe { fioTrayJobs(c.as_ptr()) };
}

/// Shows the Dock icon only while a window is on screen.
pub fn sync_dock() {
    unsafe { fioSyncDock() };
}

/// Brings the app forward.
pub fn activate() {
    unsafe { fioActivate() };
}

/// While transfers run, tell macOS this is user-requested work: no App Nap
/// throttling and no idle sleep, even when the window is hidden.
pub fn set_transfers_active(on: bool) {
    unsafe { fioSetActive(i32::from(on)) };
}

/// The text next to the icon has no NULs and comes back unharmed.
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cstr_keeps_text_and_drops_nuls() {
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
