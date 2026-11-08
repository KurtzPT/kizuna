// Package logfile is a daily log file. Today's lines go to kizuna.log; on the
// first line of a new day it is renamed kizuna-YYYY-MM-DD.log (the day its
// lines were written) and a new kizuna.log starts. Dated files older than the
// retention are deleted automatically. Safe for concurrent use.
package logfile

import (
	"errors"
	"fmt"
	"io"
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

	f   *os.File
	day string // date of the lines in f
}

// Open opens dir/<base>.log for appending. keepDays is how many past days of
// dated files are kept (0 = never delete).
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
	if st, err := os.Stat(l.current()); err == nil {
		if day := st.ModTime().Format(dateLayout); day != l.today() {
			if err := l.archive(day); err != nil {
				return nil, err
			}
		}
	}
	if err := l.openCurrent(); err != nil {
		return nil, err
	}
	l.cleanup()
	return l, nil
}

func (l *File) current() string { return filepath.Join(l.dir, l.base+".log") }
func (l *File) dated(day string) string {
	return filepath.Join(l.dir, fmt.Sprintf("%s-%s.log", l.base, day))
}
func (l *File) today() string { return l.now().Format(dateLayout) }

func (l *File) openCurrent() error {
	f, err := os.OpenFile(l.current(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f, l.day = f, l.today()
	return nil
}

// Write appends p (one log line), starting a new file first on a new day.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if today := l.today(); today != l.day {
		l.f.Close()
		// A failed rename (e.g. another program holds the file on Windows)
		// keeps today's lines in kizuna.log rather than losing them.
		archErr := l.archive(l.day)
		if err := l.openCurrent(); err != nil {
			l.f = nil
			return 0, errors.Join(archErr, err)
		}
		l.cleanup()
	}
	if l.f == nil { // reopening failed on an earlier day change: try again
		if err := l.openCurrent(); err != nil {
			return 0, err
		}
	}
	return l.f.Write(p)
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
