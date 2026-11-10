package plex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakePlex answers the three calls the Renamer makes and notes them.
type fakePlex struct {
	shows  string            // the answer to /library/all
	extras map[string]string // show rating key -> the answer to its /extras
	status int               // answered instead of 200 when set
	calls  []string          // "GET /library/all?…", in order
}

func (f *fakePlex) RoundTrip(req *http.Request) (*http.Response, error) {
	if got := req.Header.Get("X-Plex-Token"); got != "secret" {
		return nil, fmt.Errorf("token header = %q", got)
	}
	if strings.Contains(req.URL.String(), "secret") {
		return nil, errors.New("the token is in the URL")
	}
	f.calls = append(f.calls, req.Method+" "+req.URL.RequestURI())
	body, status := "{}", http.StatusOK
	switch {
	case f.status != 0:
		status = f.status
	case req.URL.Path == "/library/all":
		body = f.shows
	case strings.HasSuffix(req.URL.Path, "/extras"):
		body = f.extras[strings.Split(req.URL.Path, "/")[3]]
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func extra(key, title, subtype, file string) string {
	return fmt.Sprintf(`{"ratingKey":%q,"title":%q,"subtype":%q,"Media":[{"Part":[{"file":%q}]}]}`, key, title, subtype, file)
}

func container(items ...string) string {
	return `{"MediaContainer":{"Metadata":[` + strings.Join(items, ",") + `]}}`
}

const guid = "tv.plex.agents.custom.kizuna.anidb://show/anidb-23"

func TestRename(t *testing.T) {
	plex := &fakePlex{
		shows: container(`{"ratingKey":"100","title":"Show"}`, `{"ratingKey":"200","title":"Show"}`),
		extras: map[string]string{
			"100": container(
				extra("101", "OP01", "opening", "/media/Show/Show - OP01.mkv"),
				extra("102", "OP01a", "opening", "/media/Show/Show - OP01a.mkv"),
				extra("103", "Opening 2", "opening", "/media/Show/Show - OP02.mkv"), // already right
				extra("104", "NCED1", "ending", "/media/Show/Show - NCED1.mkv"),
				extra("105", "ED1", "ending", "/media/Show/Show - ED1.mkv"),
				extra("106", "Trailer", "trailer", "/media/Show/Trailers/OP1.mkv"), // not an opening for Plex
				`{"ratingKey":"107","title":"OP9","subtype":"opening"}`,            // no file
				extra("108", "Odd", "opening", "/media/Show/Show - Special.mkv"),   // no pattern in the name
			),
			"200": container(extra("201", "NCOP", "opening", `D:\Anime\Show\NCOP.mkv`)),
		},
	}
	r := New("http://plex:32400/", "secret", plex)
	if err := r.Rename(context.Background(), guid); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /library/all?type=2&guid=tv.plex.agents.custom.kizuna.anidb%3A%2F%2Fshow%2Fanidb-23",
		"GET /library/metadata/100/extras",
		"PUT /library/metadata/101?title.value=Opening+1",
		"PUT /library/metadata/102?title.value=Opening+1a",
		"PUT /library/metadata/104?title.value=Ending+1+%28creditless%29",
		"PUT /library/metadata/105?title.value=Ending+1",
		"GET /library/metadata/200/extras",
		"PUT /library/metadata/201?title.value=Opening",
	}
	if !slices.Equal(plex.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(plex.calls, "\n"), strings.Join(want, "\n"))
	}
}

// A show Plex doesn't have, and one without extras, need no change.
func TestRenameNothingToDo(t *testing.T) {
	for _, plex := range []*fakePlex{
		{shows: container()},
		{shows: `{"MediaContainer":{"size":0}}`},
		{shows: container(`{"ratingKey":"100"}`), extras: map[string]string{"100": `{"MediaContainer":{"size":0}}`}},
	} {
		if err := New("http://plex:32400", "secret", plex).Rename(context.Background(), guid); err != nil {
			t.Error(err)
		}
		for _, c := range plex.calls {
			if strings.HasPrefix(c, "PUT") {
				t.Errorf("unexpected %s", c)
			}
		}
	}
}

// A failure names the call and Plex's status, never the token.
func TestRenameError(t *testing.T) {
	err := New("http://plex:32400", "secret", &fakePlex{status: http.StatusUnauthorized}).Rename(context.Background(), guid)
	if err == nil || !strings.Contains(err.Error(), "GET /library/all: status 401") || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v", err)
	}
	err = New("http://plex:32400", "secret", &fakePlex{shows: "<html>"}).Rename(context.Background(), guid)
	if err == nil || !strings.Contains(err.Error(), "/library/all") {
		t.Errorf("err = %v", err)
	}
}

// Queue plans both looks once per show, however often Plex asks, and plans
// them again once they are over.
func TestQueue(t *testing.T) {
	plex := &fakePlex{shows: container()}
	r := New("http://plex:32400", "secret", plex)
	type planned struct {
		after time.Duration
		run   func()
	}
	var plan []planned
	r.after = func(d time.Duration, f func()) { plan = append(plan, planned{d, f}) }

	r.Queue(guid)
	r.Queue(guid)
	if len(plan) != len(passes) || plan[0].after != passes[0] || plan[1].after != passes[1] {
		t.Fatalf("planned %d looks, want one per pass (%v)", len(plan), passes)
	}
	for _, p := range plan {
		p.run()
	}
	if len(plex.calls) != len(passes) {
		t.Errorf("%d calls, want %d: %v", len(plex.calls), len(passes), plex.calls)
	}
	plan = nil
	r.Queue(guid)
	if len(plan) != len(passes) {
		t.Errorf("a later refresh planned %d looks, want %d", len(plan), len(passes))
	}

	// After Stop nothing is planned, and a look already planned does nothing.
	plex.calls = nil
	r.Stop()
	for _, p := range plan {
		p.run()
	}
	r.Queue("other")
	if len(plex.calls) != 0 || len(plan) != len(passes) {
		t.Errorf("after Stop: calls %v, %d planned", plex.calls, len(plan))
	}
}

// More shows than maxPending aren't planned: memory stays bounded.
func TestQueueBounded(t *testing.T) {
	r := New("http://plex:32400", "secret", &fakePlex{})
	planned := 0
	r.after = func(time.Duration, func()) { planned++ }
	for i := range maxPending + 5 {
		r.Queue(fmt.Sprint("show-", i))
	}
	if planned != maxPending*len(passes) {
		t.Errorf("planned %d looks, want %d", planned, maxPending*len(passes))
	}
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"http://192.168.1.10:32400", "https://plex.example.com", "http://plex:32400/"} {
		if why := CheckURL(ok); why != "" {
			t.Errorf("CheckURL(%q) = %q", ok, why)
		}
	}
	for _, bad := range []string{"192.168.1.10:32400", "plex", "ftp://plex", "http://", "http://a b"} {
		if CheckURL(bad) == "" {
			t.Errorf("CheckURL(%q) accepted", bad)
		}
	}
}
