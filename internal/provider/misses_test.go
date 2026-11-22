package provider

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"kizuna/internal/fetch"
)

func TestMissesLedger(t *testing.T) {
	down := errors.New("down")
	notFound := fmt.Errorf("tvdb text: %w", &fetch.StatusError{Code: 404, URL: "https://tvdb"})

	t.Run("a 404 or no error is no miss", func(t *testing.T) {
		var m misses
		m.failed(missTVDBText, nil)
		m.failed(missTVDBText, notFound)
		m.seasonFailed("tmdb", 2, notFound)
		if !m.empty() {
			t.Errorf("got %v, want an empty ledger", m.names())
		}
	})

	t.Run("ahead records nothing", func(t *testing.T) {
		ahead.failed(missTVDBText, down)
		ahead.add(missCrewRecords)
		ahead.seasonFailed("tmdb", 2, down)
		if !ahead.empty() {
			t.Errorf("ahead holds %v, want nothing", ahead.names())
		}
	})

	t.Run("names: misses in table order, then season art sorted", func(t *testing.T) {
		var m misses
		m.seasonFailed("tvdb", 2, down)
		m.failed(missPlexImages, down)
		m.seasonFailed("tmdb", 10, down)
		m.failed(missTVDBText, down)
		m.seasonFailed("tmdb", 2, down)
		m.seasonFailed("tmdb", 2, down) // read again: named once
		m.failed(missPlexShow, down)
		want := []string{"tvdb text", "plex", "plex images", "tmdb season 10 art", "tmdb season 2 art", "tvdb season 2 art"}
		if got := m.names(); !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("merge", func(t *testing.T) {
		var build, actor misses
		actor.add(missLaterEntries)
		build.merge(&actor)
		if !build.has(missLaterEntries) || build.empty() {
			t.Error("the build's ledger lacks the actor's miss")
		}
	})

	panics := func(t *testing.T, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		f()
	}
	t.Run("a nil ledger panics on any read", func(t *testing.T) {
		var m *misses
		panics(t, func() { m.failed(missTVDBText, nil) })
	})
	t.Run("a merged ledger panics on any read", func(t *testing.T) {
		var build, actor misses
		build.merge(&actor)
		panics(t, func() { actor.failed(missTVDBText, nil) })
		panics(t, func() { build.merge(&actor) })
	})
	t.Run("merging ahead panics and leaves it ahead", func(t *testing.T) {
		var build misses
		panics(t, func() { build.merge(ahead) })
		if !ahead.isAhead() {
			t.Error("ahead is no longer the read-ahead ledger")
		}
	})
}
