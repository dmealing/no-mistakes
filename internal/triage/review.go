package triage

import (
	"fmt"
	"strconv"
)

// ReviewClass is which first review a change gets: the cheap one, or the
// strong one that costs about three times as much at the same size.
type ReviewClass string

const (
	ReviewCheap  ReviewClass = "cheap"
	ReviewStrong ReviewClass = "strong"
)

// ReviewFor is the review class of a change whose effective tier (after the
// repository's own script and the risk-path lift) is tier: strong on any risk
// path, or for CODE over SmallChangeLines production lines; cheap otherwise.
// It is the one owner of that rule; the agent wrapper reads it from the
// `review:` line of `axi triage`.
func ReviewFor(tier Tier, c Change) ReviewClass {
	if tier == TierRisk || len(c.RiskPaths) > 0 {
		return ReviewStrong
	}
	if tier == TierCode && c.ProductionLines > SmallChangeLines {
		return ReviewStrong
	}
	return ReviewCheap
}

// ReviewLine is the `review:` value of a triage report: the class, why, and
// how much room the change leaves before the next limit.
func ReviewLine(tier Tier, c Change) string {
	lines := c.ProductionLines
	if ReviewFor(tier, c) == ReviewCheap {
		if lines > SmallChangeLines {
			return fmt.Sprintf("cheap - %s tier; %s production lines", tier, groupDigits(lines))
		}
		return fmt.Sprintf("cheap - %s of %d production lines, no risk path; room for %s more", groupDigits(lines), SmallChangeLines, groupDigits(SmallChangeLines-lines))
	}
	why := ""
	if len(c.RiskPaths) > 0 {
		why = fmt.Sprintf("risk path %s (1 of %d); ", c.RiskPaths[0], len(c.RiskPaths))
	}
	size := groupDigits(lines) + " production lines"
	switch {
	case lines > MaxBundleLines:
		return fmt.Sprintf("strong - %s%s; over the %s-line bundle limit", why, size, groupDigits(MaxBundleLines))
	case lines > StrongBundleLines:
		return fmt.Sprintf("strong - %s%s; past %s, room to %s", why, size, groupDigits(StrongBundleLines), groupDigits(MaxBundleLines))
	default:
		return fmt.Sprintf("strong - %s%s; room to %s", why, size, groupDigits(StrongBundleLines))
	}
}

// groupDigits writes n with thousands separators: 1240 -> "1,240".
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	neg := ""
	if n < 0 {
		neg, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return neg + s
}
