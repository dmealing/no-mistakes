//go:build windows

package testslots

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockByteOffset keeps the locked byte past the holder record: LockFileEx
// ranges are mandatory, so a lock over the record would make cross-handle
// reads fail.
const lockByteOffset = 0xFFFFFFFF

// tryLockFile reports (false, nil) only when another handle holds the lock;
// any other failure is returned so an unlockable file never looks busy.
func tryLockFile(f *os.File) (bool, error) {
	ol := &windows.Overlapped{Offset: lockByteOffset}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
	switch err {
	case nil:
		return true, nil
	case windows.ERROR_LOCK_VIOLATION, windows.ERROR_IO_PENDING:
		return false, nil
	}
	return false, err
}

func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockByteOffset}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
