package config

import (
	"strings"
	"testing"
)

func TestMerge_ReviewInlineFixes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		source string
		want   bool
	}{
		"default on": {source: "log_level: info\n", want: true},
		"global off": {source: "review:\n  inline_fixes: false\n", want: false},
		"global on":  {source: "review:\n  inline_fixes: true\n", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			global, err := LoadGlobalFromBytes([]byte(tc.source))
			if err != nil {
				t.Fatalf("LoadGlobalFromBytes() = %v", err)
			}
			if got := MergeForRemote(global, &RepoConfig{}, "https://github.com/acme/widget.git").Review.InlineFixes; got != tc.want {
				t.Fatalf("InlineFixes = %v, want %v", got, tc.want)
			}
		})
	}
}

// inline_fixes is global-only: a repository_overrides entry cannot set it, and
// the strict decoder rejects the key there like any other unaccepted field.
func TestLoadGlobal_RepositoryOverrideRejectsReviewInlineFixes(t *testing.T) {
	t.Parallel()
	source := "repository_overrides:\n  https://github.com/acme/widget.git:\n    review:\n      inline_fixes: false\n"
	if _, err := LoadGlobalFromBytes([]byte(source)); err == nil || !strings.Contains(err.Error(), "inline_fixes") {
		t.Fatalf("LoadGlobalFromBytes() = %v, want a rejection naming inline_fixes", err)
	}
}

// inline_fixes is an operator setting: a repository's own .no-mistakes.yaml
// cannot turn it on or off.
func TestMerge_RepoConfigCannotSetReviewInlineFixes(t *testing.T) {
	t.Parallel()
	repo, err := LoadRepoFromBytes([]byte("review:\n  inline_fixes: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	global, err := LoadGlobalFromBytes([]byte("log_level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !MergeForRemote(global, repo, "https://github.com/acme/widget.git").Review.InlineFixes {
		t.Fatal("a repository config turned off the operator's inline_fixes setting")
	}
}
