//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package main

import (
	"errors"
	"os"
)

// tryLock has no file lock to take on this system. Refusing is safer than
// pretending: two processes renewing a sign-in at once would lose it.
func tryLock(*os.File) (bool, error) {
	return false, errors.New("file locking is not supported on this system")
}

// fileBusy says whether err is another process having the file open, which
// stops nobody on this system.
func fileBusy(error) bool { return false }
