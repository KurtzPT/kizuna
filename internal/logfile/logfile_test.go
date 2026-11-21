package logfile

import (
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones below where the machine has no zone database (Windows)
)

func names(dir string) []string {
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
	if got := names(dir); !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kizuna-2026-11-05.log")); string(b) != "line\n" {
		t.Errorf("dated file content %q", b)
	}
}

// kizuna.log is dated once, when the local date changes, whatever the zone's
// clock does around midnight. Each line is its own wall clock time.
func TestOneRolloverPerLocalDay(t *testing.T) {
	t.Setenv("ZONEINFO", "") // LoadLocation reads it
	type line struct {
		utc  string // the instant the clock gives
		wall string // the same instant in the zone: checks the zone's rules are the ones this test is about
		day  string // the day of the file the line belongs in; "" = wall's day
	}
	for _, c := range []struct {
		name, zone string
		lines      []line
	}{
		// 6 Sep 2026: 00:00 -04 doesn't exist, the clock goes from 23:59:59 to
		// 01:00 -03. time.Date gives 5 Sep 23:00 for that midnight, and the file
		// used to be dated again at every line of that last hour.
		{"no midnight: summer time starts at 00:00", "America/Santiago", []line{
			{utc: "2026-09-06 02:59", wall: "2026-09-05 22:59 -0400"},
			{utc: "2026-09-06 03:00", wall: "2026-09-05 23:00 -0400"},
			{utc: "2026-09-06 03:30", wall: "2026-09-05 23:30 -0400"},
			{utc: "2026-09-06 03:59", wall: "2026-09-05 23:59 -0400"},
			{utc: "2026-09-06 04:00", wall: "2026-09-06 01:00 -0300"},
			{utc: "2026-09-06 04:01", wall: "2026-09-06 01:01 -0300"},
			{utc: "2026-09-07 02:59", wall: "2026-09-06 23:59 -0300"}, // a 23-hour day
			{utc: "2026-09-07 03:00", wall: "2026-09-07 00:00 -0300"},
		}},
		// 1 Nov 2026: at 01:00 -04 the clock goes back to 00:00 -05.
		{"midnight twice: summer time ends at 01:00", "America/Havana", []line{
			{utc: "2026-11-01 03:59", wall: "2026-10-31 23:59 -0400"},
			{utc: "2026-11-01 04:00", wall: "2026-11-01 00:00 -0400"},
			{utc: "2026-11-01 04:59", wall: "2026-11-01 00:59 -0400"},
			{utc: "2026-11-01 05:00", wall: "2026-11-01 00:00 -0500"},
			{utc: "2026-11-01 05:30", wall: "2026-11-01 00:30 -0500"},
			{utc: "2026-11-01 06:00", wall: "2026-11-01 01:00 -0500"},
			{utc: "2026-11-02 04:59", wall: "2026-11-01 23:59 -0500"}, // a 25-hour day
			{utc: "2026-11-02 05:00", wall: "2026-11-02 00:00 -0500"},
		}},
		// 25 Oct 2026: at 00:00 +03 the clock goes back to 24 Oct 23:00 +02.
		{"the last hour twice: summer time ends at 00:00", "Asia/Beirut", []line{
			{utc: "2026-10-24 20:30", wall: "2026-10-24 23:30 +0300"},
			{utc: "2026-10-24 20:59", wall: "2026-10-24 23:59 +0300"},
			{utc: "2026-10-24 21:00", wall: "2026-10-24 23:00 +0200"},
			{utc: "2026-10-24 21:59", wall: "2026-10-24 23:59 +0200"},
			{utc: "2026-10-24 22:00", wall: "2026-10-25 00:00 +0200"},
			{utc: "2026-10-24 22:01", wall: "2026-10-25 00:01 +0200"},
		}},
		{"an ordinary midnight", "America/Santiago", []line{
			{utc: "2026-11-02 02:59", wall: "2026-11-01 23:59 -0300"},
			{utc: "2026-11-02 03:00", wall: "2026-11-02 00:00 -0300"},
			{utc: "2026-11-02 03:01", wall: "2026-11-02 00:01 -0300"},
			{utc: "2026-11-03 02:59", wall: "2026-11-02 23:59 -0300"},
		}},
		// A clock set back over midnight (by hand, or a time correction): the
		// line stamped yesterday goes in today's file, which is not dated early.
		{"the clock is set back to yesterday", "UTC", []line{
			{utc: "2026-11-01 23:59", wall: "2026-11-01 23:59 +0000"},
			{utc: "2026-11-02 00:00", wall: "2026-11-02 00:00 +0000"},
			{utc: "2026-11-01 23:58", wall: "2026-11-01 23:58 +0000", day: "2026-11-02"},
			{utc: "2026-11-01 23:59", wall: "2026-11-01 23:59 +0000", day: "2026-11-02"},
			{utc: "2026-11-02 00:00", wall: "2026-11-02 00:00 +0000"},
			{utc: "2026-11-02 23:59", wall: "2026-11-02 23:59 +0000"},
			{utc: "2026-11-03 00:00", wall: "2026-11-03 00:00 +0000"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			loc, err := time.LoadLocation(c.zone)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			var clock time.Time
			reads := 0
			set := func(l line) {
				at, err := time.Parse("2006-01-02 15:04", l.utc)
				if err != nil {
					t.Fatal(err)
				}
				clock = at.In(loc)
				if got := clock.Format("2006-01-02 15:04 -0700"); got != l.wall {
					t.Fatalf("%s UTC is %s in %s, want %s: not the zone rules this test is about", l.utc, got, c.zone, l.wall)
				}
			}
			set(c.lines[0])
			// With days to keep, each dating reads the clock once more (to delete
			// old files): the clock reads count the datings.
			l, err := open(dir, "kizuna", 30, func() time.Time { reads++; return clock })
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			want := map[string]string{} // file name -> its lines
			day := ""                   // of the lines in kizuna.log
			for i, ln := range c.lines {
				set(ln)
				lineDay := ln.day
				if lineDay == "" {
					lineDay = ln.wall[:len(dateLayout)]
				}
				wantReads := 1
				if i > 0 && lineDay != day {
					wantReads = 2
					want["kizuna-"+day+".log"] = want["kizuna.log"]
					want["kizuna.log"] = ""
				}
				day = lineDay
				want["kizuna.log"] += ln.wall + "\n"
				reads = 0
				if _, err := l.Write([]byte(ln.wall + "\n")); err != nil {
					t.Fatalf("%s: %v", ln.wall, err)
				}
				if reads != wantReads {
					t.Errorf("%s: %d dating(s) of kizuna.log, want %d", ln.wall, reads-1, wantReads-1)
				}
				// Checked after each line: a file dated early and filled up later
				// ends with the same lines.
				if got := names(dir); len(got) != len(want) {
					t.Fatalf("after %s: files %v, want %d", ln.wall, got, len(want))
				}
			}
			for name, lines := range want {
				if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != lines {
					t.Errorf("%s: %q, %v; want %q", name, b, err, lines)
				}
			}
		})
	}
}

// A huge keepDays keeps every log; the date math used to wrap and delete them.
func TestHugeRetentionKeepsAll(t *testing.T) {
	for _, keep := range []int{foreverDays, 9999999999999999, math.MaxInt} {
		dir := t.TempDir()
		clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
		l, err := open(dir, "kizuna", keep, func() time.Time { return clock })
		if err != nil {
			t.Fatal(err)
		}
		for day := 0; day < 3; day++ { // 1 Nov ... 3 Nov
			_, _ = l.Write([]byte("line\n"))
			clock = clock.Add(24 * time.Hour)
		}
		l.Close()
		want := []string{"kizuna-2026-11-01.log", "kizuna-2026-11-02.log", "kizuna.log"}
		if got := names(dir); !slices.Equal(got, want) {
			t.Errorf("keep %d: files %v, want %v", keep, got, want)
		}
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
	if _, err := l.Write([]byte("today\n")); err != nil || l.day != "2026-11-06" { // tries again, fails again, keeps going
		t.Errorf("write: %v, day %s; want no error (Open reported it), the file still of 2026-11-06", err, l.day)
	}
	l.Close()
	if b, _ := os.ReadFile(cur); string(b) != "yesterday\ntoday\n" {
		t.Errorf("current file %q", b)
	}
}

// A failed rename on a new day keeps the lines in kizuna.log, which keeps
// its day: the first failure is reported, and the file is dated again
// retryDating later, as its own day. A failed reopen loses only the lines
// until the file can be opened again.
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
	if n, err := l.Write([]byte("2 Nov a\n")); err == nil || n != len("2 Nov a\n") {
		t.Errorf("rename failed: wrote %d, %v; want the line kept in kizuna.log and the error", n, err)
	}
	clock = clock.Add(retryDating - time.Second)
	if _, err := l.Write([]byte("2 Nov b\n")); err != nil || l.retry.IsZero() {
		t.Errorf("again within retryDating: %v, want no error, no try", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kizuna.log")); string(b) != "1 Nov\n2 Nov a\n2 Nov b\n" {
		t.Errorf("kizuna.log %q", b)
	}
	clock = clock.Add(time.Second)
	if _, err := l.Write([]byte("2 Nov c\n")); err != nil {
		t.Errorf("failing again: %v, want it reported only once", err)
	}
	if l.day != "2026-11-01" {
		t.Errorf("kizuna.log's day %s, want 2026-11-01 until it is dated", l.day)
	}
	if err := os.Rename(filepath.Join(dir, "kizuna-2026-11-01.log"), filepath.Join(dir, "blocker")); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(retryDating)
	if _, err := l.Write([]byte("2 Nov d\n")); err != nil {
		t.Errorf("once the dated name is free: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kizuna-2026-11-01.log")); string(b) != "1 Nov\n2 Nov a\n2 Nov b\n2 Nov c\n" {
		t.Errorf("dated kizuna-2026-11-01.log %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kizuna.log")); string(b) != "2 Nov d\n" || l.day != "2026-11-02" {
		t.Errorf("kizuna.log %q of %s, want the new line of 2026-11-02", b, l.day)
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
	if got := names(dir); !slices.Equal(got, []string{"kizuna-2026-11-06.log", "kizuna.log"}) {
		t.Fatalf("files %v", got)
	}
	if b, _ := os.ReadFile(cur); string(b) != "today\n" {
		t.Errorf("current file %q", b)
	}
}

// A dated file that is already there (it should not be) gets yesterday's
// lines after its own, and kizuna.log starts empty.
func TestOldFileIsAppendedToAnExistingDatedFile(t *testing.T) {
	dir := t.TempDir()
	cur, dated := filepath.Join(dir, "kizuna.log"), filepath.Join(dir, "kizuna-2026-11-06.log")
	_ = os.WriteFile(dated, []byte("earlier\n"), 0o644)
	_ = os.WriteFile(cur, []byte("yesterday\n"), 0o644)
	yesterday := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	_ = os.Chtimes(cur, yesterday, yesterday)
	l, err := open(dir, "kizuna", 0, func() time.Time { return yesterday.Add(24 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("today\n"))
	l.Close()
	if b, _ := os.ReadFile(dated); string(b) != "earlier\nyesterday\n" {
		t.Errorf("dated file %q", b)
	}
	if b, _ := os.ReadFile(cur); string(b) != "today\n" {
		t.Errorf("current file %q", b)
	}
}

// failingFile is a file whose write or close fails.
type failingFile struct {
	wrote              strings.Builder // not embedded: io.Copy would use its WriteString and skip Write
	writeErr, closeErr error
	closed             bool
}

func (f *failingFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.wrote.Write(p)
}

func (f *failingFile) Close() error {
	f.closed = true
	return f.closeErr
}

// A close that fails may have lost the last lines: it is an error like a
// failed copy, so the caller keeps kizuna.log. The file is closed either way.
func TestAppendAllReportsAFailedClose(t *testing.T) {
	writeErr, closeErr := errors.New("disk full"), errors.New("close failed")
	for _, c := range []struct {
		name string
		f    failingFile
		want error
	}{
		{"all written", failingFile{}, nil},
		{"close fails", failingFile{closeErr: closeErr}, closeErr},
		{"copy fails", failingFile{writeErr: writeErr}, writeErr},
		{"both fail", failingFile{writeErr: writeErr, closeErr: closeErr}, writeErr},
	} {
		err := appendAll(&c.f, strings.NewReader("yesterday\n"))
		if err != c.want || !c.f.closed {
			t.Errorf("%s: err %v, closed %v, want %v and closed", c.name, err, c.f.closed, c.want)
		}
		if c.f.writeErr == nil && c.f.wrote.String() != "yesterday\n" {
			t.Errorf("%s: wrote %q", c.name, c.f.wrote.String())
		}
	}
}

// slog drops a writer's errors: through WarnVia, a dating failure is
// logged as a warning once, right after the line that met it, without
// waiting on the handler's or the file's lock.
func TestWarnViaLogsDatingFailureOnce(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	l, err := open(dir, "kizuna", 0, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	log := slog.New(l.WarnVia(slog.NewTextHandler(l, nil))).With("root", "anidb-1")
	log.Info("1 Nov")
	block(t, dir, "2026-11-01")
	clock = clock.Add(24 * time.Hour)
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Info("2 Nov a") // the dating fails
		clock = clock.Add(retryDating)
		log.Info("2 Nov b") // fails again: not warned again
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging hung: the warning waited on a lock its own line held")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "kizuna.log"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 || !strings.Contains(lines[2], "level=WARN") || !strings.Contains(lines[2], "can't rename kizuna.log") ||
		strings.Contains(lines[2], "root=") || !strings.Contains(lines[3], "2 Nov b") {
		t.Errorf("kizuna.log:\n%s\nwant the 2 Nov a line, one warning without the logger's attrs, then 2 Nov b", b)
	}
	if err := l.Err(); err != nil {
		t.Errorf("Err after the warning: %v, want nil (handed out once)", err)
	}
}
