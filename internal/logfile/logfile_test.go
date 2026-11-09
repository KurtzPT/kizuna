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

// block makes dating kizuna.log as day fail: a folder takes the dated name.
func block(t *testing.T, dir, day string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, "kizuna-"+day+".log"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// When yesterday's kizuna.log can't be dated at start, Open still logs to
// the file (appending to it) and reports why.
func TestOpenWhenArchivingFails(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "kizuna.log")
	_ = os.WriteFile(cur, []byte("yesterday\n"), 0o644)
	yesterday := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	_ = os.Chtimes(cur, yesterday, yesterday)
	block(t, dir, "2026-11-06")
	l, err := open(dir, "kizuna", 0, func() time.Time { return yesterday.Add(24 * time.Hour) })
	if l == nil || err == nil {
		t.Fatalf("got %v, %v; want a File and the archive error", l, err)
	}
	if _, err := l.Write([]byte("today\n")); err != nil { // tries again, fails again, keeps going
		t.Errorf("write: %v", err)
	}
	l.Close()
	if b, _ := os.ReadFile(cur); string(b) != "yesterday\ntoday\n" {
		t.Errorf("current file %q", b)
	}
}

// A failed rename on a new day keeps the lines in kizuna.log; a failed
// reopen loses only the lines until the file can be opened again.
func TestWriteWhenRotatingFails(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	l, err := open(dir, "kizuna", 0, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("1 Nov\n"))
	block(t, dir, "2026-11-01")
	clock = clock.Add(24 * time.Hour)
	if _, err := l.Write([]byte("2 Nov\n")); err != nil {
		t.Errorf("rename failed: %v, want the line kept in kizuna.log", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kizuna.log")); string(b) != "1 Nov\n2 Nov\n" {
		t.Errorf("kizuna.log %q", b)
	}

	l.dir = filepath.Join(dir, "later") // not there yet: kizuna.log can't be opened
	clock = clock.Add(24 * time.Hour)
	for _, line := range []string{"3 Nov a\n", "3 Nov b\n"} {
		if _, err := l.Write([]byte(line)); err == nil {
			t.Errorf("%q: no error while the file can't be opened", line)
		}
	}
	if err := os.Mkdir(l.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("3 Nov c\n")); err != nil {
		t.Errorf("after the folder came back: %v", err)
	}
	l.Close()
	if b, _ := os.ReadFile(filepath.Join(l.dir, "kizuna.log")); string(b) != "3 Nov c\n" {
		t.Errorf("reopened kizuna.log %q", b)
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
