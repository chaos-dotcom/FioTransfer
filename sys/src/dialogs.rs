//! Native panels and alerts.
//!
//! `mac/dialogs.m` answers the Go app's Wails dialog and clipboard calls with
//! AppKit directly, and adds the front-window PNG the measurement harness
//! takes. The strings those shims return are `malloc`ed copies, so reading one
//! and giving its memory back both happen here.
//!
//! Threading: these are synchronous, and they block until the panel closes.
//! AppKit only wants panels on the main thread, so `dialogs.m` hops to the main
//! queue itself when it is called from anywhere else and waits there.

use std::ffi::c_char;
#[cfg(target_os = "macos")]
use std::ffi::{c_int, c_void, CStr};
#[cfg(target_os = "macos")]
use crate::strings::cstr;

#[cfg(target_os = "macos")]
extern "C" {
    fn fioOpenFiles(title: *const c_char) -> *mut c_char;
    fn fioOpenDir(title: *const c_char, def: *const c_char, can_create: c_int) -> *mut c_char;
    fn fioSaveFile(title: *const c_char, def_name: *const c_char) -> *mut c_char;
    fn fioQuestion(title: *const c_char, message: *const c_char, ok: *const c_char, cancel: *const c_char) -> c_int;
    fn fioAlert(title: *const c_char, message: *const c_char);
    fn fioClipboardGet() -> *mut c_char;
    fn fioClipboardSet(t: *const c_char);
    fn fioWindowPNG(path: *const c_char) -> c_int;
    fn fioFree(p: *mut c_void);
}

/// Reads a string the C side handed over and gives its memory back.
///
/// Only called on macOS: it is the other half of the dialog shims that copy
/// their answers out with `fioCopy()` (`mac/dialogs.m`).
#[cfg(target_os = "macos")]
pub fn take_string(p: *mut c_char) -> String {
    if p.is_null() {
        return String::new();
    }
    let s = unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned();
    unsafe { fioFree(p.cast::<c_void>()) };
    s
}

/// Reads a string the C side handed over and gives its memory back; nothing
/// allocates one outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn take_string(_p: *mut c_char) -> String {
    String::new()
}

/// Asks for files to upload, as the JSON array `fioOpenFiles` answers; empty
/// when cancelled.
#[cfg(target_os = "macos")]
pub fn open_files(title: &str) -> String {
    let t = cstr(title);
    take_string(unsafe { fioOpenFiles(t.as_ptr()) })
}

/// Asks for files to upload; empty outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn open_files(_title: &str) -> String {
    String::new()
}

/// Asks for one folder; `default_dir` opens the panel there, `can_create` lets
/// it make a new folder. Empty when cancelled.
#[cfg(target_os = "macos")]
pub fn open_dir(title: &str, default_dir: &str, can_create: bool) -> String {
    let (t, d) = (cstr(title), cstr(default_dir));
    take_string(unsafe { fioOpenDir(t.as_ptr(), d.as_ptr(), i32::from(can_create)) })
}

/// Asks for one folder; empty outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn open_dir(_title: &str, _default_dir: &str, _can_create: bool) -> String {
    String::new()
}

/// Asks where to write a file; empty when cancelled.
#[cfg(target_os = "macos")]
pub fn save_file(title: &str, default_name: &str) -> String {
    let (t, n) = (cstr(title), cstr(default_name));
    take_string(unsafe { fioSaveFile(t.as_ptr(), n.as_ptr()) })
}

/// Asks where to write a file; empty outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn save_file(_title: &str, _default_name: &str) -> String {
    String::new()
}

/// Asks a question with two answers: true when `ok` was pressed. The buttons
/// are labelled exactly `ok` and `cancel`, and `cancel` is the default button.
#[cfg(target_os = "macos")]
pub fn question(title: &str, message: &str, ok: &str, cancel: &str) -> bool {
    let (t, m, o, c) = (cstr(title), cstr(message), cstr(ok), cstr(cancel));
    unsafe { fioQuestion(t.as_ptr(), m.as_ptr(), o.as_ptr(), c.as_ptr()) != 0 }
}

/// Asks a question; never answered outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn question(_title: &str, _message: &str, _ok: &str, _cancel: &str) -> bool {
    false
}

/// Shows an error, with an OK button.
#[cfg(target_os = "macos")]
pub fn alert(title: &str, message: &str) {
    let (t, m) = (cstr(title), cstr(message));
    unsafe { fioAlert(t.as_ptr(), m.as_ptr()) };
}

/// Shows an error; nowhere to show it outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn alert(_title: &str, _message: &str) {}

/// The clipboard as plain text; empty when it holds no text.
#[cfg(target_os = "macos")]
pub fn clipboard_get() -> String {
    take_string(unsafe { fioClipboardGet() })
}

/// The clipboard as plain text; empty outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn clipboard_get() -> String {
    String::new()
}

/// Replaces the clipboard with plain text.
#[cfg(target_os = "macos")]
pub fn clipboard_set(t: &str) {
    let c = cstr(t);
    unsafe { fioClipboardSet(c.as_ptr()) };
}

/// Replaces the clipboard with plain text; nothing to replace outside macOS.
#[cfg(not(target_os = "macos"))]
pub fn clipboard_set(_t: &str) {}

/// Writes the front window as a PNG and says whether it did.
#[cfg(target_os = "macos")]
pub fn window_png(path: &str) -> bool {
    let p = cstr(path);
    unsafe { fioWindowPNG(p.as_ptr()) != 0 }
}

/// Writes the front window as a PNG; false where there is no window.
#[cfg(not(target_os = "macos"))]
pub fn window_png(_path: &str) -> bool {
    false
}
