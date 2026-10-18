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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func reviewJSON(t *testing.T, findings Findings) *agent.Result {
	t.Helper()
	if findings.Items == nil {
		findings.Items = []Finding{}
	}
	if findings.RiskLevel == "" {
		findings.RiskLevel = "low"
		findings.RiskRationale = "bounded"
		findings.RiskScope = types.FindingsRiskScopeSourceOrExternal
	}
	j, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return &agent.Result{Output: j}
}

func inlineFixClaim() Finding {
	return Finding{
		Severity:      "info",
		File:          "feature.txt",
		Line:          1,
		Description:   "fixed typo 'featrue' in feature.txt",
		Action:        types.ActionNoOp,
		ReviewScope:   types.FindingReviewScopeSource,
		AppliedInline: true,
	}
}

// An inline-only round commits the reviewer's edit, verifies it in one
// focused read-only turn, and certifies the new head without a fix round.
func TestReviewStep_InlineFixIsCommittedVerifiedAndCertified(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		switch opts.Purpose {
		case "review":
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code, typo fixed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return reviewJSON(t, Findings{Items: []Finding{inlineFixClaim()}, ReviewedPaths: []string{"feature.txt"}}), nil
		case reviewInlineVerifyPurpose:
			return reviewJSON(t, Findings{ReviewedPaths: []string{"feature.txt"}}), nil
		}
		t.Fatalf("unexpected agent purpose %q", opts.Purpose)
		return nil, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.InlineFixes = true

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("inline-only round should not park: %s", outcome.Findings)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("expected review + verification turns, got %d calls", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "Inline quick fixes (enabled for this pass)") {
		t.Error("main review turn must carry the inline-fix permission")
	}
	verify := ag.calls[1]
	if strings.Contains(verify.Prompt, "Inline quick fixes (enabled for this pass)") {
		t.Error("verification turn must never be allowed to apply inline fixes")
	}
	if !strings.Contains(verify.Prompt, "Do NOT edit, create, or delete any file") {
		t.Error("verification turn must be read-only")
	}
	if !strings.Contains(verify.Prompt, headSHA) || !strings.Contains(verify.Prompt, sctx.Run.HeadSHA) {
		t.Error("verification prompt must name the inline commit range")
	}
	if sctx.Run.HeadSHA == headSHA {
		t.Fatal("inline edit was not committed")
	}
	if outcome.ReviewApprovedHeadSHA != sctx.Run.HeadSHA {
		t.Fatalf("approved head %s, want the verified inline head %s", outcome.ReviewApprovedHeadSHA, sctx.Run.HeadSHA)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree left dirty: %q", status)
	}
	if got := lastCommitMessage(t, dir); !strings.Contains(got, "apply reviewer inline fixes") {
		t.Fatalf("inline commit message = %q", got)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || !findings.Items[0].AppliedInline || findings.Items[0].Action != types.ActionNoOp || findings.Items[0].Severity != "info" {
		t.Fatalf("expected one applied-inline info/no-op record, got %+v", findings.Items)
	}
}

// A problem the verification turn finds in an inline edit is an ordinary
// blocking finding, so the normal fix round and full re-review take over.
func TestReviewStep_InlineFixVerificationFindingParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == reviewInlineVerifyPurpose {
			return reviewJSON(t, Findings{Items: []Finding{{
				Severity: "warning", File: "feature.txt", Line: 1, Action: types.ActionAutoFix,
				ReviewScope: types.FindingReviewScopeSource, Description: "inline edit changed behaviour; revert it",
			}}}), nil
		}
		os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code changed\n"), 0o644)
		return reviewJSON(t, Findings{Items: []Finding{inlineFixClaim()}, ReviewedPaths: []string{"feature.txt"}}), nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.InlineFixes = true

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a verification finding against an inline edit must park the round")
	}
	if !strings.Contains(outcome.Findings, "inline edit changed behaviour") {
		t.Fatalf("verification finding missing from round findings: %s", outcome.Findings)
	}
}

// A verification turn that cannot run never certifies the inline edit.
func TestReviewStep_InlineFixVerificationFailureParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == reviewInlineVerifyPurpose {
			return &agent.Result{Text: "no json"}, nil
		}
		os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code tidy\n"), 0o644)
		return reviewJSON(t, Findings{Items: []Finding{inlineFixClaim()}, ReviewedPaths: []string{"feature.txt"}}), nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.InlineFixes = true

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("an unverified inline edit must park the round")
	}
	if !strings.Contains(outcome.Findings, "could not be verified") {
		t.Fatalf("expected an unverified-inline-fix finding, got %s", outcome.Findings)
	}
}

// A claimed inline fix with no edit behind it goes back to the fixer; a claim
// can never drop a defect.
func TestReviewStep_InlineClaimWithoutEditReturnsToTheFixer(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return reviewJSON(t, Findings{Items: []Finding{inlineFixClaim()}, ReviewedPaths: []string{"feature.txt"}}), nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.InlineFixes = true

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("no edit means no verification turn, got %d calls", len(ag.calls))
	}
	if !outcome.NeedsApproval {
		t.Fatal("an unbacked inline claim must become a blocking finding")
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 || findings.Items[0].AppliedInline || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("expected the claim returned as an auto-fix finding, got %+v", findings.Items)
	}
}

// With inline fixes off the prompt carries no permission, nothing is
// committed, and a stray applied_inline field is ignored.
func TestReviewStep_InlineFixesOffKeepsTodaysReview(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	claim := inlineFixClaim()
	claim.Severity = "warning"
	claim.Action = types.ActionAutoFix
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return reviewJSON(t, Findings{Items: []Finding{claim}, ReviewedPaths: []string{"feature.txt"}}), nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ag.calls[0].Prompt, "Inline quick fixes") {
		t.Error("inline-fix permission must be absent when the setting is off")
	}
	if len(ag.calls) != 1 || sctx.Run.HeadSHA != headSHA {
		t.Fatal("inline fixes off must not verify or commit anything")
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if findings.Items[0].AppliedInline || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("stray claim should be ignored, got %+v", findings.Items[0])
	}
}

// A pass that starts on a dirty worktree cannot attribute edits to the
// reviewer, so inline fixes are off for it.
func TestReviewStep_InlineFixesOffOnDirtyWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("x"), 0o644)

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return reviewJSON(t, Findings{ReviewedPaths: []string{"feature.txt"}}), nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.InlineFixes = true

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ag.calls[0].Prompt, "Inline quick fixes") {
		t.Error("a dirty worktree must turn inline fixes off for the pass")
	}
}

// The review prompt asks for a per-finding fix estimate, and the fix prompt
// carries exactly one stable routing line summarizing the findings it fixes.
func TestReviewStep_FixPromptCarriesFixEstimateLine(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "review-fix" {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix"}`)}, nil
		}
		return reviewJSON(t, Findings{ReviewedPaths: fullReviewCoverage(t, dir, baseSHA)}), nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[` +
		`{"id":"r1","severity":"error","file":"a.go","description":"typo in error text","action":"auto-fix","fix_complexity":"simple","fix_lines":2,"fix_files":1},` +
		`{"id":"r2","severity":"info","file":"b.go","description":"race","action":"auto-fix","fix_complexity":"moderate","fix_lines":10,"fix_files":2}` +
		`],"summary":"2 issues"}`

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	fix := ag.calls[0].Prompt
	want := "\nFix complexity: moderate; estimated size: 12 lines across 3 files\n"
	if !strings.Contains(fix, want) {
		t.Fatalf("fix prompt missing routing line %q", want)
	}
	if strings.Count(fix, types.FixEstimatePrefix) != 1 {
		t.Fatalf("fix prompt must carry the routing line exactly once")
	}
	review := ag.calls[1].Prompt
	if !strings.Contains(review, `set fix_complexity to "simple"`) || !strings.Contains(review, "rather than by its severity") {
		t.Error("review prompt must ask for a per-finding fix complexity judged by reasoning, not severity")
	}
	if !strings.Contains(string(ag.calls[1].JSONSchema), `"fix_complexity"`) {
		t.Error("review schema must declare fix_complexity")
	}
}

// An intent-conformance finding parks the review as ask-user whatever
// severity or action the reviewer gave it, so it is never auto-approved and
// later rejected by the PR step.
func TestReviewStep_IntentConformanceFindingAlwaysParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return reviewJSON(t, Findings{Items: []Finding{{
			Severity: "info", File: "feature.txt", Line: 1, Action: types.ActionNoOp,
			ReviewScope: types.FindingReviewScopeSource, Category: types.FindingCategoryIntentConformance,
			Description: "intent requires X; change omits it",
		}}, ReviewedPaths: []string{"feature.txt"}}), nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("an intent-conformance finding must park the review")
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if got := findings.Items[0]; got.Action != types.ActionAskUser || got.Severity != types.FindingSeverityWarning {
		t.Fatalf("intent-conformance finding must become an ask-user warning, got %+v", got)
	}
}

// A verification turn that edits files is not trusted: the round parks and its
// stray edits are discarded so no later step can commit unreviewed work.
func TestReviewStep_InlineVerifierEditsAreDiscardedAndPark(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == reviewInlineVerifyPurpose {
			os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("verifier rewrite\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644)
			return reviewJSON(t, Findings{ReviewedPaths: []string{"feature.txt"}}), nil
		}
		os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code tidy\n"), 0o644)
		return reviewJSON(t, Findings{Items: []Finding{inlineFixClaim()}, ReviewedPaths: []string{"feature.txt"}}), nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.InlineFixes = true

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "could not be verified") {
		t.Fatalf("a verifier that edits files must park the round: %s", outcome.Findings)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("verifier edits left in the worktree: %q", status)
	}
	got, err := os.ReadFile(filepath.Join(dir, "feature.txt"))
	if err != nil || string(got) != "feature code tidy\n" {
		t.Fatalf("feature.txt = %q, want the committed inline fix kept", got)
	}
}
