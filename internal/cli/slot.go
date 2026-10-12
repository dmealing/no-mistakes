package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testslots"
	"github.com/spf13/cobra"
)

// slotHeldEnv marks a process tree that already holds a test slot, so a nested
// `slot run` runs directly instead of queueing behind its own parent.
const slotHeldEnv = "NM_TEST_SLOT_HELD"

func newSlotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "slot",
		Short: "Share the host-wide test slot pool used by pipeline Test steps",
	}
	cmd.AddCommand(newSlotRunCmd(), newSlotStatusCmd())
	return cmd
}

func newSlotRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run -- <command> [args...]",
		Short: "Run a command inside a host-wide test slot",
		Long: "Run a command once a test slot is free, so a full local test suite started outside a pipeline " +
			"shares the test_slots cap with pipeline Test steps. While waiting it prints one line to stderr " +
			"naming the holders. The slot is released when the command exits for any reason, and the command's " +
			"exit code is passed through.",
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

func newSlotStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show who holds the host-wide test slots",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return fmt.Errorf("resolve paths: %w", err)
			}
			cfg, err := config.LoadGlobal(p.ConfigFile())
			if err != nil {
				return err
			}
			return writeSlotStatus(cmd.OutOrStdout(), testslots.NewPool(p.TestSlotsDir(), cfg.TestSlots), cfg.TestSlots)
		},
	}
}

func writeSlotStatus(w io.Writer, pool *testslots.Pool, size int) error {
	if size <= 0 {
		_, err := fmt.Fprintln(w, "test_slots: unlimited (0)")
		return err
	}
	holders := pool.Holders()
	if _, err := fmt.Fprintf(w, "test_slots: %d, in use: %d\n", size, len(holders)); err != nil {
		return err
	}
	for _, h := range holders {
		if _, err := fmt.Fprintf(w, "  slot %d: %s\n", h.Slot, h.Describe()); err != nil {
			return err
		}
	}
	return nil
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

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	child := exec.Command(args[0], args[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	child.Env = append(os.Environ(), slotHeldEnv+"=1")

	// A pipeline step's agent (or a nested slot run) already sits inside a
	// slot of its own; queueing behind that would deadlock a full pool.
	if os.Getenv(slotHeldEnv) == "" && !gatecontext.MarkerPresent() {
		cwd, _ := os.Getwd()
		label := fmt.Sprintf("slot run: %s (in %s)", truncateLabel(strings.Join(args, " "), 80), cwd)
		waitedLine := false
		lease, _, err := testslots.NewPool(p.TestSlotsDir(), cfg.TestSlots).Acquire(ctx, label, func(holders []testslots.Holder) {
			if waitedLine {
				return
			}
			waitedLine = true
			fmt.Fprintf(cmd.ErrOrStderr(), "no-mistakes: waiting for a test slot (test_slots=%d); held by %s\n", cfg.TestSlots, testslots.DescribeHolders(holders))
		})
		if err != nil {
			return fmt.Errorf("wait for test slot: %w", err)
		}
		defer lease.Release()
	}

	// Ctrl-C reaches the child directly through the shared process group; a
	// SIGTERM sent to this process alone is forwarded.
	if err := child.Start(); err != nil {
		return &exitError{code: 127, err: fmt.Errorf("start %s: %w", args[0], err)}
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- child.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		_ = child.Process.Signal(os.Interrupt)
		select {
		case waitErr = <-waitDone:
		case <-time.After(10 * time.Second):
			_ = child.Process.Kill()
			waitErr = <-waitDone
		}
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

func truncateLabel(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
