package procguard

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// processHasMarker reports whether pid's initial environment holds the exact
// marker entry. An unreadable environment (another user's process, a
// non-dumpable one, one that already exited) does not.
func processHasMarker(pid int, marker string) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return false
	}
	want := []byte(marker)
	for _, entry := range bytes.Split(data, []byte{0}) {
		if bytes.Equal(entry, want) {
			return true
		}
	}
	return false
}

// statFields returns pid's /proc stat fields from the state (field 3) on.
func statFields(pid int) []string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	// The command name sits in parentheses and may itself contain spaces or
	// parentheses, so the fields after it start past the last ')'.
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(stat[end+1:])
}

// parentPID returns pid's parent, or 0 when it cannot be read.
func parentPID(pid int) int {
	fields := statFields(pid)
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// startTime returns pid's start time in clock ticks since boot, or 0 when it
// cannot be read.
func startTime(pid int) uint64 {
	fields := statFields(pid)
	if len(fields) < 20 {
		return 0
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return ticks
}
