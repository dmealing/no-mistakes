package triage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchGlobFollowsShellCaseSemantics(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.md", "docs/guide/intro.md", true}, // * crosses /
		{"docs/*", "docs/a/b.txt", true},
		{"docs/*", "src/docs/a.txt", false},
		{"*.test.[jt]s", "web/app.test.ts", true},
		{"*.test.[jt]s", "web/app.test.cs", false},
		{"*[Aa]uth*", "internal/oauth/token.go", true},
		{"[!a]bc", "xbc", true},
		{"[!a]bc", "abc", false},
		{"file?.go", "file1.go", true},
		{"a.b", "axb", false}, // . is literal
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestClassifyStrictestFileWins(t *testing.T) {
	risk := DefaultRiskPatterns
	cases := []struct {
		files []string
		want  Tier
	}{
		{[]string{"README.md", "docs/x.txt"}, TierDocs},
		{[]string{"README.md", "internal/x/x_test.go"}, TierTest},
		{[]string{"internal/x/x_test.go", "internal/x/x.go"}, TierCode},
		{[]string{"internal/x/x.go", "internal/auth/token.go"}, TierRisk},
		{[]string{"internal/auth/token_test.go"}, TierTest}, // a test on a risk path is still a test
	}
	for _, c := range cases {
		if got := Classify(c.files, risk); got != c.want {
			t.Errorf("Classify(%v) = %s, want %s", c.files, got, c.want)
		}
	}
	if SkipFor(TierDocs) != "test,lint,document" || SkipFor(TierTest) != "document" || SkipFor(TierCode) != "" || SkipFor(TierRisk) != "" {
		t.Fatal("SkipFor changed")
	}
}

func TestReadHold(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if h, err := ReadHold(root, now); h != nil || err != nil {
		t.Fatalf("no hold file: got %v, %v", h, err)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(HoldPath(root), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("who: installer\nwhy: install v2\nuntil: the install finishes\n")
	h, _ := ReadHold(root, now)
	if h == nil || h.Who != "installer" || h.Why != "install v2" || !strings.Contains(h.String(), "until: the install finishes") {
		t.Fatalf("hold = %+v", h)
	}
	write("until: 2026-11-01T11:00:00Z\n")
	if h, _ := ReadHold(root, now); h != nil {
		t.Fatalf("an expired hold is still active: %+v", h)
	}
	write("until: 2026-11-01T13:00:00Z\n")
	if h, _ := ReadHold(root, now); h == nil {
		t.Fatal("a hold until a future time is not active")
	}
	write("garbage\n")
	if h, _ := ReadHold(root, now); h == nil || h.String() != "garbage" {
		t.Fatalf("an unparseable hold file must still hold: %+v", h)
	}
}

func TestProjectKeyJoinsSpellingsOfOneUpstream(t *testing.T) {
	want := "github.com/owner/repo"
	for _, u := range []string{
		"git@github.com:Owner/Repo.git",
		"https://github.com/owner/repo",
		"https://github.com/owner/repo.git/",
		"ssh://git@github.com/owner/repo.git",
	} {
		if got := ProjectKey(u); got != want {
			t.Errorf("ProjectKey(%q) = %q, want %q", u, got, want)
		}
	}
	if ProjectKey("") != "" {
		t.Error("empty URL must yield no key")
	}
	if got := ProjectKey("/srv/git/repo.git"); got != "/srv/git/repo" {
		t.Errorf("local path key = %q", got)
	}
	if got := GitHubSlug("git@github.com:Owner/Repo.git"); got != "Owner/Repo" {
		t.Errorf("GitHubSlug = %q", got)
	}
}

func TestRecordOverrideAppends(t *testing.T) {
	root := t.TempDir()
	for _, reason := range []string{"urgent fix", "nothing to bundle with"} {
		if err := RecordOverride(root, OverrideRecord{Branch: "b", HeadSHA: "abc", Rules: []string{string(RuleThirdBranch)}, Reason: reason}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, OverrideLogName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"reason":"urgent fix"`) || !strings.Contains(lines[1], `"rules":["third-branch"]`) {
		t.Fatalf("override log = %q", data)
	}
}
