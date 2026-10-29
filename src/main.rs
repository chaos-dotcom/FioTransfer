
//! FioTransfer: fast uploads to and downloads from Frame.io.
//!
//! The same window, the same frontend, the same menu bar item and Finder
//! services as the Go app, with the transfer engine and the app itself in Rust.

use fiotransfer as fio;

fn main() {
    let hidden = fio::login::started_hidden();

    // Finder launches the app to serve a right-click request; the provider must
    // be installed before the app finishes starting.
    fio::services::init();

    let settings = fio::config::load();
    let app = fio::app::App::new(settings);

    let rt = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("FioTransfer: could not start: {}", e);
            std::process::exit(1);
        }
    };
    let _guard = rt.enter();

    // The app's own background work (history, manager, watcher, tray loop).
    {
        let app = app.clone();
        rt.spawn(async move { app.start().await });
    }

    fio::shot::maybe_run();

    let code = fio::gui::run(app.clone(), fio::gui::WindowOptions { start_hidden: hidden });
    app.shutdown();
    std::process::exit(code);
}
