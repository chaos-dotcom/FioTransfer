//! `dialogs::take_string`: the ownership half of the C boundary.
//!
//! The shims in `mac/dialogs.m` answer with a `malloc`ed copy (`fioCopy()`),
//! and `take_string` is the only function that reads one and gives the memory
//! back. That ownership is testable without AppKit: the test supplies the
//! symbol C would supply — `fioFree` in `mac/dialogs.m` is `free()` — and
//! records every pointer it is handed. A `take_string` that forgets to free, or
//! frees something other than the pointer it read, shows up as a wrong count or
//! a wrong pointer without any panel being opened.
//!
//! Everything else in `dialogs.rs` (open_files, question, the clipboard, the
//! window PNG) needs a real AppKit main thread and a human; those are named
//! under "what this suite cannot reach" in the report.

use std::ffi::{c_char, c_void};
use std::sync::Mutex;

use fiotransfer_sys::take_string;
use proptest::prelude::*;

use common::arb_bytes;

/// The pointers `fioFree` was called with, in order.
static FREED: Mutex<Vec<usize>> = Mutex::new(Vec::new());

/// `FREED` is process-wide, so the tests in this file take turns: without this,
/// one test's `clear()` lands in the middle of another's case.
static LOCK: Mutex<()> = Mutex::new(());

fn lock() -> std::sync::MutexGuard<'static, ()> {
    LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

/// Stands in for `mac/dialogs.m`'s `fioFree`, which is `free()`.
///
/// # Safety
/// Called by `take_string` with a pointer the test allocated with `malloc`.
#[no_mangle]
pub unsafe extern "C" fn fioFree(p: *mut c_void) {
    FREED.lock().unwrap_or_else(|e| e.into_inner()).push(p as usize);
    libc::free(p);
}

fn clear() {
    FREED.lock().unwrap_or_else(|e| e.into_inner()).clear();
}

fn freed() -> Vec<usize> {
    FREED.lock().unwrap_or_else(|e| e.into_inner()).clone()
}

/// A `malloc`ed, NUL-terminated copy of `bytes`: exactly what `fioCopy()` hands
/// the app.
fn copy_like_c(bytes: &[u8]) -> *mut c_char {
    let n = bytes.len() + 1;
    let p = unsafe { libc::malloc(n) } as *mut u8;
    assert!(!p.is_null(), "malloc({n})");
    unsafe {
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), p, bytes.len());
        *p.add(bytes.len()) = 0;
    }
    p as *mut c_char
}

proptest! {
    #![proptest_config(ProptestConfig { cases: 48, ..ProptestConfig::default() })]

    /// The string comes back whole (as a lossy decode of the bytes before the
    /// first NUL) and the buffer is handed back exactly once.
    #[test]
    fn take_string_reads_and_frees_exactly_once(bytes in arb_bytes()) {
        let _g = lock();
        clear();
        let p = copy_like_c(&bytes);
        let got = take_string(p);
        prop_assert_eq!(&got, &common::lossy_c_prefix(&bytes));
        prop_assert_eq!(freed(), vec![p as usize]);
    }
}

#[test]
fn take_string_of_null_is_empty_and_frees_nothing() {
    let _g = lock();
    clear();
    assert_eq!(take_string(std::ptr::null_mut()), "");
    assert_eq!(freed(), Vec::<usize>::new());
}

#[test]
fn take_string_of_an_empty_buffer() {
    let _g = lock();
    clear();
    let p = copy_like_c(b"");
    assert_eq!(take_string(p), "");
    assert_eq!(freed(), vec![p as usize]);
}

mod common;
