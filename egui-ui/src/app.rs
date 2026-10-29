//! The shell: window layout, which screen shows, and the toasts.
//! WORKER E owns this file.

use crate::model::UiState;

pub struct FioApp {
    pub state: UiState,
}

impl FioApp {
    pub fn new(state: UiState) -> Self {
        FioApp { state }
    }
}

impl eframe::App for FioApp {
    fn update(&mut self, ctx: &egui::Context, _frame: &mut eframe::Frame) {
        // Placeholder shell: WORKER E replaces this with the real layout
        // (sidebar 240 px, main area, top bar) and the view switch.
        egui::CentralPanel::default().show(ctx, |ui| {
            ui.heading("FioTransfer");
            ui.label(format!("{:?} view", self.state.view));
        });
    }
}
