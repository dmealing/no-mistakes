package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/triage"
)

// triageRunFixture is a fake daemon (olderDaemonFixture) whose run database
// also lists `lanes` other branches of the same project in validation,
// registered from another checkout. The operator's branch is an empty commit:
// a small change.
func triageRunFixture(t *testing.T, lanes ...string) (launched *[]string, p *paths.Paths) {
	t.Helper()
	launched = olderDaemonFixture(t, nil)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repos, err := d.GetRepos()
	if err != nil || len(repos) != 1 {
		t.Fatalf("fixture repos = %v, %v", repos, err)
	}
	// A bare gate so a launch the check lets through can push.
	gate := p.RepoDir(repos[0].ID)
	cliGit(t, t.TempDir(), "init", "-q", "--bare", gate)
	cliGit(t, gate, "config", "receive.advertisePushOptions", "true")
	wd, _ := os.Getwd()
	cliGit(t, wd, "remote", "add", "no-mistakes", gate)
	peer, err := d.InsertRepo(t.TempDir(), repos[0].UpstreamURL, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range lanes {
		if _, err := d.InsertRun(peer.ID, "refs/heads/"+branch, strings.Repeat("a", 40), strings.Repeat("b", 40)); err != nil {
			t.Fatal(err)
		}
	}
	return launched, p
}

func runAxiRunForTriageTest(t *testing.T, override *string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.Flags().String(triageOverrideFlag, "", "")
	if override != nil {
		if err := cmd.Flags().Set(triageOverrideFlag, *override); err != nil {
			t.Fatal(err)
		}
	}
	err := runAxiRunWithLaunchProof(cmd, false, nil, "ship the widget", "", false, "", "", 2*time.Second)
	return out.String(), err
}

func TestAxiRunStopsTheThirdBranchOfAProject(t *testing.T) {
	launched, _ := triageRunFixture(t, "lane-a", "lane-b")

	out, err := runAxiRunForTriageTest(t, nil)
	if code, ok := err.(*exitError); !ok || code.code != triageStopExitCode {
		t.Fatalf("err = %v, want exit %d\n%s", err, triageStopExitCode, out)
	}
	if !strings.Contains(out, "verdict: stop") || !strings.Contains(out, string(triage.RuleThirdBranch)) || !strings.Contains(out, "lane-a") || !strings.Contains(out, "lane-b") {
		t.Fatalf("output does not report the third-branch stop:\n%s", out)
	}
	if len(*launched) != 0 {
		t.Fatalf("a stopped check still launched: %v", *launched)
	}
}

func TestAxiRunStopsASmallChangeWithASibling(t *testing.T) {
	launched, _ := triageRunFixture(t, "lane-a")

	out, err := runAxiRunForTriageTest(t, nil)
	if code, ok := err.(*exitError); !ok || code.code != triageStopExitCode {
		t.Fatalf("err = %v, want exit %d\n%s", err, triageStopExitCode, out)
	}
	if !strings.Contains(out, string(triage.RuleSmallWithSibling)) || !strings.Contains(out, "--triage-override") {
		t.Fatalf("output does not report the small-change stop and how to override it:\n%s", out)
	}
	if len(*launched) != 0 {
		t.Fatalf("a stopped check still launched: %v", *launched)
	}
}

func TestAxiRunOverrideStartsTheRunAndRecordsTheReason(t *testing.T) {
	launched, p := triageRunFixture(t, "lane-a")
	reason := "urgent fix for a broken main"

	out, _ := runAxiRunForTriageTest(t, &reason)
	if len(*launched) == 0 {
		t.Fatalf("an overridden check did not launch the run:\n%s", out)
	}
	if !strings.Contains(out, "override_reason: "+reason) {
		t.Fatalf("output does not show the override:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(p.Root(), triage.OverrideLogName))
	if err != nil {
		t.Fatalf("override not recorded: %v", err)
	}
	if !strings.Contains(string(data), `"reason":"`+reason+`"`) || !strings.Contains(string(data), `"rules":["small-change-with-sibling"]`) || !strings.Contains(string(data), `"branch":"feature/omit"`) {
		t.Fatalf("override record = %s", data)
	}
}

func TestAxiRunOverrideNeedsAReason(t *testing.T) {
	launched, _ := triageRunFixture(t, "lane-a")
	blank := "   "

	out, err := runAxiRunForTriageTest(t, &blank)
	if err == nil || !strings.Contains(out, "needs the reason") {
		t.Fatalf("a blank override reason was accepted: %v\n%s", err, out)
	}
	if len(*launched) != 0 {
		t.Fatalf("launched without a reason: %v", *launched)
	}
}

func TestAxiRunHoldBlocksANewRunEvenWithAnOverride(t *testing.T) {
	launched, p := triageRunFixture(t)
	if err := os.WriteFile(triage.HoldPath(p.Root()), []byte("who: installer\nwhy: install a new build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reason := "urgent"

	out, err := runAxiRunForTriageTest(t, &reason)
	if code, ok := err.(*exitError); !ok || code.code != triageStopExitCode {
		t.Fatalf("err = %v, want exit %d\n%s", err, triageStopExitCode, out)
	}
	if !strings.Contains(out, "rule: hold") && !strings.Contains(out, "hold,") {
		t.Fatalf("output does not report the hold:\n%s", out)
	}
	if !strings.Contains(out, "install a new build") || !strings.Contains(out, "cannot be overridden") {
		t.Fatalf("output does not explain the hold:\n%s", out)
	}
	if len(*launched) != 0 {
		t.Fatalf("launched under a hold: %v", *launched)
	}
}

func TestAxiRunGoesWithOneSiblingAndALargeChange(t *testing.T) {
	launched, _ := triageRunFixture(t, "lane-a")
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("// line\n")
	}
	wd, _ := os.Getwd()
	if err := os.WriteFile(filepath.Join(wd, "big.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, wd, "add", "big.go")
	cliGit(t, wd, "commit", "-m", "big")

	out, _ := runAxiRunForTriageTest(t, nil)
	if len(*launched) == 0 {
		t.Fatalf("a large change with one sibling must start:\n%s", out)
	}
	if !strings.Contains(out, "1 other branch of this project: lane-a") {
		t.Fatalf("output does not print the bundling facts:\n%s", out)
	}
}

// commitRepoTriageScript commits a code file (built-in tier CODE) and puts a
// scripts/nm-triage.sh whose body is script in the checkout, ignored by git so
// the script itself is not part of the change.
func commitRepoTriageScript(t *testing.T, script string) {
	t.Helper()
	wd, _ := os.Getwd()
	if err := os.WriteFile(filepath.Join(wd, ".git", "info", "exclude"), []byte("/scripts/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wd, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "scripts", "nm-triage.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "widget.go"), []byte("package widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, wd, "add", "widget.go")
	cliGit(t, wd, "commit", "-m", "widget")
}

func runAxiTriageForTest(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := runAxiTriage(cmd, triageOptions{offline: true}, false); err != nil {
		t.Fatalf("axi triage: %v\n%s", err, out.String())
	}
	return out.String()
}

var reportedTier = regexp.MustCompile(`(?m)^\s*tier: (\S+)`)

func tierIn(t *testing.T, out string) string {
	t.Helper()
	m := reportedTier.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no tier reported:\n%s", out)
	}
	return m[1]
}

// The repository's own script decides the tier for both commands: its
// documented `TIER: DOCS` maps to DOCS, where the built-in rules say CODE.
func TestAxiTriageAndAxiRunReportTheRepositoryScriptsTier(t *testing.T) {
	launched, _ := triageRunFixture(t)
	commitRepoTriageScript(t, "#!/usr/bin/env bash\necho 'TIER: DOCS (repository relaxation)'\n")

	triageOut := runAxiTriageForTest(t)
	runOut, _ := runAxiRunForTriageTest(t, nil)
	if len(*launched) == 0 {
		t.Fatalf("the run did not start:\n%s", runOut)
	}
	if got := tierIn(t, triageOut); got != "DOCS" {
		t.Fatalf("axi triage tier = %s, want the script's DOCS:\n%s", got, triageOut)
	}
	if got := tierIn(t, runOut); got != "DOCS" {
		t.Fatalf("axi run tier = %s, want the script's DOCS (as axi triage reports):\n%s", got, runOut)
	}
}

func TestAxiTriageAndAxiRunNoteAnUnusableRepositoryScript(t *testing.T) {
	for name, script := range map[string]string{
		"fails":   "#!/usr/bin/env bash\necho 'TIER: DOCS'\nexit 1\n",
		"no tier": "#!/usr/bin/env bash\necho 'nothing to say'\n",
	} {
		t.Run(name, func(t *testing.T) {
			triageRunFixture(t)
			commitRepoTriageScript(t, script)

			triageOut := runAxiTriageForTest(t)
			runOut, _ := runAxiRunForTriageTest(t, nil)
			for cmdName, out := range map[string]string{"axi triage": triageOut, "axi run": runOut} {
				if got := tierIn(t, out); got != "CODE" {
					t.Fatalf("%s tier = %s, want the built-in CODE:\n%s", cmdName, got, out)
				}
				var notes []string
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(line, "repo_script:") {
						notes = append(notes, line)
					}
				}
				if len(notes) != 1 || !strings.Contains(notes[0], "scripts/nm-triage.sh") || !strings.Contains(notes[0], "the built-in tier was used") {
					t.Fatalf("%s does not print one note about the script: %q\n%s", cmdName, notes, out)
				}
			}
		})
	}
}
