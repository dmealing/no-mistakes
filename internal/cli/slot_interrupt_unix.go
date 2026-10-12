//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// interruptSlotCommandGroup sends SIGINT to every process in the group the
// slot run command leads.
func interruptSlotCommandGroup(child *exec.Cmd) error {
	return syscall.Kill(-child.Process.Pid, syscall.SIGINT)
}
