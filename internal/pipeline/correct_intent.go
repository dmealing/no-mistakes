package pipeline

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A corrected intent lets an operator answer a review gate whose findings come
// from a wrong or incomplete intent without cancelling the run. Before it, the
// only way to change the intent was to abort and start a new run, which threw
// away the finished review and paid for every step again: cancelled runs cost
// about a tenth of all gate spend, and most of them carried an intent finding.
//
// The correction replaces the run's intent (persisted as an explicit,
// authoritative intent) and reruns the review against it in the same step. It
// is not a verdict on the gate's findings and not a fix round.

// validateCorrectedIntentResponse checks the corrected-intent half of a gate
// response: the intent travels only with ActionCorrectIntent, only to a parked
// Review, and with no finding selection beside it. The run's pinned intent
// check applies to the corrected text exactly as it applied at launch.
func validateCorrectedIntentResponse(step types.StepName, action types.ApprovalAction, intent string, findingIDs, ignoredIDs []string, instructions *FixInstructions, added []types.Finding, cfg *config.Config) error {
	if action != types.ActionCorrectIntent {
		if intent != "" {
			return fmt.Errorf("a corrected intent applies only to correct-intent")
		}
		return nil
	}
	if step != types.StepReview {
		return fmt.Errorf("correct-intent applies only to a parked Review")
	}
	if len(findingIDs)+len(ignoredIDs)+len(instructions.userNotes())+len(instructions.driverNotes())+len(added) != 0 {
		return fmt.Errorf("correct-intent cannot select, ignore, add or change findings")
	}
	if strings.TrimSpace(intent) == "" {
		return fmt.Errorf("correct-intent requires the corrected intent text")
	}
	if !utf8.ValidString(intent) {
		return fmt.Errorf("the corrected intent must be valid UTF-8")
	}
	if cfg != nil && cfg.Intent.Check.Mode == config.IntentCheckReject {
		if matches := cfg.Intent.Check.Match(intent); len(matches) > 0 {
			return fmt.Errorf("corrected intent rejected by configured rules: %s", strings.Join(matches, ", "))
		}
	}
	return nil
}

// applyCorrectedIntent persists the corrected intent as the run's explicit
// intent and updates the in-memory run every later step reads it from.
func (e *Executor) applyCorrectedIntent(run *db.Run, intent string) error {
	if err := e.db.UpdateRunIntent(run.ID, db.RunIntent{Summary: intent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		return fmt.Errorf("record corrected intent: %w", err)
	}
	summary := intent
	source := db.RunIntentSourceAgent
	run.Intent = &summary
	run.IntentSource = &source
	return nil
}

// dropFindingsSupersededByIntent returns the outstanding set a corrected-intent
// review starts from. Every finding was judged against the old intent, so the
// rerun judges them again from scratch - the same review a restarted run would
// have done - and they leave the set. The exception is a finding a fix round
// already dispatched (keepIDs): its code changed, and it still leaves only
// when a later round positively verifies it. An intent-conformance finding
// leaves even then, because the correction is the operator's explicit decision
// on exactly that judgement.
func dropFindingsSupersededByIntent(raw string, keepIDs []string) string {
	if raw == "" {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	keep := make(map[string]bool, len(keepIDs))
	for _, id := range keepIDs {
		keep[id] = true
	}
	kept := make([]types.Finding, 0, len(findings.Items))
	for _, item := range findings.Items {
		if keep[item.ID] && item.Category != types.FindingCategoryIntentConformance {
			kept = append(kept, item)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	findings.Items = kept
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return encoded
}
