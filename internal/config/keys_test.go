package config

import (
	"strings"
	"testing"
)

// checkSettings are the settings CheckKeys reads: a test sets all of them,
// so none comes from the machine.
var checkSettings = []string{"ANIDB_CLIENT", "ANIDB_CLIENTVER", "TMDB_API_KEY", "MAL_CLIENT_ID", "PLEX_TOKEN"}

// checkKeys loads the settings with name set to value (and an AniDB client,
// so ANIDB_CLIENTVER counts) and returns CheckKeys' answer.
func checkKeys(t *testing.T, name, value string) error {
	t.Helper()
	for _, n := range checkSettings {
		t.Setenv(n, "")
	}
	t.Setenv("ANIDB_CLIENT", "myclient")
	t.Setenv(name, value)
	return Load().CheckKeys()
}

// Keys as the sites hand them out start. Spaces and line breaks around a
// value are trimmed when it is read, so they don't count.
func TestCheckKeysAccepts(t *testing.T) {
	cases := []struct{ name, value string }{
		{"ANIDB_CLIENT", "myclient"},
		{"ANIDB_CLIENT", "kizunatest2"},
		{"ANIDB_CLIENTVER", "1"},
		{"ANIDB_CLIENTVER", "12"},
		{"ANIDB_CLIENTVER", "0"},
		{"ANIDB_CLIENTVER", ""}, // the default, 1
		{"TMDB_API_KEY", "0123456789abcdef0123456789abcdef"},
		{"TMDB_API_KEY", " 0123456789ABCDEF0123456789ABCDEF\r\n"},
		{"TMDB_API_KEY", ""},
		{"MAL_CLIENT_ID", "0123456789abcdef0123456789abcdef"},
		{"PLEX_TOKEN", "aB3-x_Yz9Q0pLmN7kR2s"},
		{"PLEX_TOKEN", "eyJhbGciOi.eyJzdWIiOi.c2ln-_~"}, // a JWT-style token
		{"PLEX_TOKEN", "\tAbc123\t"},
	}
	for _, c := range cases {
		if err := checkKeys(t, c.name, c.value); err != nil {
			t.Errorf("%s=%q: %v, want it accepted", c.name, c.value, err)
		}
	}
}

// A key with a character no real key has stops the start, whichever
// setting holds it, and the error names that setting.
func TestCheckKeysRefuses(t *testing.T) {
	bad := []string{"ab&cd", "ab#cd", "ab cd", "ab\tcd", "ab\r\ncd", "ab\ncd", "ab%41", "ab+cd", "ab=cd", "ab?cd",
		"ab/cd", "ab;cd", "ab@cd", "ab\"cd", "ab\x01cd", "ab\x7fcd", "abécd"}
	for _, name := range []string{"ANIDB_CLIENT", "TMDB_API_KEY", "MAL_CLIENT_ID", "PLEX_TOKEN"} {
		for _, v := range bad {
			err := checkKeys(t, name, v)
			if err == nil || !strings.HasPrefix(err.Error(), name+" holds a character no key has") {
				t.Errorf("%s=%q: %v, want it refused", name, v, err)
			}
		}
	}
	// The environment can't hold a NUL byte: checked on a Config instead.
	if err := (Config{TMDBKey: "ab\x00cd"}).CheckKeys(); err == nil {
		t.Error("a NUL byte in TMDB_API_KEY: accepted, want it refused")
	}
	for _, v := range []string{"1.0", "v1", "-1", "+1", "1 2", "one", "99999999999999999999"} {
		if err := checkKeys(t, "ANIDB_CLIENTVER", v); err == nil || !strings.HasPrefix(err.Error(), "ANIDB_CLIENTVER is not a whole number") {
			t.Errorf("ANIDB_CLIENTVER=%q: %v, want it refused", v, err)
		}
	}
}

// Without an AniDB client nothing goes to AniDB, so its version isn't
// checked.
func TestCheckKeysClientVerWithoutClient(t *testing.T) {
	for _, n := range checkSettings {
		t.Setenv(n, "")
	}
	t.Setenv("ANIDB_CLIENTVER", "v1")
	if err := Load().CheckKeys(); err != nil {
		t.Errorf("no ANIDB_CLIENT: %v, want no error", err)
	}
}

// Every bad setting is named at once, and the error never shows a part of
// a value: it goes to the log.
func TestCheckKeysErrorHidesValue(t *testing.T) {
	for _, n := range checkSettings {
		t.Setenv(n, "")
	}
	values := map[string]string{"ANIDB_CLIENT": "QZXW&JKVY", "TMDB_API_KEY": "PQRS#TUVW", "MAL_CLIENT_ID": "ghjk\r\nmnbv",
		"PLEX_TOKEN": "XcVb wQeR", "ANIDB_CLIENTVER": "7.53"}
	for n, v := range values {
		t.Setenv(n, v)
	}
	err := Load().CheckKeys()
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for n, v := range values {
		if !strings.Contains(msg, n) {
			t.Errorf("%s isn't named in %q", n, msg)
		}
		// Any three characters in a row of the value would be a part of it.
		for i := 0; i+3 <= len(v); i++ {
			if part := v[i : i+3]; strings.Contains(msg, part) {
				t.Errorf("the error shows %q of %s's value: %q", part, n, msg)
			}
		}
	}
}
