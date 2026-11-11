package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Rule names a reason the pre-run check stops a new run.
type Rule string

const (
	// RuleHold: a fleet-wide hold on new runs is active. It cannot be
	// overridden here; the daemon refuses the run too.
	RuleHold Rule = "hold"
	// RuleInValidationConflict: the branch conflicts with a branch of the
	// same project that is in validation, so one of the two runs is wasted.
	RuleInValidationConflict Rule = "in-validation-conflict"
	// RuleThirdBranch: two other branches of the project are already in
	// validation; this one would be the third.
	RuleThirdBranch Rule = "third-branch"
	// RuleSmallWithSibling: a small change while a sibling branch of the
	// project is in validation - it should ride with a sibling lane.
	RuleSmallWithSibling Rule = "small-change-with-sibling"
)

// Stop is one rule that stops the run.
type Stop struct {
	Rule   Rule
	Detail string
}

// Overridable reports whether a stated reason may override the stop.
func (s Stop) Overridable() bool { return s.Rule != RuleHold }

// Input is what one check needs.
type Input struct {
	// Dir is the checkout; Head the commit to validate; Branch its name.
	Dir    string
	Head   string
	Branch string
	// Base is the commit or ref the change is measured from; empty leaves
	// the change unclassified (and the small-change rule unapplied).
	Base string
	// Paths, DB and Repo locate the run database and this checkout's
	// registration. DB nil skips every in-validation rule; Repo may be nil
	// for an unregistered checkout, which then matches by ProjectKey only.
	Paths      *paths.Paths
	DB         *db.DB
	Repo       *db.Repo
	ProjectKey string
	Now        time.Time
}

// Result is the full set of facts and the verdict.
type Result struct {
	Hold         *Hold
	Change       *Change
	ChangeErr    error
	Lanes        []Lane
	LanesErr     error
	Conflicts    []Conflict
	Unchecked    []string
	ConflictsErr error
	Stops        []Stop
}

// Overridable reports whether every stop may be overridden with a reason.
func (r Result) Overridable() bool {
	for _, s := range r.Stops {
		if !s.Overridable() {
			return false
		}
	}
	return true
}

// StopRules lists the stop rules in order.
func (r Result) StopRules() []string {
	out := make([]string, 0, len(r.Stops))
	for _, s := range r.Stops {
		out = append(out, string(s.Rule))
	}
	return out
}

// Check gathers the facts that decide whether a new run may start and applies
// the stop rules. It only reads: the hold file, the run database, and git
// (test merges write their objects to a throwaway directory). An error
// reading one fact is recorded on the result and that rule is not applied;
// the run is never stopped on a fact that could not be read, except the hold,
// which fails closed.
func Check(ctx context.Context, in Input) Result {
	var res Result
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	if in.Paths != nil {
		res.Hold, _ = ReadHold(in.Paths.Root(), now)
	}
	if in.Base != "" {
		risk, err := RiskPatterns(in.Dir)
		if err == nil {
			var c Change
			c, err = DiffChange(ctx, in.Dir, in.Base, in.Head, risk)
			if err == nil {
				res.Change = &c
			}
		}
		res.ChangeErr = err
	}
	if in.DB != nil && in.Paths != nil {
		res.Lanes, res.LanesErr = LanesInValidation(in.DB, in.Paths, in.ProjectKey, in.Repo, in.Branch)
		if targets, dirs := LaneTargets(res.Lanes); len(targets) > 0 {
			res.Conflicts, res.Unchecked, res.ConflictsErr = MergeConflicts(ctx, in.Dir, in.Head, targets, dirs)
		}
	}
	res.Stops = Evaluate(res)
	return res
}

// Evaluate applies the stop rules to gathered facts.
func Evaluate(res Result) []Stop {
	var stops []Stop
	if res.Hold != nil {
		stops = append(stops, Stop{Rule: RuleHold, Detail: "new validation runs are on hold: " + res.Hold.String()})
	}
	for _, c := range res.Conflicts {
		if c.Kind != ConflictInValidation {
			continue
		}
		stops = append(stops, Stop{Rule: RuleInValidationConflict, Detail: fmt.Sprintf("conflicts with %s in %s: fold the two branches into one, or wait until that run finishes", c.Label, strings.Join(c.Files, ", "))})
	}
	if len(res.Lanes) > MaxOtherBranchesInValidation {
		stops = append(stops, Stop{Rule: RuleThirdBranch, Detail: fmt.Sprintf("%d other branches of this project are in validation (%s): this would be branch %d; bundle it into one of them or wait", len(res.Lanes), laneNames(res.Lanes), len(res.Lanes)+1)})
	}
	if res.Change != nil && res.Change.Small() && len(res.Lanes) > 0 {
		stops = append(stops, Stop{Rule: RuleSmallWithSibling, Detail: fmt.Sprintf("small change (%d production lines, %d or fewer) while %s of this project is in validation: ride with a sibling lane instead of a run of its own", res.Change.ProductionLines, SmallChangeLines, laneNames(res.Lanes))})
	}
	return stops
}

func laneNames(lanes []Lane) string {
	names := make([]string, 0, len(lanes))
	for _, l := range lanes {
		names = append(names, l.Branch)
	}
	return strings.Join(names, ", ")
}

// OverrideLogName is the append-only record of overridden stops, one JSON
// object per line, under the no-mistakes home.
const OverrideLogName = "triage-overrides.ndjson"

// OverrideRecord is one run started over the check's stops.
type OverrideRecord struct {
	At      string   `json:"at"`
	RepoID  string   `json:"repo_id,omitempty"`
	Branch  string   `json:"branch"`
	HeadSHA string   `json:"head_sha"`
	RunID   string   `json:"run_id,omitempty"`
	Rules   []string `json:"rules"`
	Reason  string   `json:"reason"`
}

// RecordOverride appends rec to the override log under root.
func RecordOverride(root string, rec OverrideRecord) error {
	if rec.At == "" {
		rec.At = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, OverrideLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
