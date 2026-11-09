package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// episodePages: the first page's error is the list's, a later 404 ends the
// list, any other later error leaves it incomplete; a last page stops it.
func TestEpisodePages(t *testing.T) {
	down := errors.New("down")
	notFound := &fetch.StatusError{Code: 404, URL: "x"}
	cases := []struct {
		name  string
		pages []error // one per page read; nil = a page with another after it
		read  int
		want  func(error) bool
	}{
		{"first page fails", []error{down}, 1, func(err error) bool { return errors.Is(err, down) && !errors.Is(err, ErrIncomplete) }},
		{"first page 404", []error{notFound}, 1, fetch.IsNotFound},
		{"404 past the last page", []error{nil, nil, notFound}, 3, func(err error) bool { return err == nil }},
		{"later page fails", []error{nil, down}, 2, func(err error) bool { return errors.Is(err, ErrIncomplete) }},
	}
	for _, c := range cases {
		read := 0
		err := episodePages(7, 1, func(page int) (bool, error) {
			if page != 1+read {
				t.Errorf("%s: read page %d, want %d", c.name, page, 1+read)
			}
			read++
			return true, c.pages[page-1]
		})
		if read != c.read || !c.want(err) {
			t.Errorf("%s: read %d pages, err %v", c.name, read, err)
		}
	}

	read := 0
	err := episodePages(7, 0, func(page int) (bool, error) { read++; return page < 2, nil })
	if read != 3 || err != nil {
		t.Errorf("last page: read %d pages (want 3), err %v", read, err)
	}
	read = 0
	_ = episodePages(7, 0, func(int) (bool, error) { read++; return true, nil })
	if read != 50 {
		t.Errorf("endless list: read %d pages, want 50", read)
	}
}

// A failed login blocks no episode record: the next call asks for them as
// soon as TheTVDB is back. A record TheTVDB answers 404 for is blocked, and
// forgotten once its time is up.
func TestTVDBEpisodeCrewLoginBlip(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login":               `503 {}`,
		v4 + "/episodes/1/extended": `{"data":{"characters":[{"personName":"D","peopleType":"Director"}]}}`,
	}, "v4key", "", "")
	ctx := context.Background()
	if crew, pending := tv.EpisodeCrew(ctx, []int{1, 3}); len(crew) != 0 || pending {
		t.Fatalf("login down: crew %v pending %v, want none", crew, pending)
	}
	n.mu.Lock()
	n.answers[v4+"/login"] = `{"data":{"token":"t4"}}`
	n.mu.Unlock()
	tv.mu.Lock()
	tv.loginRetry = time.Time{} // the login pause is over
	tv.mu.Unlock()

	crew, pending := tv.EpisodeCrew(ctx, []int{1, 3})
	if pending || strings.Join(crew[1].Directors, ",") != "D" {
		t.Fatalf("login back: crew %+v pending %v, want episode 1's", crew, pending)
	}
	tv.crewMu.Lock()
	_, failed1 := tv.crewFailed[1]
	_, failed3 := tv.crewFailed[3]
	tv.crewMu.Unlock()
	if failed1 || !failed3 {
		t.Errorf("blocked: 1 %v 3 %v, want only 3 (404)", failed1, failed3)
	}

	tv.crewMu.Lock()
	tv.crewFailed[3] = time.Now().Add(-time.Second)
	tv.crewMu.Unlock()
	tv.queueCrew(nil)
	tv.crewMu.Lock()
	left := len(tv.crewFailed)
	tv.crewMu.Unlock()
	if left != 0 {
		t.Errorf("%d expired failures kept, want 0", left)
	}
}

// A source that isn't set up may be nil: it's just off.
func TestNilSourcesDisabled(t *testing.T) {
	var (
		a *AniDB
		v *TVDB
		d *TMDB
		m *MAL
	)
	if a.Enabled() || v.Enabled() || d.Enabled() || m.Enabled() {
		t.Error("a nil source says it's enabled")
	}
}

// tokenNet is TheTVDB v4 with expiring tokens: login n hands out token "t<n>",
// and data requests get a 401 unless they carry the good token.
type tokenNet struct {
	mu            sync.Mutex
	good          string // "" = every token is refused
	logins, calls int
}

func (n *tokenNet) RoundTrip(r *http.Request) (*http.Response, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	code, body := http.StatusOK, `{"data":{"name":"Fresh"}}`
	switch {
	case strings.HasSuffix(r.URL.Path, "/login"):
		n.logins++
		body = fmt.Sprintf(`{"data":{"token":"t%d"}}`, n.logins)
	case n.good == "" || r.Header.Get("Authorization") != "Bearer "+n.good:
		n.calls++
		code, body = http.StatusUnauthorized, `{"status":"failure","message":"Unauthorized"}`
	default:
		n.calls++
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func (n *tokenNet) counts() (logins, calls int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.logins, n.calls
}

func newTokenTest(t *testing.T, good string) (*TVDB, *tokenNet, string) {
	t.Helper()
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tokenNet{good: good}
	f.SetTransport(n)
	return NewTVDB(f, "v4key", "", ""), n, dir
}

// A 401 without a cached copy: the token is dropped and the call tried once
// more with a new one.
func TestTVDB401RetriesWithNewToken(t *testing.T) {
	tv, n, _ := newTokenTest(t, "t2")
	name, _, err := tv.Overview(context.Background(), 9)
	if err != nil || name != "Fresh" {
		t.Fatalf("overview %q %v, want the answer after a new login", name, err)
	}
	if logins, calls := n.counts(); logins != 2 || calls != 2 {
		t.Errorf("%d logins %d calls, want 2 and 2", logins, calls)
	}
}

// A 401 that a new token doesn't fix is the call's error, after one retry.
func TestTVDB401RetriedOnce(t *testing.T) {
	tv, n, _ := newTokenTest(t, "")
	if _, _, err := tv.Overview(context.Background(), 9); !unauthorized(err) {
		t.Fatalf("err %v, want the 401", err)
	}
	if logins, calls := n.counts(); logins != 2 || calls != 2 {
		t.Errorf("%d logins %d calls, want 2 and 2", logins, calls)
	}
}

// A 401 with a stale cached copy: the copy answers, and the token is dropped
// all the same, so the next call logs in again and gets a fresh answer.
func TestTVDB401WithStaleCopy(t *testing.T) {
	tv, n, dir := newTokenTest(t, "t2")
	p := filepath.Join(dir, "tvdb", "9", "eng.json")
	if err := fetch.WriteFile(p, []byte(`{"data":{"name":"Stale"}}`)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if name, _, err := tv.Overview(ctx, 9); err != nil || name != "Stale" {
		t.Fatalf("overview %q %v, want the stale copy", name, err)
	}
	if logins, calls := n.counts(); logins != 1 || calls != 1 {
		t.Errorf("%d logins %d calls, want 1 and 1 (the stale copy answers, no retry)", logins, calls)
	}
	if name, _, err := tv.Overview(ctx, 9); err != nil || name != "Fresh" {
		t.Fatalf("next overview %q %v, want a fresh answer with a new token", name, err)
	}
	if logins, calls := n.counts(); logins != 2 || calls != 2 {
		t.Errorf("%d logins %d calls, want 2 and 2", logins, calls)
	}
}
