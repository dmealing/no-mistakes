package types

import (
	"fmt"
	"strings"
)

// Fix complexity constants: the reviewer's per-finding judgment of how much
// reasoning a fix needs. Severity says how much a finding matters; complexity
// says how hard it is to fix, and an urgent finding can be a one-line fix
// while a minor one can need a cross-file redesign.
const (
	FixComplexitySimple   = "simple"
	FixComplexityModerate = "moderate"
	FixComplexityComplex  = "complex"
)

// FixEstimatePrefix starts the one stable line the review fix prompt carries
// so an agent wrapper reading the prompt can route the fix round by it.
const FixEstimatePrefix = "Fix complexity: "

// NormalizeFixComplexity returns the known complexity spelled by value, or ""
// when value is empty or not part of the vocabulary.
func NormalizeFixComplexity(value string) string {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case FixComplexitySimple, FixComplexityModerate, FixComplexityComplex:
		return v
	default:
		return ""
	}
}

func fixComplexityRank(value string) int {
	switch value {
	case FixComplexitySimple:
		return 0
	case FixComplexityModerate:
		return 1
	default:
		return 2
	}
}

// FixEstimateLine summarizes the findings a fix round is handed as exactly
// one line:
//
//	Fix complexity: <simple|moderate|complex>; estimated size: <N> lines across <M> files
//
// Complexity is the highest among the findings; a finding with no recognized
// estimate (an older payload, a user-added finding) counts as complex, so a
// missing estimate routes toward the stronger model rather than the cheaper
// one. Size is the sum of the per-finding estimates; negative values are
// ignored. With no findings at all it reports complex, because nothing was
// estimated.
func FixEstimateLine(items []Finding) string {
	complexity := FixComplexitySimple
	if len(items) == 0 {
		complexity = FixComplexityComplex
	}
	lines, files := 0, 0
	for _, item := range items {
		c := NormalizeFixComplexity(item.FixComplexity)
		if c == "" {
			c = FixComplexityComplex
		}
		if fixComplexityRank(c) > fixComplexityRank(complexity) {
			complexity = c
		}
		if item.FixLines > 0 {
			lines += item.FixLines
		}
		if item.FixFiles > 0 {
			files += item.FixFiles
		}
	}
	return fmt.Sprintf("%s%s; estimated size: %d lines across %d files", FixEstimatePrefix, complexity, lines, files)
}
