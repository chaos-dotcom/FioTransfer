
//! FioTransfer — fast uploads to and downloads from Frame.io.
//!
//! A Rust port of the Go/Wails app of the same name: the same Frame.io V4 API,
//! the same transfer engine behaviour, the same frontend assets rendered in the
//! same WebKit view, and the same AppKit menu bar item, download panel and
//! Finder services.

pub mod api;
pub mod auth;
pub mod config;
pub mod limiter;
pub mod localcert;
pub mod netx;
pub mod secret;

pub mod history;
pub mod share;
pub mod update;
pub mod xfer;

pub mod app;
pub mod dialogs;
pub mod gui;
pub mod login;
pub mod notify;
pub mod quick;
pub mod services;
pub mod shot;
pub mod tray;

pub use app::App;

/// The version this build reports: `Cargo.toml`'s, unless a build overrides it
/// (`FIO_VERSION=0.0.0-abc1234` for a development build off a commit).
pub const VERSION: &str = match option_env!("FIO_VERSION") {
    Some(v) => v,
    None => env!("CARGO_PKG_VERSION"),
};
