package config

import (
	"errors"
	"strconv"
	"strings"
)

// keySettings: the keys and client names that go into a request as they
// are written: in a URL's query (ANIDB_CLIENT, TMDB_API_KEY) or in a header
// (MAL_CLIENT_ID, PLEX_TOKEN). TheTVDB's keys and PIN are left out: they
// travel in a JSON body, which escapes every character.
func (c Config) keySettings() [4]struct{ name, value string } {
	return [...]struct{ name, value string }{
		{"ANIDB_CLIENT", c.AniDBClient},
		{"TMDB_API_KEY", c.TMDBKey},
		{"MAL_CLIENT_ID", c.MALClientID},
		{"PLEX_TOKEN", c.PlexToken},
	}
}

// CheckKeys fails when a key or client name holds a character no real one
// has, or ANIDB_CLIENTVER isn't a number. Such a value is never sent: a "&"
// or "#" cuts a URL short (and the logged URL would show the key's tail), a
// line break breaks a header, and AniDB counts a bad client against the
// user. The error names the setting, never its value or a part of it.
func (c Config) CheckKeys() error {
	var bad []string
	for _, s := range c.keySettings() {
		if s.value != "" && !plainKey(s.value) {
			bad = append(bad, s.name+" holds a character no key has (only letters, digits and - . _ ~): copy it again from the site that gave it")
		}
	}
	// The version goes into AniDB's URL next to the client name. Without a
	// client nothing is sent to AniDB, so the version doesn't matter then.
	if c.AniDBClient != "" && !version(c.AniDBClientVer) {
		bad = append(bad, "ANIDB_CLIENTVER is not a whole number: set the version registered with your AniDB client, such as 1")
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

// plainKey reports whether every byte of s is one a URL carries unchanged
// (RFC 3986's unreserved characters): ASCII letters, digits, "-", ".", "_"
// and "~". Every key these sites hand out fits: TheMovieDb's and
// MyAnimeList's are hex, a Plex token is letters, digits, "-" and "_", and
// an AniDB client name is a plain lower-case word.
func plainKey(s string) bool {
	for i := range len(s) {
		b := s[i]
		if !('a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~') {
			return false
		}
	}
	return true
}

// version reports whether s is a whole number, as AniDB's clientver is.
// Digits only: strconv alone would also take a sign.
func version(s string) bool {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil // too many digits to be a version
}
