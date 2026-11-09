package fetch

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A rename that fails because the file is in use is tried again; any other
// error is returned at once.
func TestRetryBusy(t *testing.T) {
	inUse := &os.LinkError{Op: "rename", Err: errSharingViolation}
	for _, c := range []struct {
		name  string
		errs  []error // what each try returns
		calls int
		ok    bool
	}{
		{"in use, then free", []error{inUse, &os.LinkError{Op: "rename", Err: syscall.ERROR_ACCESS_DENIED}, nil}, 3, true},
		{"other error", []error{errors.New("disk gone")}, 1, false},
		{"always in use", []error{inUse}, len(renameWaits) + 1, false},
	} {
		to := filepath.Join(t.TempDir(), "file") // a file that may be written: access denied is waited out
		if err := os.WriteFile(to, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		calls := 0
		var slept []time.Duration
		err := retryBusy(func() error {
			e := c.errs[min(calls, len(c.errs)-1)]
			calls++
			return e
		}, func(err error) bool { return busy(err, to) }, func(d time.Duration) { slept = append(slept, d) })
		if (err == nil) != c.ok || calls != c.calls || len(slept) != calls-1 {
			t.Errorf("%s: err %v after %d tries (%d waits), want ok=%v after %d", c.name, err, calls, len(slept), c.ok, c.calls)
		}
	}
}

// Access denied is waited out only while the target is a file that may be
// written: a read-only file or a folder won't change by waiting.
func TestBusyAccessDenied(t *testing.T) {
	dir := t.TempDir()
	file, readOnly, folder := filepath.Join(dir, "file"), filepath.Join(dir, "read-only"), filepath.Join(dir, "folder")
	for _, p := range []string{file, readOnly} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(readOnly, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o644) }) // so TempDir's cleanup can remove it
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	denied := &os.LinkError{Op: "rename", Err: syscall.ERROR_ACCESS_DENIED}
	for _, c := range []struct {
		to   string
		err  error
		want bool
	}{
		{file, denied, true},
		{readOnly, denied, false},
		{folder, denied, false},
		{filepath.Join(dir, "missing"), denied, false},
		{readOnly, &os.LinkError{Op: "rename", Err: errSharingViolation}, true},
		{file, errors.New("disk gone"), false},
	} {
		if got := busy(c.err, c.to); got != c.want {
			t.Errorf("busy(%v, %s) = %v, want %v", c.err, filepath.Base(c.to), got, c.want)
		}
	}

	// A save onto the read-only file or the folder fails at once.
	for _, to := range []string{readOnly, folder} {
		calls := 0
		err := retryBusy(func() error {
			calls++
			return os.Rename(file, to)
		}, func(err error) bool { return busy(err, to) }, func(time.Duration) {})
		if err == nil || calls != 1 {
			t.Errorf("rename onto %s: err %v after %d tries, want an error after 1", filepath.Base(to), err, calls)
		}
	}
}
