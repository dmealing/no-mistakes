package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

const (
	carryRepoYAML   = "commands:\n  test: make test\n"
	carryGlobalYAML = "log_level: info\nreview:\n  round_cap: 2\n"
)

// carryBuild is the no-mistakes build that created both runs of a carry.
const carryBuild = "9f2c4e1"

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
	// The run's agent is what its Review selects, so it is the agent the
	// approved review's turn is recorded on.
	h.ag = &mockAgent{name: "claude", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(coverageFindingJSON(reviewed))}, nil
	}}
	h.sctx = newTestContextWithDBRecords(t, h.ag, r.dir, r.base, r.head(), config.Commands{})
	h.sctx.Config.Review.Incremental = true
	h.sctx.Config.ReplayRepoYAML = []byte(carryRepoYAML)
	h.sctx.Config.ReplayGlobalYAML = []byte(carryGlobalYAML)
	h.sctx.Log = func(msg string) { h.logs = append(h.logs, msg) }
	build := carryBuild
	h.sctx.Run.NoMistakesBuildSHA = &build
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
	// reviewer is the agent the approved review's turn is recorded on: ""
	// is the agent this run selects, "none" records no turn.
	reviewer        string
	configuredModel string
	// build and version are the stamps of the no-mistakes build that created
	// the run; unstamped leaves what InsertRun recorded for this test binary.
	build     string
	version   string
	unstamped bool
	// carriedFrom makes the approved review one that carried that run's
	// approval instead of reviewing, so it ran no turn of its own.
	carriedFrom string
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
	if p.build == "" {
		p.build = carryBuild
	}
	if p.version == "" {
		p.version = "v1.109.0-local.1"
	}
	d := h.sctx.DB
	prior, err := d.InsertRunWithIntentAndLaunchNonce(h.sctx.Repo.ID, h.sctx.Run.Branch, p.head, h.sctx.Run.BaseSHA, p.intent, "", "", "", p.prBase, false, p.plan, p.profile)
	if err != nil {
		t.Fatal(err)
	}
	if !p.unstamped {
		if err := d.SetRunBuildForTest(prior.ID, p.version, p.build); err != nil {
			t.Fatal(err)
		}
	}
	step, err := d.InsertStepResult(prior.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := p.findings
	if p.carriedFrom != "" {
		decided := types.Findings{}
		if err := json.Unmarshal([]byte(p.findings), &decided); err != nil {
			t.Fatal(err)
		}
		findings = carriedPayload(p.carriedFrom, p.head, reviewCarriedPriorItems(decided))
	}
	if p.reviewer != "none" {
		reviewer := p.reviewer
		if reviewer == "" {
			reviewer = "claude"
		}
		turn := db.AgentInvocation{RunID: prior.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: reviewer, ExitStatus: "ok"}
		if p.configuredModel != "" {
			model := p.configuredModel
			turn.ConfiguredModel = &model
		}
		if _, err := d.InsertAgentInvocation(turn); err != nil {
			t.Fatal(err)
		}
	}
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
	marker, _ := carriedMarker(t, assertCarriedOutcome(t, h, prior))
	return marker
}

func assertCarriedOutcome(t *testing.T, h *carryHarness, prior *db.Run) *pipeline.StepOutcome {
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
	return outcome
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
	findings := parsedFindings(t, outcome)
	if _, ok := carriedMarker(t, outcome); ok {
		t.Fatalf("a full review must not record a carried approval: %s", outcome.Findings)
	}
	if findings.CarriedReview != nil {
		t.Fatalf("a full review must not record a carried review: %s", outcome.Findings)
	}
	if scope := findings.ReviewCoverageScope; scope != nil && scope.Incremental {
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
	findings := parsedFindings(t, outcome)
	if scope := findings.ReviewCoverageScope; scope == nil || !scope.Incremental || scope.BaseSHA == r.base {
		t.Fatalf("an incremental review must record the base it judged from as incremental: %+v", scope)
	}
	if carried := findings.CarriedReview; carried == nil || carried.SourceRunID != prior.ID || len(carried.Findings) != 1 || carried.Findings[0].Description != "earlier decided note" {
		t.Fatalf("an incremental review must record the approval it builds on and that review's findings: %+v", carried)
	}
	if len(findings.Items) != 0 {
		t.Fatalf("a carried finding must never join the round's own findings: %s", outcome.Findings)
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
		"approved review recorded no review turn": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, reviewer: "none"} },
			reason: "recorded no review invocation",
		},
		"approved review ran on another agent": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, reviewer: "codex"} },
			reason: "was reviewed by another reviewer",
		},
		"this run selects another agent": {
			tweak:  func(h *carryHarness) { h.ag.name = "codex" },
			reason: "was reviewed by another reviewer",
		},
		"approved review ran on another configured model": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, configuredModel: "opus"} },
			reason: "used configured model",
		},
		"another no-mistakes build": {
			tweak:  func(h *carryHarness) { build := "build-2"; h.sctx.Run.NoMistakesBuildSHA = &build },
			reason: "is not this run's",
		},
		"approved run on another build": {
			prior:  func(r *carryRepo) approvedReview { return approvedReview{head: r.approved, build: "build-2"} },
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
		"approved run on another version, SHA unstamped": {
			prior: func(r *carryRepo) approvedReview {
				return approvedReview{head: r.approved, build: "unknown", version: "v2.0.0"}
			},
			reason: "is not this run's",
		},
		"this run on another version, SHA unstamped": {
			tweak: func(h *carryHarness) {
				sha, version := "unknown", "v2.0.0"
				h.sctx.Run.NoMistakesBuildSHA = &sha
				h.sctx.Run.NoMistakesVersion = &version
			},
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

// carriedPayload is the record a Review leaves when it carries sourceRun's
// approval of approvedHead: the notice, and what that review had decided.
func carriedPayload(sourceRunID, approvedHead string, decided []types.Finding) string {
	payload, _ := json.Marshal(types.Findings{
		Items: []types.Finding{{ID: types.FindingIDReviewCarried, Severity: types.FindingSeverityInfo, Action: types.ActionNoOp,
			Description: fmt.Sprintf("review carried from run %s at %s (the same head was already approved)", sourceRunID, shortObjectID(approvedHead))}},
		RiskLevel: "low", RiskRationale: "approval carried from an earlier review",
		CarriedReview: &types.CarriedReview{SourceRunID: sourceRunID, Findings: decided},
	})
	return string(payload)
}

// The placeholders an unstamped binary records are buildinfo's own defaults,
// and they name no build: two different unstamped builds record the same ones.
func TestRunBuildNamesARecordedBuildOnly(t *testing.T) {
	t.Parallel()
	if buildinfo.Commit != "unknown" || buildinfo.CurrentVersion() != "dev" {
		t.Skip("this test binary records a real build")
	}
	text := func(s string) *string { return &s }
	cases := map[string]struct {
		sha, version *string
		want         string
	}{
		"a build SHA": {text("abc1234"), text("v1.0.0"), "abc1234"},
		"an unstamped SHA falls back to the version": {text(buildinfo.Commit), text("v1.2.3"), "v1.2.3"},
		"an unstamped binary":                        {text(buildinfo.Commit), text(buildinfo.CurrentVersion()), ""},
		"empty stamps":                               {text(""), text(""), ""},
		"blank stamps":                               {text("  "), text(" \n"), ""},
		"no stamps recorded":                         {nil, nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := runBuild(&db.Run{NoMistakesVersion: tc.version, NoMistakesBuildSHA: tc.sha}); got != tc.want {
				t.Fatalf("runBuild() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A binary made by a plain go build stamps no build identity: InsertRun
// records the placeholders above. No run created by one can carry an
// approval, whichever side of the carry it created.
func TestIncrementalReview_AnUnstampedBuildCannotCarry(t *testing.T) {
	t.Parallel()
	if buildinfo.Commit != "unknown" || buildinfo.CurrentVersion() != "dev" {
		t.Skip("this test binary records a real build")
	}
	for name, thisRunStamped := range map[string]bool{"neither run stamped": false, "only this run stamped": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCarryRepo(t)
			h := newCarryHarness(t, r, []string{"feature.txt", "shared.txt"})
			if !thisRunStamped {
				recorded, err := h.sctx.DB.GetRun(h.sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				h.sctx.Run.NoMistakesVersion, h.sctx.Run.NoMistakesBuildSHA = recorded.NoMistakesVersion, recorded.NoMistakesBuildSHA
			}
			prior := h.recordPrior(t, approvedReview{head: r.approved, unstamped: true})
			reason := "this run recorded no no-mistakes build identity"
			if thisRunStamped {
				reason = fmt.Sprintf("run %s recorded no no-mistakes build identity", prior.ID)
			}
			assertFull(t, h, reason)
		})
	}
}

// A Review that carried an approval ran no turn of its own. The next Review
// is proven against the turn that did run, and carries what that review
// decided, so a branch keeps its approval across more than one carry.
func TestIncrementalReview_ACarriedApprovalIsFollowedToTheReviewThatRan(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	note := types.Finding{ID: "review-1", Severity: types.FindingSeverityWarning, File: "feature.txt", Line: 1, Description: "left as a note", Action: types.ActionAutoFix}
	decided := findingsJSON(t, types.Findings{Items: []types.Finding{note}, RiskLevel: "low", RiskRationale: "ok"})
	first := h.recordPrior(t, approvedReview{head: r.approved, findings: decided})
	second := h.recordPrior(t, approvedReview{head: r.approved, reviewer: "none", carriedFrom: first.ID, findings: decided})

	outcome := assertCarriedOutcome(t, h, second)
	marker, _ := carriedMarker(t, outcome)
	carried := parsedFindings(t, outcome).CarriedReview
	if carried == nil || carried.SourceRunID != second.ID || !slices.Equal(carried.Findings, []types.Finding{note}) {
		t.Fatalf("a carried review must record the run and findings it carries: %+v", carried)
	}
	if !strings.Contains(marker.Description, "its 1 recorded finding carried as decided") {
		t.Fatalf("marker %q should count the finding carried through run %s", marker.Description, second.ID)
	}
}

// The review a carried approval is followed to must have run on this run's
// reviewer; an approval carried on behalf of another agent does not carry on.
func TestIncrementalReview_ACarriedApprovalIsProvenAgainstTheReviewThatRan(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, []string{"feature.txt", "shared.txt"})
	first := h.recordPrior(t, approvedReview{head: r.approved, reviewer: "codex"})
	h.recordPrior(t, approvedReview{head: r.approved, reviewer: "none", carriedFrom: first.ID})
	assertFull(t, h, "run "+first.ID+" was reviewed by another reviewer")
}

// What an approved Review leaves decided is its own findings without the
// pipeline's notices, then the ones it had carried and did not report again.
func TestCarriedReviewFindingsAccumulateWithoutNoticesOrRepeats(t *testing.T) {
	t.Parallel()
	first := types.Finding{ID: "review-1", Description: "first note"}
	again := types.Finding{ID: "review-1", Description: "reported again by the later review"}
	second := types.Finding{ID: "review-2", Description: "second note"}
	notices := []types.Finding{{ID: types.FindingIDReviewRoundCapNotes}, {ID: types.FindingIDReviewCarried}}
	cases := map[string]struct {
		approved types.Findings
		want     []types.Finding
	}{
		"nothing recorded":         {types.Findings{}, nil},
		"only pipeline notices":    {types.Findings{Items: notices}, nil},
		"its own findings":         {types.Findings{Items: []types.Finding{first, notices[0]}}, []types.Finding{first}},
		"only what it carried":     {types.Findings{Items: notices, CarriedReview: &types.CarriedReview{Findings: []types.Finding{first}}}, []types.Finding{first}},
		"its own, then carried":    {types.Findings{Items: []types.Finding{again}, CarriedReview: &types.CarriedReview{Findings: []types.Finding{first, second}}}, []types.Finding{again, second}},
		"an unrecorded id repeats": {types.Findings{Items: []types.Finding{{Description: "no id"}}, CarriedReview: &types.CarriedReview{Findings: []types.Finding{{Description: "no id"}}}}, []types.Finding{{Description: "no id"}, {Description: "no id"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			approved := tc.approved
			if got := carriedReviewFindings(&approved); !slices.Equal(got, tc.want) {
				t.Fatalf("carriedReviewFindings() = %+v, want %+v", got, tc.want)
			}
		})
	}
	var many []types.Finding
	for i := range maxCarriedReviewFindings + 10 {
		many = append(many, types.Finding{ID: fmt.Sprintf("review-%d", i)})
	}
	if got := carriedReviewFindings(&types.Findings{Items: many}); len(got) != maxCarriedReviewFindings {
		t.Fatalf("a carried record holds %d findings, want it bounded at %d", len(got), maxCarriedReviewFindings)
	}
}

// axi retry records a carried Review on the continuation run under the
// daemon's own build stamp. The run that ran the Review gave the approval, so
// its build is the one compared.
func TestIncrementalReview_ARetryCarriedApprovalComparesTheOriginRunsBuild(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		origin, retry, current string
		carried                bool
	}{
		"one build throughout":                         {carryBuild, carryBuild, carryBuild, true},
		"upgraded before the retry":                    {carryBuild, "build-2", "build-2", false},
		"upgraded after the approval, then downgraded": {"build-2", carryBuild, carryBuild, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCarryRepo(t)
			h := newCarryHarness(t, r, []string{"feature.txt", "shared.txt"})
			d := h.sctx.DB
			origin := h.recordPrior(t, approvedReview{head: r.approved, build: tc.origin})
			steps, err := d.GetStepsByRun(origin.ID)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := d.InsertRunWithIntentAndLaunchNonce(h.sctx.Repo.ID, h.sctx.Run.Branch, r.approved, h.sctx.Run.BaseSHA, nil, "", "", "", "", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if step.StepName != types.StepReview {
					continue
				}
				if _, err := d.InsertCarriedStepResult(retry.ID, step); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.UpdateRunReviewApprovedHeadSHA(retry.ID, r.approved); err != nil {
				t.Fatal(err)
			}
			if err := d.SetRunBuildForTest(retry.ID, "v1.109.0-local.1", tc.retry); err != nil {
				t.Fatal(err)
			}
			h.sctx.Run.NoMistakesBuildSHA = &tc.current
			if tc.carried {
				assertCarried(t, h, retry)
				return
			}
			assertFull(t, h, fmt.Sprintf("run %s (%q) is not this run's", origin.ID, tc.origin))
		})
	}
}

// With quota routing on, each run's agent order is chosen per run. The
// configuration does not change, so only the recorded review turn can tell
// that this run starts on an agent which never reviewed the earlier work.
func TestIncrementalReview_AnotherSelectedReviewerGetsAFullReviewWhateverTheConfiguration(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, []string{"feature.txt", "shared.txt"})
	h.ag.name = "codex"
	prior := h.recordPrior(t, approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "quota_routing:\n  enabled: true\n"})
	assertFull(t, h, "run "+prior.ID+" was reviewed by another reviewer")
}

// A quota routing change alone does not block a carry: the recorded review
// turn still names this run's reviewer.
func TestIncrementalReview_AQuotaRoutingChangeAloneDoesNotBlockTheCarry(t *testing.T) {
	t.Parallel()
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, nil)
	prior := h.recordPrior(t, approvedReview{head: r.approved, globalYAML: carryGlobalYAML + "quota_routing:\n  enabled: true\n"})
	assertCarried(t, h, prior)
}

// On a git older than 2.38, merge-tree has no --write-tree. The fallback must
// name the git, not blame a conflict on a base that merges cleanly.
func TestIncrementalReview_OldGitFallsBackToAFullReviewNamingGit(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in git is a shell script")
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Skip("git not available")
	}
	r := newCarryRepo(t)
	h := newCarryHarness(t, r, []string{"feature.txt", "shared.txt"})
	h.recordPrior(t, approvedReview{head: r.approved})
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in\n\t*' --write-tree '*|*' merge-tree -h '*) echo 'usage: git merge-tree <base-tree> <branch1> <branch2>'; exit 129;;\nesac\nexec \"$CARRY_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h.sctx.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "CARRY_REAL_GIT=" + realGit}

	assertFull(t, h, "git is too old for merge-tree --write-tree (needs 2.38 or newer)")
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
