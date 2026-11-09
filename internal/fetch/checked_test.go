package fetch

import (
	"errors"
	"strconv"
	"testing"
)

// Checked hands its parse back only for the bytes it validated, not for a
// cached copy with the same content that Get answered with instead; a
// broken download keeps the earlier parse.
func TestCheckedReusesOnlyItsBytes(t *testing.T) {
	c := &Checked[int]{Parse: func(b []byte) (int, error) {
		n, err := strconv.Atoi(string(b))
		if err != nil {
			return 0, errors.New("not a number")
		}
		return n, nil
	}}
	data := []byte("42")
	if err := c.Validate(data); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Result(data); !ok || got != 42 {
		t.Errorf("same bytes: %d, %v", got, ok)
	}
	if got, ok := c.Result([]byte("42")); ok {
		t.Errorf("other bytes: %d, want no result", got)
	}
	if _, ok := c.Result(nil); ok {
		t.Error("no bytes: want no result")
	}
	if err := c.Validate([]byte("x")); err == nil {
		t.Error("broken: want an error")
	}
	if got, ok := c.Result(data); !ok || got != 42 {
		t.Errorf("after a broken download: %d, %v; the earlier parse must stay", got, ok)
	}
}
