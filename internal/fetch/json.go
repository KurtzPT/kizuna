package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// errNotJSON: a 200 answer that isn't JSON, like a captive portal's or a
// CDN's HTML page.
var errNotJSON = errors.New("not JSON")

// GetJSON is Get for a JSON API: the answer is decoded into out, which it
// replaces. A downloaded answer must decode (after r's own Validate), or it
// is neither cached nor used, like any rejected one: an answer that isn't
// JSON, or JSON of another shape (an error object where a list belongs).
// It is decoded once, in the check, into a value of its own: out gets it
// only if this call's download is the answer, so a download that outlives
// its caller (ctx) never writes to out.
func (c *Client) GetJSON(ctx context.Context, r Request, out any) error {
	own := r.Validate
	target := reflect.ValueOf(out)
	var decoded reflect.Value // set by this call's download (its goroutine), before the answer is handed over
	r.Validate = func(data []byte) error {
		if own != nil {
			if err := own(data); err != nil {
				return err
			}
		}
		if target.Kind() != reflect.Pointer || target.IsNil() {
			if !json.Valid(data) {
				return errNotJSON
			}
			return nil // the Unmarshal below reports the bad out
		}
		v := reflect.New(target.Type().Elem())
		if err := json.Unmarshal(data, v.Interface()); err != nil {
			if se := (*json.SyntaxError)(nil); errors.As(err, &se) {
				return fmt.Errorf("%w: %v", errNotJSON, err)
			}
			return fmt.Errorf("unexpected JSON: %w", err)
		}
		decoded = v
		return nil
	}
	data, err := c.Get(ctx, r)
	if err != nil {
		return err
	}
	// With ctx still live, Get waited for this call's download, if it ran
	// one: decoded is set for these very bytes, or not at all (a cache hit,
	// a 304, a download joined from another call). With ctx done, the bytes
	// may be a stale copy while the download still runs: decoded isn't read.
	if ctx.Err() == nil && decoded.IsValid() {
		target.Elem().Set(decoded.Elem())
		return nil
	}
	if target.Kind() == reflect.Pointer && !target.IsNil() {
		target.Elem().SetZero() // replaced, not merged into what out held
	}
	return json.Unmarshal(data, out)
}
