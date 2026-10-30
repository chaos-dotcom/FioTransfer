//! The window: the app menu, the window chrome and the WebKit JS bridge.
//!
//! `mac/window.m` is the AppKit side, unchanged from the Go app. `build.rs`
//! compiles it into the app's binary; this module only declares it.

#[cfg(target_os = "macos")]
use std::ffi::c_char;
#[cfg(target_os = "macos")]
use crate::strings::cstr_or_empty;

#[cfg(target_os = "macos")]
extern "C" {
    fn fioMenuInit(cb: extern "C" fn(i32));
    fn fioWindowStyle();
    fn fioSnapshotWebView(path: *const c_char) -> i32;
    fn fioEvalJS(js: *const c_char);
    fn fioEvalJSFile(js: *const c_char, path: *const c_char) -> i32;
}

/// Builds the app menu; `cb` is called with the chosen item's tag.
#[cfg(target_os = "macos")]
pub fn menu_init(cb: extern "C" fn(i32)) {
    unsafe { fioMenuInit(cb) };
}

/// Builds the app menu; `cb` is called with the chosen item's tag.
#[cfg(not(target_os = "macos"))]
pub fn menu_init(_cb: extern "C" fn(i32)) {
    // No AppKit outside macOS: there is no menu to build.
}

/// The transparent title bar, the full-size content view and the Chrome-style
/// traffic light placement the Go app's window had.
#[cfg(target_os = "macos")]
pub fn window_style() {
    unsafe { fioWindowStyle() };
}

/// The transparent title bar, full-size content view and traffic lights.
#[cfg(not(target_os = "macos"))]
pub fn window_style() {}

/// The webview's own pixels, for the measuring harness. Must not be called on
/// the main thread (it waits for the main queue).
#[cfg(target_os = "macos")]
pub fn snapshot_webview(path: &str) -> bool {
    let c = cstr_or_empty(path);
    unsafe { fioSnapshotWebView(c.as_ptr()) == 1 }
}

/// The webview's own pixels as a PNG; false where there is no webview.
#[cfg(not(target_os = "macos"))]
pub fn snapshot_webview(_path: &str) -> bool {
    false
}

/// Runs `js` in the page.
#[cfg(target_os = "macos")]
pub fn eval_js(js: &str) {
    let c = cstr_or_empty(js);
    unsafe { fioEvalJS(c.as_ptr()) }
}

/// Runs `js` in the page.
#[cfg(not(target_os = "macos"))]
pub fn eval_js(_js: &str) {}

/// Runs `js` in the page and writes its answer to `path`; true when it did.
#[cfg(target_os = "macos")]
pub fn eval_js_to_file(js: &str, path: &str) -> bool {
    let j = cstr_or_empty(js);
    let p = cstr_or_empty(path);
    unsafe { fioEvalJSFile(j.as_ptr(), p.as_ptr()) == 1 }
}

/// Runs `js` in the page and writes its answer to `path`; false without a page.
#[cfg(not(target_os = "macos"))]
pub fn eval_js_to_file(_js: &str, _path: &str) -> bool {
    false
}
