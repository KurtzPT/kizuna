// Package logfile is a daily log file. Today's lines go to kizuna.log; on the
// first line of a new day it is renamed kizuna-YYYY-MM-DD.log (the day its
// lines were written) and a new kizuna.log starts. Dated files older than the
// retention are deleted automatically. Safe for concurrent use.
package logfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const dateLayout = "2006-01-02"

type File struct {
	mu       sync.Mutex
	dir      string
	base     string // "kizuna"
	keepDays int    // 0 = keep forever
	now      func() time.Time

	f    *os.File
	day  string    // date of the lines in kizuna.log
	next time.Time // the local midnight that ends day: the first line from then on dates the file
	// After a failed dating, kizuna.log keeps its day and its new lines, and
	// is dated again from retry on. The first failure is reported (failed).
	retry  time.Time
	failed bool
}

// retryDating: how long a kizuna.log that couldn't be dated waits before
// the next try (each try closes and reopens the file).
const retryDating = time.Minute

// midnightAfter is the start of the local day after t's.
func midnightAfter(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

// Open opens dir/<base>.log for appending. keepDays is how many past days of
// dated files are kept (0 = never delete). When only dating an old
// kizuna.log failed (e.g. a viewer holds it on Windows), Open still returns
// a working File, appending to that file, along with the error to report;
// the first line written tries again.
func Open(dir, base string, keepDays int) (*File, error) {
	return open(dir, base, keepDays, time.Now)
}

func open(dir, base string, keepDays int, now func() time.Time) (*File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &File{dir: dir, base: base, keepDays: keepDays, now: now}
	// A kizuna.log left from an earlier day (Kizuna was stopped over midnight)
	// is dated by its last write.
	var archErr error
	l.setDay(now())
	if st, err := os.Stat(l.current()); err == nil {
		if at := st.ModTime(); at.Format(dateLayout) != l.day {
			if err := l.archive(at.Format(dateLayout)); err != nil {
				archErr = fmt.Errorf("can't rename the old %s.log to its date; appending to it: %w", base, err)
				// Its lines are still in it: the first Write tries to date them again.
				l.setDay(at)
				l.failed = true // reported here
			}
		}
	}
	if err := l.openCurrent(); err != nil {
		return nil, errors.Join(archErr, err)
	}
	l.cleanup()
	return l, archErr
}

func (l *File) current() string { return filepath.Join(l.dir, l.base+".log") }
func (l *File) dated(day string) string {
	return filepath.Join(l.dir, fmt.Sprintf("%s-%s.log", l.base, day))
}

// setDay makes t's day the day of kizuna.log's lines.
func (l *File) setDay(t time.Time) { l.day, l.next = t.Format(dateLayout), midnightAfter(t) }

func (l *File) openCurrent() error {
	f, err := os.OpenFile(l.current(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// Write appends p (one log line), dating the file first on a new day. When
// the dating fails (e.g. another program holds the file on Windows), the
// line still goes to kizuna.log, which keeps its day and is dated again
// retryDating later: the first such failure is Write's error (the line was
// written all the same), later ones aren't.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var archErr error
	if now := l.now(); !now.Before(l.next) && !now.Before(l.retry) {
		if l.f != nil {
			l.f.Close()
			l.f = nil
		}
		// No kizuna.log (it couldn't be opened): nothing to date.
		if err := l.archive(l.day); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if !l.failed {
				archErr = fmt.Errorf("can't rename %s.log to %s; appending to it, trying again every %v: %w", l.base, filepath.Base(l.dated(l.day)), retryDating, err)
			}
			l.retry, l.failed = now.Add(retryDating), true
		} else {
			l.setDay(now)
			l.retry, l.failed = time.Time{}, false
			l.cleanup()
		}
	}
	if l.f == nil { // closed above, or reopening failed before: open it again
		if err := l.openCurrent(); err != nil {
			return 0, errors.Join(archErr, err)
		}
	}
	n, err := l.f.Write(p)
	return n, errors.Join(err, archErr)
}

// archive renames kizuna.log to kizuna-<day>.log. If that file already
// exists (should not happen), the lines are appended to it.
func (l *File) archive(day string) error {
	target := l.dated(day)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return os.Rename(l.current(), target)
	}
	src, err := os.Open(l.current())
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		src.Close()
		return err
	}
	_, err = io.Copy(dst, src)
	src.Close()
	dst.Close()
	if err != nil {
		return err
	}
	return os.Remove(l.current())
}

// cleanup deletes dated files more than keepDays days old.
func (l *File) cleanup() {
	if l.keepDays <= 0 {
		return
	}
	oldest := l.now().AddDate(0, 0, -l.keepDays).Format(dateLayout)
	files, _ := filepath.Glob(filepath.Join(l.dir, l.base+"-*.log"))
	for _, f := range files {
		day := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), l.base+"-"), ".log")
		if _, err := time.Parse(dateLayout, day); err == nil && day < oldest {
			_ = os.Remove(f)
		}
	}
}

func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}
