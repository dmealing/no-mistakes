package config

import "testing"

func TestMerge_ReviewInlineFixes(t *testing.T) {
	t.Parallel()

	const remote = "https://github.com/acme/widget.git"
	for name, tc := range map[string]struct {
		source string
		want   bool
		other  bool
	}{
		"default on":        {source: "log_level: info\n", want: true, other: true},
		"global off":        {source: "review:\n  inline_fixes: false\n", want: false, other: false},
		"override off wins": {source: "repository_overrides:\n  " + remote + ":\n    review:\n      inline_fixes: false\n", want: false, other: true},
		"override on wins":  {source: "review:\n  inline_fixes: false\nrepository_overrides:\n  " + remote + ":\n    review:\n      inline_fixes: true\n", want: true, other: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			global, err := LoadGlobalFromBytes([]byte(tc.source))
			if err != nil {
				t.Fatalf("LoadGlobalFromBytes() = %v", err)
			}
			if got := MergeForRemote(global, &RepoConfig{}, remote).Review.InlineFixes; got != tc.want {
				t.Fatalf("InlineFixes = %v, want %v", got, tc.want)
			}
			if got := MergeForRemote(global, &RepoConfig{}, "https://github.com/acme/other.git").Review.InlineFixes; got != tc.other {
				t.Fatalf("non-matching remote InlineFixes = %v, want %v", got, tc.other)
			}
		})
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
