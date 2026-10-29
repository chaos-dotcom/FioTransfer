//! FioTransfer's front end, rebuilt with egui.
//!
//! The webview front end (`frontend/dist`) and this one are two renderers over
//! the same state: `model.rs` holds what a screen needs to draw, and every view
//! reads it. Nothing here talks to Frame.io; it is given a [`model::UiState`].

pub mod app;
pub mod model;
pub mod theme;
pub mod views;

pub use app::FioApp;

/// The window the webview front end gets, so both open the same size.
pub const WINDOW_WIDTH: f32 = 1180.0;
pub const WINDOW_HEIGHT: f32 = 780.0;
pub const MIN_WIDTH: f32 = 860.0;
pub const MIN_HEIGHT: f32 = 560.0;
