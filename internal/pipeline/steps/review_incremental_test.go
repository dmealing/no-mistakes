package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

const (
	carryRepoYAML   = "commands:\n  test: make test\n"
	carryGlobalYAML = "log_level: info\nreview:\n  round_cap: 2\n"
)

// carryRepo is a branch whose head was approved by an earlier run's Review:
// main holds shared.txt (40 lines) and other.txt; the feature branch changed
// shared.txt line 5 and added feature.txt, and that head is the approved one.
type carryRepo struct {
	t        *testing.T
	dir      string
	base     string
	approved string
}

func newCarryRepo(t *testing.T) *carryRepo {
	t.Helper()
	dir := t.TempDir()
	r := &carryRepo{t: t, dir: dir}
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	var shared strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&shared, "shared line %d\n", i)
	}
	r.write("shared.txt", shared.String())
	r.write("other.txt", "other\n")
	r.base = r.commit("base")
	gitCmd(t, dir, "checkout", "-b", "feature")
	r.editLine("shared.txt", 5, "feature change")
	r.write("feature.txt", "feature code\n")
	r.approved = r.commit("feature work")
	return r
}

func (r *carryRepo) write(path, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, path), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *carryRepo) editLine(path string, line int, text string) {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, path))
	if err != nil {
		r.t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	lines[line-1] = text
	r.write(path, strings.Join(lines, "\n"))
}

func (r *carryRepo) commit(msg string) string {
	r.t.Helper()
	gitCmd(r.t, r.dir, "add", "-A")
	gitCmd(r.t, r.dir, "commit", "-m", msg)
	return gitCmd(r.t, r.dir, "rev-parse", "HEAD")
}

// onMain commits change on main, then returns to feature.
func (r *carryRepo) onMain(change func()) {
	r.t.Helper()
	gitCmd(r.t, r.dir, "checkout", "main")
	change()
	r.commit("base movement")
	gitCmd(r.t, r.dir, "checkout", "feature")
}

func (r *carryRepo) mergeMain() string {
	r.t.Helper()
	gitCmd(r.t, r.dir, "merge", "--no-ff", "main", "-m", "merge main")
	return gitCmd(r.t, r.dir, "rev-parse", "HEAD")
}

// mergeMainResolvingOurs merges main through a conflict, keeping the
// branch's side.
func (r *carryRepo) mergeMainResolvingOurs() string {
	r.t.Helper()
	cmd := exec.Command("git", "merge", "--no-ff", "main", "-m", "merge main")
	cmd.Dir = r.dir
	_ = cmd.Run()
	gitCmd(r.t, r.dir, "checkout", "--ours", "--", ".")
	return r.commit("merge main, keeping ours")
}

func (r *carryRepo) head() string {
	r.t.Helper()
	return gitCmd(r.t, r.dir, "rev-parse", "HEAD")
}

// carryHarness is a Review step context for the run at the repo's current
// head, with review.incremental on and the configuration the approved Review
// recorded.
type carryHarness struct {
	sctx *pipeline.StepContext
	ag   *mockAgent
	logs []string
	plan *verificationplan.Snapshot
}

func newCarryHarness(t *testing.T, r *carryRepo, reviewed []string) *carryHarness {
	t.Helper()
	h := &carryHarness{}
	h.ag = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(coverageFindingJSON(reviewed))}, nil
	}}
	h.sctx = newTestContextWithDBRecords(t, h.ag, r.dir, r.base, r.head(), config.Commands{})
	h.sctx.Config.Review.Incremental = true
	h.sctx.Config.ReplayRepoYAML = []byte(carryRepoYAML)
	h.sctx.Config.ReplayGlobalYAML = []byte(carryGlobalYAML)
	h.sctx.Log = func(msg string) { h.logs = append(h.logs, msg) }
	h.plan = newCarryPlan(t)
	return h
}

// newCarryPlan is a verification plan snapshot as `axi run --verification-plan`
// captures it: a digest-checked file outside the repository.
func newCarryPlan(t *testing.T) *verificationplan.Snapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verification-plan.txt")
	content := "verify the feature end to end\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &verificationplan.Snapshot{ID: "planned-run", Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
}

// approvedReview records an earlier run of the same branch whose Review
// completed with an approval of head, under the given intent and config.
type approvedReview struct {
	head       string
	intent     *db.RunIntent
	repoYAML   string
	globalYAML string
	findings   string
	prBase     string
	noApproval bool
	profile    *agentcfg.PiProfile
	plan       *verificationplan.Snapshot
}

func (h *carryHarness) recordPrior(t *testing.T, p approvedReview) *db.Run {
	t.Helper()
	if p.repoYAML == "" {
		p.repoYAML = carryRepoYAML
	}
	if p.globalYAML == "" {
		p.globalYAML = carryGlobalYAML
	}
	if p.findings == "" {
		p.findings = `{"findings":[],"risk_level":"low","risk_rationale":"clean"}`
	}
	d := h.sctx.DB
	prior, err := d.InsertRunWithIntentAndLaunchNonce(h.sctx.Repo.ID, h.sctx.Run.Branch, p.head, h.sctx.Run.BaseSHA, p.intent, "", "", "", p.prBase, false, p.plan, p.profile)
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(prior.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := p.findings
	if _, err := d.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, p.head, p.head, "trusted-sha", []byte(p.globalYAML), []byte(p.repoYAML), 0); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if p.noApproval {
		if err := d.UpdateStepStatus(step.ID, types.StepStatusFailed); err != nil {
			t.Fatal(err)
		}
		return prior
	}
	if err := d.CompleteReviewStep(step.ID, prior.ID, p.head, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	return prior
}

func (h *carryHarness) execute(t *testing.T) *pipeline.StepOutcome {
	t.Helper()
	outcome, err := (&ReviewStep{}).Execute(h.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	return outcome
}

func (h *carryHarness) logged(substr string) bool {
	return slices.ContainsFunc(h.logs, func(line string) bool { return strings.Contains(line, substr) })
}

func carriedMarker(t *testing.T, outcome *pipeline.StepOutcome) (types.Finding, bool) {
	t.Helper()
	findings := parsedFindings(t, outcome)
	for _, item := range findings.Items {
		if item.ID == types.FindingIDReviewCarried {
			return item, true
		}
	}
	return types.Finding{}, false
}

func parsedFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	return findings
}

// assertCarried: the Review spent no agent turn, approved the run head, and
// recorded where the approval came from.
func assertCarried(t *testing.T, h *carryHarness, prior *db.Run) types.Finding {
	t.Helper()
	outcome := h.execute(t)
	if len(h.ag.calls) != 0 {
		t.Fatalf("a carried review ran %d agent turns, want 0; logs: %v", len(h.ag.calls), h.logs)
	}
	if outcome.NeedsApproval || outcome.ReviewApprovedHeadSHA != h.sctx.Run.HeadSHA || !outcome.ReviewCoverageComplete {
		t.Fatalf("carried review must approve the run head: %+v", outcome)
	}
	marker, ok := carriedMarker(t, outcome)
	if !ok {
		t.Fatalf("carried review recorded no %s marker: %s", types.FindingIDReviewCarried, outcome.Findings)
	}
	if !strings.Contains(marker.Description, "carried from run "+prior.ID+" at ") || marker.Action != types.ActionNoOp || marker.Severity != types.FindingSeverityInfo {
		t.Fatalf("marker = %+v, want an info no-op naming run %s", marker, prior.ID)
	}
	return marker
}

// assertFull: the Review ran a full review of the whole branch, and said why.
func assertFull(t *testing.T, h *carryHarness, reason string) {
	t.Helper()
	outcome := h.execute(t)
	if len(h.ag.calls) == 0 {
		t.Fatalf("expected a full review, but no agent turn ran; logs: %v", h.logs)
	}
	if strings.Contains(h.ag.calls[0].Prompt, "Incremental review") {
		t.Fatalf("a full review must not carry the incremental section")
	}
	if _, ok := carriedMarker(t, outcome); ok {
		t.Fatalf("a full review must not record a carried approval: %s", outcome.Findings)
	}
	if scope := parsedFindings(t, outcome).ReviewCoverageScope; scope != nil && scope.Incremental {
		t.Fatalf("a full review must not record an incremental scope: %+v", scope)
	}
	if !slices.Contains(outcome.ReviewablePaths, "feature.txt") {
		t.Fatalf("a full review must cover the whole branch, reviewable = %v", outcome.ReviewablePaths)
	}
	if reason != "" && !h.logged("full review: ") {
		t.Fatalf("a fallback must log why it reviews in full; logs: %v", h.logs)
	}
	if reason != "" && !h.logged(reason) {
		t.Fatalf("logs %v do not name the reason %q", h.logs, reason)
	}
}

func TestIncrementalReview_BaseMergeWithoutOverlapCarriesTheApproval(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	r.onMain(func() { r.write("other.txt", "other changed on main\n") })
	r.mergeMain()
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved})
	marker := assertCarried(t, h, prior)
	if !strings.Contains(marker.Description, "no new branch work") {
		t.Fatalf("marker %q should say the branch added no work", marker.Description)
	}
}

func TestIncrementalReview_SharedFileTouchedOutsideReviewedHunksCarries(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	// Line 30 is far from the reviewed change at line 5.
	r.onMain(func() { r.editLine("shared.txt", 30, "main change far away") })
	r.mergeMain()
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved})
	assertCarried(t, h, prior)
}

func TestIncrementalReview_SameApprovedHeadCarries(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved})
	marker := assertCarried(t, h, prior)
	if !strings.Contains(marker.Description, "the same head was already approved") {
		t.Fatalf("marker %q should name the same-head carry", marker.Description)
	}
}

func TestIncrementalReview_CarriedFindingsAreCountedInTheMarker(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved, findings: `{"findings":[{"id":"review-1","severity":"warning","file":"feature.txt","line":1,"description":"left as a note","action":"auto-fix"}],"risk_level":"low","risk_rationale":"ok"}`})
	marker := assertCarried(t, h, prior)
	if !strings.Contains(marker.Description, "its 1 recorded finding carried as decided") {
		t.Fatalf("marker %q should count the carried finding", marker.Description)
	}
}

func TestIncrementalReview_NewCommitsAreReviewedAlone(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	r.onMain(func() { r.write("other.txt", "other changed on main\n") })
	r.mergeMain()
	r.write("new.txt", "new branch work\n")
	r.commit("new branch work")
	h := newCarryHarness(t, r, []string{"new.txt"})
	prior := h.recordPrior(t, approvedReview{head: r.approved, findings: `{"findings":[{"id":"review-1","severity":"info","file":"feature.txt","line":1,"description":"earlier decided note","action":"no-op"}],"risk_level":"low","risk_rationale":"ok"}`})

	outcome := h.execute(t)
	if len(h.ag.calls) != 1 {
		t.Fatalf("agent turns = %d, want 1 incremental review", len(h.ag.calls))
	}
	if !slices.Equal(outcome.ReviewablePaths, []string{"new.txt"}) {
		t.Fatalf("reviewable = %v, want only the new commit's file", outcome.ReviewablePaths)
	}
	if outcome.NeedsApproval || outcome.ReviewApprovedHeadSHA != h.sctx.Run.HeadSHA {
		t.Fatalf("a clean incremental review approves the run head: %+v", outcome)
	}
	prompt := h.ag.calls[0].Prompt
	for _, want := range []string{"Incremental review", "Run " + prior.ID + " reviewed and approved head " + r.approved, "earlier decided note", "review scope: only the branch's changes since the review approved at " + r.approved} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("incremental prompt is missing %q", want)
		}
	}
	if _, ok := carriedMarker(t, outcome); ok {
		t.Fatalf("an incremental review that reviewed new work records no carried marker")
	}
	if scope := parsedFindings(t, outcome).ReviewCoverageScope; scope == nil || !scope.Incremental || scope.BaseSHA == r.base {
		t.Fatalf("an incremental review must record the base it judged from as incremental: %+v", scope)
	}
	if !h.logged("incremental review: run " + prior.ID) {
		t.Fatalf("logs %v should announce the incremental review", h.logs)
	}
}

// A new commit made before the base merge is branch work too.
func TestIncrementalReview_CommitBeforeTheMergeIsReviewed(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	r.write("new.txt", "new branch work\n")
	r.commit("new branch work")
	r.onMain(func() { r.write("other.txt", "other changed on main\n") })
	r.mergeMain()
	h := newCarryHarness(t, r, []string{"new.txt"})
	h.recordPrior(t, approvedReview{head: r.approved})
	outcome := h.execute(t)
	if !slices.Equal(outcome.ReviewablePaths, []string{"new.txt"}) {
		t.Fatalf("reviewable = %v, want only the new commit's file", outcome.ReviewablePaths)
	}
}

// An edit made inside the merge commit itself is not a clean merge of the
// base, so it is reviewed as branch work.
func TestIncrementalReview_EditInsideTheMergeCommitIsReviewed(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	r.onMain(func() { r.write("other.txt", "other changed on main\n") })
	gitCmd(t, r.dir, "merge", "--no-ff", "--no-commit", "main")
	r.write("feature.txt", "feature code edited during the merge\n")
	r.commit("merge main with an edit")
	h := newCarryHarness(t, r, []string{"feature.txt"})
	h.recordPrior(t, approvedReview{head: r.approved})
	outcome := h.execute(t)
	if len(h.ag.calls) != 1 || !slices.Equal(outcome.ReviewablePaths, []string{"feature.txt"}) {
		t.Fatalf("calls = %d, reviewable = %v, want one review of the edited file", len(h.ag.calls), outcome.ReviewablePaths)
	}
}

func TestIncrementalReview_FallsBackToAFullReview(t *testing.T) {
	t.Parallel()
	full := []string{"feature.txt", "shared.txt"}
	cases := map[string]struct {
		setup  func(r *carryRepo)
		prior  func(r *carryRepo) approvedReview
		tweak  func(h *carryHarness)
		reason string
	}{
		"base movement overlaps a reviewed hunk": {
			setup: func(r *carryRepo) {
				r.onMain(func() { r.editLine("shared.txt", 7, "main change near the feature") })
				r.mergeMain()
			},
			reason: "base movement since the approved review touches reviewed code in shared.txt",
		},
		"base movement adds the same new file": {
			setup: func(r *carryRepo) {
				r.onMain(func() { r.write("feature.txt", "main's own feature file\nmore\n") })
				r.mergeMainResolvingOurs()
			},
			reason: "touches reviewed code in feature.txt",
		},
		"force-pushed branch": {
			setup: func(r *carryRepo) {
				gitCmd(r.t, r.dir, "commit", "--amend", "-m", "rewritten feature work")
			},
			reason: "is not an ancestor",
		},
		"prior review not approved": {
			prior: func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, noApproval: true} },
		},
		"changed explicit intent": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, intent: &db.RunIntent{Summary: "old goal", Source: db.RunIntentSourceAgent, Score: 1}}
			},
			reason: "the run intent changed",
		},
		"changed repository config": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, repoYAML: "commands:\n  test: make check\n"}
			},
			reason: "the repository configuration changed since the approved review (commands)",
		},
		"changed global review config": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: "log_level: info\nreview:\n  round_cap: 3\n"}
			},
			reason: "the global review configuration changed since the approved review (review)",
		},
		"changed review agent": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "agent: codex\n"}
			},
			reason: "(agent)",
		},
		"configuration not recorded": {
			tweak:  func(h *carryHarness) { h.sctx.Config.ReplayRepoYAML = nil },
			reason: "did not record its configuration",
		},
		"incremental review turned off": {
			tweak: func(h *carryHarness) { h.sctx.Config.Review.Incremental = false },
		},
		"different PR base branch": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, prBase: "release"} },
			reason: "targeted base branch",
		},
		"model profile pinned on the approved run only": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, profile: carryPin} },
			reason: "different model profile",
		},
		"model profile pinned on this run only": {
			tweak:  func(h *carryHarness) { h.sctx.Run.PiProfile = carryPin },
			reason: "different model profile",
		},
		"changed pinned model": {
			prior: func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, profile: carryPin} },
			tweak: func(h *carryHarness) {
				h.sctx.Run.PiProfile = &agentcfg.PiProfile{Model: "acme/kit-pro", Effort: agentcfg.EffortHigh}
			},
			reason: "different model profile",
		},
		"changed pinned effort": {
			prior: func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, profile: carryPin} },
			tweak: func(h *carryHarness) {
				h.sctx.Run.PiProfile = &agentcfg.PiProfile{Model: "acme/kit", Effort: agentcfg.EffortLow}
			},
			reason: "different model profile",
		},
		"changed agent args override": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "agent_args_override:\n  claude:\n    - --model\n    - sonnet\n"}
			},
			reason: "(agent_args_override)",
		},
		"changed agent binary path": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "agent_path_override:\n  claude: /opt/claude\n"}
			},
			reason: "(agent_path_override)",
		},
		"changed ACP bridge path": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "acpx_path: /opt/acpx\n"}
			},
			reason: "(acpx_path)",
		},
		"changed ACP registry override": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "acp_registry_overrides:\n  cursor: cursor-agent acp\n"}
			},
			reason: "(acp_registry_overrides)",
		},
		"changed quota routing": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "quota_routing:\n  enabled: true\n"}
			},
			reason: "(quota_routing)",
		},
		"another no-mistakes build": {
			tweak:  func(h *carryHarness) { build := "build-2"; h.sctx.Run.NoMistakesBuildSHA = &build },
			reason: "is not this run's",
		},
		"another version, no build SHA recorded": {
			tweak: func(h *carryHarness) {
				version := "v2.0.0"
				h.sctx.Run.NoMistakesBuildSHA = nil
				h.sctx.Run.NoMistakesVersion = &version
			},
			reason: "is not this run's",
		},
		"no recorded build": {
			tweak:  func(h *carryHarness) { h.sctx.Run.NoMistakesBuildSHA = nil; h.sctx.Run.NoMistakesVersion = nil },
			reason: "is not this run's",
		},
		"verification plan on the approved run only": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, plan: carryPlan} },
			reason: "different verification plans",
		},
		"verification plan on this run only": {
			tweak:  func(h *carryHarness) { h.sctx.Run.VerificationPlan = h.plan },
			reason: "different verification plans",
		},
		"changed verification plan": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, plan: carryPlan} },
			tweak:  func(h *carryHarness) { h.sctx.Run.VerificationPlan = h.plan },
			reason: "different verification plans",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCarryRepo(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			h := newCarryHarness(t, r, full)
			p := approvedReview{head: r.approved}
			if tc.prior != nil {
				p = tc.prior(r)
			}
			h.recordPrior(t, p)
			if tc.tweak != nil {
				tc.tweak(h)
			}
			assertFull(t, h, tc.reason)
		})
	}
}

// The incremental switch itself is not a review setting: turning it on after
// a Review ran under the default must not force a full Review.
func TestIncrementalReview_TheSwitchItselfIsNotAConfigChange(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	h.sctx.Config.ReplayGlobalYAML = []byte(carryGlobalYAML + "  incremental: true\n")
	prior := h.recordPrior(t, approvedReview{head: r.approved})
	assertCarried(t, h, prior)
}

// An unrelated global setting does not affect the Review and does not block
// a carry.
func TestIncrementalReview_UnrelatedGlobalSettingDoesNotBlockTheCarry(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	h.sctx.Config.ReplayGlobalYAML = []byte("log_level: debug\nreview:\n  round_cap: 2\n")
	prior := h.recordPrior(t, approvedReview{head: r.approved})
	assertCarried(t, h, prior)
}

// Two inferred intents are not an acceptance contract; only an explicit one
// must match.
func TestIncrementalReview_InferredIntentsDoNotBlockTheCarry(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved, intent: &db.RunIntent{Summary: "a transcript guess", Source: "claude", Score: 0.4}})
	assertCarried(t, h, prior)
}

// carryPin is a per-run model profile as `axi run --model/--effort` records it.
var carryPin = &agentcfg.PiProfile{Model: "acme/kit", Effort: agentcfg.EffortHigh}

// carryPlan is the approved run's verification plan: a digest no other plan in
// these tests has.
var carryPlan = &verificationplan.Snapshot{ID: "planned-run", SHA256: "approved-run-plan"}

// The same per-run model profile on both runs keeps the reviewer, so the
// approval carries.
func TestIncrementalReview_TheSamePinnedModelProfileCarries(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	h.sctx.Run.PiProfile = carryPin
	prior := h.recordPrior(t, approvedReview{head: r.approved, profile: carryPin})
	assertCarried(t, h, prior)
}

// The same verification plan on both runs keeps what the Review judges
// against, so the approval carries.
func TestIncrementalReview_TheSameVerificationPlanCarries(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	h.sctx.Run.VerificationPlan = h.plan
	prior := h.recordPrior(t, approvedReview{head: r.approved, plan: &verificationplan.Snapshot{ID: h.plan.ID, SHA256: h.plan.SHA256}})
	assertCarried(t, h, prior)
}

// A fix round can find the incremental scope empty (the fixer deleted the file
// its finding was about) while that finding is still outstanding. The PR body
// renders the carried notice in place of every other finding, so the notice
// belongs to a Review with nothing outstanding, never to a fix round.
func TestIncrementalReview_AFixRoundWithAnEmptyScopeRecordsNoCarriedNotice(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	r.write("new.txt", "new branch work\n")
	r.commit("new branch work")
	if err := os.Remove(filepath.Join(r.dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved})
	h.sctx.PriorRounds = 1
	h.sctx.Fixing = true
	h.sctx.SkipFixExecution = true
	h.sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"new.txt","line":1,"description":"fix me","action":"auto-fix"}]}`

	outcome := h.execute(t)
	if len(h.ag.calls) != 0 {
		t.Fatalf("an empty scope ran %d agent turns, want 0", len(h.ag.calls))
	}
	if _, ok := carriedMarker(t, outcome); ok {
		t.Fatalf("a fix round recorded the carried notice over an outstanding finding: %s", outcome.Findings)
	}
	findings := parsedFindings(t, outcome)
	if len(findings.Items) != 0 || findings.RiskRationale != "no reviewable changes" {
		t.Fatalf("findings = %s, want the ordinary no-changes outcome", outcome.Findings)
	}
	if outcome.NeedsApproval || outcome.ReviewApprovedHeadSHA != h.sctx.Run.HeadSHA || !outcome.ReviewCoverageComplete {
		t.Fatalf("an empty scope must still approve the run head: %+v", outcome)
	}
	if h.logged("carried from run " + prior.ID) {
		t.Fatalf("the step log announced a carry: %v", h.logs)
	}
}

// The latest approved Review is the one carried, and it must be an ancestor:
// an older approval of an ancestor does not rescue a rewritten branch.
func TestIncrementalReview_UsesTheLatestApprovedReview(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, []string{"feature.txt", "shared.txt"})
	h.recordPrior(t, approvedReview{head: r.base})
	// The newer approval names a head this branch no longer contains.
	gitCmd(t, r.dir, "commit", "--amend", "-m", "rewritten")
	rewritten := r.head()
	gitCmd(t, r.dir, "reset", "--hard", r.approved)
	gitCmd(t, r.dir, "update-ref", "refs/keep/rewritten", rewritten)
	h.recordPrior(t, approvedReview{head: rewritten})
	assertFull(t, h, "is not an ancestor")
}

func TestHunksOverlap(t *testing.T) {
	t.Parallel()
	reviewed := []lineRange{{5, 5}}
	for _, tc := range []struct {
		moved []lineRange
		want  bool
	}{
		{[]lineRange{{30, 30}}, false},
		{[]lineRange{{9, 9}}, false},
		{[]lineRange{{8, 8}}, true},
		{[]lineRange{{1, 2}}, true},
		{[]lineRange{{5, 6}}, true},
	} {
		if got := hunksOverlap(reviewed, tc.moved, reviewCarryHunkMargin); got != tc.want {
			t.Errorf("hunksOverlap(%v, %v) = %v, want %v", reviewed, tc.moved, got, tc.want)
		}
	}
}
