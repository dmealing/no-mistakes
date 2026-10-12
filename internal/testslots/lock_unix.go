//go:build !windows

package testslots

import (
	"os"
	"syscall"
)

// tryLockFile takes a non-blocking exclusive flock on f. The kernel drops the
// lock when every fd on the open file description closes, including on crash
// or SIGKILL, so a held lock always means a live holder. Only EWOULDBLOCK
// means another holder has the slot (false, nil); any other failure is
// returned, because a filesystem that cannot lock would otherwise look like a
// pool that is full forever.
func tryLockFile(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch err {
		case nil:
			return true, nil
		case syscall.EWOULDBLOCK:
			return false, nil
		case syscall.EINTR:
			continue
		}
		return false, err
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
