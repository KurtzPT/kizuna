// Package fetch downloads URLs with a disk cache, per-host rate limits and
// de-duplication of identical in-flight requests.
//
// Every data source (AniDB, TVDB, TMDB, MAL...) goes through one Client, so
// caching and throttling rules live in exactly one place.
package fetch

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UserAgent identifies Kizuna to every source. main sets it from the build
// version before the first request ("kizuna/v0.1.0 (...)"); set it only then.
var UserAgent = "kizuna/dev (personal Plex metadata provider)"

// maxBody: larger answers are refused (the biggest list is ~10 MB).
const maxBody = 64 << 20

// Client is safe for concurrent use.
type Client struct {
	dir  string // cache root, e.g. /data/cache
	http *http.Client

	mu       sync.Mutex
	inflight map[string]*call      // cache key -> running download
	limits   map[string]*hostLimit // host -> throttle
}

type call struct {
	done   chan struct{}
	data   []byte
	err    error
	ticket *ticket // its place in the host's queue (nil = host not throttled)
}

// Request describes one download.
type Request struct {
	URL     string
	Key     string        // cache file name (relative); "" = don't cache
	TTL     time.Duration // how long a cached copy is fresh
	Method  string        // default GET
	Body    []byte        // POST body
	Headers map[string]string
	// Validate rejects answers that must not be cached (e.g. AniDB error pages).
	Validate func([]byte) error
	// NoStale: report a failed download instead of answering with a stale
	// cached copy (for callers that track failures themselves).
	NoStale bool
	// Done, if set, gets the outcome of the network request once it ends,
	// even when the caller stopped waiting (ctx). Not called on a cache hit.
	Done func(error)
	// Quiet: the caller logs failures itself; fetch only logs them at debug.
	Quiet bool
	// Revalidate: when the cached copy expires, ask the server whether it
	// changed (If-None-Match with the ETag saved next to it) instead of
	// downloading it again. For big lists that rarely change. Needs Key.
	Revalidate bool
	// Background: nobody waits for the answer now (an AniDB prefetch). On a
	// throttled host it waits behind every foreground request; a foreground
	// caller that joins its download moves it up.
	Background bool
}

func New(cacheDir string) *Client {
	return &Client{
		dir: cacheDir,
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 16, // TMDB seasons are fetched in parallel: reuse their connections
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}},
		inflight: map[string]*call{},
		limits:   map[string]*hostLimit{},
	}
}

// SetTransport replaces how requests reach the network: tests answer them
// from a fake, at the sources' real URLs.
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }

// Throttle makes requests to host wait at least gap between each other.
func (c *Client) Throttle(host string, gap time.Duration) {
	c.mu.Lock()
	c.limits[host] = newHostLimit(gap)
	c.mu.Unlock()
}

// Cached returns a cached file and its age, without any network access.
func (c *Client) Cached(key string) ([]byte, time.Duration, bool) {
	if key == "" {
		return nil, 0, false
	}
	p := c.path(key)
	at, ok := modTime(p)
	if !ok {
		return nil, 0, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, 0, false
	}
	return data, time.Since(at), true
}

// CachedAt returns when a cached file was last written, without reading it.
func (c *Client) CachedAt(key string) (time.Time, bool) {
	if key == "" {
		return time.Time{}, false
	}
	return modTime(c.path(key))
}

func modTime(path string) (time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

// Get returns a fresh cached copy, or downloads it. When the download fails
// a stale cached copy is returned instead (sources go down; old data beats none).
func (c *Client) Get(ctx context.Context, r Request) ([]byte, error) {
	start := time.Now()
	if data, ok := c.fresh(ctx, r); ok {
		observe(ctx, r, start, KindCache, nil)
		return data, nil
	}
	data, err := c.download(ctx, r)
	if err != nil {
		if r.NoStale {
			observe(ctx, r, start, KindFail, err)
			return nil, err
		}
		if stale, age, ok := c.Cached(r.Key); ok {
			slog.Debug("download failed; using stale cache", "key", r.Key, "age", age.Round(time.Minute), "err", err)
			observe(ctx, r, start, KindStale, err)
			return stale, nil
		}
		observe(ctx, r, start, KindFail, err)
		return nil, err
	}
	observe(ctx, r, start, KindNet, nil)
	return data, nil
}

// fresh returns r's cached copy when it is younger than r.TTL. The file's
// age is checked first: an expired copy is not read.
func (c *Client) fresh(ctx context.Context, r Request) ([]byte, bool) {
	if r.TTL <= 0 || r.Key == "" {
		return nil, false
	}
	p := c.path(r.Key)
	at, ok := modTime(p)
	if !ok {
		return nil, false
	}
	age := time.Since(at)
	debug := slog.Default().Enabled(ctx, slog.LevelDebug) // the log args aren't built otherwise
	if age >= r.TTL {
		if debug {
			slog.Debug("cache expired", "key", r.Key, "age", age.Round(time.Minute), "ttl", r.TTL)
		}
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	if debug {
		slog.Debug("cache hit", "key", r.Key, "age", age.Round(time.Minute), "ttl", r.TTL)
	}
	return data, true
}

// download runs at most one network request per key at a time; concurrent
// callers wait for the same result. The download itself is not cancelled when
// a caller gives up (ctx), so a slow AniDB answer still lands in the cache.
func (c *Client) download(ctx context.Context, r Request) ([]byte, error) {
	id := r.Key
	if id == "" {
		id = r.Method + " " + r.URL
	}
	c.mu.Lock()
	cl, running := c.inflight[id]
	if running && !r.Background {
		cl.ticket.promote()
	}
	if !running {
		cl = &call{done: make(chan struct{}), ticket: c.enqueue(r)}
		c.inflight[id] = cl
		go func() {
			cl.data, cl.err = c.fetch(r, cl.ticket)
			// Out of inflight before Done: a caller arriving once Done has
			// run must start its own download, not join this ended one
			// (AniDB counts each call in Done's caller).
			c.mu.Lock()
			delete(c.inflight, id)
			c.mu.Unlock()
			if r.Done != nil {
				r.Done(cl.err)
			}
			close(cl.done) // after Done: a waiter sees the outcome already recorded
		}()
	}
	c.mu.Unlock()
	select {
	case <-cl.done:
		return cl.data, cl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) fetch(r Request, t *ticket) ([]byte, error) {
	start := time.Now()
	data, status, waited, err := c.do(r, t)
	took := time.Since(start) - waited
	attrs := []any{"method", cmp.Or(r.Method, http.MethodGet), "url", Redact(r.URL), "status", status,
		"bytes", len(data), "took", took.Round(time.Millisecond), "throttled", waited.Round(time.Millisecond)}
	var se *StatusError
	switch {
	case err == nil:
		slog.Debug("http", attrs...)
	case r.Quiet || (errors.As(err, &se) && se.Code == http.StatusNotFound):
		slog.Debug("http failed", append(attrs, "err", err)...) // 404: the source just has no such entry
	case errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden):
		slog.Error("source rejected the request: check its API key / client id", append(attrs, "err", err)...)
	default:
		slog.Warn("source request failed", append(attrs, "err", err)...)
	}
	return data, err
}

func (c *Client) do(r Request, t *ticket) (data []byte, status int, waited time.Duration, err error) {
	waited = t.wait()

	method := cmp.Or(r.Method, http.MethodGet)
	req, err := http.NewRequest(method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return nil, 0, waited, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	cached, tag := c.revalidation(r)
	if cached != nil {
		req.Header.Set("If-None-Match", tag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) { // its message repeats the whole URL, secrets included
			ue.URL = Redact(ue.URL)
		}
		return nil, 0, waited, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, resp.StatusCode, waited, err
	}
	if len(data) > maxBody { // never cache a cut-off answer
		return nil, resp.StatusCode, waited, fmt.Errorf("answer larger than %d MB", maxBody>>20)
	}
	if resp.StatusCode == http.StatusNotModified && cached != nil {
		// Unchanged: the cached copy (validated when it was stored) is fresh again.
		c.Touch(r.Key)
		slog.Debug("not modified; cached copy is current", "key", r.Key, "etag", tag)
		return cached, resp.StatusCode, waited, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, waited, &StatusError{Code: resp.StatusCode, URL: Redact(r.URL)}
	}
	if data, err = gunzip(data, maxBody); err != nil { // AniDB always gzips, even when not asked to
		return nil, resp.StatusCode, waited, err
	}
	if r.Validate != nil {
		if err := r.Validate(data); err != nil {
			return nil, resp.StatusCode, waited, fmt.Errorf("rejected answer: %w", err)
		}
	}
	switch {
	case r.Key == "":
	case !r.Revalidate:
		c.store(r.Key, data)
	default:
		// The old ETag goes first: it must never be paired with other bytes,
		// or a 304 would vouch for the wrong copy. No new ETag (or a failed
		// write) just means a full download next time.
		_ = os.Remove(c.etagPath(r.Key))
		if c.store(r.Key, data) {
			if tag := resp.Header.Get("ETag"); tag != "" {
				_ = os.WriteFile(c.etagPath(r.Key), []byte(tag), 0o644)
			}
		}
	}
	return data, resp.StatusCode, waited, nil
}

// revalidation returns the cached copy and its saved ETag when r may be
// revalidated; nil when it must be downloaded in full (no copy, no ETag).
func (c *Client) revalidation(r Request) ([]byte, string) {
	if !r.Revalidate || r.Key == "" {
		return nil, ""
	}
	tag, err := os.ReadFile(c.etagPath(r.Key))
	if err != nil || len(bytes.TrimSpace(tag)) == 0 {
		return nil, ""
	}
	data, err := os.ReadFile(c.path(r.Key))
	if err != nil {
		return nil, ""
	}
	return data, string(bytes.TrimSpace(tag))
}

// enqueue queues r on its host's throttle; nil when the host has none.
// Needs c.mu.
func (c *Client) enqueue(r Request) *ticket {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil // http.NewRequest reports it
	}
	if l := c.limits[u.Host]; l != nil {
		return l.enqueue(r.Background)
	}
	return nil
}

// store writes a cache file and reports whether it did.
func (c *Client) store(key string, data []byte) bool {
	if err := WriteFile(c.path(key), data); err != nil {
		slog.Warn("cache write failed", "key", key, "err", err)
		return false
	}
	return true
}

// WriteFile replaces path with data atomically: a reader (or the next start,
// after a crash or with a full disk) sees the old file or the new one, never
// half of it. The data goes to path+".tmp" in the same folder first, which
// one writer at a time may use.
func WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err // nothing of ours to clean up
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync() // on disk before the rename makes it the file
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Touch marks a cached file as fresh again (a 304 answer: the source says
// the cached copy is still current).
func (c *Client) Touch(key string) {
	now := time.Now()
	_ = os.Chtimes(c.path(key), now, now)
}

func (c *Client) path(key string) string {
	return filepath.Join(c.dir, filepath.FromSlash(key))
}

// etagPath: where a Revalidate request keeps its ETag, next to the file.
func (c *Client) etagPath(key string) string { return c.path(key) + ".etag" }

// StatusError is a non-200 HTTP answer.
type StatusError struct {
	Code int
	URL  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d from %s", e.Code, e.URL) }

// IsNotFound reports whether err is an HTTP 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// gunzip unpacks a gzipped answer (any other is returned as is), up to
// limit bytes. A corrupt or bigger one is an error: it must never be cached.
func gunzip(data []byte, limit int64) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("corrupt gzip answer: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("corrupt gzip answer: %w", err)
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("answer larger than %d MB unpacked", limit>>20)
	}
	return out, nil
}

// Redact hides secrets in logged URLs: every query parameter whose name
// contains "key" or "token" (api_key, X-Plex-Token...), and AniDB's client
// name ("client").
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k := range q {
		if lk := strings.ToLower(k); strings.Contains(lk, "key") || strings.Contains(lk, "token") || lk == "client" {
			q.Set(k, "***")
		}
	}
	u.RawQuery = strings.ReplaceAll(q.Encode(), "%2A%2A%2A", "***")
	return u.String()
}
