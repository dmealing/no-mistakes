package triage

import (
	"context"
	"strings"
	"testing"
)

func TestReviewLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tier  Tier
		c     Change
		class ReviewClass
		want  string
	}{
		{"cheap with room", TierCode, Change{ProductionLines: 212}, ReviewCheap,
			"cheap - 212 of 300 production lines, no risk path; room for 88 more"},
		{"cheap at the limit", TierCode, Change{ProductionLines: 300}, ReviewCheap,
			"cheap - 300 of 300 production lines, no risk path; room for 0 more"},
		{"strong by size", TierCode, Change{ProductionLines: 412}, ReviewStrong,
			"strong - 412 production lines; room to 600"},
		{"strong by risk path", TierRisk, Change{ProductionLines: 140, RiskPaths: []string{"db/migrations/001.sql", "auth.go", "go.mod"}}, ReviewStrong,
			"strong - risk path db/migrations/001.sql (1 of 3); 140 production lines; room to 600"},
		{"past the strong bundle", TierCode, Change{ProductionLines: 700}, ReviewStrong,
			"strong - 700 production lines; past 600, room to 1,000"},
		{"over the bundle limit", TierCode, Change{ProductionLines: 1240}, ReviewStrong,
			"strong - 1,240 production lines; over the 1,000-line bundle limit"},
		// The repository's script relaxed the tier: the old wrapper sent only
		// CODE over 300 lines to the strong review, and so does this.
		{"relaxed tier stays cheap", TierTest, Change{ProductionLines: 412}, ReviewCheap,
			"cheap - TEST tier; 412 production lines"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewFor(tc.tier, tc.c); got != tc.class {
				t.Fatalf("ReviewFor = %s, want %s", got, tc.class)
			}
			if got := ReviewLine(tc.tier, tc.c); got != tc.want {
				t.Fatalf("ReviewLine =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func noteRules(notes []Note) string {
	var out []string
	for _, n := range notes {
		out = append(out, string(n.Rule))
	}
	return strings.Join(out, ",")
}

func TestNotes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      Change
		want   string
		detail string
	}{
		{"risk path is the minor part of a small change", Change{ProductionLines: 120, RiskPaths: []string{"go.mod"}, RiskLines: 4}, string(NoteRiskPathInSmallChange),
			"4 lines on a risk path move all 120 lines to the strong review: ship the risk-path part with other strong-review work, or alone."},
		{"risk path is the change", Change{ProductionLines: 120, RiskPaths: []string{"auth.go"}, RiskLines: 60}, "", ""},
		{"risk path in a large change", Change{ProductionLines: 301, RiskPaths: []string{"go.mod"}, RiskLines: 2}, "", ""},
		{"fix bundle just over the cheap limit", Change{ProductionLines: 340, FixCommits: 2}, string(NoteJustOverCheapLimit),
			"40 lines over the cheap-review limit: split into two bundles under 300, or move one fix out."},
		{"top of the band", Change{ProductionLines: 450, FixCommits: 5}, string(NoteJustOverCheapLimit), ""},
		{"past the band", Change{ProductionLines: 451, FixCommits: 5}, "", ""},
		{"one fix just over", Change{ProductionLines: 340, FixCommits: 1}, "", ""},
		{"at the cheap limit", Change{ProductionLines: 300, FixCommits: 4}, "", ""},
		{"just over with a risk path", Change{ProductionLines: 340, FixCommits: 3, RiskPaths: []string{"go.mod"}, RiskLines: 2}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			res := Result{Change: &c}
			notes := Notes(res)
			if got := noteRules(notes); got != tc.want {
				t.Fatalf("notes = %q, want %q", got, tc.want)
			}
			if tc.detail != "" && notes[0].Detail != tc.detail {
				t.Fatalf("detail =\n  %q\nwant\n  %q", notes[0].Detail, tc.detail)
			}
			// A note is advice: it never becomes a stop.
			if len(Evaluate(res)) != 0 {
				t.Fatalf("stops = %v, want none", Evaluate(res))
			}
		})
	}
}

func TestEvaluate_BundleOverLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Change
		stop bool
	}{
		{"fix bundle over the limit", Change{ProductionLines: 1240, FixCommits: 3}, true},
		{"large single feature", Change{ProductionLines: 1240, FixCommits: 2}, false},
		{"fix bundle at the limit", Change{ProductionLines: 1000, FixCommits: 6}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			res := Result{Change: &c}
			res.Stops = Evaluate(res)
			if !tc.stop {
				if len(res.Stops) != 0 {
					t.Fatalf("stops = %q, want none", stopRules(res))
				}
				return
			}
			if got := stopRules(res); got != string(RuleBundleOverLimit) {
				t.Fatalf("stops = %q, want only %s", got, RuleBundleOverLimit)
			}
			want := "fix bundle of 1,240 production lines: bundles over 1,000 cost 1.7 times a single change that size and 22% needed a third run; split by module"
			if res.Stops[0].Detail != want {
				t.Fatalf("detail =\n  %q\nwant\n  %q", res.Stops[0].Detail, want)
			}
			if !res.Overridable() {
				t.Fatal("bundle-over-limit must be overridable with a reason")
			}
		})
	}
}

// DiffChange reads the per-file counts the risk-path note needs and the fix
// commits the bundle rules need from git itself.
func TestDiffChange_CountsRiskLinesAndFixCommits(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	base := commit(t, repo, "base", map[string]string{"code.go": "package x\n"})
	commit(t, repo, "fix(widget): first", map[string]string{"widget.go": lines(100, "// w")})
	commit(t, repo, "Fix the gadget", map[string]string{"go.mod": lines(4, "// m")})
	commit(t, repo, "feat: add a thing", map[string]string{"thing.go": lines(16, "// t"), "thing_test.go": lines(50, "// t")})
	head := commit(t, repo, "fix: docs only", map[string]string{"README.md": "r\n"})

	c, err := DiffChange(context.Background(), repo, base, head, DefaultRiskPatterns)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProductionLines != 120 || c.RiskLines != 4 || c.FixCommits != 3 {
		t.Fatalf("production=%d risk=%d fixes=%d, want 120, 4, 3", c.ProductionLines, c.RiskLines, c.FixCommits)
	}
	if got := noteRules(Notes(Result{Change: &c})); got != string(NoteRiskPathInSmallChange) {
		t.Fatalf("notes = %q, want %s", got, NoteRiskPathInSmallChange)
	}
}
