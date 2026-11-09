package provider

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// A show or season match with includeChildren=1 carries its seasons or
// episodes inline, the same items .../children lists; without it, none.
func TestMatchIncludeChildren(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	serve := func(method, path, body string) Container {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		var c Container
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || rec.Code != 200 {
			t.Fatalf("%s %s: HTTP %d %s (%v)", method, path, rec.Code, rec.Body, err)
		}
		return c
	}
	const matches = "/tvdb/library/metadata/matches"
	for _, c := range []struct{ body, children string }{
		{`{"type":2,"title":"Kizuna Test","includeChildren":1}`, "/tvdb/library/metadata/tvdb-500/children"},
		{`{"type":3,"parentTitle":"Kizuna Test","index":1,"includeChildren":"1"}`, "/tvdb/library/metadata/tvdb-500-s1/children"},
	} {
		got := serve("POST", matches, c.body).MediaContainer.Metadata
		if len(got) != 1 || got[0].Children == nil {
			t.Fatalf("%s: got %+v, want one item with children", c.body, got)
		}
		want := serve("GET", c.children, "").MediaContainer.Metadata
		if len(want) == 0 || got[0].Children.Size != len(want) || !reflect.DeepEqual(got[0].Children.Metadata, want) {
			t.Errorf("%s: children %+v, want %+v", c.body, got[0].Children, want)
		}
	}
	for _, body := range []string{`{"type":2,"title":"Kizuna Test"}`, `{"type":3,"parentTitle":"Kizuna Test","index":1,"includeChildren":0}`} {
		if got := serve("POST", matches, body).MediaContainer.Metadata; len(got) != 1 || got[0].Children != nil {
			t.Errorf("%s: got %+v, want one item without children", body, got)
		}
	}
}
