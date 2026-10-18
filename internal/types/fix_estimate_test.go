package types

import (
	"encoding/json"
	"testing"
)

func TestFixEstimateLine(t *testing.T) {
	cases := []struct {
		name  string
		items []Finding
		want  string
	}{
		{"no findings", nil, "Fix complexity: complex; estimated size: 0 lines across 0 files"},
		{"all simple", []Finding{
			{FixComplexity: "simple", FixLines: 1, FixFiles: 1},
			{FixComplexity: " Simple ", FixLines: 3, FixFiles: 1},
		}, "Fix complexity: simple; estimated size: 4 lines across 2 files"},
		{"max wins", []Finding{
			{FixComplexity: "simple", FixLines: 2, FixFiles: 1},
			{FixComplexity: "moderate", FixLines: 10, FixFiles: 2},
		}, "Fix complexity: moderate; estimated size: 12 lines across 3 files"},
		{"missing estimate counts as complex", []Finding{
			{FixComplexity: "simple", FixLines: 2, FixFiles: 1},
			{Severity: "info"},
		}, "Fix complexity: complex; estimated size: 2 lines across 1 files"},
		{"unknown value counts as complex, negatives ignored", []Finding{
			{FixComplexity: "trivial", FixLines: -5, FixFiles: -1},
		}, "Fix complexity: complex; estimated size: 0 lines across 0 files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FixEstimateLine(tc.items); got != tc.want {
				t.Fatalf("FixEstimateLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// Finding.UnmarshalJSON copies fields explicitly, so a new field must survive
// a round trip or it drops on every parse.
func TestFindingRoundTripKeepsFixEstimateAndInlineFields(t *testing.T) {
	in := Finding{ID: "r1", Severity: "info", Description: "d", Action: ActionNoOp, FixComplexity: "simple", FixLines: 3, FixFiles: 1, AppliedInline: true}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Finding
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip lost fields: got %+v, want %+v", out, in)
	}
}

func TestHasIntentConformanceFinding(t *testing.T) {
	if HasIntentConformanceFinding([]Finding{{Category: FindingCategoryLint}}) {
		t.Fatal("non-conformance category must not match")
	}
	if !HasIntentConformanceFinding([]Finding{{}, {Category: FindingCategoryIntentConformance}}) {
		t.Fatal("conformance category must match")
	}
}
