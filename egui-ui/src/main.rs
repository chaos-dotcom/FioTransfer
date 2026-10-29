//! FioTransfer's egui front end.
//!
//! `cargo run -p fiotransfer-egui` opens the same four screens as the webview
//! front end, drawn with egui instead of HTML.

use fiotransfer_egui::model::UiState;
use fiotransfer_egui::{FioApp, MIN_HEIGHT, MIN_WIDTH, WINDOW_HEIGHT, WINDOW_WIDTH};

fn main() -> eframe::Result<()> {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([WINDOW_WIDTH, WINDOW_HEIGHT])
            .with_min_inner_size([MIN_WIDTH, MIN_HEIGHT])
            .with_title("FioTransfer"),
        ..Default::default()
    };
    eframe::run_native("FioTransfer", options, Box::new(|cc| {
        cc.egui_ctx.set_style(fiotransfer_egui::theme::style());
        Ok(Box::new(FioApp::new(UiState::default())))
    }))
}
