package fetch

import "sync"

// Checked parses a download in full as its Request.Validate, before it's
// cached, so a broken download (a stray "&", a cut-off file) is rejected and
// the good cached copy stays. It keeps that parse, so the caller doesn't
// parse the same bytes twice. Validate can still run after Get returned
// (Get doesn't cancel downloads), hence the lock.
type Checked[T any] struct {
	Parse func([]byte) (T, error)

	mu   sync.Mutex
	data []byte
	val  T
}

// Validate parses b and keeps the result; use it as (or in) Request.Validate.
func (c *Checked[T]) Validate(b []byte) error {
	v, err := c.Parse(b)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.data, c.val = b, v
	c.mu.Unlock()
	return nil
}

// Result returns what Validate parsed when data is those very bytes (the
// download Get answered with); false when it is anything else, such as a
// cached copy Get answered with instead: parse that one.
func (c *Checked[T]) Result(data []byte) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) == 0 || len(c.data) != len(data) || &c.data[0] != &data[0] {
		var zero T
		return zero, false
	}
	return c.val, true
}
