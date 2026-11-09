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
	return retryBusy(func() error { return os.Rename(from, to) }, time.Sleep)
}

// retryBusy runs op again after each of renameWaits while it fails because
// the file is in use.
func retryBusy(op func() error, sleep func(time.Duration)) error {
	err := op()
	for _, wait := range renameWaits {
		if !busy(err) {
			return err
		}
		sleep(wait)
		err = op()
	}
	return err
}

func busy(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == syscall.ERROR_ACCESS_DENIED || errno == errSharingViolation)
}
