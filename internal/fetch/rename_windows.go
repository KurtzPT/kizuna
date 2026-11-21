package fetch

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// errSharingViolation: another process has the file open without letting
// it be replaced (not in package syscall).
const errSharingViolation syscall.Errno = 32

// renameWaits: how long rename waits before each retry (about 0.3 s in all).
var renameWaits = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond,
	80 * time.Millisecond, 160 * time.Millisecond}

// rename replaces to with from. On Windows that fails while someone reads
// to (a cache read, a virus scanner): those readers are quick, so it is
// tried again a few times, like Go's own robustio.
func rename(from, to string) error {
	return retryBusy(func() error { return os.Rename(from, to) }, func(err error) bool { return busy(err, to) }, time.Sleep)
}

// retryBusy runs op again after each of renameWaits while it fails with an
// error inUse says is the file being in use.
func retryBusy(op func() error, inUse func(error) bool, sleep func(time.Duration)) error {
	err := op()
	for _, wait := range renameWaits {
		if !inUse(err) {
			return err
		}
		sleep(wait)
		err = op()
	}
	return err
}

// busy: the rename onto to failed because someone has to open. A sharing
// violation always says so. Access denied does too (a file being deleted, a
// handle without FILE_SHARE_DELETE), but also comes from failures waiting
// won't fix: to is read-only or a folder, or the rights are missing. Those
// would hold every save for the whole of renameWaits, so access denied counts
// only while to is a regular file that may be written.
func busy(err error, to string) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case errSharingViolation:
		return true
	case syscall.ERROR_ACCESS_DENIED:
		st, err := os.Lstat(to)
		return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o200 != 0
	}
	return false
}
