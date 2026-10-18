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
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// estimatedFindings is a findings payload whose step recorded estimates.
const estimatedFindings = `{"findings":[` +
	`{"id":"f1","severity":"error","file":"a.go","description":"first","action":"auto-fix","fix_complexity":"simple","fix_lines":3,"fix_files":1},` +
	`{"id":"f2","severity":"info","file":"b.go","description":"second","action":"auto-fix","fix_complexity":"moderate","fix_lines":4,"fix_files":1}` +
	`],"summary":"two issues"}`

const estimatedLine = "\nFix complexity: moderate; estimated size: 7 lines across 2 files\n"

func assertOneFixEstimateLine(t *testing.T, prompt, want string) {
	t.Helper()
	if !strings.Contains(prompt+"\n", want) {
		t.Fatalf("fix prompt missing %q:\n%s", strings.TrimSpace(want), prompt)
	}
	if n := strings.Count(prompt, types.FixEstimatePrefix); n != 1 {
		t.Fatalf("fix prompt carries %d fix estimate lines, want exactly 1", n)
	}
}

func TestLintStep_FixPromptCarriesFixEstimateLine(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		os.WriteFile(filepath.Join(dir, "lint-fix.txt"), []byte("fixed"), 0o644)
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = estimatedFindings
	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	assertOneFixEstimateLine(t, ag.calls[0].Prompt, estimatedLine)
}

// A finding from a failing configured command has no agent judgement behind
// it, so it routes as complex with an unknown (zero) size.
func TestLintStep_FixPromptForCommandFailureDefaultsToComplex(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"lint-1","severity":"warning","description":"linter found issues (exit code 1)","action":"auto-fix"}],"summary":"main.go:10: unused variable x"}`
	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	assertOneFixEstimateLine(t, ag.calls[0].Prompt, "\nFix complexity: complex; estimated size: 0 lines across 0 files\n")
}

func TestTestStep_FixPromptCarriesFixEstimateLine(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix tests","findings":[],"tested":["go test ./..."],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"works","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = estimatedFindings
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	assertOneFixEstimateLine(t, ag.calls[0].Prompt, estimatedLine)
}

func TestCustomGateStep_FixPromptCarriesFixEstimateLine(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		os.WriteFile(filepath.Join(dir, "gate-satisfied.txt"), []byte("ok"), 0o644)
		return &agent.Result{Output: json.RawMessage(`{"summary":"satisfy gate"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = estimatedFindings
	step := &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepTest, Command: fileGateCommand("gate-satisfied.txt")}}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	assertOneFixEstimateLine(t, ag.calls[0].Prompt, estimatedLine)
}

func TestDocumentStep_FixPromptCarriesFixEstimateLine(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.PreviousFindings = estimatedFindings
	assertOneFixEstimateLine(t, (&DocumentStep{}).buildPrompt(sctx, baseSHA, "none", false), estimatedLine)

	sctx.PreviousFindings = ""
	if strings.Contains((&DocumentStep{}).buildPrompt(sctx, baseSHA, "none", false), types.FixEstimatePrefix) {
		t.Fatal("a first document pass hands over no findings and must carry no fix estimate line")
	}
}

func TestCIStep_FixPromptCarriesFixEstimateLine(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	var prompt string
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompt = opts.Prompt
		return &agent.Result{}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	if _, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false)); err != nil {
		t.Fatal(err)
	}
	// A failing check is recorded without agent judgement.
	assertOneFixEstimateLine(t, prompt, "\nFix complexity: complex; estimated size: 0 lines across 0 files")
}

// Every agent that records findings is asked to estimate each fix, and every
// findings schema accepts the estimate.
func TestFindingProducersRequestFixEstimates(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string]json.RawMessage{
		"findings":     findingsSchema,
		"test":         testFindingsSchema,
		"review":       reviewFindingsSchema,
		"housekeeping": housekeepingFindingsSchema,
	} {
		var parsed struct {
			Properties struct {
				Findings struct {
					Items struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"items"`
				} `json:"findings"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Fatalf("%s schema is not valid JSON: %v", name, err)
		}
		for _, field := range []string{"fix_complexity", "fix_lines", "fix_files"} {
			if _, ok := parsed.Properties.Findings.Items.Properties[field]; !ok {
				t.Errorf("%s schema missing %s", name, field)
			}
		}
	}

	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	if !strings.Contains((&DocumentStep{}).buildPrompt(sctx, baseSHA, "none", true), fixEstimateRule) {
		t.Error("document prompt must ask for fix estimates")
	}
}

func TestLintAndTestAgentPromptsRequestFixEstimates(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"clean","findings":[],"tested":["checked"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"works","result":"pass","live":true,"evidence":"checked","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) < 2 {
		t.Fatalf("expected a lint agent turn and a test agent turn, got %d calls", len(ag.calls))
	}
	for i, call := range ag.calls {
		if !strings.Contains(call.Prompt, fixEstimateRule) {
			t.Errorf("agent call %d (%s) does not ask for fix estimates", i, call.Purpose)
		}
	}
}
