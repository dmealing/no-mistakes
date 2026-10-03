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

// newUpstreamBodyContext builds a PR step context whose Test step recorded a
// path-only local artifact, a captioned local artifact, an embeddable evidence
// file, and an inline content artifact, plus an authoritative intent.
func newUpstreamBodyContext(t *testing.T, upstreamURL, forkURL string) (*pipeline.StepContext, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"fix: tidy","body":"## What Changed\n\n- tidy"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[`+
		`{"kind":"log","label":"Local only log","path":%q},`+
		`{"kind":"log","label":"Captioned log","path":%q,"content":"captioned evidence text"},`+
		`{"kind":"log","label":"Server log","path":%q},`+
		`{"kind":"log","label":"Inline log","content":%q}]}`, localOnly, captioned, embedded, upstreamTestInlineText)
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(testStep.ID, 1, "initial", &findings, nil, 300); err != nil {
		t.Fatal(err)
	}
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

func insertCompletedReview(t *testing.T, sctx *pipeline.StepContext, findings string) {
	t.Helper()
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if findings == "" {
		return
	}
	if err := sctx.DB.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
}

func TestPRStep_RefusesWhileApprovedIntentConformanceFindingIsUnresolved(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	insertCompletedReview(t, sctx, `{"findings":[{"id":"review-1","severity":"error","description":"range does not contain the change the intent marks as its subject","action":"ask-user","review_scope":"source","category":"intent-conformance"}],"summary":"intent mismatch","risk_level":"high","risk_rationale":"needs maintainer decision"}`)

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
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"fix: tidy","body":"## What Changed\n\n- tidy"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	insertCompletedReview(t, sctx, `{"findings":[{"id":"review-1","severity":"warning","description":"naming nit","action":"ask-user","review_scope":"source"}],"summary":"nit","risk_level":"low","risk_rationale":"small"}`)

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
	if !strings.Contains(string(reviewFindingsSchema), `"category": {"type": "string", "enum": ["`+types.FindingCategoryIntentConformance+`"]}`) {
		t.Fatalf("review findings schema must accept the intent-conformance category:\n%s", reviewFindingsSchema)
	}
}
