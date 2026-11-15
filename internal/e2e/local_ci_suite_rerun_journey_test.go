//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// localSuiteEvidenceRule is the local evidence turn's complete-suite
// instruction. A configured commands.test that passed at the validated head
// replaces it with the "already ran and passed" wording instead.
const localSuiteEvidenceRule = "After the targeted scenarios, run this repository's complete regression test suite"

// TestLocalCIModeConfiguredSuiteBroadRegressionJourney drives the real binary,
// daemon, gate push, and Test step under each ci_mode and commands.test
// outcome, then reads the evidence-turn prompt the fake agent received. It
// proves who owns the repository-wide suite: a passing configured command at
// the validated head is that run, so local mode does not walk the suite again;
// every other case keeps the complete-suite instruction.
func TestLocalCIModeConfiguredSuiteBroadRegressionJourney(t *testing.T) {
	type journey struct {
		name   string
		ciMode string
		// testCmd is the trusted commands.test script body ("" = unset).
		testCmd string
		// localCheck is a machine-local additional test check body ("" = none).
		localCheck string
		// wantPark: the configured command failed, so the Test step parks.
		wantPark bool
		// wantPassedRule: the evidence turn must say the configured suite
		// already ran and passed and must not ask for the complete suite.
		wantPassedRule bool
		// wantGitHubWording: the github-mode targeted wording remains.
		wantGitHubWording bool
	}
	for _, tc := range []journey{
		{
			name:           "local_passing_configured_suite_is_not_rerun",
			ciMode:         "local",
			testCmd:        "echo 'ok 1 - all green'\nexit 0\n",
			wantPassedRule: true,
		},
		{
			name:     "local_failed_configured_suite_still_gets_complete_suite_run",
			ciMode:   "local",
			testCmd:  "echo 'not ok 1 - broken'\nexit 3\n",
			wantPark: true,
		},
		{
			name:    "local_without_configured_suite_keeps_complete_suite_run",
			ciMode:  "local",
			testCmd: "",
		},
		{
			name:       "local_machine_local_check_never_stands_in_for_the_suite",
			ciMode:     "local",
			testCmd:    "",
			localCheck: "echo 'local smoke ok'\nexit 0\n",
		},
		{
			name:              "github_passing_configured_suite_keeps_remote_ci_wording",
			ciMode:            "github",
			testCmd:           "echo 'ok 1 - all green'\nexit 0\n",
			wantGitHubWording: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: "ci_mode: " + tc.ciMode})
			config := "ignore_patterns:\n  - '*.generated.go'\n"
			if tc.testCmd != "" {
				h.CommitChange("main", "scripts/test.sh", "#!/bin/sh\n"+tc.testCmd, "add test script")
				config += "commands:\n  test: sh scripts/test.sh\n"
			}
			h.CommitChange("main", ".no-mistakes.yaml", config, "configure test command")
			if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
				t.Fatalf("push trusted config: %v\n%s", err, out)
			}
			if tc.localCheck != "" {
				check := filepath.Join(t.TempDir(), "local-check.sh")
				writeMachineLocalScript(t, check, tc.localCheck)
				setupMachineLocalCommands(t, h, "    commands:\n      test:\n        additional:\n          - sh "+check+"\n")
			} else if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}

			branch := "suite-" + strings.ReplaceAll(tc.name, "_", "-")
			h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
			h.PushToGate(branch)

			status := types.StepStatusCompleted
			if tc.wantPark {
				status = types.StepStatusAwaitingApproval
			}
			run := waitForStepStatus(t, h, branch, types.StepTest, status, 180*time.Second)
			t.Logf("run %s reached Test status %s", run.ID, status)

			prompts := testStepPrompts(h)
			if len(prompts) != 1 {
				t.Fatalf("Test step invoked the agent %d times, want exactly one evidence turn", len(prompts))
			}
			prompt := prompts[0]
			hasFullSuite := strings.Contains(prompt, localSuiteEvidenceRule)
			hasPassedRule := strings.Contains(prompt, "already ran and passed at this target commit")

			switch {
			case tc.wantPassedRule:
				if hasFullSuite {
					t.Errorf("local evidence turn re-asks for the complete suite after the configured command passed")
				}
				if !hasPassedRule || !strings.Contains(prompt, "Configured test command already ran successfully as baseline: `sh scripts/test.sh`") {
					t.Errorf("local evidence turn does not record the passed configured suite as broad regression")
				}
				if !strings.Contains(prompt, "Do NOT run this repository's complete regression test suite again in this turn.") {
					t.Errorf("local evidence turn lacks the do-not-rerun instruction")
				}
			case tc.wantGitHubWording:
				if hasFullSuite || hasPassedRule {
					t.Errorf("github evidence turn changed its broad-regression wording")
				}
				if !strings.Contains(prompt, "remote CI owns broad regression") {
					t.Errorf("github evidence turn lost the remote-CI ownership wording")
				}
			default:
				if !hasFullSuite {
					t.Errorf("local evidence turn dropped the complete-suite instruction when no passing configured suite exists")
				}
				if hasPassedRule {
					t.Errorf("local evidence turn claims a configured suite passed when none did")
				}
			}
			if tc.wantPark && !strings.Contains(prompt, "Configured test command failed with exit code 3") {
				t.Errorf("failed baseline not carried into the evidence prompt")
			}
			// The targeted-scenario contract survives in every mode.
			if !strings.Contains(prompt, "Derive the scenarios this change must satisfy, then run each one against the real running product") {
				t.Errorf("evidence prompt dropped scenario derivation")
			}
		})
	}
}
