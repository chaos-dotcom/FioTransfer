
//! macOS Keychain storage (the Go app uses github.com/zalando/go-keyring).

use security_framework::passwords::{delete_generic_password, get_generic_password, set_generic_password};

pub const SERVICE: &str = "FioTransfer";

pub fn get(key: &str) -> Option<String> {
    match get_generic_password(SERVICE, key) {
        Ok(bytes) => Some(String::from_utf8_lossy(&bytes).into_owned()),
        Err(_) => None,
    }
}

pub fn set(key: &str, value: &str) -> Result<(), String> {
    set_generic_password(SERVICE, key, value.as_bytes()).map_err(|e| e.to_string())
}

pub fn delete(key: &str) {
    let _ = delete_generic_password(SERVICE, key);
}
