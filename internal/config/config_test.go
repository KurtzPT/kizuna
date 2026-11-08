package config

import (
	"slices"
	"testing"
)

func TestSourceList(t *testing.T) {
	t.Setenv("X_SOURCES", " TVDB, foo, anidb, tvdb ")
	if got := sourceList("X_SOURCES", "anidb", "anidb", "tvdb"); !slices.Equal(got, []string{"tvdb", "anidb"}) {
		t.Errorf("got %v", got)
	}
	t.Setenv("X_SOURCES", "none")
	if got := sourceList("X_SOURCES", "anidb", "anidb"); got != nil {
		t.Errorf("none: %v", got)
	}
	if got := sourceList("UNSET_SOURCES", "tmdb,tvdb", "tvdb", "tmdb"); !slices.Equal(got, []string{"tmdb", "tvdb"}) {
		t.Errorf("default: %v", got)
	}
}

func TestSize(t *testing.T) {
	cases := map[string]uint64{"": 7, "500": 500 << 20, "500MB": 500 << 20, "2gb": 2 << 30, "1.5 GB": 3 << 29, "1TB": 1 << 40, "0": 0, "lots": 7}
	for v, want := range cases {
		t.Setenv("X_SIZE", v)
		if got := size("X_SIZE", 7); got != want {
			t.Errorf("%q: got %d, want %d", v, got, want)
		}
	}
}

func TestListenAddr(t *testing.T) {
	cases := []struct{ listen, port, want string }{
		{"", "", ":32600"},                            // default
		{"", "32700", ":32700"},                       // PORT
		{"", "70000", ":32600"},                       // out of range: default
		{"", "abc", ":32600"},                         // not a number: default
		{"127.0.0.1:9000", "32700", "127.0.0.1:9000"}, // LISTEN wins
	}
	for _, c := range cases {
		t.Setenv("LISTEN", c.listen)
		t.Setenv("PORT", c.port)
		if got := listenAddr(); got != c.want {
			t.Errorf("LISTEN=%q PORT=%q: got %q, want %q", c.listen, c.port, got, c.want)
		}
	}
}
