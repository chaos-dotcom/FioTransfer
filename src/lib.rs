
//! FioTransfer — fast uploads to and downloads from Frame.io.
//!
//! A Rust port of the Go/Wails app of the same name: the same Frame.io V4 API,
//! the same transfer engine behaviour, the same frontend assets rendered in the
//! same WebKit view, and the same AppKit menu bar item, download panel and
//! Finder services.

#![deny(unsafe_code)]
// `unsafe` is allowed only where this app has to call C: the AppKit shims in
// `mac/` and the handful of `libc` calls listed below. Anywhere else it is a
// build error, which is the point.

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

/// Set at build time: `FIO_VERSION=1.2.3` or `-X`-style; "dev" when unset.
pub const VERSION: &str = match option_env!("FIO_VERSION") {
    Some(v) => v,
    None => "dev",
};
