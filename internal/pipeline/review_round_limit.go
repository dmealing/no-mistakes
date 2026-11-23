package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// HasReviewFixRoundLimit identifies an exhausted Review budget. Automatic
// resolvers must leave this gate for an explicit decision, never approve it.
func HasReviewFixRoundLimit(raw string) bool {
	return hasFindingID(raw, types.FindingIDReviewFixRoundsExhausted)
}

func reviewFixRoundCount(rounds []*db.StepRound) int {
	count := 0
	for _, round := range rounds {
		if round.IsFixRound() {
			count++
		}
	}
	return count
}

func (e *Executor) reviewFixRoundLimit(stepID string) (int, error) {
	step, err := e.db.GetStepResult(stepID)
	if err != nil {
		return 0, err
	}
	if step == nil {
		return 0, fmt.Errorf("Review step not found")
	}
	// An active step from before the feature was unbounded. A config edit
	// must not retroactively impose a limit on it during recovery.
	if step.ReviewMaxFixRounds == nil {
		return 0, nil
	}
	if *step.ReviewMaxFixRounds < 0 {
		return 0, fmt.Errorf("invalid pinned Review fix-round limit")
	}
	return *step.ReviewMaxFixRounds, nil
}

func reviewFixRoundLimitMessage(used, limit int) string {
	return fmt.Sprintf("fix_rounds_exhausted: Review used %d/%d fix rounds; the gate remains parked with its remaining findings", used, limit)
}

func reviewFixRoundLimitRefusal(used, limit int, gateFindings string) *RespondRefusal {
	help := "Inspect the remaining findings and explicitly approve, skip, or abort. A new run gets a new budget; changing config does not extend this run."
	if HasProtectedPathRefusal(gateFindings) {
		help = "Approve is also rejected because this gate holds a protected-path refusal: skip this step or abort the run. A new run gets a new budget; changing config does not extend this run."
	}
	return &RespondRefusal{Message: reviewFixRoundLimitMessage(used, limit), Help: help}
}

func appendReviewFixRoundLimit(raw string, used, limit int) (string, error) {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", err
	}
	findings.Items = append(findings.Items, types.Finding{
		ID:          types.FindingIDReviewFixRoundsExhausted,
		Severity:    "warning",
		Action:      types.ActionAskUser,
		Description: reviewFixRoundLimitMessage(used, limit),
	})
	return types.MarshalFindingsJSON(findings)
}

// reviewRoundCapReached reports whether a Review step has spent the global
// review.round_cap (0 = off): cap review rounds means at most cap-1 fix
// rounds, so the round after the last allowed fix is the final one. Answer,
// coverage-continuation, and intent rounds are not fix rounds and never
// count.
func reviewRoundCapReached(roundCap, fixRounds int) bool {
	return roundCap > 0 && fixRounds >= roundCap-1
}

// reviewRoundCapNotesAllowed reports whether a capped Review round may
// complete with its remaining findings recorded as notes. It never may while
// any remaining finding is an error or needs a person (ask-user, including an
// open question, an intent-conformance finding, a protected-path refusal, and
// the review.max_fix_rounds notice), or while the round's coverage record is
// incomplete: those keep parking at the existing gate, so the cap never
// silently passes an error or an unverified file.
func reviewRoundCapNotesAllowed(findingsJSON string, coverageComplete bool) bool {
	return coverageComplete && findingsCount(findingsJSON) > 0 &&
		!hasErrorFindingsJSON(findingsJSON) && !hasAskUserFindingsJSON(findingsJSON)
}

func reviewRoundCapNotesMessage(roundCap, remaining int) string {
	return fmt.Sprintf("review round cap reached (%d %s, review.round_cap): %d remaining %s left unresolved as %s (no further fix round)",
		roundCap, pluralize(roundCap, "round", "rounds"), remaining, pluralize(remaining, "finding", "findings"), pluralize(remaining, "note", "notes"))
}

// appendReviewRoundCapNotes marks the findings a capped Review completes on.
func appendReviewRoundCapNotes(raw string, roundCap int) (string, error) {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return "", err
	}
	findings.Items = append(findings.Items, types.Finding{
		ID:          types.FindingIDReviewRoundCapNotes,
		Severity:    types.FindingSeverityInfo,
		Action:      types.ActionNoOp,
		Description: reviewRoundCapNotesMessage(roundCap, len(findings.Items)),
	})
	return types.MarshalFindingsJSON(findings)
}
