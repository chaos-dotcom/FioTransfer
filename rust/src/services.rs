//! Finder's right-click menu.
//!
//! "Upload with FioTransfer" and "Create Watch Folder in FioTransfer"
//! (NSServices in `build/darwin/Info.plist`, served by
//! `mac/services_darwin.m`, unchanged from the Go app). macOS launches the app
//! to serve one if it isn't running, so requests wait here until the app has
//! started, and the frontend takes them once it is ready
//! (`TakeServiceRequests`).
//!
//! Threading: `goService` arrives on the main thread from the AppKit service
//! provider, and `request` runs the host's callbacks inline on whichever thread
//! called it — the Go app used a goroutine for this, here a host must not block
//! (it posts a window show and an event into the app's loop and returns).

use std::ffi::{c_char, c_int, CStr};
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};

/// One right-click request, as the frontend receives it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ServiceRequest {
    /// upload | watch
    pub kind: String,
    pub paths: Vec<String>,
}

/// What a started app does with a request: show its window and tell the
/// frontend to look at `take`.
pub trait ServiceHost: Send + Sync + 'static {
    fn show_window(&self);
    fn emit_service(&self);
}

extern "C" {
    fn fioServicesInit();
}

#[derive(Default)]
struct State {
    pending: Vec<ServiceRequest>,
    /// set once the app has started
    host: Option<Arc<dyn ServiceHost>>,
}

static STATE: Mutex<State> = Mutex::new(State { pending: Vec::new(), host: None });

fn lock() -> std::sync::MutexGuard<'static, State> {
    STATE.lock().unwrap_or_else(|e| e.into_inner())
}

/// Queues a Finder request, then tells the app about it if it has started.
/// A watch request keeps only the paths that are folders, and is dropped when
/// none are: only folders can be watched.
pub fn request(kind: &str, paths: Vec<String>) {
    let mut r = ServiceRequest { kind: kind.to_string(), paths };
    if r.kind == "watch" {
        // only folders can be watched
        let dirs: Vec<String> = r
            .paths
            .into_iter()
            .filter(|p| std::fs::metadata(p).map(|st| st.is_dir()).unwrap_or(false))
            .collect();
        if dirs.is_empty() {
            return;
        }
        r.paths = dirs;
    }
    let host = {
        let mut st = lock();
        st.pending.push(r);
        st.host.clone()
    };
    if let Some(h) = host {
        serve(&h);
    }
}

/// Hands the app over, so requests that came in while it was starting are
/// served.
pub fn start(app: Arc<dyn ServiceHost>) {
    let waiting = {
        let mut st = lock();
        st.host = Some(app.clone());
        st.pending.len()
    };
    if waiting > 0 {
        serve(&app);
    }
}

fn serve(host: &Arc<dyn ServiceHost>) {
    host.show_window();
    host.emit_service();
}

/// Returns the right-click requests not handled yet (the app's
/// `TakeServiceRequests`). An empty list, never a missing one.
pub fn take() -> Vec<ServiceRequest> {
    std::mem::take(&mut lock().pending)
}

/// Installs the Finder services provider; call it before the event loop starts.
pub fn init() {
    unsafe { fioServicesInit() };
}

/// A string the service passed in; NULL reads as empty.
fn from_c(p: *const c_char) -> String {
    if p.is_null() {
        return String::new();
    }
    unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned()
}

/// Called from services_darwin.m with the pasteboard's paths as a JSON array.
/// `kind` is 1 for the watch-folder service and 0 for upload.
#[no_mangle]
#[allow(non_snake_case)]
pub extern "C" fn goService(kind: c_int, paths: *const c_char) {
    let ps: Vec<String> = match serde_json::from_str(&from_c(paths)) {
        Ok(v) => v,
        Err(_) => return,
    };
    if ps.is_empty() {
        return;
    }
    let k = if kind == 1 { "watch" } else { "upload" };
    request(k, ps);
}

#[cfg(test)]
mod tests {
    use super::*;

    // The queue is process-wide, so one test covers it and puts it back.
    #[test]
    fn requests_are_validated_and_taken_once() {
        let had = take();
        request("upload", vec!["/tmp/a.mov".into(), "/tmp/b.mov".into()]);
        let got = take();
        assert_eq!(got.len(), 1, "one request queued");
        assert_eq!(got[0].kind, "upload");
        assert_eq!(got[0].paths, vec!["/tmp/a.mov", "/tmp/b.mov"]);
        assert!(take().is_empty(), "taken requests are gone");

        let dir = std::env::temp_dir().join("fio-services-test");
        std::fs::create_dir_all(&dir).expect("temp dir");
        let file = dir.join("not-a-folder.txt");
        std::fs::write(&file, b"x").expect("temp file");
        // A file and a path that isn't there are both dropped; with nothing
        // left there is no request at all.
        request("watch", vec![file.display().to_string(), "/no/such/path".into()]);
        assert!(take().is_empty(), "files and missing paths are not watch folders");
        request("watch", vec![file.display().to_string(), dir.display().to_string()]);
        let got = take();
        assert_eq!(got.len(), 1, "the folder survives");
        assert_eq!(got[0].paths, vec![dir.display().to_string()]);
        let _ = std::fs::remove_dir_all(&dir);

        for r in had {
            request(&r.kind, r.paths);
        }
    }

    #[test]
    fn request_json_matches_the_frontend() {
        // TakeServiceRequests is read by app.js as {kind, paths}.
        let r = ServiceRequest { kind: "watch".into(), paths: vec!["/tmp".into()] };
        assert_eq!(
            serde_json::to_string(&r).expect("json"),
            r#"{"kind":"watch","paths":["/tmp"]}"#
        );
    }
}
