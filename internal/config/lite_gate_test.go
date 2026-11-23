package config

import (
	"strings"
	"testing"
)

// The three lite-gate cost switches are on when absent and each one is
// turned off by setting its key.
func TestMerge_LiteGateSwitches(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		source        string
		roundCap      int
		coverageFirst bool
		skipInternal  bool
	}{
		"defaults on":            {source: "log_level: info\n", roundCap: DefaultReviewRoundCap, coverageFirst: true, skipInternal: true},
		"round cap off":          {source: "review:\n  round_cap: 0\n", roundCap: 0, coverageFirst: true, skipInternal: true},
		"round cap raised":       {source: "review:\n  round_cap: 4\n", roundCap: 4, coverageFirst: true, skipInternal: true},
		"coverage every round":   {source: "review:\n  coverage_first_round_only: false\n", roundCap: DefaultReviewRoundCap, coverageFirst: false, skipInternal: true},
		"document always runs":   {source: "document:\n  skip_internal: false\n", roundCap: DefaultReviewRoundCap, coverageFirst: true, skipInternal: false},
		"all explicitly enabled": {source: "review:\n  round_cap: 2\n  coverage_first_round_only: true\ndocument:\n  skip_internal: true\n", roundCap: 2, coverageFirst: true, skipInternal: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			global, err := LoadGlobalFromBytes([]byte(tc.source))
			if err != nil {
				t.Fatalf("LoadGlobalFromBytes() = %v", err)
			}
			cfg := MergeForRemote(global, &RepoConfig{}, "https://github.com/acme/widget.git")
			if cfg.Review.RoundCap != tc.roundCap || cfg.Review.CoverageFirstRoundOnly != tc.coverageFirst || cfg.Document.SkipInternal != tc.skipInternal {
				t.Fatalf("got round_cap=%d coverage_first_round_only=%v skip_internal=%v, want %d %v %v",
					cfg.Review.RoundCap, cfg.Review.CoverageFirstRoundOnly, cfg.Document.SkipInternal, tc.roundCap, tc.coverageFirst, tc.skipInternal)
			}
		})
	}
}

func TestLoadGlobal_RejectsNegativeReviewRoundCap(t *testing.T) {
	t.Parallel()
	if _, err := LoadGlobalFromBytes([]byte("review:\n  round_cap: -1\n")); err == nil || !strings.Contains(err.Error(), "round_cap") {
		t.Fatalf("LoadGlobalFromBytes() = %v, want a rejection naming round_cap", err)
	}
}

// The switches are global-only: a repository_overrides entry cannot set them.
func TestLoadGlobal_RepositoryOverrideRejectsLiteGateSwitches(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"review:\n      round_cap: 5\n", "review:\n      coverage_first_round_only: false\n", "document:\n      skip_internal: false\n"} {
		source := "repository_overrides:\n  https://github.com/acme/widget.git:\n    " + block
		if _, err := LoadGlobalFromBytes([]byte(source)); err == nil {
			t.Fatalf("LoadGlobalFromBytes(%q) accepted a global-only switch", source)
		}
	}
}

// A repository's own .no-mistakes.yaml cannot change the operator's switches.
func TestMerge_RepoConfigCannotSetLiteGateSwitches(t *testing.T) {
	t.Parallel()
	repo, err := LoadRepoFromBytes([]byte("review:\n  round_cap: 9\n  coverage_first_round_only: false\ndocument:\n  skip_internal: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobalFromBytes([]byte("log_level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := MergeForRemote(global, repo, "https://github.com/acme/widget.git")
	if cfg.Review.RoundCap != DefaultReviewRoundCap || !cfg.Review.CoverageFirstRoundOnly || !cfg.Document.SkipInternal {
		t.Fatalf("a repository config changed an operator lite-gate switch: %+v %+v", cfg.Review, cfg.Document)
	}
}
