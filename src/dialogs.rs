//! Native panels and alerts.
//!
//! Wails answered the Go app's dialog and clipboard calls; `mac/dialogs.m`
//! answers them here with AppKit directly, and adds the front-window PNG the
//! measurement harness takes. Same shapes as `runtime.OpenFileDialog`,
//! `SaveFileDialog`, `MessageDialog`, `ClipboardGetText`/`ClipboardSetText`.
//!
//! Threading: these are synchronous, and they block until the panel closes.
//! AppKit only wants panels on the main thread, so `dialogs.m` hops to the main
//! queue itself when it is called from anywhere else and waits there — the Rust
//! side stays a plain C call and may be used from a tokio task. The one case it
//! can't serve is a dialog asked for before the app's event loop is running:
//! that is answered as cancelled rather than waiting for a main queue that
//! never drains.

#![allow(unsafe_code)] // calls the NSOpenPanel/NSAlert shims in mac/dialogs.m

use std::ffi::{c_char, c_int, c_void, CStr, CString};

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

fn cstr(s: &str) -> CString {
    match CString::new(s) {
        Ok(c) => c,
        Err(_) => {
            let cleaned: String = s.chars().map(|c| if c == '\0' { ' ' } else { c }).collect();
            CString::new(cleaned).unwrap_or_else(|_| CString::new("").expect("empty"))
        }
    }
}

/// Reads a string the C side handed over and gives its memory back.
fn take_string(p: *mut c_char) -> String {
    if p.is_null() {
        return String::new();
    }
    let s = unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned();
    unsafe { fioFree(p.cast::<c_void>()) };
    s
}

/// The paths out of `fioOpenFiles`' JSON answer.
fn paths_from_json(json: &str) -> Vec<String> {
    match serde_json::from_str::<Vec<String>>(json) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("FioTransfer: could not read the chosen paths: {}", e);
            Vec::new()
        }
    }
}

/// Asks for files to upload; empty when cancelled.
pub fn open_files(title: &str) -> Vec<String> {
    let t = cstr(title);
    paths_from_json(&take_string(unsafe { fioOpenFiles(t.as_ptr()) }))
}

/// Asks for one folder; `None` when cancelled. `default_dir` opens the panel
/// there, and `can_create` lets the panel make a new folder.
pub fn open_dir(title: &str, default_dir: &str, can_create: bool) -> Option<String> {
    let (t, d) = (cstr(title), cstr(default_dir));
    let path = take_string(unsafe { fioOpenDir(t.as_ptr(), d.as_ptr(), i32::from(can_create)) });
    if path.is_empty() {
        None
    } else {
        Some(path)
    }
}

/// Asks where to write a file; `None` when cancelled.
pub fn save_file(title: &str, default_name: &str) -> Option<String> {
    let (t, n) = (cstr(title), cstr(default_name));
    let path = take_string(unsafe { fioSaveFile(t.as_ptr(), n.as_ptr()) });
    if path.is_empty() {
        None
    } else {
        Some(path)
    }
}

/// Asks a question with two answers: true when `ok` was pressed. Buttons are
/// labelled exactly `ok` and `cancel`, and `cancel` is the default button — the
/// Go app's questions all had Cancel as their default, so Return means Cancel
/// and `ok` is clicked (or reached with Tab). Escape does nothing, as it did
/// through Wails.
pub fn question(title: &str, message: &str, ok: &str, cancel: &str) -> bool {
    let (t, m, o, c) = (cstr(title), cstr(message), cstr(ok), cstr(cancel));
    unsafe { fioQuestion(t.as_ptr(), m.as_ptr(), o.as_ptr(), c.as_ptr()) != 0 }
}

/// Shows an error, with an OK button.
pub fn error(title: &str, message: &str) {
    let (t, m) = (cstr(title), cstr(message));
    unsafe { fioAlert(t.as_ptr(), m.as_ptr()) };
}

/// The clipboard as plain text; empty when it holds no text.
pub fn clipboard_text() -> String {
    take_string(unsafe { fioClipboardGet() })
}

/// Replaces the clipboard with plain text.
pub fn clipboard_set_text(t: &str) {
    let c = cstr(t);
    unsafe { fioClipboardSet(c.as_ptr()) };
}

/// Writes the front window as a PNG and says whether it did. The measurement
/// harness reads the file to check the layout it just rendered.
pub fn window_png(path: &str) -> bool {
    let p = cstr(path);
    unsafe { fioWindowPNG(p.as_ptr()) != 0 }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_cancelled_panel_is_an_empty_answer() {
        // What dialogs.m sends back for a cancelled file panel.
        assert!(paths_from_json("[]").is_empty());
    }

    #[test]
    fn paths_come_back_in_order() {
        let json = "[\"/Users/a/One.mov\",\"/Users/a/two words.mov\"]";
        assert_eq!(paths_from_json(json), vec!["/Users/a/One.mov", "/Users/a/two words.mov"]);
    }

    #[test]
    fn a_bad_answer_is_no_files_not_a_panic() {
        assert!(paths_from_json("not json").is_empty());
    }
}
