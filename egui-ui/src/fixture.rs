//! The fixture the screenshot captures draw.
//!
//! One state, drawn two ways: `tests/snapshots.rs` renders it natively through
//! `egui_kittest`/wgpu, and `src/wasm.rs` draws the same state in a browser
//! (`?fixture=1`). A running app hands the shell the same thing, so this is also
//! what the two front ends are compared on.
//!
//! Nothing here does I/O or touches a screen: it builds a [`UiState`] and stops.

use crate::model::{
    FileView, HistoryRow, JobView, Settings, Snapshot, Toast, ToastKind, UiState, UpdateState, User,
    View, WatchRow,
};

fn job(
    id: &str,
    kind: &str,
    title: &str,
    status: &str,
    total: i64,
    done: i64,
) -> JobView {
    JobView {
        id: id.to_string(),
        kind: kind.to_string(),
        title: title.to_string(),
        status: status.to_string(),
        total,
        done,
        ..JobView::default()
    }
}

/// A queue with one upload in flight and one download finished.
pub fn fixture_state(view: View) -> UiState {
    let mut upload = job(
        "up-1",
        "upload",
        "A007_C012_sh010_v003.mov",
        "running",
        8_400_000_000,
        3_150_000_000,
    );
    upload.source = "/Users/editor/Renders/A007_C012_sh010_v003.mov".into();
    upload.dest = "Shows / Nightfall / Ep 04 / Renders".into();
    upload.phase = "Uploading 3 of 8 files".into();
    upload.speed = 92_400_000.0;
    upload.eta = 54.0;
    upload.files_total = 8;
    upload.files_done = 3;
    upload.files_active = 16;
    upload.started_at = 1_793_311_000_000;

    let mut download = job(
        "dn-1",
        "download",
        "Nightfall_Ep04_Dailies",
        "done",
        21_500_000_000,
        21_500_000_000,
    );
    download.source = "https://f.io/9f2c1a".into();
    download.dest = "/Users/editor/Downloads/Nightfall_Ep04_Dailies".into();
    download.files_total = 42;
    download.files_done = 42;
    download.elapsed = 754.0;
    download.avg_speed = 28_500_000.0;
    download.finished_at = 1_793_310_400_000;

    let files = vec![
        FileView {
            name: "A007_C012_sh010_v003.mov".into(),
            rel_path: "A007_C012_sh010_v003.mov".into(),
            size: 2_100_000_000,
            done: 2_100_000_000,
            status: "done".into(),
            note: "Verified".into(),
            ..FileView::default()
        },
        FileView {
            name: "A007_C012_sh010_v003.mxf".into(),
            rel_path: "A007_C012_sh010_v003.mxf".into(),
            size: 6_300_000_000,
            done: 1_050_000_000,
            status: "running".into(),
            retries: 1,
            ..FileView::default()
        },
    ];

    let history = vec![
        HistoryRow {
            title: "Nightfall_Ep04_Dailies".into(),
            kind: "download".into(),
            status: "done".into(),
            when: "Today, 09:41".into(),
            files: "42/42".into(),
            size: "21.5 GB".into(),
        },
        HistoryRow {
            title: "A006_C003_sh020_v002.mov".into(),
            kind: "upload".into(),
            status: "partial".into(),
            when: "Yesterday, 18:02".into(),
            files: "7/8".into(),
            size: "4.10 GB".into(),
        },
        HistoryRow {
            title: "Trailer_cutdown_v009.mov".into(),
            kind: "upload".into(),
            status: "failed".into(),
            when: "27 Oct, 11:15".into(),
            files: "0/1".into(),
            size: "820 MB".into(),
        },
    ];

    let watches = vec![
        WatchRow {
            local: "/Users/editor/Renders".into(),
            label: "Shows / Nightfall / Ep 04 / Renders".into(),
            direction: "upload".into(),
            enabled: true,
            pending: 3,
        },
        WatchRow {
            local: "/Users/editor/Downloads/Dailies".into(),
            label: "Shows / Nightfall / Ep 04 / Dailies".into(),
            direction: "download".into(),
            enabled: false,
            pending: 0,
        },
    ];

    UiState {
        view,
        logged_in: true,
        user: Some(User {
            name: "Dana Whitfield".into(),
            email: "dana@nightfall.tv".into(),
        }),
        settings: Settings {
            client_id: "9f2c1a4e".into(),
            redirect_port: 8765,
            streams: 16,
            segment_mb: 64,
            download_dir: "/Users/editor/Downloads/FioTransfer".into(),
            no_folder_logs: false,
            no_update_check: false,
            last_folder_label: "Shows / Nightfall / Ep 04 / Renders".into(),
        },
        snapshot: Snapshot {
            jobs: vec![upload, download],
            speed: 92_400_000.0,
            active_streams: 16,
            stream_limit: 16,
        },
        history,
        watches,
        toasts: vec![
            Toast {
                text: "Share link copied".into(),
                kind: ToastKind::Ok,
                seconds: 3.5,
            },
            Toast {
                text: "Upload failed: A006_C003_sh020_v002.mov — the connection dropped".into(),
                kind: ToastKind::Err,
                seconds: 3.5,
            },
        ],
        destination: "Shows / Nightfall / Ep 04 / Renders".into(),
        // The Settings screen's environment: a development build, no update
        // channel, and the log where the app would keep it.
        version: "dev".into(),
        open_at_login: true,
        log_dir: "/Users/dana/Library/Logs/FioTransfer".into(),
        home: "/Users/dana".into(),
        update: UpdateState {
            current: "dev".into(),
            blocker: "development build".into(),
            ..UpdateState::default()
        },
        expanded: Some("up-1".into()),
        files,
    }
}
