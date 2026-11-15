package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// testStepPromptsForCIMode drives one Test step in fix mode under the given
// resolved CI mode and returns the fix-round prompt and the evidence-turn
// prompt, in that order.
func testStepPromptsForCIMode(t *testing.T, mode config.CIMode) (fixPrompt, evidencePrompt string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix targeted failure","findings":[],"tested":["go test ./internal/cli -run TestDoctor"],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./internal/cli -run TestDoctor","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.CIMode = mode
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("expected a fix turn and an evidence turn, got %d agent calls", len(ag.calls))
	}
	return ag.calls[0].Prompt, ag.calls[1].Prompt
}

// Upstream github mode keeps remote CI as the broad-regression owner, so the
// Test prompts must carry the unchanged targeted-validation wording.
func TestTestStep_GitHubCIMode_DefersBroadRegressionToRemoteCI(t *testing.T) {
	t.Parallel()
	fixPrompt, evidencePrompt := testStepPromptsForCIMode(t, config.CIModeGitHub)

	for _, want := range []string{
		"Do NOT run the complete repository test suite. Local Test is targeted validation of the failure and the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary. Keep verification focused on the failure and intent.",
	} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("github-mode fix prompt missing %q, got:\n%s", want, fixPrompt)
		}
	}
	for _, want := range []string{
		"Do NOT run the complete repository test suite. Local Test is targeted validation of the requested intent; remote CI owns broad regression and remains mandatory before a PR is ready.",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary.",
	} {
		if !strings.Contains(evidencePrompt, want) {
			t.Errorf("github-mode evidence prompt missing %q, got:\n%s", want, evidencePrompt)
		}
	}
	for name, prompt := range map[string]string{"fix": fixPrompt, "evidence": evidencePrompt} {
		if strings.Contains(prompt, "This step owns broad regression for this change") {
			t.Errorf("github-mode %s prompt must not claim local ownership of broad regression:\n%s", name, prompt)
		}
	}
}

// When CI is local no forge checks run for this change, so the evidence turn
// owns broad regression and no prompt may defer to remote CI. An unset mode
// resolves to local, matching config.DefaultCIMode.
func TestTestStep_LocalCIMode_OwnsBroadRegression(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mode config.CIMode
	}{
		{name: "explicit local", mode: config.CIModeLocal},
		{name: "unset resolves to local", mode: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixPrompt, evidencePrompt := testStepPromptsForCIMode(t, tc.mode)

			for _, want := range []string{
				"Do NOT run the complete repository test suite in this fix round.",
				"this step's own evidence turn runs the broad regression suite after you finish",
				"does NOT move that broad run into this fix round",
			} {
				if !strings.Contains(fixPrompt, want) {
					t.Errorf("local-mode fix prompt missing %q, got:\n%s", want, fixPrompt)
				}
			}
			for _, want := range []string{
				"After the targeted scenarios, run this repository's complete regression test suite",
				"This step owns broad regression for this change: no later step and no external system runs one.",
				"Report a suite failure as a finding even when this change did not obviously cause it.",
				"Run the targeted scenarios first and the complete regression suite second.",
			} {
				if !strings.Contains(evidencePrompt, want) {
					t.Errorf("local-mode evidence prompt missing %q, got:\n%s", want, evidencePrompt)
				}
			}
			// Nothing downstream will catch a regression, so no agent-facing
			// text may say remote CI is mandatory or will run at all.
			for name, prompt := range map[string]string{"fix": fixPrompt, "evidence": evidencePrompt} {
				for _, forbid := range []string{
					"remote CI",
					"mandatory before a PR is ready",
					"CI owns broad regression",
				} {
					if strings.Contains(prompt, forbid) {
						t.Errorf("local-mode %s prompt still defers to remote CI via %q:\n%s", name, forbid, prompt)
					}
				}
			}
			// The targeted-scenario contract is not traded away for the suite.
			if !strings.Contains(evidencePrompt, "Derive the scenarios this change must satisfy, then run each one against the real running product") {
				t.Errorf("local-mode evidence prompt dropped scenario derivation, got:\n%s", evidencePrompt)
			}
		})
	}
}

// fullSuiteEvidenceRule is the opening of the local evidence turn's
// complete-suite instruction, which a passing configured suite replaces.
const fullSuiteEvidenceRule = "After the targeted scenarios, run this repository's complete regression test suite"

// localEvidencePrompts drives one local-mode Test step with the given
// configured commands and returns every agent prompt in call order, plus the
// run head before and after the step (a fix round advances it).
func localEvidencePrompts(t *testing.T, cmds config.Commands, fixing bool, additional ...string) (prompts []string, startHead, endHead string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Only the fix turn edits; the evidence turn leaves the head alone.
			if strings.HasPrefix(opts.Prompt, "Fix the failing tests") {
				os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix targeted failure","findings":[],"tested":["go test ./internal/cli -run TestDoctor"],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./internal/cli -run TestDoctor","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	sctx.Config.CIMode = config.CIModeLocal
	if len(additional) > 0 {
		sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: additional}}
	}
	if fixing {
		sctx.Fixing = true
		sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix"}],"summary":"FAIL: TestFoo"}`
	}
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	for _, call := range ag.calls {
		prompts = append(prompts, call.Prompt)
	}
	return prompts, headSHA, sctx.Run.HeadSHA
}

func assertFullSuiteAsked(t *testing.T, evidencePrompt string) {
	t.Helper()
	for _, want := range []string{
		fullSuiteEvidenceRule,
		"This step owns broad regression for this change: no later step and no external system runs one.",
		"Run the targeted scenarios first and the complete regression suite second.",
	} {
		if !strings.Contains(evidencePrompt, want) {
			t.Errorf("evidence prompt must still ask for the complete suite; missing %q, got:\n%s", want, evidencePrompt)
		}
	}
	if strings.Contains(evidencePrompt, "already ran and passed at this target commit") {
		t.Errorf("evidence prompt claims a configured suite passed:\n%s", evidencePrompt)
	}
}

// A configured commands.test that passed at the validated head already is the
// broad regression run, so the local evidence turn keeps its targeted
// scenarios but is not asked to walk the complete suite a second time, and
// still records where broad regression came from.
func TestTestStep_LocalCIMode_PassedConfiguredSuiteIsNotRerun(t *testing.T) {
	t.Parallel()
	prompts, _, _ := localEvidencePrompts(t, config.Commands{Test: "exit 0"}, false)
	if len(prompts) != 1 {
		t.Fatalf("expected one evidence turn, got %d agent calls", len(prompts))
	}
	evidencePrompt := prompts[0]
	for _, want := range []string{
		"Broad regression for this change is the configured test command `exit 0`, which already ran and passed at this target commit",
		"Do NOT run this repository's complete regression test suite again in this turn.",
		"Record in \"tested\" that broad regression came from the configured test command `exit 0` with its passing outcome",
		"Run the targeted scenarios only; the configured test command's passing run at this commit already supplied broad regression.",
		"Configured test command already ran successfully as baseline: `exit 0`",
		"Derive the scenarios this change must satisfy, then run each one against the real running product",
	} {
		if !strings.Contains(evidencePrompt, want) {
			t.Errorf("evidence prompt missing %q, got:\n%s", want, evidencePrompt)
		}
	}
	for _, forbid := range []string{fullSuiteEvidenceRule, "the complete regression suite second", "remote CI"} {
		if strings.Contains(evidencePrompt, forbid) {
			t.Errorf("evidence prompt still asks for %q after the configured suite passed:\n%s", forbid, evidencePrompt)
		}
	}
}

// Without commands.test the evidence turn is the only broad regression run, so
// it keeps asking for the complete suite; a machine-local additional check is
// the operator's addition, not the repository's suite, and never stands in.
func TestTestStep_LocalCIMode_AbsentConfiguredSuiteStillRunsFullSuite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		additional []string
	}{
		{name: "nothing configured"},
		{name: "only a passing machine-local check", additional: []string{"exit 0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prompts, _, _ := localEvidencePrompts(t, config.Commands{}, false, tc.additional...)
			if len(prompts) != 1 {
				t.Fatalf("expected one evidence turn, got %d agent calls", len(prompts))
			}
			assertFullSuiteAsked(t, prompts[0])
		})
	}
}

// A failed configured suite proves nothing passed at this head, so the evidence
// turn keeps the complete-suite run.
func TestTestStep_LocalCIMode_FailedConfiguredSuiteStillRunsFullSuite(t *testing.T) {
	t.Parallel()
	prompts, _, _ := localEvidencePrompts(t, config.Commands{Test: "exit 3"}, false)
	if len(prompts) != 1 {
		t.Fatalf("expected one evidence turn, got %d agent calls", len(prompts))
	}
	if !strings.Contains(prompts[0], "Configured test command failed with exit code 3: `exit 3`") {
		t.Fatalf("evidence prompt missing the failed baseline, got:\n%s", prompts[0])
	}
	assertFullSuiteAsked(t, prompts[0])
}

// A fix round commits a new head before the configured suite runs, so that
// suite runs at the head the evidence turn validates and its pass counts there.
func TestTestStep_LocalCIMode_ConfiguredSuiteRerunsAtTheFixRoundsHead(t *testing.T) {
	t.Parallel()
	prompts, startHead, fixedHead := localEvidencePrompts(t, config.Commands{Test: "exit 0"}, true)
	if len(prompts) != 2 {
		t.Fatalf("expected a fix turn and an evidence turn, got %d agent calls", len(prompts))
	}
	if fixedHead == startHead || !strings.Contains(prompts[1], "- target commit: "+fixedHead) {
		t.Fatalf("evidence turn must validate the fix round's head %q, got:\n%s", fixedHead, prompts[1])
	}
	if !strings.Contains(prompts[1], "which already ran and passed at this target commit") || strings.Contains(prompts[1], fullSuiteEvidenceRule) {
		t.Errorf("configured suite passed at the fix round's head, but the evidence prompt still asks for a full run:\n%s", prompts[1])
	}
	if !strings.Contains(prompts[0], "Do NOT run the complete repository test suite in this fix round.") {
		t.Errorf("fix-round rules changed, got:\n%s", prompts[0])
	}
}

// The decision itself: a pass counts only for the repository's own command, at
// the head the evidence turn validates, in local mode.
func TestWithConfiguredSuiteResult(t *testing.T) {
	t.Parallel()
	passed := []checkResult{{Command: "make test"}}
	for _, tc := range []struct {
		name         string
		scope        testRegressionScope
		testCmd      string
		baseline     []checkResult
		baselineHead string
		evidenceHead string
		wantNarrowed bool
	}{
		{name: "passed at the validated head", scope: localRegressionScope, testCmd: "make test", baseline: passed, baselineHead: "h1", evidenceHead: "h1", wantNarrowed: true},
		{name: "passed alongside a failed machine-local check", scope: localRegressionScope, testCmd: "make test", baseline: []checkResult{{Command: "make test"}, {Command: "extra", Local: true, ExitCode: 1}}, baselineHead: "h1", evidenceHead: "h1", wantNarrowed: true},
		{name: "passed at an older head", scope: localRegressionScope, testCmd: "make test", baseline: passed, baselineHead: "h0", evidenceHead: "h1"},
		{name: "not run", scope: localRegressionScope, testCmd: "make test", evidenceHead: "h1"},
		{name: "failed", scope: localRegressionScope, testCmd: "make test", baseline: []checkResult{{Command: "make test", ExitCode: 2}}, baselineHead: "h1", evidenceHead: "h1"},
		{name: "only a machine-local check passed", scope: localRegressionScope, baseline: []checkResult{{Command: "extra", Local: true}}, baselineHead: "h1", evidenceHead: "h1"},
		{name: "github mode is untouched", scope: githubRegressionScope, testCmd: "make test", baseline: passed, baselineHead: "h1", evidenceHead: "h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.scope.withConfiguredSuiteResult(tc.testCmd, tc.baseline, tc.baselineHead, tc.evidenceHead)
			if got.fixSuiteRule != tc.scope.fixSuiteRule || got.fixOverrideRule != tc.scope.fixOverrideRule {
				t.Fatalf("fix-round rules must never change")
			}
			if narrowed := got != tc.scope; narrowed != tc.wantNarrowed {
				t.Fatalf("narrowed = %v, want %v (evidence rule %q)", narrowed, tc.wantNarrowed, got.evidenceSuiteRule)
			}
		})
	}
}
