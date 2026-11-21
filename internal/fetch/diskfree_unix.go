//go:build linux || darwin || freebsd

package fetch

import "syscall"

// diskFree returns the bytes available to this (non-root) user on the disk
// holding path.
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Signed on FreeBSD, and negative once a non-root user is into the
	// reserved blocks: that is no space, not a huge amount.
	avail := int64(st.Bavail)
	if avail < 0 {
		avail = 0
	}
	return uint64(avail) * uint64(st.Bsize), nil
}
