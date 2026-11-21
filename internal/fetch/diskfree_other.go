//go:build !linux && !darwin && !freebsd && !windows

package fetch

import "errors"

func diskFree(string) (uint64, error) {
	return 0, errors.New("free space check not supported on this system")
}
