package main

import (
	"os"
	"syscall"
	"unsafe"
)

// The syscall package has no LockFileEx, so it is called by name.
var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
)

// tryLock takes the exclusive lock on f if nobody holds it.
func tryLock(f *os.File) (bool, error) {
	var ol syscall.Overlapped
	ok, _, err := procLockFileEx.Call(f.Fd(), lockfileFailImmediately|lockfileExclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	switch {
	case ok != 0:
		return true, nil
	case err == errorLockViolation || err == syscall.ERROR_IO_PENDING:
		return false, nil
	default:
		return false, err
	}
}
