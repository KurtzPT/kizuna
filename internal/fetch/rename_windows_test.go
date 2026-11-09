package fetch

import (
	"errors"
	"os"
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
		{"in use, then free", []error{inUse, syscall.ERROR_ACCESS_DENIED, nil}, 3, true},
		{"other error", []error{errors.New("disk gone")}, 1, false},
		{"always in use", []error{inUse}, len(renameWaits) + 1, false},
	} {
		calls := 0
		var slept []time.Duration
		err := retryBusy(func() error {
			e := c.errs[min(calls, len(c.errs)-1)]
			calls++
			return e
		}, func(d time.Duration) { slept = append(slept, d) })
		if (err == nil) != c.ok || calls != c.calls || len(slept) != calls-1 {
			t.Errorf("%s: err %v after %d tries (%d waits), want ok=%v after %d", c.name, err, calls, len(slept), c.ok, c.calls)
		}
	}
}
