//go:build !windows

package fetch

import "os"

// rename replaces to with from (atomic on Unix, even while to is open).
func rename(from, to string) error { return os.Rename(from, to) }
