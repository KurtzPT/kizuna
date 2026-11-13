package fetch

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	got := Redact("https://x/y?api_key=SECRET&X-Plex-Token=TOKEN&client=MYCLIENT&clientver=1&page=2")
	for _, secret := range []string{"SECRET", "TOKEN", "MYCLIENT"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%s not redacted: %s", secret, got)
		}
	}
	if !strings.Contains(got, "page=2") || !strings.Contains(got, "clientver=1") || !strings.Contains(got, "api_key=***") {
		t.Fatalf("redacted too much: %s", got)
	}
}

// A network error repeats the whole URL in its message: it must be redacted too.
func TestNetworkErrorRedacted(t *testing.T) {
	c := New(t.TempDir())
	c.SetTransport(roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	}))
	_, _, _, err := c.do(Request{URL: "http://127.0.0.1:1/x?api_key=SECRET&client=MYCLIENT"}, nil)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "MYCLIENT") {
		t.Fatalf("secret in error: %v", err)
	}
}

// WriteFile replaces a file whole or not at all: a write that fails leaves
// the old content, and a good one leaves no temporary file behind.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFile(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o755); err != nil { // the temporary file can't be written
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new")); err == nil {
		t.Error("want an error when the temporary file can't be written")
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("after a failed write: %q, want the old content", b)
	}
	if fi, err := os.Stat(path + ".tmp"); err != nil || !fi.IsDir() {
		t.Errorf("what was at the temporary path must be left alone: %v", err)
	}

	path = filepath.Join(t.TempDir(), "sub", "state.json") // the folder is created
	for _, data := range []string{"old", "new"} {
		if err := WriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Errorf("after a write: %q", b)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
}

// bodyNet answers every request with status (0 = 200) and body, and counts
// them. A test changes status or body only while no request is under way.
type bodyNet struct {
	status int
	body   string
	sent   atomic.Int32
}

func (n *bodyNet) RoundTrip(r *http.Request) (*http.Response, error) {
	n.sent.Add(1)
	return &http.Response{StatusCode: cmp.Or(n.status, http.StatusOK), Body: io.NopCloser(strings.NewReader(n.body)),
		Header: http.Header{}, Request: r}, nil
}

// redirectNet answers every request to its first host with a redirect to
// another host, and records the hosts asked.
type redirectNet struct {
	mu    sync.Mutex
	asked []string
}

func (n *redirectNet) RoundTrip(r *http.Request) (*http.Response, error) {
	n.mu.Lock()
	n.asked = append(n.asked, r.URL.Host)
	n.mu.Unlock()
	if r.URL.Host == "api.example" {
		return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(strings.NewReader("")),
			Header: http.Header{"Location": {"https://elsewhere.example/x"}}, Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}, Request: r}, nil
}

// NoRedirect: a redirect is the request's answer, an error, not followed:
// Go would carry the request's headers (a token) to the other host. Without
// it, a redirect is followed as before.
func TestNoRedirect(t *testing.T) {
	c := New(t.TempDir())
	net := &redirectNet{}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/x", Headers: map[string]string{"X-Plex-Token": "secret"}, NoRedirect: true}
	_, err := c.Get(context.Background(), r)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Fatalf("err %v, want the 302 as an error", err)
	}
	if strings.Join(net.asked, " ") != "api.example" {
		t.Fatalf("asked %v, want only the first host", net.asked)
	}
	r.NoRedirect = false
	if data, err := c.Get(context.Background(), r); err != nil || string(data) != "ok" {
		t.Fatalf("without NoRedirect: %q %v, want the redirect followed", data, err)
	}
}

// Before runs once the request's turn comes; an error stops it: nothing is
// sent, nothing cached, and Done gets that error.
func TestBeforeStopsRequest(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: "ok"}
	c.SetTransport(net)
	calls := &net.sent
	stop := errors.New("banned meanwhile")
	var done error
	_, err := c.Get(context.Background(), Request{URL: "http://api.example/x", Key: "x", NoStale: true,
		Before: func() error { return stop }, Done: func(err error) { done = err }})
	if !errors.Is(err, stop) || !errors.Is(done, stop) || calls.Load() != 0 {
		t.Fatalf("err %v, Done got %v, %d requests sent", err, done, calls.Load())
	}
	if _, _, ok := c.Cached("x"); ok {
		t.Fatal("cached")
	}
	if _, err := c.Get(context.Background(), Request{URL: "http://api.example/x", Before: func() error { return nil }}); err != nil || calls.Load() != 1 {
		t.Fatalf("Before allowed it: err %v, %d requests sent", err, calls.Load())
	}
}

// Done runs once the download is no longer in flight: a caller that comes
// after it starts its own download instead of joining the ended one.
func TestDoneAfterLeavingInflight(t *testing.T) {
	c := New(t.TempDir())
	c.SetTransport(&bodyNet{body: "ok"})
	stillInflight := true
	r := Request{URL: "http://api.example/x", Key: "x"}
	r.Done = func(error) {
		c.mu.Lock()
		_, stillInflight = c.inflight[r.Key]
		c.mu.Unlock()
	}
	if _, err := c.Get(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if stillInflight {
		t.Fatal("Done ran while the download was still in flight")
	}
}

func gzipped(t *testing.T, s string) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// gunzip unpacks up to its limit; a bigger or corrupt answer is an error,
// never the packed bytes passed on as if they were the answer.
func TestGunzipLimit(t *testing.T) {
	z := gzipped(t, "0123456789")
	if out, err := gunzip(z, 10); err != nil || string(out) != "0123456789" {
		t.Fatalf("at the limit: %q %v", out, err)
	}
	if out, err := gunzip(z, 9); err == nil {
		t.Fatalf("over the limit: %q, want an error", out)
	}
	if out, err := gunzip(z[:len(z)-6], 100); err == nil { // cut off
		t.Fatalf("corrupt: %q, want an error", out)
	}
	if out, err := gunzip([]byte("<anime/>"), 1); err != nil || string(out) != "<anime/>" {
		t.Fatalf("not gzipped: %q %v, want it as is", out, err)
	}
}

// A corrupt gzipped answer fails the download and isn't cached.
func TestCorruptGzipNotCached(t *testing.T) {
	c := New(t.TempDir())
	z := gzipped(t, "<anime/>")
	c.SetTransport(&bodyNet{body: string(z[:len(z)-6])})
	if _, err := c.Get(context.Background(), Request{URL: "http://api.example/x", Key: "x", NoStale: true}); err == nil {
		t.Fatal("want an error")
	}
	if _, _, ok := c.Cached("x"); ok {
		t.Fatal("corrupt answer cached")
	}
}

// Get: a fresh copy is answered without the network; an expired one is
// downloaded again; when that fails (or the answer is rejected by Validate,
// which also keeps it out of the cache) the stale copy is answered, unless
// NoStale.
func TestGetStaleFallback(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: "v1"}
	c.SetTransport(net)
	calls := &net.sent
	ctx := context.Background()
	r := Request{URL: "http://api.example/x", Key: "x", TTL: time.Hour,
		Validate: func(b []byte) error {
			if string(b) == "bad" {
				return errors.New("bad answer")
			}
			return nil
		}}
	get := func(r Request) string {
		t.Helper()
		data, err := c.Get(ctx, r)
		if err != nil {
			return "error"
		}
		return string(data)
	}
	if got := get(r); got != "v1" || calls.Load() != 1 {
		t.Fatalf("first get %q, %d calls", got, calls.Load())
	}
	if got := get(r); got != "v1" || calls.Load() != 1 {
		t.Fatalf("fresh copy: %q, %d calls (want no new call)", got, calls.Load())
	}

	noStale := r
	noStale.NoStale = true
	for _, answer := range []struct {
		status int
		body   string
	}{{500, "oops"}, {200, "bad"}} {
		expire(t, c, r.Key)
		net.status, net.body = answer.status, answer.body
		if got := get(r); got != "v1" {
			t.Errorf("%d %q: got %q, want the stale copy", answer.status, answer.body, got)
		}
		if got := get(noStale); got != "error" {
			t.Errorf("%d %q with NoStale: got %q, want an error", answer.status, answer.body, got)
		}
		if data, _, _ := c.Cached(r.Key); string(data) != "v1" {
			t.Errorf("%d %q: cache now holds %q", answer.status, answer.body, data)
		}
	}
	net.status, net.body = 200, "v2"
	if got := get(r); got != "v2" {
		t.Fatalf("source back: %q", got)
	}
}

func TestPruneOldestCheapFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	files := []struct {
		rel string
		age time.Duration
	}{
		{"anidb/anime/1.xml", 300 * time.Hour}, // oldest, but costly: last
		{"tvdb/1/old.json", 200 * time.Hour},
		{"tmdb/tv-1.json", 100 * time.Hour},
		{"mal/v2-1.json", 1 * time.Hour},
		{"anidb/animetitles.xml", 999 * time.Hour}, // never deleted
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, now.Add(-f.age), now.Add(-f.age))
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		return err == nil
	}

	if n, freed := prune(dir, 150); n != 2 || freed != 200 { // two oldest cheap files
		t.Fatalf("prune 150: %d files, %d bytes", n, freed)
	}
	if exists("tvdb/1/old.json") || exists("tmdb/tv-1.json") || !exists("mal/v2-1.json") || !exists("anidb/anime/1.xml") {
		t.Fatal("wrong files deleted")
	}
	prune(dir, 1000) // everything it may delete
	if exists("mal/v2-1.json") || exists("anidb/anime/1.xml") || !exists("anidb/animetitles.xml") {
		t.Fatal("full prune: AniDB entry should go last, the title dump never")
	}
}

// A cache folder that is a link is pruned through, never removed itself;
// links inside it are left alone, and so is a temporary file a WriteFile
// may still be filling.
func TestPruneLinksAndTemp(t *testing.T) {
	real, base := t.TempDir(), t.TempDir()
	outside := filepath.Join(base, "outside.json")
	link := filepath.Join(base, "cache")
	dirLink(t, real, link)
	now := time.Now()
	for _, f := range []struct {
		path string
		age  time.Duration
	}{
		{filepath.Join(real, "tvdb.json"), time.Hour},
		{filepath.Join(real, "old.json.tmp"), time.Hour}, // left by a crash
		{filepath.Join(real, "new.json.tmp"), 0},         // being written
		{outside, time.Hour},
	} {
		if err := os.WriteFile(f.path, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(f.path, now.Add(-f.age), now.Add(-f.age))
	}
	inner := filepath.Join(real, "elsewhere") // a link to the folder holding outside.json
	dirLink(t, base, inner)
	if n, freed := prune(link, 1<<30); n != 2 || freed != 200 {
		t.Errorf("deleted %d files, %d bytes; want the old cache file and the old temporary file", n, freed)
	}
	for _, p := range []string{link, inner, outside, filepath.Join(real, "new.json.tmp")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

// dirLink makes link point to the folder target: a symlink, or on Windows
// without the symlink privilege a junction (what a mounted cache often is).
func dirLink(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err != nil && runtime.GOOS == "windows" {
		var out []byte
		out, err = exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, out)
		}
	}
	if err != nil {
		t.Skipf("can't make a folder link here: %v", err)
	}
}

func TestDiskFree(t *testing.T) {
	if free, err := diskFree(t.TempDir()); err != nil || free == 0 {
		t.Fatalf("free %d, err %v", free, err)
	}
	if _, err := diskFree(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing folder: err %v, want not-exist", err)
	}
}

// etagServer serves body with etag ("" = none) and answers 304 to a
// matching If-None-Match, which it records.
type etagServer struct {
	mu         sync.Mutex
	body, etag string
	inm        []string
	before304  func() // if set, runs before a 304 is answered
}

func (e *etagServer) set(body, etag string) {
	e.mu.Lock()
	e.body, e.etag = body, etag
	e.mu.Unlock()
}

// sent returns the If-None-Match of request n (0 = the first).
func (e *etagServer) sent(n int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inm[n]
}

func (e *etagServer) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		inm := r.Header.Get("If-None-Match")
		e.inm = append(e.inm, inm)
		if e.etag != "" {
			w.Header().Set("ETag", e.etag)
			if inm == e.etag {
				if e.before304 != nil {
					e.before304()
				}
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		_, _ = w.Write([]byte(e.body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/list.xml"
}

// expire makes a cached file look older than any TTL.
func expire(t *testing.T, c *Client, key string) {
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(c.path(key), old, old); err != nil {
		t.Fatal(err)
	}
}

// A cached file is read with its own time; Fresh reads it only while it is
// younger than the TTL. A folder or a missing file is no copy.
func TestCachedReads(t *testing.T) {
	c := New(t.TempDir())
	if err := WriteFile(c.path("a.json"), []byte("abc")); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(c.path("a.json"), at, at); err != nil {
		t.Fatal(err)
	}
	if data, got, ok := c.CachedSince("a.json"); !ok || string(data) != "abc" || !got.Equal(at) {
		t.Errorf("CachedSince: %q, %v, %v; want abc at %v", data, got, ok, at)
	}
	if data, ok := c.Fresh(context.Background(), "a.json", 2*time.Hour); !ok || string(data) != "abc" {
		t.Errorf("Fresh within the TTL: %q, %v", data, ok)
	}
	if data, ok := c.Fresh(context.Background(), "a.json", time.Minute); ok || data != nil {
		t.Errorf("Fresh past the TTL: %q, %v; want nothing", data, ok)
	}
	if _, ok := c.Fresh(context.Background(), "a.json", 0); ok {
		t.Error("Fresh with no TTL answered")
	}
	if err := os.Mkdir(c.path("dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dir", "missing", ""} {
		if _, _, ok := c.CachedSince(key); ok {
			t.Errorf("CachedSince(%q) answered", key)
		}
		if _, ok := c.Fresh(context.Background(), key, time.Hour); ok {
			t.Errorf("Fresh(%q) answered", key)
		}
	}
}

// An unchanged list is answered with 304: the cached copy is returned and
// fresh again, without running Validate; a changed one replaces it and its ETag.
func TestRevalidateNotModified(t *testing.T) {
	srv := &etagServer{body: "v1", etag: `"a1"`}
	c := New(t.TempDir())
	var validated atomic.Int32
	r := Request{URL: srv.start(t), Key: "lists/list.xml", TTL: time.Hour, Revalidate: true,
		Validate: func([]byte) error { validated.Add(1); return nil }}
	ctx := context.Background()
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v1" {
		t.Fatalf("first get: %q %v", data, err)
	}
	if tag, err := os.ReadFile(c.etagPath(r.Key)); err != nil || string(tag) != `"a1"` {
		t.Fatalf("etag stored: %q %v", tag, err)
	}

	expire(t, c, r.Key)
	srv.set("v1 again (must not be downloaded)", `"a1"`)
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v1" {
		t.Fatalf("304: %q %v, want the cached copy", data, err)
	}
	if srv.sent(1) != `"a1"` || validated.Load() != 1 {
		t.Fatalf("If-None-Match %q, Validate ran %d times (want once, not on the 304)", srv.sent(1), validated.Load())
	}
	if _, age, ok := c.Cached(r.Key); !ok || age > time.Minute {
		t.Fatalf("a 304 must make the cached copy fresh: age %v", age)
	}

	expire(t, c, r.Key)
	srv.set("v2", `"b2"`)
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v2" {
		t.Fatalf("changed list: %q %v", data, err)
	}
	if tag, _ := os.ReadFile(c.etagPath(r.Key)); string(tag) != `"b2"` {
		t.Fatalf("new etag %q", tag)
	}
}

// Without an ETag (none sent, or its file pruned) the list is just
// downloaded in full.
func TestRevalidateWithoutETag(t *testing.T) {
	srv := &etagServer{body: "v1"}
	c := New(t.TempDir())
	r := Request{URL: srv.start(t), Key: "lists/list.xml", TTL: time.Hour, Revalidate: true}
	ctx := context.Background()
	if _, err := c.Get(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.etagPath(r.Key)); !os.IsNotExist(err) {
		t.Fatalf("no ETag sent, but an .etag file exists: %v", err)
	}
	expire(t, c, r.Key)
	srv.set("v2", "")
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v2" || srv.sent(1) != "" {
		t.Fatalf("full download: %q %v, If-None-Match %q", data, err, srv.sent(1))
	}

	srv.set("v2", `"c3"`) // the server starts sending one; then the .etag file is pruned
	expire(t, c, r.Key)
	if _, err := c.Get(ctx, r); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(c.etagPath(r.Key))
	expire(t, c, r.Key)
	srv.set("v3", `"d4"`)
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v3" || srv.sent(3) != "" {
		t.Fatalf("pruned etag: %q %v, If-None-Match %q", data, err, srv.sent(3))
	}
}

// With an ETag but no cached copy (pruned, its .etag file left), nothing is
// asked conditionally: the list is downloaded in full.
func TestRevalidateWithoutCopy(t *testing.T) {
	srv := &etagServer{body: "v1", etag: `"a1"`}
	c := New(t.TempDir())
	r := Request{URL: srv.start(t), Key: "lists/list.xml", TTL: time.Hour, Revalidate: true}
	ctx := context.Background()
	if _, err := c.Get(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.path(r.Key)); err != nil {
		t.Fatal(err)
	}
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v1" || srv.sent(1) != "" {
		t.Fatalf("no copy: %q %v, If-None-Match %q; want a full download", data, err, srv.sent(1))
	}
}

// The cached copy is read only once the 304 comes. Gone by then (pruned
// while the request was out), the call fails and the ETag goes with it, so
// the next call downloads the list in full: the ETag never vouches for
// bytes it wasn't saved with.
func TestRevalidateCopyGoneBy304(t *testing.T) {
	srv := &etagServer{body: "v1", etag: `"a1"`}
	c := New(t.TempDir())
	r := Request{URL: srv.start(t), Key: "lists/list.xml", TTL: time.Hour, Revalidate: true}
	ctx := context.Background()
	if _, err := c.Get(ctx, r); err != nil {
		t.Fatal(err)
	}
	expire(t, c, r.Key)
	srv.mu.Lock()
	srv.before304 = func() { // the request is out, its answer not yet
		if err := os.Remove(c.path(r.Key)); err != nil {
			t.Error(err)
		}
	}
	srv.mu.Unlock()
	if data, err := c.Get(ctx, r); err == nil || srv.sent(1) != `"a1"` {
		t.Fatalf("copy gone by the 304: %q %v, If-None-Match %q; want an error", data, err, srv.sent(1))
	}
	if _, err := os.Stat(c.etagPath(r.Key)); !os.IsNotExist(err) {
		t.Fatalf("the .etag file is kept without its copy: %v", err)
	}
	srv.set("v2", `"a1"`)
	if data, err := c.Get(ctx, r); err != nil || string(data) != "v2" || srv.sent(2) != "" {
		t.Fatalf("next call: %q %v, If-None-Match %q; want a full download", data, err, srv.sent(2))
	}
}
