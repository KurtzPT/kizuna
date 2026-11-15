package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"time"
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
//
// A cached copy that doesn't decode into out is no answer: a fresh one is
// downloaded again (a good answer replaces it), and a stale one doesn't
// stand in for a failed download. The file is never removed: it stays for
// the next try.
//
// ready, if set, runs once the cache has no fresh answer, before the
// download: it returns the request to send (TheTVDB adds its login's token).
// Its error fails the call as a failed download would, with nothing sent:
// a stale copy answers unless the request it returned has NoStale.
func (c *Client) GetJSON(ctx context.Context, r Request, out any, ready func(Request) (Request, error)) error {
	_, err := c.getJSON(ctx, r, out, ready, nil)
	return err
}

// GetShared is GetJSON for a value its callers share and only read (see
// Memo). A fresh cached copy is decoded once per file time and kept in m,
// by r.Key: a later call that finds the file fresh with that same time
// gets the kept value, without the file being read, and the read is
// reported as the cache hit it stands for. Whatever else answers (a
// download, a stale copy) is decoded as GetJSON does, and isn't kept: the
// next fresh read keeps it. The value returned must not be changed.
func GetShared[T any](ctx context.Context, c *Client, m *Memo[string, *T], r Request, ready func(Request) (Request, error)) (*T, error) {
	var kept *T
	known := func(at time.Time) bool {
		if v, vat, ok := m.Get(r.Key); ok && vat.Equal(at) {
			kept = v
			return true
		}
		return false
	}
	out := new(T)
	at, err := c.getJSON(ctx, r, out, ready, known)
	switch {
	case err != nil:
		return nil, err
	case kept != nil:
		return kept, nil
	case !at.IsZero():
		m.Put(r.Key, at, out)
	}
	return out, nil
}

// getJSON is GetJSON, with freshKnown's known: a fresh copy it knows is not read
// and out is left as it is. at: the fresh copy's file time, when that copy
// answered (known, or decoded into out); zero when anything else did.
func (c *Client) getJSON(ctx context.Context, r Request, out any, ready func(Request) (Request, error), known func(time.Time) bool) (time.Time, error) {
	start := time.Now()
	broken := false // the fresh copy doesn't decode: no stale copy to fall back on either
	if data, at, ok, hit := c.freshKnown(ctx, r, known); ok {
		if hit {
			observe(ctx, r, start, KindCache, nil)
			return at, nil
		}
		err := decode(data, out)
		if err == nil {
			observe(ctx, r, start, KindCache, nil)
			return at, nil
		}
		if ie := (*json.InvalidUnmarshalError)(nil); errors.As(err, &ie) {
			return time.Time{}, err // the caller's out is wrong (nil, not a pointer), not the copy
		}
		broken = true
		slog.Warn("cached copy doesn't decode; downloading it again", "key", r.Key, "err", err)
		if r.Revalidate {
			// Its ETag goes first, so no 304 can vouch for the broken copy: the
			// download is a full one, and saves the new ETag with the new bytes.
			_ = os.Remove(c.etagPath(r.Key))
		}
	}
	if ready != nil {
		var err error
		if r, err = ready(r); err != nil {
			if !r.NoStale && !broken && c.staleJSON(ctx, r.Key, start, err, out) {
				return time.Time{}, nil
			}
			return time.Time{}, err
		}
	}
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
	data, err := c.download(ctx, r)
	if err != nil {
		if !r.NoStale && !broken && c.staleJSON(ctx, r.Key, start, err, out) {
			return time.Time{}, nil
		}
		observe(ctx, r, start, KindFail, err)
		return time.Time{}, err
	}
	observe(ctx, r, start, KindNet, nil)
	// With ctx still live, download waited for this call's download, if it
	// ran one: decoded is set for these very bytes, or not at all (a 304, a
	// download joined from another call).
	if ctx.Err() == nil && decoded.IsValid() {
		target.Elem().Set(decoded.Elem())
		return time.Time{}, nil
	}
	return time.Time{}, decode(data, out)
}

// staleJSON decodes key's cached copy, however old, into out, for a call
// that failed with err, as stale does. A copy that doesn't decode is no
// answer, and isn't reported as one.
func (c *Client) staleJSON(ctx context.Context, key string, start time.Time, err error, out any) bool {
	data, at, ok := c.CachedSince(key)
	if !ok {
		return false
	}
	if derr := decode(data, out); derr != nil {
		slog.Warn("stale cached copy doesn't decode; not used", "key", key, "err", derr)
		return false
	}
	usedStale(ctx, key, at, start, err)
	return true
}

// decode replaces what out holds with data's JSON. out is zeroed first, and
// again when data doesn't decode: no field of an earlier value, nor half of
// this one, is left behind.
func decode(data []byte, out any) error {
	zero(out)
	err := json.Unmarshal(data, out)
	if err != nil {
		zero(out)
	}
	return err
}

func zero(out any) {
	if v := reflect.ValueOf(out); v.Kind() == reflect.Pointer && !v.IsNil() {
		v.Elem().SetZero()
	}
}
