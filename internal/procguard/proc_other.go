//go:build unix && !linux

package procguard

// processHasMarker cannot read another process's environment here, so nothing
// counts as this run's. The daemon does not install the guard on this
// platform (Supported), so this only matters for a hand-run wrapper.
func processHasMarker(pid int, marker string) bool { return false }

// parentPID is not tracked here; with nothing counted as this run's, pkill
// signals nothing and needs no ancestor exclusion.
func parentPID(pid int) int { return 0 }

// startTime is not tracked here; nothing counts as this run's, so there is
// nothing to order.
func startTime(pid int) uint64 { return 0 }
