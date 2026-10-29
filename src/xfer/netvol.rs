//! Is a path on a network volume?

/// MNT_LOCAL in <sys/mount.h>.
const MNT_LOCAL: u32 = 0x1000;

/// Reports whether `dir` is on a network share (SMB, NFS, AFP, WebDAV…).
///
/// The mount flags are only visible through `statfs`, which has no safe std
/// wrapper; this is the one `unsafe` call the module needs (the port already
/// uses `libc` this way in `localcert.rs`).
pub fn is_network_volume(dir: &str) -> bool {
    let Ok(c) = std::ffi::CString::new(dir) else {
        return false;
    };
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statfs(c.as_ptr(), &mut st) } != 0 {
        return false;
    }
    st.f_flags & MNT_LOCAL == 0
}
