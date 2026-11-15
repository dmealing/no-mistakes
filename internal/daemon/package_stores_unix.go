//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// sameFilesystem reports whether a and b are on one filesystem, the condition
// for a hardlink between them. Anything that cannot be inspected is not.
func sameFilesystem(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	as, aok := ai.Sys().(*syscall.Stat_t)
	bs, bok := bi.Sys().(*syscall.Stat_t)
	return aok && bok && as.Dev == bs.Dev
}
