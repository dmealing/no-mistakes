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
