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

// A failed login blocks no episode record and leaves the crew pending: the
// next call asks for them as soon as TheTVDB is back. A record TheTVDB
// answers 404 for is blocked, not pending, and forgotten once its time is up.
func TestTVDBEpisodeCrewLoginBlip(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login":               `503 {}`,
		v4 + "/episodes/1/extended": `{"data":{"characters":[{"personName":"D","peopleType":"Director"}]}}`,
	}, "v4key", "", "")
	ctx := context.Background()
	if crew, pending := tv.EpisodeCrew(ctx, []int{1, 3}, time.Time{}); len(crew) != 0 || !pending {
		t.Fatalf("login down: crew %v pending %v, want none, pending", crew, pending)
	}
	if c := n.count(v4+"/login") + n.count(v4+"/episodes/1/extended") + n.count(v4+"/episodes/3/extended"); c != 1 {
		t.Errorf("login down: %d requests, want 1 (the login; no record is asked without a token)", c)
	}
	n.mu.Lock()
	n.answers[v4+"/login"] = `{"data":{"token":"t4"}}`
	n.mu.Unlock()
	tv.mu.Lock()
	tv.loginRetry = time.Time{} // the login pause is over
	tv.mu.Unlock()

	crew, pending := tv.EpisodeCrew(ctx, []int{1, 3}, time.Time{})
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
	tv.queueCrew(nil, crewTTL)
	tv.crewMu.Lock()
	left := len(tv.crewFailed)
	tv.crewMu.Unlock()
	if left != 0 {
		t.Errorf("%d expired failures kept, want 0", left)
	}
}

// downNet is tvdbNet with paths that fail the way a dead connection does:
// an error, no answer from TheTVDB.
type downNet struct {
	*tvdbNet
	down map[string]bool // by "host/path"; under tvdbNet's mu
}

func (n *downNet) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL.Host + r.URL.Path
	n.mu.Lock()
	down := n.down[u]
	if down {
		n.asked = append(n.asked, u)
	}
	n.mu.Unlock()
	if down {
		return nil, errors.New("connection reset")
	}
	return n.tvdbNet.RoundTrip(r)
}

// A record whose request failed without an answer from TheTVDB (a network
// error) leaves the crew pending, so the show is rebuilt soon. The other
// records' crew is returned. The record isn't blocked, and nothing asks for
// it again before the next call, which gets it once the network is back.
func TestTVDBEpisodeCrewNetworkError(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := &downNet{tvdbNet: &tvdbNet{answers: map[string]string{
		v4 + "/login":               `{"data":{"token":"t4"}}`,
		v4 + "/episodes/1/extended": `{"data":{"characters":[{"personName":"D1","peopleType":"Director"}]}}`,
		v4 + "/episodes/2/extended": `{"data":{"characters":[{"personName":"D2","peopleType":"Director"}]}}`,
		v4 + "/episodes/3/extended": `{"data":{"characters":[{"personName":"D3","peopleType":"Director"}]}}`,
	}}, down: map[string]bool{v4 + "/episodes/2/extended": true}}
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "")
	ctx := context.Background()

	crew, pending := tv.EpisodeCrew(ctx, []int{1, 2, 3}, time.Time{})
	if !pending {
		t.Error("record 2 failed on the network: want pending, so the show is rebuilt soon")
	}
	if _, ok := crew[2]; ok || strings.Join(crew[1].Directors, ",") != "D1" || strings.Join(crew[3].Directors, ",") != "D3" {
		t.Errorf("crew %+v, want records 1 and 3 only", crew)
	}
	tv.crewQueue.Wait() // a retry in the background would be counted too
	tv.crewMu.Lock()
	_, blocked := tv.crewFailed[2]
	tv.crewMu.Unlock()
	if c := n.count(v4 + "/episodes/2/extended"); c != 1 || blocked {
		t.Errorf("record 2 asked %d times, blocked %v; want 1, not blocked", c, blocked)
	}

	n.mu.Lock()
	n.down = nil // the network is back
	n.mu.Unlock()
	crew, pending = tv.EpisodeCrew(ctx, []int{1, 2, 3}, time.Time{})
	if pending || strings.Join(crew[2].Directors, ",") != "D2" {
		t.Errorf("network back: crew %+v pending %v, want record 2's, not pending", crew, pending)
	}
	if c := n.count(v4 + "/episodes/2/extended"); c != 2 {
		t.Errorf("record 2 asked %d times in two calls, want 2", c)
	}
	if c := n.count(v4 + "/episodes/1/extended"); c != 1 {
		t.Errorf("record 1 asked %d times, want 1 (cached)", c)
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

// A stale cached episode record is served and refreshed in the background,
// but not while a refresh of it has failed lately (crewRetry).
func TestTVDBStaleCrewFailedNotRefreshed(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		v4 + "/login": `{"data":{"token":"t4"}}`,
		// 7: 404 now
		v4 + "/episodes/8/extended": `{"data":{"characters":[{"personName":"New","peopleType":"Director"}]}}`,
	}}
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "")
	old := time.Now().Add(-crewTTL - time.Hour)
	for _, id := range []int{7, 8} {
		p := filepath.Join(dir, "tvdb", "episodes", fmt.Sprintf("%d.json", id))
		if err := fetch.WriteFile(p, []byte(`{"data":{"characters":[{"personName":"Old","peopleType":"Director"}]}}`)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for build := range 3 {
		crew, _ := tv.EpisodeCrew(ctx, []int{7, 8}, time.Time{})
		tv.crewQueue.Wait()
		if build == 0 && (strings.Join(crew[7].Directors, ",") != "Old" || strings.Join(crew[8].Directors, ",") != "Old") {
			t.Fatalf("stale crew %+v, want the cached copies", crew)
		}
	}
	if c := n.count(v4 + "/episodes/7/extended"); c != 1 {
		t.Errorf("failed refresh asked %d times in 3 builds, want 1", c)
	}
	if c := n.count(v4 + "/episodes/8/extended"); c != 1 {
		t.Errorf("refreshed record asked %d times, want 1 (fresh since)", c)
	}
}

// A series' episode records are kept 30 days once its latest episode aired
// 60 days ago or more, else 7: the same line as AniDB's. A partial date
// counts from the last day it can mean (LastDay); none known, or one still
// to come, is "airing".
func TestCrewLife(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	day := func(s string) time.Time {
		d, ok := LastDay(s)
		if !ok {
			t.Fatalf("LastDay(%q) unreadable", s)
		}
		return d
	}
	for _, c := range []struct {
		name string
		last time.Time
		want time.Duration
	}{
		{"unknown", time.Time{}, crewTTL},
		{"next episode still to come", day("2026-12-24"), crewTTL},
		{"aired today", now, crewTTL},
		{"59 days ago", now.Add(-59 * 24 * time.Hour), crewTTL},
		{"60 days ago, less a second", now.Add(-60*24*time.Hour + time.Second), crewTTL},
		{"60 days ago", now.Add(-60 * 24 * time.Hour), crewDoneTTL},
		{"61 days ago", now.Add(-61 * 24 * time.Hour), crewDoneTTL},
		{"years ago", day("1999-10-20"), crewDoneTTL},
		{"2026: the year isn't over", day("2026"), crewTTL},
		{"2025", day("2025"), crewDoneTTL},
		{"2026-09: its last day, 2026-09-30, 46 days ago", day("2026-09"), crewTTL},
		{"2026-08: 2026-08-31, 76 days ago", day("2026-08"), crewDoneTTL},
	} {
		if got := crewLife(c.last, now); got != c.want {
			t.Errorf("%s: crewLife = %v, want %v", c.name, got, c.want)
		}
	}
	if _, ok := LastDay(""); ok {
		t.Error(`LastDay(""): want unreadable`)
	}
}

// A cached record past 7 days is refreshed in the background for a series
// still airing, not for one finished long ago (30 days); past 30 days, for
// both. It answers from the cache at once either way.
func TestTVDBCrewLifeByLastAired(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		lastAired time.Time
		age       time.Duration
		refreshed bool
	}{
		{"airing, 8 days old", now.Add(-3 * 24 * time.Hour), 8 * 24 * time.Hour, true},
		{"ended long ago, 8 days old", now.Add(-1000 * 24 * time.Hour), 8 * 24 * time.Hour, false},
		{"airing, 31 days old", now.Add(-3 * 24 * time.Hour), 31 * 24 * time.Hour, true},
		{"ended long ago, 31 days old", now.Add(-1000 * 24 * time.Hour), 31 * 24 * time.Hour, true},
		{"unknown, 8 days old", time.Time{}, 8 * 24 * time.Hour, true},
	} {
		dir := t.TempDir()
		f := fetch.New(dir)
		n := &tvdbNet{answers: map[string]string{
			v4 + "/login":               `{"data":{"token":"t4"}}`,
			v4 + "/episodes/7/extended": `{"data":{"characters":[{"personName":"New","peopleType":"Director"}]}}`,
		}}
		f.SetTransport(n)
		tv := NewTVDB(f, "v4key", "", "")
		p := filepath.Join(dir, "tvdb", "episodes", "7.json")
		if err := fetch.WriteFile(p, []byte(`{"data":{"characters":[{"personName":"Old","peopleType":"Director"}]}}`)); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, c.age)
		crew, pending := tv.EpisodeCrew(context.Background(), []int{7}, c.lastAired)
		tv.crewQueue.Wait()
		if strings.Join(crew[7].Directors, ",") != "Old" || pending {
			t.Errorf("%s: crew %+v pending %v, want the cached copy at once", c.name, crew, pending)
		}
		want := 0
		if c.refreshed {
			want = 1
		}
		if got := n.count(v4 + "/episodes/7/extended"); got != want {
			t.Errorf("%s: record asked %d times, want %d", c.name, got, want)
		}
	}
}

// With both keys, a call whose v4 answer is cached and fresh doesn't log in
// first (that login could hang on a TheTVDB that's down); one that isn't
// cached does, and gets its answer.
func TestTVDBBothKeysCachedNoLogin(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		v4 + "/login":                      `{"data":{"token":"t4"}}`,
		v4 + "/series/10/translations/eng": `{"data":{"name":"Ten"}}`,
	}}
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "legacykey")
	if err := fetch.WriteFile(filepath.Join(dir, "tvdb", "9", "eng.json"), []byte(`{"data":{"name":"Cached"}}`)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if name, _, err := tv.Overview(ctx, 9); err != nil || name != "Cached" {
		t.Fatalf("cached overview %q %v", name, err)
	}
	if c := n.count(v4 + "/login"); c != 0 {
		t.Errorf("logged in %d times for a cached answer, want 0", c)
	}
	if name, _, err := tv.Overview(ctx, 10); err != nil || name != "Ten" {
		t.Fatalf("overview %q %v", name, err)
	}
	if c := n.count(v4 + "/login"); c != 1 {
		t.Errorf("logged in %d times, want 1", c)
	}
}

// With both keys, EpisodeCrew that finds the v4 key rejected says pending:
// the build's episodes may have come from v4's cache, which has no crew,
// and the rebuild takes v3's lists, which do.
func TestTVDBCrewPendingOnKeySwitch(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login": `401 {"status":"failure"}`,
		v3 + "/login": `{"token":"t3"}`,
	}, "v4key", "", "legacy")
	if crew, pending := tv.EpisodeCrew(context.Background(), []int{7}, time.Time{}); crew != nil || !pending {
		t.Fatalf("crew %v pending %v at the switch; want none, pending", crew, pending)
	}
	if tv.API() != "v3" {
		t.Fatalf("API %q, want v3", tv.API())
	}
	if crew, pending := tv.EpisodeCrew(context.Background(), []int{7}, time.Time{}); crew != nil || pending {
		t.Errorf("crew %v pending %v after the switch; want none, not pending", crew, pending)
	}
	if c := n.count(v4 + "/episodes/7/extended"); c != 0 {
		t.Errorf("v4 episode record asked %d times, want 0", c)
	}
}

// Background crew fetches wait in the throttle's background queue, behind
// every request a build waits for.
func TestTVDBBackgroundCrewYields(t *testing.T) {
	const host = "api4.thetvdb.com"
	tv, _ := newTVDBTest(t, map[string]string{
		v4 + "/login":               `{"data":{"token":"t4"}}`,
		v4 + "/series/8/artworks":   `{"data":{"artworks":[]}}`,
		v4 + "/episodes/7/extended": `{"data":{"characters":[]}}`,
	}, "v4key", "", "")
	if _, err := tv.Art(context.Background(), 8); err != nil { // logs in and takes the slot
		t.Fatal(err)
	}
	tv.f.Throttle(host, time.Hour) // the record waits until the test opens the gap
	tv.queueCrew([]int{7}, crewTTL)
	var fg, bg int
	waitQueue(t, tv.f, host, "the crew record to queue", func(f, b int) bool { fg, bg = f, b; return f+b > 0 })
	if fg != 0 || bg != 1 {
		t.Errorf("%d foreground, %d background requests waiting; want 0, 1", fg, bg)
	}
	tv.f.Throttle(host, 0)
	tv.crewQueue.Wait()
}

// A failed login (TheTVDB down, or the key rejected) is answered by the
// expired cached copy, as a failed download is; without one, or for a
// caller that asks for no stale copy, it's the call's error.
func TestTVDBLoginDownServesStale(t *testing.T) {
	for _, login := range []string{`503 {}`, `401 {"status":"failure"}`} {
		dir := t.TempDir()
		f := fetch.New(dir)
		n := &tvdbNet{answers: map[string]string{v4 + "/login": login}}
		f.SetTransport(n)
		tv := NewTVDB(f, "v4key", "", "")
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
			t.Errorf("login %s: overview %q %v, want the stale copy", login, name, err)
		}
		var r struct{}
		if err := tv.getAs(ctx, false, getOpts{noStale: true}, "/series/9/translations/eng", "9/eng.json", seriesFilesTTL, &r); !errors.Is(err, errTVDBLogin) {
			t.Errorf("login %s, no stale copy asked: err %v, want the login error", login, err)
		}
		if _, _, err := tv.Overview(ctx, 10); !errors.Is(err, errTVDBLogin) {
			t.Errorf("login %s, nothing cached: err %v, want the login error", login, err)
		}
	}
}

// A renewal that fails without rejecting the key leaves the old token in
// use while TheTVDB still accepts it; a rejection drops it. The renewal of
// a valid token runs in the background, so the call that starts it still
// gets the old token.
func TestTVDBFailedRenewalKeepsToken(t *testing.T) {
	cases := []struct {
		login string
		age   time.Duration
		ok    [2]bool // the first and the second call
	}{
		{`503 {}`, 8 * 24 * time.Hour, [2]bool{true, true}},
		{`503 {}`, 29 * 24 * time.Hour, [2]bool{false, false}}, // past its month
		{`401 {"status":"failure"}`, 8 * 24 * time.Hour, [2]bool{true, false}},
	}
	for _, c := range cases {
		tv, n := newTVDBTest(t, map[string]string{
			v4 + "/login":                     c.login,
			v4 + "/series/9/translations/eng": `{"data":{"name":"Show"}}`,
			v4 + "/series/8/translations/eng": `{"data":{"name":"Other"}}`,
		}, "v4key", "", "")
		tv.mu.Lock()
		tv.token, tv.tokenAt = "t-old", time.Now().Add(-c.age)
		tv.mu.Unlock()
		ctx := context.Background()
		for i, id := range []int{9, 8} { // the second call is in the login pause
			name, _, err := tv.Overview(ctx, id)
			if got := err == nil && name != ""; got != c.ok[i] {
				t.Errorf("login %s, token %v old, series %d: %q %v, want ok %v", c.login, c.age, id, name, err, c.ok[i])
			}
			tv.WaitIdle() // the renewal's outcome is recorded
		}
		if logins := n.count(v4 + "/login"); logins != 1 {
			t.Errorf("login %s, token %v old: %d logins, want 1", c.login, c.age, logins)
		}
	}
}

// A login TheTVDB answers 404 is a failed login, not TheTVDB having no such
// record: callers read a 404 as "nothing there". The same holds for a
// renewal's 404, which a call gets once the old token is no longer valid.
func TestTVDBLogin404IsNotNotFound(t *testing.T) {
	answers := map[string]string{
		v4 + "/login":                     `404 {}`,
		v4 + "/series/9/translations/eng": `{"data":{"name":"Show"}}`,
	}
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, errTVDBLogin) || fetch.IsNotFound(err) {
			t.Errorf("%s 404: err %v, not found %v; want a failed login, not a not-found", what, err, fetch.IsNotFound(err))
		}
	}
	ctx := context.Background()
	tv, _ := newTVDBTest(t, answers, "v4key", "", "")
	_, _, err := tv.Overview(ctx, 9)
	check("login", err)

	tv, _ = newTVDBTest(t, answers, "v4key", "", "")
	tv.mu.Lock()
	tv.token, tv.tokenAt = "t-old", time.Now().Add(-8*24*time.Hour) // due for renewal, valid for weeks
	tv.mu.Unlock()
	if _, _, err := tv.Overview(ctx, 9); err != nil {
		t.Fatalf("token due for renewal: %v, want the old token used", err)
	}
	tv.WaitIdle() // the renewal's outcome is recorded
	tv.mu.Lock()
	tv.tokenAt = time.Now().Add(-29 * 24 * time.Hour) // past its month
	tv.mu.Unlock()
	_, _, err = tv.Overview(ctx, 8)
	check("renewal", err)
}

// A renewal that ends between a call's login and its download makes fetch
// look in the cache again (a download ended). The call keeps the token it
// got: the renewal's outcome, a rejected key included, is for the calls
// after it. The test holds the call inside its ready until the renewal is
// over. CI's race run once failed TestTVDBFailedRenewalKeepsToken this way,
// when fetch ran ready again on a look-again.
func TestTVDBRenewalEndsDuringCall(t *testing.T) {
	for _, login := range []string{`401 {"status":"failure"}`, `503 {}`, `{"data":{"token":"t-new"}}`} {
		tv, _ := newTVDBTest(t, map[string]string{
			v4 + "/login":                     login,
			v4 + "/series/9/translations/eng": `{"data":{"name":"Show"}}`,
		}, "v4key", "", "")
		tv.mu.Lock()
		tv.token, tv.tokenAt = "t-old", time.Now().Add(-8*24*time.Hour) // due for renewal, valid for weeks
		tv.mu.Unlock()
		ctx := context.Background()
		r, ready := tv.request(ctx, false, getOpts{}, "/series/9/translations/eng", "9/eng.json", seriesFilesTTL)
		readies := 0
		var out struct {
			Data struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		err := tv.f.GetJSON(ctx, r, &out, func(req fetch.Request) (fetch.Request, error) {
			readies++
			req, err := ready(req) // starts the renewal
			tv.WaitIdle()          // the renewal's login ended and its outcome is recorded
			return req, err
		})
		if err != nil || out.Data.Name != "Show" {
			t.Errorf("login %s: %q %v, want Show", login, out.Data.Name, err)
		}
		if readies != 1 {
			t.Errorf("login %s: ready ran %d times, want 1", login, readies)
		}
	}
}

// A renewal still waiting on the network doesn't hold up calls: the old
// token is still valid, so they go ahead with it.
func TestTVDBRenewalDoesNotBlock(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := slowLogin{&tvdbNet{answers: map[string]string{
		v4 + "/login":                     `{"data":{"token":"t-new"}}`,
		v4 + "/series/9/translations/eng": `{"data":{"name":"Show"}}`,
		v4 + "/series/8/translations/eng": `{"data":{"name":"Other"}}`,
	}}, make(chan struct{}, 10), make(chan struct{})}
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "")
	tv.mu.Lock()
	tv.token, tv.tokenAt = "t-old", time.Now().Add(-8*24*time.Hour) // due for renewal, valid for weeks
	tv.mu.Unlock()

	renewed := make(chan error, 1)
	go func() {
		_, _, err := tv.Overview(context.Background(), 9) // starts the renewal
		renewed <- err
	}()
	<-n.started // the renewal is now waiting on the network

	// Before the fix this waited for the renewal until its ctx ended.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name, _, err := tv.Overview(ctx, 8)
	if err != nil || name != "Other" {
		t.Errorf("during the renewal: %q %v, want Other", name, err)
	}
	close(n.release)
	if err := <-renewed; err != nil {
		t.Errorf("renewing call: %v", err)
	}
	tv.WaitIdle() // the renewal itself
	tv.mu.Lock()
	token := tv.token
	tv.mu.Unlock()
	if token != "t-new" {
		t.Errorf("token %q after the renewal, want t-new", token)
	}
}

// The call that starts a renewal doesn't wait for it either: with a login
// that hangs, it and the calls after it go ahead with the old token, and
// only one login goes out.
func TestTVDBRenewerDoesNotStall(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := slowLogin{&tvdbNet{answers: map[string]string{
		v4 + "/login":                     `{"data":{"token":"t-new"}}`,
		v4 + "/series/9/translations/eng": `{"data":{"name":"Show"}}`,
		v4 + "/series/8/translations/eng": `{"data":{"name":"Other"}}`,
	}}, make(chan struct{}, 10), make(chan struct{})}
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "")
	tv.mu.Lock()
	tv.token, tv.tokenAt = "t-old", time.Now().Add(-8*24*time.Hour) // due for renewal, valid for weeks
	tv.mu.Unlock()

	// Before the fix the first call waited on the login until its ctx ended.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, c := range []struct {
		id   int
		want string
	}{{9, "Show"}, {8, "Other"}} {
		if name, _, err := tv.Overview(ctx, c.id); err != nil || name != c.want {
			t.Errorf("series %d while the login hangs: %q %v, want %s", c.id, name, err, c.want)
		}
	}
	<-n.started // the renewal reached the network
	close(n.release)
	tv.WaitIdle()
	if len(n.started) != 0 {
		t.Errorf("%d more logins, want 1 in all", len(n.started))
	}
	tv.mu.Lock()
	token := tv.token
	tv.mu.Unlock()
	if token != "t-new" {
		t.Errorf("token %q after the renewal, want t-new", token)
	}
}

// hangingNet never answers: a request on it ends only with its caller's ctx.
type hangingNet struct{ release chan struct{} }

func (n *hangingNet) RoundTrip(r *http.Request) (*http.Response, error) {
	<-n.release
	return nil, errors.New("released")
}

// staleEng caches TheTVDB's series 9 text in folder, older than any TTL.
func staleEng(t *testing.T, dir, folder string) {
	t.Helper()
	p := filepath.Join(dir, folder, "9", "eng.json")
	if err := fetch.WriteFile(p, []byte(`{"data":{"name":"Stale"}}`)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// A login that fails because the caller gave up is the call's error: the
// stale copy doesn't answer a caller that is gone.
func TestTVDBLoginCancelledNoStale(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &hangingNet{release: make(chan struct{})}
	t.Cleanup(func() { close(n.release) })
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "")
	staleEng(t, dir, "tvdb")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the login's request can only end with ctx
	var r struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	err := tv.get(ctx, false, "/series/9/translations/eng", "9/eng.json", seriesFilesTTL, &r)
	if !errors.Is(err, errTVDBLogin) || !errors.Is(err, context.Canceled) || r.Data.Name != "" {
		t.Errorf("%q, %v; want the cancelled login's error, no stale copy", r.Data.Name, err)
	}
}

// A v4 call that finds the client switched to the legacy key gives up with
// errTVDBSwitched, even with a stale v4 copy on disk.
func TestTVDBSwitchedNoStale(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(&tvdbNet{answers: map[string]string{v3 + "/login": `{"token":"t3"}`}})
	tv := NewTVDB(f, "v4key", "", "legacykey")
	tv.mu.Lock()
	tv.legacy = true // switched since the call picked v4
	tv.mu.Unlock()
	staleEng(t, dir, "tvdb")
	var r struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	err := tv.get(context.Background(), false, "/series/9/translations/eng", "9/eng.json", seriesFilesTTL, &r)
	if !errors.Is(err, errTVDBSwitched) || r.Data.Name != "" {
		t.Errorf("%q, %v; want errTVDBSwitched, no stale copy", r.Data.Name, err)
	}
}

// With both keys the start-up log says which one is used, once, when the
// client is made; with one key it says nothing.
func TestTVDBBothKeysNoted(t *testing.T) {
	const note = `msg="TVDB_API_KEY and TVDB_LEGACY_API_KEY are both set: using the v4 key; ` +
		`the legacy key takes over (with a warning) if TheTVDB rejects it"`
	for _, c := range []struct{ key, legacy string }{{"v4key", "legacykey"}, {"v4key", ""}, {"", "legacykey"}} {
		logs := captureLogs(t)
		NewTVDB(fetch.New(t.TempDir()), c.key, "", c.legacy)
		want := 0
		if c.key != "" && c.legacy != "" {
			want = 1
		}
		if n := strings.Count(logs.String(), note); n != want {
			t.Errorf("keys %q %q: the note logged %d times, want %d:\n%s", c.key, c.legacy, n, want, logs)
		}
	}
}
