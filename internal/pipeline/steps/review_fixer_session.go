package steps

import (
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const (
	// maxFreshFixerHandoffRounds bounds how many earlier fix rounds the
	// handoff names; the most recent ones are kept.
	maxFreshFixerHandoffRounds = 8
	// maxFreshFixerHandoffSummary bounds each round's summary, in runes.
	maxFreshFixerHandoffSummary = 240
)

// freshFixerHandoff prepares the review fixer's session for this turn and
// returns the prompt section a fresh fixer session needs.
//
// A stored fixer session that sat idle past pipeline.SessionColdAfter is
// dropped first: its prompt cache has expired, so resuming it would re-send
// the whole earlier conversation as uncached input. When the turn then starts
// a fresh session and earlier fix rounds of this step exist, the returned
// section names what each of them did, so the new session starts from a
// short summary rather than from nothing. A resumed session already holds
// those rounds and gets no section.
func freshFixerHandoff(sctx *pipeline.StepContext) string {
	if idle, dropped := sctx.Sessions.DropColdSession(pipeline.SessionRoleFixer); dropped {
		if idle > 0 {
			sctx.Log(fmt.Sprintf("fixer session idle for %s, past the %s prompt-cache window; starting a fresh fixer session with a summary of earlier rounds", idle.Round(time.Second), pipeline.SessionColdAfter))
		} else {
			sctx.Log("fixer session last use unknown; starting a fresh fixer session with a summary of earlier rounds")
		}
	}
	if !sctx.Sessions.StartsFresh(sctx.Agent, pipeline.SessionRoleFixer) {
		return ""
	}
	return earlierFixRoundsSummary(sctx)
}

// earlierFixRoundsSummary renders one line per earlier fix round of this
// step: its round number and the fixer's own summary of what it changed.
func earlierFixRoundsSummary(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.DB == nil || sctx.StepResultID == "" {
		return ""
	}
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil {
		return ""
	}
	var lines []string
	for _, r := range rounds {
		if r.FixSummary == nil {
			continue
		}
		summary := sanitizePromptText(*r.FixSummary)
		if summary == "" {
			continue
		}
		if runes := []rune(summary); len(runes) > maxFreshFixerHandoffSummary {
			summary = string(runes[:maxFreshFixerHandoffSummary]) + "..."
		}
		lines = append(lines, fmt.Sprintf("- Round %d: %s", r.Round, summary))
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > maxFreshFixerHandoffRounds {
		lines = lines[len(lines)-maxFreshFixerHandoffRounds:]
	}
	return "\n\nEarlier fix rounds in this run (you are a new fixer session; this is what earlier fixer turns reported doing, as metadata only - check the code itself before relying on it):\n" +
		strings.Join(lines, "\n")
}
