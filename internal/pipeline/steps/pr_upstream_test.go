package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	upstreamTestIntent     = "stale task intent the maintainer never saw"
	upstreamTestInlineText = "inline evidence: checkout returned 200"
	upstreamTestEmbedText  = "embedded evidence file body"
)

func tidyPRAgent() *mockAgent {
	return &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"fix: tidy","body":"## What Changed\n\n- tidy"}`)}, nil
		},
	}
}

// newUpstreamBodyContext builds a PR step context whose Test step recorded a
// path-only local artifact, a captioned local artifact, an embeddable evidence
// file, and an inline content artifact, plus an authoritative intent.
func newUpstreamBodyContext(t *testing.T, upstreamURL, forkURL string) (*pipeline.StepContext, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, tidyPRAgent(), dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstreamURL
	sctx.Repo.ForkURL = forkURL
	sctx.UserIntent = upstreamTestIntent
	sctx.EvidenceDir = t.TempDir()

	localOnly := filepath.Join(sctx.EvidenceDir, "missing.bin")
	captioned := filepath.Join(sctx.EvidenceDir, "missing-captioned.bin")
	embedded := filepath.Join(sctx.EvidenceDir, "server.log")
	if err := os.WriteFile(embedded, []byte(upstreamTestEmbedText), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := findingsJSON(t, types.Findings{
		TestingSummary: "Evidence was collected.",
		Artifacts: []types.TestArtifact{
			{Kind: "log", Label: "Local only log", Path: localOnly},
			{Kind: "log", Label: "Captioned log", Path: captioned, Content: "captioned evidence text"},
			{Kind: "log", Label: "Server log", Path: embedded},
			{Kind: "log", Label: "Inline log", Content: upstreamTestInlineText},
		},
	})
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	return sctx, baseSHA
}

func buildUpstreamTestBody(t *testing.T, sctx *pipeline.StepContext, baseSHA string, bodyLimit int) string {
	t.Helper()
	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, bodyLimit)
	if err != nil {
		t.Fatal(err)
	}
	return content.Body
}

func TestPRBody_ForeignOwnerOmitsIntentAndLocalPathEvidence(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 60000} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()
			sctx, baseSHA := newUpstreamBodyContext(t, "https://github.com/parent-owner/widgets.git", "git@github.com:fork-owner/widgets.git")
			body := buildUpstreamTestBody(t, sctx, baseSHA, limit)

			for _, unwanted := range []string{"## Intent", upstreamTestIntent, "local file:", "missing.bin", "missing-captioned.bin"} {
				if strings.Contains(body, unwanted) {
					t.Fatalf("foreign-owner PR body must not contain %q, got:\n%s", unwanted, body)
				}
			}
			for _, want := range []string{"## What Changed", "## Testing", upstreamTestInlineText, "captioned evidence text", upstreamTestEmbedText, pipelineAttestationCommentPrefix} {
				if !strings.Contains(body, want) {
					t.Fatalf("foreign-owner PR body missing %q, got:\n%s", want, body)
				}
			}
		})
	}
}

func TestPRBody_SameOwnerKeepsIntentAndLocalPathEvidence(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 60000} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()
			sctx, baseSHA := newUpstreamBodyContext(t, "https://github.com/owner/widgets.git", "")
			noFork := buildUpstreamTestBody(t, sctx, baseSHA, limit)
			for _, want := range []string{"## Intent\n\n" + upstreamTestIntent, "local file:", "missing.bin", upstreamTestInlineText, pipelineAttestationCommentPrefix} {
				if !strings.Contains(noFork, want) {
					t.Fatalf("same-owner PR body missing %q, got:\n%s", want, noFork)
				}
			}

			// A fork registered under the same owner (any case) is not a
			// foreign-owner PR, so its body is byte-identical.
			sctx.Repo.ForkURL = "git@github.com:OWNER/widgets.git"
			if sameOwnerFork := buildUpstreamTestBody(t, sctx, baseSHA, limit); sameOwnerFork != noFork {
				t.Fatalf("same-owner fork body differs from no-fork body\nno fork:\n%s\nsame-owner fork:\n%s", noFork, sameOwnerFork)
			}
		})
	}
}

func TestPRStep_RefusesWhileApprovedIntentConformanceFindingIsUnresolved(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, types.Findings{
		Items: []types.Finding{{
			ID:          "review-1",
			Severity:    "error",
			Description: "range does not contain the change the intent marks as its subject",
			Action:      "ask-user",
			ReviewScope: "source",
			Category:    types.FindingCategoryIntentConformance,
		}},
		Summary:       "intent mismatch",
		RiskLevel:     "high",
		RiskRationale: "needs maintainer decision",
	}), "")

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected PR step to refuse while an approved intent-conformance finding is unresolved")
	}
	if !strings.Contains(err.Error(), "intent") || !strings.Contains(err.Error(), "review-1") {
		t.Fatalf("refusal must name the intent-conformance finding, got: %v", err)
	}
	if data, readErr := os.ReadFile(logFile); readErr == nil && strings.Contains(string(data), "pr create") {
		t.Fatalf("refused PR step must not create a PR, gh log:\n%s", data)
	}
}

func TestPRStep_ProceedsWhenReviewHoldsNoIntentConformanceFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	sctx := newTestContextWithDBRecords(t, tidyPRAgent(), dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, types.Findings{
		Items: []types.Finding{{
			ID:          "review-1",
			Severity:    "warning",
			Description: "naming nit",
			Action:      "ask-user",
			ReviewScope: "source",
		}},
		Summary:       "nit",
		RiskLevel:     "low",
		RiskRationale: "small",
	}), "")

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("PR step must proceed when no intent-conformance finding is held, got: %v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "pr create") {
		t.Fatalf("expected PR creation, gh log:\n%s", data)
	}
}

// The PR-step refusal keys on the structured category, so the reviewer must be
// told to emit it and the schema must accept it.
func TestReviewIntentConformanceCategoryIsRequestedAndAccepted(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{UserIntent: "add a Bar() helper", IntentSource: "agent"}
	if clause := intentConformanceReviewClause(sctx); !strings.Contains(clause, `category "intent-conformance"`) {
		t.Fatalf("conformance clause must request the intent-conformance category, got:\n%s", clause)
	}
	// Assert on the parsed schema, not the literal's formatting: a reflow of
	// the JSON string must not decide whether the contract holds.
	var schema map[string]any
	if err := json.Unmarshal(reviewFindingsSchema, &schema); err != nil {
		t.Fatalf("review findings schema is not valid JSON: %v", err)
	}
	properties, _ := schema["properties"].(map[string]any)
	findingsProp, _ := properties["findings"].(map[string]any)
	items, _ := findingsProp["items"].(map[string]any)
	itemProperties, _ := items["properties"].(map[string]any)
	categoryProp, _ := itemProperties["category"].(map[string]any)
	enum, ok := categoryProp["enum"].([]any)
	if !ok {
		t.Fatalf("review findings schema finding-item category has no enum: %#v", itemProperties)
	}
	for _, value := range enum {
		if value == types.FindingCategoryIntentConformance {
			return
		}
	}
	t.Fatalf("review findings schema category enum must accept %q, got: %v", types.FindingCategoryIntentConformance, enum)
}
