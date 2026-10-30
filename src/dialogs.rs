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
//!
//! The shims themselves, and the `CString`/`CStr`/`fioFree` work around them,
//! are `fiotransfer_sys::dialogs`'s; this module is what the app asks for.

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
    paths_from_json(&fiotransfer_sys::dialogs::open_files(title))
}

/// Asks for one folder; `None` when cancelled. `default_dir` opens the panel
/// there, and `can_create` lets the panel make a new folder.
pub fn open_dir(title: &str, default_dir: &str, can_create: bool) -> Option<String> {
    let path = fiotransfer_sys::dialogs::open_dir(title, default_dir, can_create);
    if path.is_empty() {
        None
    } else {
        Some(path)
    }
}

/// Asks where to write a file; `None` when cancelled.
pub fn save_file(title: &str, default_name: &str) -> Option<String> {
    let path = fiotransfer_sys::dialogs::save_file(title, default_name);
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
    fiotransfer_sys::dialogs::question(title, message, ok, cancel)
}

/// Shows an error, with an OK button.
pub fn error(title: &str, message: &str) {
    fiotransfer_sys::dialogs::alert(title, message);
}

/// The clipboard as plain text; empty when it holds no text.
pub fn clipboard_text() -> String {
    fiotransfer_sys::dialogs::clipboard_get()
}

/// Replaces the clipboard with plain text.
pub fn clipboard_set_text(t: &str) {
    fiotransfer_sys::dialogs::clipboard_set(t);
}

/// Writes the front window as a PNG and says whether it did. The measurement
/// harness reads the file to check the layout it just rendered.
pub fn window_png(path: &str) -> bool {
    fiotransfer_sys::dialogs::window_png(path)
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
