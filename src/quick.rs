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
//!
//! The C calls and the `goQuick*` entry points the panel calls back are
//! `fiotransfer_sys::quick`'s; this module registers what answers them.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

/// A link preview: the link is fine.
pub const QUICK_OK: i32 = 1;
/// The link needs a (different) passphrase.
pub const QUICK_PASS: i32 = 2;
/// The link can't be downloaded: `info`'s name is the message.
pub const QUICK_ERROR: i32 = 3;

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
    install();
}

/// Tells the C side which functions answer the panel's buttons. Idempotent, so
/// `show` installs them as well in case it runs before `start`.
fn install() {
    fiotransfer_sys::quick::set_quick_hooks(fiotransfer_sys::quick::QuickHooks {
        inspect: inspect_from_c,
        dest: dest_from_c,
        start: start_from_c,
    });
}

/// Opens the panel, or brings it forward with what it holds. A panel that was
/// closed starts over with `link` (may be empty) and `dir`.
pub fn show(link: &str, dir: &str) {
    install();
    fiotransfer_sys::quick::quick_show(link, dir);
}

/// Answers `inspect(seq)`: the link's name (or a message) and its detail, with
/// `warn` in the notice color.
pub fn info(seq: i32, state: i32, name: &str, detail: &str, warn: &str) {
    fiotransfer_sys::quick::quick_info(seq, state, name, detail, warn);
}

/// Closes the panel, or shows why the download didn't start.
pub fn started(err: &str) {
    fiotransfer_sys::quick::quick_started(err);
}

/// The panel's link needed a preview (called from `quick_darwin.m` through
/// `fiotransfer_sys::quick::goQuickInspect`).
fn inspect_from_c(seq: i32, link: &str, pass: &str) {
    if let Some(h) = host() {
        h.inspect(seq, link.to_string(), pass.to_string());
    }
}

/// The panel's folder button changed the download folder.
fn dest_from_c(dir: &str) {
    if let Some(h) = host() {
        h.dest(dir.to_string());
    }
}

/// The panel's Download button was pressed.
fn start_from_c(link: &str, pass: &str, dir: &str, subfolder: bool) {
    if let Some(h) = host() {
        h.start(link.to_string(), pass.to_string(), dir.to_string(), subfolder);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn from_c_reads_utf8_and_null() {
        // What the panel's strings go through (fiotransfer_sys::from_c_str).
        use std::ffi::CString;
        let c = CString::new("Frame.io link").expect("no nul");
        assert_eq!(fiotransfer_sys::from_c_str(c.as_ptr()), "Frame.io link");
        assert_eq!(fiotransfer_sys::from_c_str(std::ptr::null()), "");
    }

    #[test]
    fn states_match_quick_darwin_m() {
        // qShowInfo() treats 1 as fine, 2 as a passphrase prompt, 3 as an error.
        assert_eq!((QUICK_OK, QUICK_PASS, QUICK_ERROR), (1, 2, 3));
    }
}
