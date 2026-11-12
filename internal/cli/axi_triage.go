package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/triage"
	"github.com/spf13/cobra"
)

// triageStopExitCode is the exit code of a check that says do not start the
// run (and of `--conflicts` when it finds one), so scripts can branch on it.
const triageStopExitCode = 3

func newAxiTriageCmd() *cobra.Command {
	var base, baseBranch, head, gated string
	var skipOnly, conflictsOnly, postMerge, offline bool
	cmd := &cobra.Command{
		Use:   "triage",
		Short: "Pre-run check: classify the change, print the run command, and say whether a new run may start",
		Long: "Classifies the current branch's change by the paths it touches and prints the\n" +
			"run command with the right --skip, the pre-run review advice, and the\n" +
			"bundling facts: how many other branches of this project are in validation\n" +
			"now and whether this is a small change.\n\n" +
			"It then applies the rules `axi run` also applies before starting a NEW run,\n" +
			"and prints verdict: stop (exit 3) when any holds:\n" +
			"  hold                      a fleet-wide hold on new runs is active\n" +
			"                            (<NM_HOME>/hold, written by the local install);\n" +
			"                            it cannot be overridden\n" +
			"  in-validation-conflict    the branch conflicts with a branch of this\n" +
			"                            project that is in validation now\n" +
			"  third-branch              two other branches of this project are already\n" +
			"                            in validation\n" +
			"  small-change-with-sibling the change has 300 production lines or fewer\n" +
			"                            while a sibling branch is in validation: ride\n" +
			"                            with a sibling lane instead\n" +
			"  bundle-over-limit         a bundle of fixes (3+ commits whose subject\n" +
			"                            starts with fix) over 1,000 production lines;\n" +
			"                            a large single feature is not stopped\n" +
			"`axi run --triage-override REASON` starts the run anyway over every stop\n" +
			"except the hold, and records the reason in <NM_HOME>/" + triage.OverrideLogName + ".\n\n" +
			"review: says which first review the change gets: cheap (300 production\n" +
			"lines or fewer, no risk path) or strong (over 300, or any risk path), and\n" +
			"the room left: 300 for a cheap bundle, 600 for a strong one, never over\n" +
			"1,000. notes: (advice, never a stop) flags a small change whose minor part\n" +
			"is on a risk path (risk-path-in-small-change) and a bundle of 2+ fixes at\n" +
			"301-450 production lines (just-over-cheap-limit).\n\n" +
			"As advice only, it also test-merges the branch against its base and\n" +
			"against every other open GitHub pull request (skipped by --offline).\n\n" +
			"Tiers (the strictest changed file wins):\n" +
			"  DOCS  prose / agent config only -> --skip test,lint,document\n" +
			"  TEST  tests (+ docs) only       -> --skip document\n" +
			"  CODE  anything else             -> full run\n" +
			"  RISK  CODE on a risk path       -> full run + an independent review first\n" +
			"A repository may add risk globs in .nm-triage-risk, and its own\n" +
			"scripts/nm-triage.sh tier wins over the built-in one here and in\n" +
			"`axi run` (a risk path still lifts any tier to RISK; not consulted with\n" +
			"--base). When that script fails or prints no TIER: line, repo_script\n" +
			"says so and the built-in tier is used.\n\n" +
			"--skip-only prints only the --skip value (plain text, for scripts).\n" +
			"--conflicts runs only the conflict checks: exit 0 clean, 3 conflicts, 2\n" +
			"cannot check; --head REF checks that ref instead of HEAD.\n" +
			"--post-merge [--gated SHA] judges HEAD as one merge of the base on top of\n" +
			"a head that already passed validation: exit 0 when only docs/tests were\n" +
			"hand-resolved (run the local test suite instead of a new run), 4 when a\n" +
			"new run is needed. .nm-triage-nonprod adds globs that count as\n" +
			"non-production there.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv("NM_TRIAGE_NO_CONFLICTS") == "1" {
				offline = true
			}
			opts := triageOptions{base: base, baseBranch: baseBranch, head: head, gated: gated, offline: offline}
			switch {
			case postMerge:
				return runAxiTriagePostMerge(cmd, opts)
			case conflictsOnly:
				return runAxiTriageConflicts(cmd, opts)
			default:
				return runAxiTriage(cmd, opts, skipOnly)
			}
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "measure the change from this commit or ref instead of the merge base with the base branch")
	cmd.Flags().StringVar(&baseBranch, "base-branch", "", "the run's base branch, as passed to `axi run --base-branch` (default: the repository's default branch)")
	cmd.Flags().BoolVar(&skipOnly, "skip-only", false, "print only the --skip value (empty for CODE and RISK)")
	cmd.Flags().BoolVar(&conflictsOnly, "conflicts", false, "run only the conflict checks (exit 0 clean, 3 conflicts, 2 cannot check)")
	cmd.Flags().StringVar(&head, "head", "", "with --conflicts: check this ref instead of HEAD")
	cmd.Flags().BoolVar(&postMerge, "post-merge", false, "judge HEAD as one merge of the base on top of a validated head (exit 0 no new run, 4 new run)")
	cmd.Flags().StringVar(&gated, "gated", "", "with --post-merge: the head that passed validation (default HEAD^1)")
	cmd.Flags().BoolVar(&offline, "offline", false, "do not fetch the base or query open pull requests")
	return cmd
}

type triageOptions struct {
	base, baseBranch, head, gated string
	offline                       bool
}

// triageEnv is the read-only context a check runs in. It never starts the
// daemon or migrates the database: an unregistered checkout or a missing
// database just means no in-validation facts.
type triageEnv struct {
	root   string
	p      *paths.Paths
	d      *db.DB
	repo   *db.Repo
	branch string
	key    string
}

func openTriageEnv(ctx context.Context, root string) *triageEnv {
	env := &triageEnv{root: root}
	if p, err := paths.New(); err == nil {
		env.p = p
		if d, err := db.OpenReadOnly(p.DB()); err == nil {
			env.d = d
			if repo, err := findRepo(d); err == nil {
				env.repo = repo
			}
		}
	}
	env.branch, _ = git.CurrentBranch(ctx, root)
	if env.repo != nil {
		env.key = triage.ProjectKey(env.repo.UpstreamURL)
	}
	if env.key == "" {
		if remote, err := git.Run(ctx, root, "remote", "get-url", "origin"); err == nil {
			env.key = triage.ProjectKey(remote)
		}
	}
	return env
}

func (e *triageEnv) close() {
	if e.d != nil {
		e.d.Close()
	}
}

// triageBase resolves the commit a change is measured from: --base when
// given, else the merge base of head with the run's base branch on origin
// (or the repository default).
func triageBase(ctx context.Context, root, head, explicit, baseBranch string, repo *db.Repo) (base, baseRef string) {
	baseRef = triageBaseRef(ctx, root, baseBranch, repo)
	if explicit != "" {
		return explicit, baseRef
	}
	candidates := []string{baseRef}
	if baseBranch == "" {
		if repo != nil && repo.DefaultBranch != "" {
			candidates = append(candidates, "origin/"+repo.DefaultBranch)
		}
		candidates = append(candidates, "origin/main", "main", "master")
	}
	for _, ref := range candidates {
		if mb, err := git.Run(ctx, root, "merge-base", head, ref); err == nil && mb != "" {
			return mb, ref
		}
	}
	return "", baseRef
}

func triageBaseRef(ctx context.Context, root, baseBranch string, repo *db.Repo) string {
	if b := strings.TrimSpace(baseBranch); b != "" {
		return "origin/" + b
	}
	ref := triage.DefaultBaseRef(ctx, root)
	if ref == "origin/main" && repo != nil && repo.DefaultBranch != "" {
		ref = "origin/" + repo.DefaultBranch
	}
	return ref
}

var repoScriptTier = regexp.MustCompile(`(?m)^TIER: ([A-Z]+)`)

const repoScriptPath = "scripts/nm-triage.sh"

// triageTier is the tier `axi triage` and `axi run`'s pre-run check both
// report: the built-in classification, replaced by the tier the
// repository's own scripts/nm-triage.sh prints (it knows that repository's
// deliberate relaxations; not consulted with an explicit --base), then lifted
// to RISK by any risk path. note is one line saying why the script, when it
// exists, did not decide the tier.
func triageTier(ctx context.Context, root string, change triage.Change, explicitBase bool) (tier, scriptTier triage.Tier, note string) {
	tier = change.Tier
	if !explicitBase && len(change.Files) > 0 {
		scriptTier, note = repoScriptOverride(ctx, root)
		if scriptTier != "" {
			tier = scriptTier
		}
	}
	if len(change.RiskPaths) > 0 {
		tier = triage.TierRisk
	}
	return tier, scriptTier, note
}

// repoScriptOverride runs the repository's own scripts/nm-triage.sh from the
// caller's checkout and maps its tier, exactly as the standalone script did.
func repoScriptOverride(ctx context.Context, root string) (triage.Tier, string) {
	script := filepath.Join(root, filepath.FromSlash(repoScriptPath))
	if _, err := os.Stat(script); err != nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", script)
	c.Dir = root
	out, err := c.Output()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return "", repoScriptPath + " timed out after 60s; the built-in tier was used"
	case err != nil:
		return "", fmt.Sprintf("%s failed (%v); the built-in tier was used", repoScriptPath, err)
	}
	m := repoScriptTier.FindStringSubmatch(string(out))
	if m == nil {
		return "", repoScriptPath + " printed no TIER: line; the built-in tier was used"
	}
	switch {
	case m[1] == "FULL":
		return triage.TierCode, ""
	case strings.HasPrefix(m[1], "TEST"):
		return triage.TierTest, ""
	case m[1] == "DOCS":
		return triage.TierDocs, ""
	}
	return "", fmt.Sprintf("%s printed an unknown tier %q; the built-in tier was used", repoScriptPath, m[1])
}

type triageLaneRow struct {
	Branch string `toon:"branch"`
	Run    string `toon:"run"`
	State  string `toon:"state"`
}

type triageConflictRow struct {
	Kind  string `toon:"kind"`
	With  string `toon:"with"`
	Files string `toon:"files"`
}

type triageStopRow struct {
	Rule   string `toon:"rule"`
	Detail string `toon:"detail"`
}

func runAxiTriage(cmd *cobra.Command, opts triageOptions, skipOnly bool) error {
	ctx := cmd.Context()
	root, err := git.FindGitRoot(".")
	if err != nil {
		return emitError(cmd, 2, "not in a git repository")
	}
	env := openTriageEnv(ctx, root)
	defer env.close()
	head, err := git.Run(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return emitError(cmd, 2, fmt.Sprintf("resolve HEAD: %v", err))
	}
	base, baseRef := triageBase(ctx, root, head, opts.base, opts.baseBranch, env.repo)
	if base == "" {
		return emitError(cmd, 2, "cannot find a base to measure the change from",
			"Pass --base <commit> or --base-branch <branch>, or fetch origin")
	}
	res := triage.Check(ctx, triage.Input{
		Dir: root, Head: head, Branch: env.branch, Base: base,
		Paths: env.p, DB: env.d, Repo: env.repo, ProjectKey: env.key,
	})
	if res.ChangeErr != nil {
		return emitError(cmd, 2, fmt.Sprintf("classify the change: %v", res.ChangeErr))
	}
	change := *res.Change
	if len(change.Files) == 0 {
		if skipOnly {
			return nil
		}
		emitDoc(cmd, toon.Field{Key: "tier", Value: fmt.Sprintf("none: no committed changes vs %s - nothing to validate", shortSHA(base))})
		return nil
	}
	tier, scriptTier, scriptNote := triageTier(ctx, root, change, opts.base != "")
	skip := triage.SkipFor(tier)
	if skipOnly {
		fmt.Fprintln(cmd.OutOrStdout(), skip)
		return nil
	}

	// Advice only: the base and open-PR test merges.
	advisory, unchecked := triageAdvisoryConflicts(ctx, root, head, baseRef, env.branch, res.Lanes, opts.offline)
	conflicts := append(append([]triage.Conflict(nil), res.Conflicts...), advisory...)
	unchecked = append(append([]string(nil), res.Unchecked...), unchecked...)
	if res.ConflictsErr != nil {
		unchecked = append(unchecked, "branches in validation: "+res.ConflictsErr.Error())
	}
	if res.LanesErr != nil {
		unchecked = append(unchecked, "branches in validation: "+res.LanesErr.Error())
	}

	summary := []toon.Field{
		{Key: "branch", Value: env.branch},
		{Key: "base", Value: fmt.Sprintf("%s (%s)", shortSHA(base), baseRef)},
		{Key: "tier", Value: string(tier)},
		{Key: "review", Value: triage.ReviewLine(tier, change)},
		{Key: "files", Value: len(change.Files)},
		{Key: "production_files", Value: change.ProductionFiles},
		{Key: "production_lines", Value: change.ProductionLines},
	}
	if scriptTier != "" {
		summary = append(summary, toon.Field{Key: "repo_script_tier", Value: string(scriptTier)})
	}
	if scriptNote != "" {
		summary = append(summary, toon.Field{Key: "repo_script", Value: scriptNote})
	}
	if len(change.RiskPaths) > 0 {
		summary = append(summary, toon.Field{Key: "risk_paths", Value: change.RiskPaths})
	}
	fields := []toon.Field{
		{Key: "triage", Value: toon.NewObject(summary...)},
		{Key: "run", Value: triageRunCommand(skip, opts.baseBranch)},
		{Key: "pre_run", Value: triage.PreRunAdvice(tier, change.ProductionLines)},
		triageBundlingField(res, env.d != nil),
	}
	if res.Hold != nil {
		fields = append(fields, toon.Field{Key: "hold", Value: res.Hold.String()})
	}
	if len(conflicts) > 0 {
		rows := make([]triageConflictRow, 0, len(conflicts))
		for _, c := range conflicts {
			rows = append(rows, triageConflictRow{Kind: c.Kind, With: c.Label, Files: strings.Join(c.Files, ", ")})
		}
		fields = append(fields, toon.Field{Key: "conflicts", Value: rows})
	} else {
		fields = append(fields, toon.Field{Key: "conflicts", Value: "none (being behind the base alone is not a reason to merge it or run again)"})
	}
	if len(unchecked) > 0 {
		fields = append(fields, toon.Field{Key: "unchecked", Value: unchecked})
	}
	fields = append(fields, triageNotesFields(res)...)
	fields = append(fields, triageVerdictFields(res)...)
	var help []string
	for _, c := range advisory {
		switch c.Kind {
		case triage.ConflictBase:
			help = append(help, "The branch conflicts with its base: merge the base and resolve before validating. If this branch already passed validation, run `no-mistakes axi triage --post-merge --gated <validated head>` after the merge: a tests/docs-only resolution needs the local test suite, not another run.")
		case triage.ConflictOpenPR:
			help = append(help, fmt.Sprintf("The branch conflicts with %s: report it to whoever dispatched you (fold the branches into one, or wait for that PR to merge, then merge the base).", c.Label))
		}
	}
	help = append(help, triageStopHelp(res)...)
	if len(help) > 0 {
		fields = append(fields, toon.Field{Key: "help", Value: help})
	}
	emitDoc(cmd, fields...)
	if len(res.Stops) > 0 {
		return &exitError{code: triageStopExitCode}
	}
	return nil
}

func triageRunCommand(skip, baseBranch string) string {
	parts := []string{"no-mistakes axi run"}
	if skip != "" {
		parts = append(parts, "--skip "+skip)
	}
	if baseBranch != "" {
		parts = append(parts, "--base-branch "+baseBranch)
	}
	parts = append(parts, `--intent "..."`)
	return strings.Join(parts, " ")
}

// triageBundlingField is the bundling facts printed at the moment of
// validation: the project's other branches in validation and the change size.
func triageBundlingField(res triage.Result, haveDB bool) toon.Field {
	var fields []toon.Field
	switch {
	case !haveDB:
		fields = append(fields, toon.Field{Key: "in_validation", Value: "unknown (no run database)"})
	case res.LanesErr != nil:
		fields = append(fields, toon.Field{Key: "in_validation", Value: "unknown: " + res.LanesErr.Error()})
	default:
		fields = append(fields, toon.Field{Key: "in_validation", Value: fmt.Sprintf("%d other %s of this project", len(res.Lanes), pluralWord(len(res.Lanes), "branch", "branches"))})
		if len(res.Lanes) > 0 {
			rows := make([]triageLaneRow, 0, len(res.Lanes))
			for _, l := range res.Lanes {
				state := "in a step"
				if l.Parked {
					state = "parked at a gate"
				}
				rows = append(rows, triageLaneRow{Branch: l.Branch, Run: l.RunID, State: state})
			}
			fields = append(fields, toon.Field{Key: "branches", Value: rows})
		}
	}
	if res.Change != nil {
		size := "no"
		if res.Change.Small() {
			size = "yes"
		}
		fields = append(fields, toon.Field{Key: "small_change", Value: fmt.Sprintf("%s: %d production lines (small is %d or fewer)", size, res.Change.ProductionLines, triage.SmallChangeLines)})
	}
	return toon.Field{Key: "bundling", Value: toon.NewObject(fields...)}
}

type triageNoteRow struct {
	Rule   string `toon:"rule"`
	Detail string `toon:"detail"`
}

// triageNotesFields is the bundle-shape advice; it never stops a run.
func triageNotesFields(res triage.Result) []toon.Field {
	if len(res.Notes) == 0 {
		return nil
	}
	rows := make([]triageNoteRow, 0, len(res.Notes))
	for _, n := range res.Notes {
		rows = append(rows, triageNoteRow{Rule: string(n.Rule), Detail: n.Detail})
	}
	return []toon.Field{{Key: "notes", Value: rows}}
}

func triageVerdictFields(res triage.Result) []toon.Field {
	if len(res.Stops) == 0 {
		return []toon.Field{{Key: "verdict", Value: "go"}}
	}
	rows := make([]triageStopRow, 0, len(res.Stops))
	for _, s := range res.Stops {
		rows = append(rows, triageStopRow{Rule: string(s.Rule), Detail: s.Detail})
	}
	return []toon.Field{{Key: "verdict", Value: "stop"}, {Key: "stops", Value: rows}}
}

func triageStopHelp(res triage.Result) []string {
	if len(res.Stops) == 0 {
		return nil
	}
	help := []string{"Do NOT start the run: report the stop lines to whoever dispatched you."}
	if res.Hold != nil {
		help = append(help, fmt.Sprintf("The hold cannot be overridden; start the run after %s is removed.", res.Hold.Path))
	}
	if res.Overridable() {
		help = append(help, `To start anyway, state why: no-mistakes axi run --triage-override "<reason: an urgent fix, nothing to bundle with>" ... (the reason is recorded)`)
	}
	return help
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// triageAdvisoryConflicts test-merges the branch against its base and the
// other open GitHub pull requests: the standalone script's checks, kept as
// advice. Branches already checked as in validation are not repeated.
func triageAdvisoryConflicts(ctx context.Context, root, head, baseRef, branch string, lanes []triage.Lane, offline bool) ([]triage.Conflict, []string) {
	var targets []triage.MergeTarget
	var unchecked []string
	if t, err := triage.BaseTarget(ctx, root, baseRef, !offline); err == nil {
		targets = append(targets, t)
	} else {
		unchecked = append(unchecked, err.Error())
	}
	if !offline {
		skip := map[string]bool{triage.ShortBranch(branch): true}
		for _, l := range lanes {
			skip[l.Branch] = true
		}
		prs, err := triage.OpenPRTargets(ctx, root, head, skip)
		if err != nil {
			unchecked = append(unchecked, err.Error())
		}
		targets = append(targets, prs...)
	}
	conflicts, more, err := triage.MergeConflicts(ctx, root, head, targets, nil)
	if err != nil {
		unchecked = append(unchecked, err.Error())
	}
	return conflicts, append(unchecked, more...)
}

func runAxiTriageConflicts(cmd *cobra.Command, opts triageOptions) error {
	ctx := cmd.Context()
	root, err := git.FindGitRoot(".")
	if err != nil {
		return emitError(cmd, 2, "not in a git repository")
	}
	env := openTriageEnv(ctx, root)
	defer env.close()
	ref := "HEAD"
	branch := env.branch
	if opts.head != "" {
		ref = opts.head
		branch = strings.TrimPrefix(triage.ShortBranch(opts.head), "origin/")
	}
	head, err := git.Run(ctx, root, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil || head == "" {
		return emitError(cmd, 2, "no such ref: "+ref)
	}
	_, baseRef := triageBase(ctx, root, head, opts.base, opts.baseBranch, env.repo)
	var lanes []triage.Lane
	var inValidation []triage.Conflict
	var unchecked []string
	if env.d != nil && env.p != nil {
		var lanesErr error
		lanes, lanesErr = triage.LanesInValidation(env.d, env.p, env.key, env.repo, branch)
		if lanesErr != nil {
			unchecked = append(unchecked, "branches in validation: "+lanesErr.Error())
		}
		targets, dirs := triage.LaneTargets(lanes)
		var more []string
		var mergeErr error
		inValidation, more, mergeErr = triage.MergeConflicts(ctx, root, head, targets, dirs)
		unchecked = append(unchecked, more...)
		if mergeErr != nil {
			unchecked = append(unchecked, mergeErr.Error())
		}
	} else {
		unchecked = append(unchecked, "branches in validation: no run database")
	}
	advisory, more := triageAdvisoryConflicts(ctx, root, head, baseRef, branch, lanes, opts.offline)
	unchecked = append(unchecked, more...)
	conflicts := append(inValidation, advisory...)
	fields := []toon.Field{}
	if len(conflicts) > 0 {
		rows := make([]triageConflictRow, 0, len(conflicts))
		for _, c := range conflicts {
			rows = append(rows, triageConflictRow{Kind: c.Kind, With: c.Label, Files: strings.Join(c.Files, ", ")})
		}
		fields = append(fields, toon.Field{Key: "conflicts", Value: rows})
	} else {
		fields = append(fields, toon.Field{Key: "conflicts", Value: fmt.Sprintf("none with the base, %d %s in validation, or open PRs", len(lanes), pluralWord(len(lanes), "branch", "branches"))})
	}
	if len(unchecked) > 0 {
		fields = append(fields, toon.Field{Key: "unchecked", Value: unchecked})
	}
	emitDoc(cmd, fields...)
	switch {
	case len(conflicts) > 0:
		return &exitError{code: triageStopExitCode}
	case len(unchecked) > 0:
		return &exitError{code: 2}
	}
	return nil
}

func runAxiTriagePostMerge(cmd *cobra.Command, opts triageOptions) error {
	ctx := cmd.Context()
	root, err := git.FindGitRoot(".")
	if err != nil {
		return emitError(cmd, 2, "not in a git repository")
	}
	env := openTriageEnv(ctx, root)
	defer env.close()
	baseRef := triageBaseRef(ctx, root, opts.baseBranch, env.repo)
	v, err := triage.PostMerge(ctx, root, opts.gated, baseRef)
	if err != nil {
		return emitError(cmd, 2, err.Error())
	}
	verdict := "no new run"
	if v.Regate {
		verdict = "full run"
	}
	emitDoc(cmd, toon.Field{Key: "post_merge", Value: v.Lines}, toon.Field{Key: "verdict", Value: verdict})
	if v.Regate {
		return &exitError{code: 4}
	}
	return nil
}

// triageOverrideFlag is the `axi run` flag that starts a run over the check's
// stops (all but the hold). Its value is the reason, which is recorded.
const triageOverrideFlag = "triage-override"

// newRunTriage is the pre-run check `axi run` applies before it starts a NEW
// run (never when it reattaches). fields are the bundling facts printed with
// the run; override is the record to write once the run exists.
type newRunTriage struct {
	fields   []toon.Field
	override *triage.OverrideRecord
}

// checkTriageBeforeNewRun runs the same rules and reports the same tier as
// `axi triage`, without its network checks (base fetch, open pull requests),
// and refuses to start the run on a stop unless the caller passed
// --triage-override with a reason. The hold cannot be overridden.
func checkTriageBeforeNewRun(cmd *cobra.Command, env *axiEnv, branch, headSHA, baseBranch string) (*newRunTriage, error) {
	ctx := cmd.Context()
	reason := ""
	if cmd.Flags().Lookup(triageOverrideFlag) != nil {
		reason, _ = cmd.Flags().GetString(triageOverrideFlag)
		if cmd.Flags().Changed(triageOverrideFlag) && strings.TrimSpace(reason) == "" {
			return nil, emitError(cmd, 2, "--triage-override needs the reason the run must start anyway",
				`For example --triage-override "urgent fix for a broken main" or "nothing to bundle with"`)
		}
		reason = strings.TrimSpace(reason)
	}
	root, err := git.FindGitRoot(".")
	if err != nil {
		root = "."
	}
	key := ""
	if env.repo != nil {
		key = triage.ProjectKey(env.repo.UpstreamURL)
	}
	base, _ := triageBase(ctx, root, headSHA, "", baseBranch, env.repo)
	res := triage.Check(ctx, triage.Input{
		Dir: root, Head: headSHA, Branch: branch, Base: base,
		Paths: env.p, DB: env.d, Repo: env.repo, ProjectKey: key,
	})
	facts := []toon.Field{}
	if res.Change != nil {
		tier, scriptTier, scriptNote := triageTier(ctx, root, *res.Change, false)
		facts = append(facts,
			toon.Field{Key: "tier", Value: string(tier)},
			toon.Field{Key: "review", Value: triage.ReviewLine(tier, *res.Change)},
		)
		if scriptTier != "" {
			facts = append(facts, toon.Field{Key: "repo_script_tier", Value: string(scriptTier)})
		}
		if scriptNote != "" {
			facts = append(facts, toon.Field{Key: "repo_script", Value: scriptNote})
		}
		facts = append(facts,
			toon.Field{Key: "production_lines", Value: res.Change.ProductionLines},
			toon.Field{Key: "small_change", Value: res.Change.Small()},
		)
	}
	if res.LanesErr == nil {
		names := make([]string, 0, len(res.Lanes))
		for _, l := range res.Lanes {
			names = append(names, l.Branch)
		}
		inValidation := fmt.Sprintf("%d other %s of this project", len(res.Lanes), pluralWord(len(res.Lanes), "branch", "branches"))
		if len(names) > 0 {
			inValidation += ": " + strings.Join(names, ", ")
		}
		facts = append(facts, toon.Field{Key: "in_validation", Value: inValidation})
	}
	facts = append(facts, triageNotesFields(res)...)
	gate := &newRunTriage{}
	if len(res.Stops) > 0 {
		if reason == "" || !res.Overridable() {
			fields := triageVerdictFields(res)
			fields = append([]toon.Field{{Key: "error", Value: "pre-run check stopped this new run"}, {Key: "triage", Value: toon.NewObject(facts...)}}, fields...)
			fields = append(fields, toon.Field{Key: "help", Value: triageStopHelp(res)})
			emitDoc(cmd, fields...)
			return nil, &exitError{code: triageStopExitCode}
		}
		facts = append(facts,
			toon.Field{Key: "overridden", Value: strings.Join(res.StopRules(), ", ")},
			toon.Field{Key: "override_reason", Value: reason},
		)
		gate.override = &triage.OverrideRecord{Branch: branch, HeadSHA: headSHA, Rules: res.StopRules(), Reason: reason}
		if env.repo != nil {
			gate.override.RepoID = env.repo.ID
		}
	}
	if len(facts) > 0 {
		gate.fields = []toon.Field{{Key: "triage", Value: toon.NewObject(facts...)}}
	}
	return gate, nil
}

// recordTriageOverride writes the override once the run it let start exists.
// A failed write is reported, not fatal: the run has already started.
func recordTriageOverride(cmd *cobra.Command, env *axiEnv, gate *newRunTriage, runID string) {
	if gate == nil || gate.override == nil || env.p == nil {
		return
	}
	rec := *gate.override
	rec.RunID = runID
	if err := triage.RecordOverride(env.p.Root(), rec); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not record the triage override in %s: %v\n", filepath.Join(env.p.Root(), triage.OverrideLogName), err)
	}
}
