package anidb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kizuna/internal/fetch"
)

// BenchmarkSearch runs title searches on a real dump:
//
//	KIZUNA_TITLES=/path/to/animetitles.xml go test ./internal/anidb -run x -bench Search
func BenchmarkSearch(b *testing.B) {
	src := os.Getenv("KIZUNA_TITLES")
	if src == "" {
		b.Skip("set KIZUNA_TITLES to an animetitles.xml")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "anidb"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anidb", "animetitles.xml"), data, 0o644); err != nil {
		b.Fatal(err)
	}
	t := &Titles{}
	if err := t.Load(context.Background(), fetch.New(dir)); err != nil {
		b.Fatal(err)
	}
	queries := []string{"Shingeki no Kyojin", "Hunter x Hunter", "Love Live! School Idol Project", "One Piece",
		"Sword Art Online Alicization War of Underworld", "The Melancholy of Haruhi Suzumiya", "Re:Zero kara Hajimeru Isekai Seikatsu"}
	b.ReportAllocs()
	for b.Loop() {
		for _, q := range queries {
			t.Search(q, 0, 20)
		}
	}
}
