package fetch

import (
	"os"
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskFree returns the bytes available to this user on the disk holding path.
func diskFree(path string) (uint64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail uint64
	if r, _, err := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), 0, 0); r == 0 {
		return 0, err
	}
	return avail, nil
}
