//! FioTransfer's egui front end.
//!
//! `cargo run -p fiotransfer-egui` opens the same four screens as the webview
//! front end, drawn with egui instead of HTML.
//!
//! The binary starts with an empty state (the same thing the webview shows
//! before `GetState` returns). `--snapshot <file>` seeds the Transfers list from
//! the app's own snapshot JSON (`{"jobs":[…],"speed":…,"active_streams":…,
//! "stream_limit":…}`), which is enough to look at a real queue without a
//! bridge:
//!
//! ```text
//! cargo run -p fiotransfer-egui -- --snapshot /tmp/snapshot.json
//! ```

use fiotransfer_egui::app::apply_style_once;
use fiotransfer_egui::model::{Snapshot, UiState};
use fiotransfer_egui::{FioApp, MIN_HEIGHT, MIN_WIDTH, WINDOW_HEIGHT, WINDOW_WIDTH};

fn main() -> eframe::Result<()> {
    let state = state_from_args(std::env::args().skip(1));
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([WINDOW_WIDTH, WINDOW_HEIGHT])
            .with_min_inner_size([MIN_WIDTH, MIN_HEIGHT])
            .with_title("FioTransfer"),
        ..Default::default()
    };
    eframe::run_native(
        "FioTransfer",
        options,
        Box::new(move |cc| {
            apply_style_once(&cc.egui_ctx);
            Ok(Box::new(FioApp::new(state)))
        }),
    )
}

/// The state the window opens with. Bad input is reported and ignored: this is
/// a front end, it never takes the window down over a missing file.
fn state_from_args(args: impl Iterator<Item = String>) -> UiState {
    let mut state = UiState::default();
    let args: Vec<String> = args.collect();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--snapshot" => {
                i += 1;
                match args.get(i) {
                    Some(path) => match read_snapshot(path) {
                        Ok(snapshot) => {
                            state.snapshot = snapshot;
                            state.logged_in = true;
                        }
                        Err(err) => eprintln!("fiotransfer-egui: {path}: {err}"),
                    },
                    None => eprintln!("fiotransfer-egui: --snapshot needs a file"),
                }
            }
            other => eprintln!("fiotransfer-egui: unknown argument {other}"),
        }
        i += 1;
    }
    state
}

fn read_snapshot(path: &str) -> Result<Snapshot, String> {
    let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
    serde_json::from_str(&text).map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_missing_snapshot_file_is_reported_not_fatal() {
        let state = state_from_args(
            ["--snapshot".to_string(), "/nonexistent/fio-snapshot.json".to_string()].into_iter(),
        );
        assert!(state.snapshot.jobs.is_empty());
        assert!(!state.logged_in);
    }

    #[test]
    fn a_snapshot_file_seeds_the_job_list() {
        let path = std::env::temp_dir().join(format!(
            "fiotransfer-egui-test-{}.json",
            std::process::id()
        ));
        std::fs::write(
            &path,
            r#"{"jobs":[{"id":"a","kind":"upload","title":"clip.mov","status":"running",
                "total":10,"done":5}],"speed":1.0,"active_streams":1,"stream_limit":16}"#,
        )
        .expect("write the fixture");
        let state =
            state_from_args(["--snapshot".to_string(), path.display().to_string()].into_iter());
        let _ = std::fs::remove_file(&path);
        assert_eq!(state.snapshot.jobs.len(), 1);
        assert!(state.logged_in);
        assert_eq!(state.snapshot.jobs[0].progress(), Some(0.5));
    }

    #[test]
    fn junk_in_a_snapshot_file_does_not_panic() {
        let path = std::env::temp_dir().join(format!(
            "fiotransfer-egui-junk-{}.json",
            std::process::id()
        ));
        std::fs::write(&path, "not json at all").expect("write the fixture");
        let state =
            state_from_args(["--snapshot".to_string(), path.display().to_string()].into_iter());
        let _ = std::fs::remove_file(&path);
        assert!(state.snapshot.jobs.is_empty());
    }
}
