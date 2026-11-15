//go:build !unix

package procguard

// HandleArgs never runs a wrapper here: the daemon installs the guard only on
// Linux (Supported).
func HandleArgs(args []string) (handled bool, exitCode int) { return false, 0 }
