//! FioTransfer's C boundary.
//!
//! Everything the app cannot write without `unsafe` lives here, and only here:
//! the `extern "C"` declarations for the AppKit shims in `mac/` (compiled and
//! linked by the app crate's `build.rs`; this crate only declares them) and the
//! handful of `libc` calls the app needs. The app crate is
//! `#![forbid(unsafe_code)]`, so this is the one crate in the workspace allowed
//! to write it.
//!
//! Everything public in this crate is a safe function: `&str` and `String` go
//! in and out, and the `CString`/`CStr`/`fioFree` work stays inside. The
//! AppKit code is macOS-only, gated on `target_os = "macos"`; the same
//! functions exist elsewhere and do nothing, so the crate still type-checks on
//! a platform that has no `mac/` shims to call.

#![allow(unsafe_code)] // the workspace's C boundary: the shim declarations, the pointer reads they need, and the libc calls

pub mod dialogs;
pub mod gui;
pub mod platform;
pub mod quick;
pub mod services;
pub mod strings;
pub mod tray;

pub use dialogs::take_string;
pub use strings::{cstr, from_c_str};
