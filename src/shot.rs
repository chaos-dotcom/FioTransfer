
//! The screenshot harness: with FIO_SHOT (or FIO_SHOT_FILE pointing at a spec
//! file) the app screenshots its own webview for each step and quits. It is the
//! same harness the Go build has, so the two can be compared pixel for pixel.

use std::time::Duration;

use crate::gui;

/// "dir" or "dir|js;;js": the shots land in dir.
pub fn maybe_run() {
    let mut spec = std::env::var("FIO_SHOT").unwrap_or_default();
    if let Ok(f) = std::env::var("FIO_SHOT_FILE") {
        if let Ok(s) = std::fs::read_to_string(f) {
            spec = s.trim().to_string();
        }
    }
    if spec.is_empty() {
        return;
    }
    std::thread::Builder::new()
        .name("fio-shots".into())
        .spawn(move || {
            std::thread::sleep(Duration::from_secs(9)); // let the webview paint
            let mut parts = spec.splitn(2, '|');
            let dir = parts.next().unwrap_or("/tmp").to_string();
            let steps: Vec<String> = match parts.next() {
                Some(js) => js.split(";;").map(|s| s.to_string()).collect(),
                None => vec![String::new()],
            };
            for js in steps {
                if !js.is_empty() {
                    gui::eval_js(&js);
                    std::thread::sleep(Duration::from_millis(1200));
                }
                let view = view_name(&js);
                let state = format!("{}/state-{}.txt", dir, view);
                gui::eval_js_to_file(
                    r#"JSON.stringify({nav: document.querySelectorAll('.nav-item').length, view: (document.querySelector('.nav-item.active')||{}).textContent, cls: document.getElementById('app') ? document.getElementById('app').className : 'no-app', body: document.body.innerText.length, toasts: document.getElementById('toasts') ? document.getElementById('toasts').innerText : '', html: document.body.innerHTML.length})"#,
                    &state,
                );
                let mut probe = std::env::var("FIO_PROBE").unwrap_or_default();
                if let Ok(f) = std::env::var("FIO_PROBE_FILE") {
                    if let Ok(t) = std::fs::read_to_string(f) {
                        probe = t.trim().to_string();
                    }
                }
                if !probe.is_empty() {
                    gui::eval_js_to_file(&probe, &format!("{}/probe-{}.txt", dir, view));
                }
                let name = format!("{}/{}.png", dir, view);
                let ok = gui::snapshot_webview(&name);
                println!("shot {} {}", name, ok as i32);
                std::thread::sleep(Duration::from_millis(600));
            }
            std::process::exit(0);
        })
        .ok();
}

fn view_name(js: &str) -> String {
    for v in ["transfers", "watch", "history", "settings"] {
        if js.contains(&format!("\"{}\"", v)) {
            return v.to_string();
        }
    }
    "transfers".to_string()
}
