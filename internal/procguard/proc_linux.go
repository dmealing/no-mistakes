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

// parentPID returns pid's parent, or 0 when it cannot be read.
func parentPID(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// The command name sits in parentheses and may itself contain spaces or
	// parentheses, so the fields after it start past the last ')'.
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}
