package logfile

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func names(t *testing.T, dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	var out []string
	for _, f := range files {
		out = append(out, filepath.Base(f))
	}
	slices.Sort(out)
	return out
}

func TestDailyRotationAndRetention(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 1, 23, 59, 0, 0, time.Local)
	l, err := open(dir, "kizuna", 3, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	for day := 0; day < 6; day++ { // 1 Nov ... 6 Nov
		_, _ = l.Write([]byte("line\n"))
		clock = clock.Add(24 * time.Hour)
	}
	_, _ = l.Write([]byte("line\n")) // 7 Nov: rotates 6 Nov, deletes up to 3 Nov
	l.Close()
	want := []string{"kizuna-2026-11-04.log", "kizuna-2026-11-05.log", "kizuna-2026-11-06.log", "kizuna.log"}
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kizuna-2026-11-05.log")); string(b) != "line\n" {
		t.Errorf("dated file content %q", b)
	}
}

func TestOldFileOnStartIsDated(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "kizuna.log")
	_ = os.WriteFile(cur, []byte("yesterday\n"), 0o644)
	yesterday := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	_ = os.Chtimes(cur, yesterday, yesterday)
	l, err := open(dir, "kizuna", 0, func() time.Time { return yesterday.Add(24 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("today\n"))
	l.Close()
	if got := names(t, dir); !slices.Equal(got, []string{"kizuna-2026-11-06.log", "kizuna.log"}) {
		t.Fatalf("files %v", got)
	}
	if b, _ := os.ReadFile(cur); string(b) != "today\n" {
		t.Errorf("current file %q", b)
	}
}
