//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"syscall"
)

// tryLock takes the exclusive lock on f if nobody holds it.
func tryLock(f *os.File) (bool, error) {
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err {
	case nil:
		return true, nil
	case syscall.EWOULDBLOCK, syscall.EINTR:
		return false, nil
	default:
		return false, err
	}
}

// fileBusy says whether err is another process having the file open, which
// stops nobody on this system.
func fileBusy(error) bool { return false }
