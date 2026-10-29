//! The design tokens, taken from the webview front end.
//!
//! Every value here comes from `frontend/dist/style.css` (`:root`); the egui
//! front end uses the same colours, radii, spacing and type sizes so a screen
//! reads as the same product. WORKER A fills this in from the stylesheet.

use egui::{Color32, CornerRadius, FontFamily, FontId};

// Colours (`--base`, `--panel`, … in the stylesheet).
pub const BASE: Color32 = Color32::from_rgb(0x00, 0x00, 0x00);
pub const PANEL: Color32 = Color32::from_rgb(0x12, 0x14, 0x1e);
pub const TEXT: Color32 = Color32::from_rgb(0xff, 0xff, 0xff);

/// Type sizes (`font-size` in the stylesheet).
pub fn body() -> FontId {
    FontId::new(13.0, FontFamily::Proportional)
}

/// The egui style that matches the stylesheet: dark, flat, small radii.
pub fn style() -> egui::Style {
    let mut style = egui::Style::default();
    style.visuals = egui::Visuals::dark();
    style.visuals.panel_fill = PANEL;
    style.visuals.window_fill = BASE;
    style.visuals.override_text_color = Some(TEXT);
    style.visuals.widgets.noninteractive.corner_radius = CornerRadius::same(6);
    style
}
