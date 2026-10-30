//! The download panel.
//!
//! "Download from Link…" in the menu bar opens a small panel
//! (`mac/quick_darwin.m`, unchanged from the Go app) that takes a link and a
//! destination and starts a download without the main window. The panel itself
//! previews links, so it calls back here for the preview and for the download,
//! exactly as `quick_darwin.go` did.
//!
//! Threading: every C entry point in `quick_darwin.m` hops to the main queue
//! itself (`dispatch_async(dispatch_get_main_queue(), ^{ … })`), so all the
//! functions here may be called from any thread, tokio workers included. The
//! `goQuick*` callbacks arrive on the main thread; the Go app ran them on
//! goroutines, here they run inline, so a host must not block — it should post
//! the work into the app's runtime and return, which keeps the panel
//! responsive while a link is inspected.

#![allow(unsafe_code)] // calls the download panel shims in mac/quick_darwin.m

use std::ffi::{c_char, c_int, CStr, CString};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

/// A link preview: the link is fine.
pub const QUICK_OK: i32 = 1;
/// The link needs a (different) passphrase.
pub const QUICK_PASS: i32 = 2;
/// The link can't be downloaded: `info`'s name is the message.
pub const QUICK_ERROR: i32 = 3;

extern "C" {
    fn fioQuickShow(link: *const c_char, dir: *const c_char);
    fn fioQuickInfo(seq: c_int, state: c_int, name: *const c_char, detail: *const c_char, warn: *const c_char);
    fn fioQuickStarted(err: *const c_char);
}

/// What the panel asks the app to do.
///
/// The methods take `self: Arc<Self>` because inspecting a link waits on the
/// network: the app's implementation clones the `Arc` into its runtime and
/// answers later with `info(seq, …)`. They are called on the main thread, so
/// they must hand the work off and return.
pub trait QuickHost: Send + Sync + 'static {
    /// Preview a link for the panel; answer with `info(seq, …)`.
    fn inspect(self: Arc<Self>, seq: i32, link: String, pass: String);
    /// The folder chosen in the panel becomes the download folder.
    fn dest(self: Arc<Self>, dir: String);
    /// Download the link into `dir`.
    fn start(self: Arc<Self>, link: String, pass: String, dir: String, subfolder: bool);
}

static HOST: Mutex<Option<Arc<dyn QuickHost>>> = Mutex::new(None);

/// Set once, so a panel that was never connected to the app says so instead of
/// looking broken.
static WARNED: AtomicBool = AtomicBool::new(false);

fn cstr(s: &str) -> CString {
    match CString::new(s) {
        Ok(c) => c,
        Err(_) => {
            let cleaned: String = s.chars().map(|c| if c == '\0' { ' ' } else { c }).collect();
            CString::new(cleaned).unwrap_or_else(|_| CString::new("").expect("empty"))
        }
    }
}

/// A string the panel passed in; NULL reads as empty.
fn from_c(p: *const c_char) -> String {
    if p.is_null() {
        return String::new();
    }
    unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned()
}

fn host() -> Option<Arc<dyn QuickHost>> {
    let host = HOST.lock().unwrap_or_else(|e| e.into_inner()).clone();
    if host.is_none() && !WARNED.swap(true, Ordering::Relaxed) {
        eprintln!("FioTransfer: the download panel has no host (quick::start was not called)");
    }
    host
}

/// Hands the panel's actions to the app. Call it once, after the app has
/// started; without a host the panel still opens and previews nothing.
pub fn start(host: Arc<dyn QuickHost>) {
    *HOST.lock().unwrap_or_else(|e| e.into_inner()) = Some(host);
}

/// Opens the panel, or brings it forward with what it holds. A panel that was
/// closed starts over with `link` (may be empty) and `dir`.
pub fn show(link: &str, dir: &str) {
    let (cl, cd) = (cstr(link), cstr(dir));
    unsafe { fioQuickShow(cl.as_ptr(), cd.as_ptr()) };
}

/// Answers `inspect(seq)`: the link's name (or a message) and its detail, with
/// `warn` in the notice color.
pub fn info(seq: i32, state: i32, name: &str, detail: &str, warn: &str) {
    let (cn, cd, cw) = (cstr(name), cstr(detail), cstr(warn));
    unsafe { fioQuickInfo(seq, state, cn.as_ptr(), cd.as_ptr(), cw.as_ptr()) };
}

/// Closes the panel, or shows why the download didn't start.
pub fn started(err: &str) {
    let c = cstr(err);
    unsafe { fioQuickStarted(c.as_ptr()) };
}

/// The panel's link needed a preview.
#[no_mangle]
#[allow(non_snake_case)]
pub extern "C" fn goQuickInspect(seq: c_int, link: *const c_char, pass: *const c_char) {
    if let Some(h) = host() {
        h.inspect(seq, from_c(link), from_c(pass));
    }
}

/// The panel's folder button changed the download folder.
#[no_mangle]
#[allow(non_snake_case)]
pub extern "C" fn goQuickDest(dir: *const c_char) {
    if let Some(h) = host() {
        h.dest(from_c(dir));
    }
}

/// The panel's Download button was pressed.
#[no_mangle]
#[allow(non_snake_case)]
pub extern "C" fn goQuickStart(link: *const c_char, pass: *const c_char, dir: *const c_char, subfolder: c_int) {
    if let Some(h) = host() {
        h.start(from_c(link), from_c(pass), from_c(dir), subfolder != 0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn from_c_reads_utf8_and_null() {
        let c = CString::new("Frame.io link").expect("no nul");
        assert_eq!(from_c(c.as_ptr()), "Frame.io link");
        assert_eq!(from_c(std::ptr::null()), "");
    }

    #[test]
    fn states_match_quick_darwin_m() {
        // qShowInfo() treats 1 as fine, 2 as a passphrase prompt, 3 as an error.
        assert_eq!((QUICK_OK, QUICK_PASS, QUICK_ERROR), (1, 2, 3));
    }
}
