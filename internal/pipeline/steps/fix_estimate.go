package steps

import "github.com/kunchenguid/no-mistakes/internal/types"

// Fix estimates: the step that finds a problem rates what fixing it takes,
// at the moment it records the finding, and the rating travels with the
// finding to every fix prompt. Each fix prompt carries one stable line built
// from the findings it hands over (types.FixEstimateLine), which an external
// agent wrapper reads to pick a model. no-mistakes never switches models on
// it. Findings recorded without agent judgement (a failing configured
// command, a failing CI check, a merge conflict) carry no estimate and count
// as complex.

// fixEstimateRule asks a finding-producing agent to rate each finding's fix.
const fixEstimateRule = `- For each finding, estimate what fixing it takes, judged by the reasoning the fix needs rather than by its severity: set fix_complexity to "simple" (a local, mechanical change whose correct form is obvious), "moderate" (some reasoning about surrounding code, a few call sites, or one invariant), or "complex" (cross-file design, root-cause analysis, concurrency, security, or data or migration reasoning). Set fix_lines and fix_files to your estimate of how many lines the fix changes and across how many files.`

// fixEstimateSchemaProperties is the per-finding estimate fragment every
// findings schema carries, with a leading comma so it follows "action".
const fixEstimateSchemaProperties = `,
					"fix_complexity": {"type": "string", "enum": ["simple", "moderate", "complex"], "description": "Reasoning the fix needs: simple (local, mechanical), moderate (surrounding code or a few call sites), complex (cross-file design, root cause, concurrency, security, data or migration)"},
					"fix_lines": {"type": "integer", "description": "Estimated lines the fix changes"},
					"fix_files": {"type": "integer", "description": "Estimated files the fix touches"}`

// fixEstimatePromptLine returns the routing line for a fix prompt handing
// over the findings in raw. Unparseable or empty findings report complex.
func fixEstimatePromptLine(raw string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return types.FixEstimateLine(nil)
	}
	return types.FixEstimateLine(findings.Items)
}

// fixEstimatePromptSection is fixEstimatePromptLine as its own paragraph.
func fixEstimatePromptSection(raw string) string {
	return "\n\n" + fixEstimatePromptLine(raw)
}
