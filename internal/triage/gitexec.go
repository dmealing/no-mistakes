package triage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// gitOutput runs git in dir and returns stdout and the exit code. Unlike
// internal/git it keeps stdout on a non-zero exit, because `git merge-tree`
// reports a conflict with exit 1 AND the conflicted paths on stdout. err is
// set only when git could not run or exited with a code other than 0 or 1.
func gitOutput(ctx context.Context, dir string, extraEnv []string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), extraEnv...)
	winproc.Harden(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", -1, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		code = exitErr.ExitCode()
		if code != 1 {
			return stdout.String(), code, fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
		}
	}
	return stdout.String(), code, nil
}

// gitLine runs git and returns trimmed stdout, failing on any non-zero exit.
func gitLine(ctx context.Context, dir string, args ...string) (string, error) {
	out, code, err := gitOutput(ctx, dir, nil, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("git %s: exit %d", strings.Join(args, " "), code)
	}
	return strings.TrimSpace(out), nil
}
