//! The sidebar: brand, navigation, throughput, account. WORKER B owns this file.

use crate::model::UiState;

pub fn show(ui: &mut egui::Ui, state: &mut UiState) {
    let _ = state;
    ui.label("FioTransfer");
}
