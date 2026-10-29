package xfer

import "syscall"

const mntLocal = 0x1000 // MNT_LOCAL in <sys/mount.h>

// onNetworkVolume reports whether path is on a network share (SMB, NFS, AFP, WebDAV…).
func onNetworkVolume(path string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(path, &st) == nil && st.Flags&mntLocal == 0
}
