package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// bodyNet answers every request with its body.
type bodyNet struct{ body string }

func (b *bodyNet) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(b.body)), Header: http.Header{}, Request: r}, nil
}

// A 200 answer that isn't JSON (a captive portal's page) is neither cached
// nor decoded; with an older copy cached, that one answers.
func TestGetJSONRejectsNonJSON(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `<html>Log in to the hotel wifi</html>`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/x", Key: "x.json", TTL: time.Hour}
	var out struct{ A int }
	if err := c.GetJSON(context.Background(), r, &out); !errors.Is(err, errNotJSON) {
		t.Fatalf("err %v, want not JSON", err)
	}
	if _, _, ok := c.Cached(r.Key); ok {
		t.Fatal("the HTML page was cached")
	}

	net.body = `{"A": 1}`
	if err := c.GetJSON(context.Background(), r, &out); err != nil || out.A != 1 {
		t.Fatalf("JSON answer: %+v, %v", out, err)
	}
	expire(t, c, r.Key)
	net.body = `<html>again</html>`
	out.A = 0
	if err := c.GetJSON(context.Background(), r, &out); err != nil || out.A != 1 {
		t.Errorf("non-JSON with a stale copy: %+v, %v; want the stale copy", out, err)
	}
}

// The request's own Validate still runs, after the JSON check.
func TestGetJSONKeepsOwnValidate(t *testing.T) {
	c := New(t.TempDir())
	c.SetTransport(&bodyNet{body: `{"error": "banned"}`})
	banned := errors.New("banned")
	r := Request{URL: "https://api.example/y", Key: "y.json", TTL: time.Hour,
		Validate: func(data []byte) error {
			if strings.Contains(string(data), "banned") {
				return banned
			}
			return nil
		}}
	var out map[string]string
	if err := c.GetJSON(context.Background(), r, &out); !errors.Is(err, banned) {
		t.Errorf("err %v, want the request's own Validate error", err)
	}
}
