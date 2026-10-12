//go:build !windows

package testslots

import (
	"os"
	"syscall"
)

// tryLockFile takes a non-blocking exclusive flock on f. The kernel drops the
// lock when every fd on the open file description closes, including on crash
// or SIGKILL, so a held lock always means a live holder.
func tryLockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
