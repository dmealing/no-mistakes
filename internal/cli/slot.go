package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testslots"
	"github.com/spf13/cobra"
)

const (
	// slotRunKillGrace is how long a terminated slot run command gets to
	// exit before it is killed.
	slotRunKillGrace = 10 * time.Second
	// slotRunHoldExpiredExitCode is the exit code of a slot run whose command
	// was terminated at slot_run_max_hold, following timeout(1).
	slotRunHoldExpiredExitCode = 124
)

func newSlotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "slot",
		Short: "Share the host-wide test slot pool used by pipeline Test steps",
	}
	cmd.AddCommand(newSlotRunCmd())
	return cmd
}

func newSlotRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run -- <command> [args...]",
		Short: "Run a command inside a host-wide test slot",
		Long: "Run a command once a test slot is free, so a full local test suite started outside a pipeline " +
			"shares the test_slots cap with pipeline Test steps. While waiting it prints one line to stderr " +
			"naming the holders. The slot is released when the command exits for any reason, and the command's " +
			"exit code is passed through. A command still holding its slot after slot_run_max_hold is " +
			"terminated and slot run exits non-zero.",
		// The wrapped command owns its flags: `slot run go test -race` must
		// not be parsed as slot flags.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
			if len(args) == 0 {
				return &exitError{code: 2, err: fmt.Errorf("usage: no-mistakes slot run -- <command> [args...]")}
			}
			return trackCommand("slot-run", func() error { return runInSlot(cmd, args) })
		},
	}
}

func runInSlot(cmd *cobra.Command, args []string) error {
	p, err := paths.New()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	program := filepath.Base(args[0])
	child := exec.Command(args[0], args[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	child.Env = append(os.Environ(), testslots.HeldEnv+"=1")

	// Only a process tree that already holds a slot skips the pool: queueing
	// behind its own holder would deadlock a full pool.
	var holdLimit <-chan time.Time
	if os.Getenv(testslots.HeldEnv) == "" {
		// The label reaches other runs' step logs and the state database, so
		// it names the program only, never its arguments.
		cwd, _ := os.Getwd()
		label := fmt.Sprintf("slot run: %s (in %s)", program, cwd)
		waitedLine := false
		waitCtx, stopWait := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		lease, _, err := testslots.NewPool(p.TestSlotsDir(), cfg.TestSlots).Acquire(waitCtx, label, func(holders []testslots.Holder, _ bool) {
			if waitedLine {
				return
			}
			waitedLine = true
			fmt.Fprintf(cmd.ErrOrStderr(), "no-mistakes: waiting for a test slot (test_slots=%d); held by %s\n", cfg.TestSlots, testslots.DescribeHolders(holders))
		})
		if err == nil {
			// A signal that landed as the slot was granted still stops the run.
			if err = waitCtx.Err(); err != nil {
				lease.Release()
			}
		}
		stopWait()
		if err != nil {
			return fmt.Errorf("wait for test slot: %w", err)
		}
		defer lease.Release()
		if cfg.SlotRunMaxHold > 0 {
			timer := time.NewTimer(cfg.SlotRunMaxHold)
			defer timer.Stop()
			holdLimit = timer.C
		}
	}

	if err := child.Start(); err != nil {
		return &exitError{code: 127, err: fmt.Errorf("start %s: %w", args[0], err)}
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- child.Wait() }()
	var waitErr error
	holdExpired := false
	for running := true; running; {
		select {
		case waitErr = <-waitDone:
			running = false
		case sig := <-sigs:
			// Ctrl-C reaches the command directly through the shared
			// process group, so an interrupt is never sent a second time;
			// a SIGTERM is delivered to this process alone and is forwarded.
			if sig == syscall.SIGTERM {
				waitErr = terminateSlotCommand(child, waitDone)
				running = false
			}
		case <-holdLimit:
			holdExpired = true
			waitErr = terminateSlotCommand(child, waitDone)
			running = false
		case <-cmd.Context().Done():
			waitErr = terminateSlotCommand(child, waitDone)
			running = false
		}
	}
	if holdExpired {
		return &exitError{code: slotRunHoldExpiredExitCode, err: fmt.Errorf(
			"no-mistakes: slot run terminated %s and released its test slot: it ran past slot_run_max_hold (%s)",
			program, cfg.SlotRunMaxHold)}
	}
	if waitErr == nil {
		return nil
	}
	if ee, ok := waitErr.(*exec.ExitError); ok {
		code := ee.ExitCode()
		if code < 0 {
			code = 1
		}
		return &exitError{code: code}
	}
	return &exitError{code: 1, err: waitErr}
}

// terminateSlotCommand asks the command to stop with SIGTERM, kills it after
// slotRunKillGrace (or at once where SIGTERM cannot be sent), and returns its
// wait result.
func terminateSlotCommand(child *exec.Cmd, waitDone <-chan error) error {
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		_ = child.Process.Kill()
	}
	select {
	case err := <-waitDone:
		return err
	case <-time.After(slotRunKillGrace):
		_ = child.Process.Kill()
		return <-waitDone
	}
}
