package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBinary is a stand-in for a no-mistakes build: it reports its version
// and drives a fake service whose state lives in $FAKE_STATE. `daemon busy`
// counts down from the number in $FAKE_STATE/busy and logs whether the hold
// file existed when it was asked; `daemon start` fails (after scribbling over
// the database) when $FAKE_STATE/fail-start-<version> exists, and `daemon
// stop` fails when $FAKE_STATE/fail-stop-<version> exists.
const fakeBinary = `#!/usr/bin/env bash
V="@VERSION@"
S="$FAKE_STATE"
case "$1" in
  --version) echo "no-mistakes version $V"; exit 0 ;;
  daemon)
    case "$2" in
      busy)
        n=$(cat "$S/busy" 2>/dev/null || echo 0)
        if [ -e "$NM_HOME/hold" ]; then echo held >> "$S/busy-log"; else echo unheld >> "$S/busy-log"; fi
        if [ "$n" -gt 0 ]; then echo $((n-1)) > "$S/busy"; echo "  run-$n  running  some-branch" >&2; fi
        echo "$n" ;;
      stop)
        echo "stop $V $3" >> "$S/log"
        if [ -e "$S/fail-stop-$V" ]; then exit 1; fi
        echo stopped > "$S/daemon" ;;
      start)
        echo "start $V" >> "$S/log"
        if [ -e "$S/fail-start-$V" ]; then echo garbage > "$NM_HOME/state.sqlite"; exit 1; fi
        echo running > "$S/daemon" ;;
      status)
        if [ "$(cat "$S/daemon" 2>/dev/null)" = running ]; then echo "daemon running (pid 1)"; else echo "daemon not running"; fi ;;
    esac ;;
esac
`

type installFixture struct {
	home, state, target, binary string
}

func newInstallFixture(t *testing.T) installFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the install script is a bash script for unix hosts")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	f := installFixture{
		home:   filepath.Join(dir, "home"),
		state:  filepath.Join(dir, "state"),
		target: filepath.Join(dir, "installed", "no-mistakes"),
		binary: filepath.Join(dir, "build", "no-mistakes"),
	}
	for _, d := range []string{f.home, f.state, filepath.Dir(f.target), filepath.Dir(f.binary)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, f.target, strings.ReplaceAll(fakeBinary, "@VERSION@", "v1-old"))
	writeExec(t, f.binary, strings.ReplaceAll(fakeBinary, "@VERSION@", "v2-new"))
	writeFile(t, filepath.Join(f.home, "state.sqlite"), "original database")
	writeFile(t, filepath.Join(f.state, "daemon"), "running")
	return f
}

func (f installFixture) run(t *testing.T) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "install-local-build.sh", "--binary", f.binary, "--target", f.target, "--who", "test", "--why", "testing", "--poll", "0")
	cmd.Env = append(os.Environ(), "NM_HOME="+f.home, "FAKE_STATE="+f.state)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run install script: %v", err)
	}
	return string(out), code
}

func TestInstallLocalBuild_WaitsForRunsMidStepThenInstalls(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, filepath.Join(f.state, "busy"), "2")

	out, code := f.run(t)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	// It asked three times (2, 1, 0) and the hold was in place every time.
	if got := readFile(t, filepath.Join(f.state, "busy-log")); got != "held\nheld\nheld\n" {
		t.Fatalf("busy checks = %q, want three checks all under the hold", got)
	}
	if !strings.Contains(out, "waiting: 2 run(s) in the middle of a step") {
		t.Fatalf("output does not report the wait:\n%s", out)
	}
	// Stopped with --force only after the wait, then started the new build.
	if got := readFile(t, filepath.Join(f.state, "log")); got != "stop v1-old --force\nstart v2-new\n" {
		t.Fatalf("service log = %q", got)
	}
	if v := versionOf(t, f.target); v != "no-mistakes version v2-new" {
		t.Fatalf("installed version = %q", v)
	}
	if v := versionOf(t, f.target+".prev"); v != "no-mistakes version v1-old" {
		t.Fatalf("rollback copy version = %q", v)
	}
	if _, err := os.Stat(filepath.Join(f.home, "hold")); !os.IsNotExist(err) {
		t.Fatalf("hold file left behind after a successful install (err=%v)", err)
	}
	backups, _ := filepath.Glob(filepath.Join(f.home, "backups", "install-*", "state.sqlite"))
	if len(backups) != 1 || readFile(t, backups[0]) != "original database" {
		t.Fatalf("database backup = %v", backups)
	}
}

func TestInstallLocalBuild_RollsBackWhenTheNewBuildFailsToStart(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, filepath.Join(f.state, "fail-start-v2-new"), "")

	out, code := f.run(t)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (rolled back):\n%s", code, out)
	}
	if v := versionOf(t, f.target); v != "no-mistakes version v1-old" {
		t.Fatalf("after rollback the installed version = %q, want the old build", v)
	}
	if got := readFile(t, filepath.Join(f.home, "state.sqlite")); got != "original database" {
		t.Fatalf("database after rollback = %q, want the backup restored", got)
	}
	if got := readFile(t, filepath.Join(f.state, "daemon")); got != "running\n" {
		t.Fatalf("service state after rollback = %q, want running", got)
	}
	if got := readFile(t, filepath.Join(f.state, "log")); got != "stop v1-old --force\nstart v2-new\nstop v2-new --force\nstart v1-old\n" {
		t.Fatalf("service log = %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.home, "hold")); !os.IsNotExist(err) {
		t.Fatalf("hold file left behind after a successful rollback (err=%v)", err)
	}
}

func TestInstallLocalBuild_KeepsTheHoldWhenTheRollbackFails(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, filepath.Join(f.state, "fail-start-v2-new"), "")
	writeFile(t, filepath.Join(f.state, "fail-start-v1-old"), "")

	out, code := f.run(t)
	if code != 3 {
		t.Fatalf("exit %d, want 3 (rollback failed):\n%s", code, out)
	}
	hold := readFile(t, filepath.Join(f.home, "hold"))
	if !strings.Contains(hold, "needs a person") {
		t.Fatalf("hold after a failed rollback = %q", hold)
	}
}

// A failure before the new binary is installed must not copy a stale
// rollback copy from an earlier install over the healthy target.
func TestInstallLocalBuild_FailureBeforeInstallKeepsTheTarget(t *testing.T) {
	f := newInstallFixture(t)
	writeExec(t, f.target+".prev", strings.ReplaceAll(fakeBinary, "@VERSION@", "v0-stale"))
	writeFile(t, filepath.Join(f.state, "fail-stop-v1-old"), "")

	out, code := f.run(t)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (rolled back):\n%s", code, out)
	}
	if v := versionOf(t, f.target); v != "no-mistakes version v1-old" {
		t.Fatalf("installed version = %q, want the untouched old build", v)
	}
	if v := versionOf(t, f.target+".prev"); v != "no-mistakes version v0-stale" {
		t.Fatalf("rollback copy version = %q, want it untouched", v)
	}
	if got := readFile(t, filepath.Join(f.state, "log")); got != "stop v1-old --force\nstop v1-old --force\nstart v1-old\n" {
		t.Fatalf("service log = %q, want only the old daemon restarted", got)
	}
	if _, err := os.Stat(filepath.Join(f.home, "hold")); !os.IsNotExist(err) {
		t.Fatalf("hold file left behind (err=%v)", err)
	}
}

// A backup that fails partway must leave the live database files alone: the
// rollback restores the database only from a complete backup.
func TestInstallLocalBuild_PartialBackupLeavesTheLiveDatabase(t *testing.T) {
	f := newInstallFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root can read an unreadable file, so the backup would not fail")
	}
	wal := filepath.Join(f.home, "state.sqlite-wal")
	writeFile(t, wal, "unbacked wal pages")
	if err := os.Chmod(wal, 0o000); err != nil {
		t.Fatal(err)
	}

	out, code := f.run(t)
	if err := os.Chmod(wal, 0o644); err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Fatalf("exit %d, want 2 (rolled back):\n%s", code, out)
	}
	if !strings.Contains(out, "database backup failed") {
		t.Fatalf("output does not report the failed backup:\n%s", out)
	}
	if got := readFile(t, wal); got != "unbacked wal pages" {
		t.Fatalf("live WAL = %q, want it untouched", got)
	}
	if got := readFile(t, filepath.Join(f.home, "state.sqlite")); got != "original database" {
		t.Fatalf("live database = %q", got)
	}
	if v := versionOf(t, f.target); v != "no-mistakes version v1-old" {
		t.Fatalf("installed version = %q, want the untouched old build", v)
	}
	if got := readFile(t, filepath.Join(f.state, "daemon")); got != "running\n" {
		t.Fatalf("service state = %q, want the old daemon running again", got)
	}
}

func TestInstallLocalBuild_RefusesWhileAnotherHoldExists(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, filepath.Join(f.home, "hold"), "who: someone\nwhy: another install\n")

	out, code := f.run(t)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	if v := versionOf(t, f.target); v != "no-mistakes version v1-old" {
		t.Fatalf("installed version changed to %q", v)
	}
	if got := readFile(t, filepath.Join(f.home, "hold")); !strings.Contains(got, "another install") {
		t.Fatalf("the existing hold was replaced: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.state, "log")); !os.IsNotExist(err) {
		t.Fatalf("the service was touched: %s", readFile(t, filepath.Join(f.state, "log")))
	}
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func versionOf(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	return strings.TrimSpace(string(out))
}
