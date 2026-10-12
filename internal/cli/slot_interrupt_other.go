//go:build !unix

package cli

import (
	"errors"
	"os/exec"
)

// interruptSlotCommandGroup reports that this platform cannot deliver an
// interrupt to a process group; the caller terminates the group instead.
func interruptSlotCommandGroup(*exec.Cmd) error {
	return errors.ErrUnsupported
}
