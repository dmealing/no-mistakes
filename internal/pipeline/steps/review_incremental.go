package steps

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewCarry is a proven incremental Review: the branch's latest approved
// Review still holds for everything except the commits added since it, so this
// Review covers only the diff from BaseSHA to the run head and carries the
// earlier approval, findings, and decisions for the rest.
//
// BaseSHA is the approved head merged cleanly with the run's current base
// (the approved head itself when the base did not move). The diff from it to
// the run head is therefore exactly the branch's new non-base work: a merge of
// the base that git resolves without help contributes nothing to it, and any
// hand-resolved or edited merge shows up as branch work and is reviewed.
type reviewCarry struct {
	SourceRunID  string
	ApprovedHead string
	BaseSHA      string
	// PriorFindings is the approved Review's final findings payload, carried
	// into the prompt as already-decided context. Empty when it recorded none.
	PriorFindings string
}

// reviewCarryHunkMargin is the number of lines around a reviewed hunk that
// base movement must also leave alone. It is git's default diff context: a
// base change that close to reviewed code can change what that code does, so
// it counts as overlap.
const reviewCarryHunkMargin = 3

// reviewCarryGlobalKeys are the global settings that change what a Review
// judges or who judges it. A change to any of them since the approved Review
// ran means the approval cannot carry. review.incremental itself is excluded
// from the comparison: turning the feature on does not change the review.
var reviewCarryGlobalKeys = []string{"agent", "agent_config", "review_agents", "review", "repository_overrides"}

// planIncrementalReview decides whether this Review may be incremental
// (review.incremental). It returns the proven plan, or nil and the reason a
// full Review runs ("" when there is nothing to carry from, or the feature is
// off). Every check fails toward a full Review: a wrong carry lets unreviewed
// code through, while a needless full Review only costs time.
func planIncrementalReview(sctx *pipeline.StepContext, baseSHA string) (*reviewCarry, string) {
	if sctx.Config == nil || !sctx.Config.Review.Incremental || sctx.DB == nil || sctx.Run == nil {
		return nil, ""
	}
	prior, step, err := sctx.DB.LatestApprovedReview(sctx.Run.RepoID, sctx.Run.Branch)
	if err != nil {
		return nil, fmt.Sprintf("the branch's earlier reviews could not be read: %v", err)
	}
	if prior == nil || prior.ReviewApprovedHeadSHA == nil {
		return nil, ""
	}
	carry := &reviewCarry{SourceRunID: prior.ID, ApprovedHead: strings.TrimSpace(*prior.ReviewApprovedHeadSHA)}
	if reason := reviewCarryRunMismatch(prior, sctx.Run); reason != "" {
		return nil, reason
	}
	source := step
	if step.RoundsStepID() != step.ID {
		source, err = sctx.DB.GetStepResult(step.RoundsStepID())
		if err != nil || source == nil {
			return nil, fmt.Sprintf("the approved review's record in run %s could not be read", prior.ID)
		}
	}
	if reason := reviewCarryConfigMismatch(sctx, source.ID); reason != "" {
		return nil, reason
	}
	if source.FindingsJSON != nil {
		carry.PriorFindings = *source.FindingsJSON
	}
	merged, reason := reviewCarryMergeBase(sctx, carry.ApprovedHead, baseSHA)
	if reason != "" {
		return nil, reason
	}
	carry.BaseSHA = merged
	return carry, ""
}

// reviewCarryRunMismatch compares the run-level inputs a Review judged
// against: the PR base branch and an explicit intent. An inferred intent is
// not an acceptance contract and is re-derived per run, so two inferred
// intents never block a carry; an explicit one on either side must match
// exactly (whitespace-trimmed), on both sides.
func reviewCarryRunMismatch(prior, current *db.Run) string {
	if deref(prior.PRBaseBranch) != deref(current.PRBaseBranch) {
		return fmt.Sprintf("run %s targeted base branch %q, this run targets %q", prior.ID, deref(prior.PRBaseBranch), deref(current.PRBaseBranch))
	}
	priorExplicit := db.IsAuthoritativeRunIntentSource(deref(prior.IntentSource))
	currentExplicit := db.IsAuthoritativeRunIntentSource(deref(current.IntentSource))
	if priorExplicit || currentExplicit {
		if priorExplicit != currentExplicit || strings.TrimSpace(deref(prior.Intent)) != strings.TrimSpace(deref(current.Intent)) {
			return fmt.Sprintf("the run intent changed since run %s approved its review", prior.ID)
		}
	}
	return ""
}

// reviewCarryConfigMismatch requires the repository configuration and the
// review-affecting global settings this run resolved to be the ones the
// approved Review's last recorded round ran under. Rounds record them only
// while eval.capture_provenance is on (the default); with no record on either
// side nothing can be compared, so the Review runs in full.
func reviewCarryConfigMismatch(sctx *pipeline.StepContext, stepID string) string {
	currentRepo, currentGlobal := sctx.Config.ReplayRepoYAML, sctx.Config.ReplayGlobalYAML
	if len(currentRepo) == 0 || len(currentGlobal) == 0 {
		return "this run did not record its configuration (eval.capture_provenance is off), so a changed configuration cannot be ruled out"
	}
	rounds, err := sctx.DB.GetRoundsByStep(stepID)
	if err != nil {
		return fmt.Sprintf("the approved review's rounds could not be read: %v", err)
	}
	var recordedRepo, recordedGlobal []byte
	for _, round := range rounds {
		if len(round.RepoConfigYAML) > 0 && len(round.GlobalConfigYAML) > 0 {
			recordedRepo, recordedGlobal = round.RepoConfigYAML, round.GlobalConfigYAML
		}
	}
	if recordedRepo == nil {
		return "the approved review recorded no configuration to compare"
	}
	changed, err := changedYAMLSettings(recordedRepo, currentRepo, nil)
	if err != nil {
		return err.Error()
	}
	if len(changed) > 0 {
		return fmt.Sprintf("the repository configuration changed since the approved review (%s)", strings.Join(changed, ", "))
	}
	changed, err = changedYAMLSettings(recordedGlobal, currentGlobal, reviewCarryGlobalKeys)
	if err != nil {
		return err.Error()
	}
	if len(changed) > 0 {
		return fmt.Sprintf("the global review configuration changed since the approved review (%s)", strings.Join(changed, ", "))
	}
	return ""
}

// changedYAMLSettings names the top-level settings whose values differ
// between two YAML documents, limited to keys when keys is non-nil. The
// review block's incremental switch is never compared.
func changedYAMLSettings(recorded, current []byte, keys []string) ([]string, error) {
	var before, after map[string]any
	if err := yaml.Unmarshal(recorded, &before); err != nil {
		return nil, fmt.Errorf("the approved review's recorded configuration could not be read: %v", err)
	}
	if err := yaml.Unmarshal(current, &after); err != nil {
		return nil, fmt.Errorf("this run's configuration could not be read: %v", err)
	}
	for _, doc := range []map[string]any{before, after} {
		if review, ok := doc["review"].(map[string]any); ok {
			delete(review, "incremental")
			if len(review) == 0 {
				delete(doc, "review")
			}
		}
	}
	names := map[string]bool{}
	for key := range before {
		names[key] = true
	}
	for key := range after {
		names[key] = true
	}
	var changed []string
	for key := range names {
		if keys != nil && !slices.Contains(keys, key) {
			continue
		}
		if !reflect.DeepEqual(before[key], after[key]) {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// reviewCarryMergeBase proves the git half of a carry and returns the commit
// the incremental diff starts from, or the reason it cannot:
//
//   - the approved head must still be an ancestor of the run head, so a
//     force-push or rewrite gets a full Review;
//   - the base movement since the approved head's merge base must touch no
//     reviewed hunk (within reviewCarryHunkMargin lines) in any file both
//     changed, and must not touch a reviewed file whose change has no line
//     hunks (binary, mode, or type change);
//   - the approved head must merge with the current base without a conflict.
//
// The returned commit is that merge (written as an unreferenced commit object,
// deterministic for the same inputs), or the approved head itself when the
// merge adds nothing to it.
func reviewCarryMergeBase(sctx *pipeline.StepContext, approved, baseSHA string) (string, string) {
	head := sctx.Run.HeadSHA
	if _, err := stepGitRun(sctx, "cat-file", "-e", approved+"^{commit}"); err != nil {
		return "", fmt.Sprintf("the approved head %s is not in this repository", shortObjectID(approved))
	}
	if ancestor, err := gitIsAncestor(sctx, approved, head); err != nil {
		return "", fmt.Sprintf("ancestry of the approved head %s could not be checked: %v", shortObjectID(approved), err)
	} else if !ancestor {
		return "", fmt.Sprintf("the approved head %s is not an ancestor of %s (the branch was rewritten or force-pushed)", shortObjectID(approved), shortObjectID(head))
	}
	oldBase, err := stepGitRun(sctx, "merge-base", approved, baseSHA)
	if err != nil || oldBase == "" {
		return "", fmt.Sprintf("the approved head %s shares no merge base with the base %s", shortObjectID(approved), shortObjectID(baseSHA))
	}
	if oldBase != baseSHA {
		if file, err := baseMovementOverlap(sctx, oldBase, approved, baseSHA); err != nil {
			return "", fmt.Sprintf("base movement could not be compared with the reviewed changes: %v", err)
		} else if file != "" {
			return "", fmt.Sprintf("base movement since the approved review touches reviewed code in %s", file)
		}
	}
	out, err := stepGitRun(sctx, "merge-tree", "--write-tree", "--no-messages", approved, baseSHA)
	if err != nil {
		return "", fmt.Sprintf("the approved head %s does not merge cleanly with the base %s", shortObjectID(approved), shortObjectID(baseSHA))
	}
	tree := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	approvedTree, err := stepGitRun(sctx, "rev-parse", approved+"^{tree}")
	if err != nil || tree == "" {
		return "", "the merge of the approved head with the base could not be read"
	}
	if tree == approvedTree {
		return approved, ""
	}
	// Fixed identity and dates make the object id a function of its inputs,
	// so every round of this Review resolves the same base commit.
	env := []string{
		"GIT_AUTHOR_NAME=no-mistakes", "GIT_AUTHOR_EMAIL=no-mistakes@localhost", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=no-mistakes", "GIT_COMMITTER_EMAIL=no-mistakes@localhost", "GIT_COMMITTER_DATE=1700000000 +0000",
	}
	message := fmt.Sprintf("no-mistakes: approved head %s merged with base %s", approved, baseSHA)
	merged, err := stepGitRunEnv(sctx, env, "commit-tree", "--no-gpg-sign", tree, "-p", approved, "-p", baseSHA, "-m", message)
	if err != nil || merged == "" {
		return "", "the merge of the approved head with the base could not be recorded"
	}
	return merged, ""
}

// gitIsAncestor runs merge-base --is-ancestor: exit 1 is a clean "no", any
// other failure is an error.
func gitIsAncestor(sctx *pipeline.StepContext, ancestor, descendant string) (bool, error) {
	_, err := stepGitRun(sctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// baseMovementOverlap returns the first file in which the base movement
// (oldBase..newBase) touches the reviewed change (oldBase..approved), or ""
// when no file overlaps. Both diffs start at oldBase, so their old-side line
// numbers share one coordinate system.
func baseMovementOverlap(sctx *pipeline.StepContext, oldBase, approved, newBase string) (string, error) {
	reviewed, err := stepGitRunRaw(sctx, "diff", "--name-only", "-z", "--no-renames", oldBase, approved)
	if err != nil {
		return "", err
	}
	moved, err := stepGitRunRaw(sctx, "diff", "--name-only", "-z", "--no-renames", oldBase, newBase)
	if err != nil {
		return "", err
	}
	movedSet := map[string]bool{}
	for _, path := range changedPathList(moved) {
		movedSet[path] = true
	}
	for _, path := range changedPathList(reviewed) {
		if !movedSet[path] {
			continue
		}
		reviewedHunks, ok, err := diffHunks(sctx, oldBase, approved, path)
		if err != nil {
			return "", err
		}
		if !ok {
			return path, nil
		}
		movedHunks, ok, err := diffHunks(sctx, oldBase, newBase, path)
		if err != nil {
			return "", err
		}
		if !ok || hunksOverlap(reviewedHunks, movedHunks, reviewCarryHunkMargin) {
			return path, nil
		}
	}
	return "", nil
}

// lineRange is an inclusive old-side line range of one diff hunk.
type lineRange struct{ lo, hi int }

// diffHunks returns the old-side line ranges of one file's zero-context diff.
// ok is false when the diff has no line hunks to compare (binary, mode-only,
// or type change), which the caller treats as overlap.
func diffHunks(sctx *pipeline.StepContext, from, to, path string) ([]lineRange, bool, error) {
	out, err := stepGitRunRaw(sctx, "diff", "-U0", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", from, to, "--", ":(literal)"+path)
	if err != nil {
		return nil, false, err
	}
	var hunks []lineRange
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "@@ -") {
			continue
		}
		spec, _, _ := strings.Cut(strings.TrimPrefix(line, "@@ -"), " ")
		startText, countText, hasCount := strings.Cut(spec, ",")
		start, err := strconv.Atoi(startText)
		if err != nil {
			return nil, false, nil
		}
		count := 1
		if hasCount {
			if count, err = strconv.Atoi(countText); err != nil {
				return nil, false, nil
			}
		}
		if count == 0 {
			// A pure insertion lands between line start and start+1.
			hunks = append(hunks, lineRange{start, start + 1})
			continue
		}
		hunks = append(hunks, lineRange{start, start + count - 1})
	}
	if len(hunks) == 0 || bytes.Contains([]byte(out), []byte("\nBinary files ")) {
		return nil, false, nil
	}
	return hunks, true, nil
}

// hunksOverlap reports whether any two ranges come within margin lines.
func hunksOverlap(a, b []lineRange, margin int) bool {
	for _, x := range a {
		for _, y := range b {
			if x.lo <= y.hi+margin && y.lo <= x.hi+margin {
				return true
			}
		}
	}
	return false
}

// incrementalReviewScope is the review-scope line for an incremental Review.
func incrementalReviewScope(carry *reviewCarry, head string) string {
	return fmt.Sprintf("only the branch's changes since the review approved at %s (run %s): changes between %s and %s", carry.ApprovedHead, carry.SourceRunID, carry.BaseSHA, head)
}

// incrementalReviewPromptSection tells the reviewer what the earlier approval
// already covers and hands it that review's findings as decided context.
func incrementalReviewPromptSection(carry *reviewCarry) string {
	if carry == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nIncremental review (an earlier review of this branch is carried forward):\n")
	fmt.Fprintf(&b, "- Run %s reviewed and approved head %s.", carry.SourceRunID, carry.ApprovedHead)
	if carry.BaseSHA == carry.ApprovedHead {
		b.WriteString(" The base commit above is that approved head.\n")
	} else {
		b.WriteString(" The base commit above is that approved head merged with the current base branch; the base changes it brings in touch none of the reviewed code.\n")
	}
	b.WriteString("- Review only the changes between the base commit and the target commit: the branch's work since that approval. Read other code only as context for them; it was already reviewed and approved.\n")
	prior := sanitizedPreviousFindingsForPrompt(carry.PriorFindings)
	if findings, err := types.ParseFindingsJSON(carry.PriorFindings); err == nil && len(findings.Items) > 0 && prior != "" {
		b.WriteString("- The approved review recorded these findings, already decided there. Do not report one again unless the new changes alter the code it describes:\n")
		b.WriteString(prior)
		b.WriteString("\n")
	}
	return b.String()
}

// reviewCarriedFinding is the notice a Review completes on when the run head
// adds no reviewable work to the approved head (types.FindingIDReviewCarried).
// changedNothing is true when the new head's tree is exactly the approved head
// merged with the base; false when new commits changed only paths the review
// does not cover (ignore patterns).
func reviewCarriedFinding(carry *reviewCarry, head string, changedNothing bool) Finding {
	what := "no new branch work since the approved head"
	switch {
	case carry.ApprovedHead == head:
		what = "the same head was already approved"
	case !changedNothing:
		what = "new commits change no reviewable path"
	}
	if carry.BaseSHA != carry.ApprovedHead {
		what += "; the base changes merged since touch no reviewed code"
	}
	description := fmt.Sprintf("review carried from run %s at %s (%s)", carry.SourceRunID, shortObjectID(carry.ApprovedHead), what)
	if findings, err := types.ParseFindingsJSON(carry.PriorFindings); err == nil {
		if n := len(reviewCarriedPriorItems(findings)); n > 0 {
			description += fmt.Sprintf("; its %d recorded %s carried as decided", n, pluralWordPR(n, "finding", "findings"))
		}
	}
	return Finding{
		ID:          types.FindingIDReviewCarried,
		Severity:    types.FindingSeverityInfo,
		Action:      types.ActionNoOp,
		Description: description,
	}
}

// reviewCarriedPriorItems drops the earlier review's own pipeline notices.
func reviewCarriedPriorItems(findings types.Findings) []types.Finding {
	var items []types.Finding
	for _, item := range findings.Items {
		if item.ID == types.FindingIDReviewCarried || item.ID == types.FindingIDReviewRoundCapNotes {
			continue
		}
		items = append(items, item)
	}
	return items
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
