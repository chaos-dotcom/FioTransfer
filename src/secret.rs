
//! macOS Keychain storage (the Go app uses github.com/zalando/go-keyring).

use security_framework::passwords::{delete_generic_password, get_generic_password, set_generic_password};

pub const SERVICE: &str = "FioTransfer";

/// Tests and headless runs set `FIO_NO_KEYCHAIN=1`: the keychain is then neither
/// read (which can raise a "wants access" dialog and would sign a test in with the
/// real account) nor written (so `auth` falls back to the 0600 file, which lives
/// under `HOME` and is therefore redirectable to a temporary directory).
fn disabled() -> bool {
    std::env::var_os("FIO_NO_KEYCHAIN").is_some()
}

pub fn get(key: &str) -> Option<String> {
    if disabled() {
        return None;
    }
    match get_generic_password(SERVICE, key) {
        Ok(bytes) => Some(String::from_utf8_lossy(&bytes).into_owned()),
        Err(_) => None,
    }
}

pub fn set(key: &str, value: &str) -> Result<(), String> {
    if disabled() {
        return Err("keychain disabled".to_string());
    }
    set_generic_password(SERVICE, key, value.as_bytes()).map_err(|e| e.to_string())
}

pub fn delete(key: &str) {
    if disabled() {
        return;
    }
    let _ = delete_generic_password(SERVICE, key);
}
