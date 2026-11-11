package triage

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Lane is another branch of the same project that is in validation now: it has
// a pending or running run, parked at a gate or not.
type Lane struct {
	RunID   string
	RepoID  string
	Branch  string
	HeadSHA string
	Parked  bool
	// GateDir is the gate repository holding the lane's commits.
	GateDir string
}

// ProjectKey reduces a remote URL to host/path, so the SSH and HTTPS spellings
// of one upstream (and clones registered from different checkouts) name one
// project. An empty or unparseable URL yields "".
func ProjectKey(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" {
		return ""
	}
	var host, path string
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		host, path = u.Hostname(), u.Path
	} else if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[at:], ":") && !strings.Contains(s, "://") {
		// scp-like: user@host:owner/repo.git
		rest := s[at+1:]
		host, path, _ = strings.Cut(rest, ":")
	} else {
		// A local path: the path itself is the identity.
		return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + strings.ToLower(path)
}

// ShortBranch strips refs/heads/ so stored and checked-out names compare.
func ShortBranch(branch string) string { return strings.TrimPrefix(branch, "refs/heads/") }

// projectRepos returns every registered repository of the project key (the
// same upstream may be registered from more than one checkout). An empty key
// matches only fallback itself.
func projectRepos(d *db.DB, key string, fallback *db.Repo) ([]*db.Repo, error) {
	if key == "" {
		if fallback == nil {
			return nil, nil
		}
		return []*db.Repo{fallback}, nil
	}
	repos, err := d.GetRepos()
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	var out []*db.Repo
	for _, r := range repos {
		if ProjectKey(r.UpstreamURL) == key {
			out = append(out, r)
		}
	}
	if len(out) == 0 && fallback != nil {
		out = append(out, fallback)
	}
	return out, nil
}

// LanesInValidation lists the other branches of the project that are in
// validation now, newest run first, one entry per branch. branch (this
// branch) is excluded wherever it is registered.
func LanesInValidation(d *db.DB, p *paths.Paths, key string, repo *db.Repo, branch string) ([]Lane, error) {
	repos, err := projectRepos(d, key, repo)
	if err != nil || len(repos) == 0 {
		return nil, err
	}
	byID := make(map[string]*db.Repo, len(repos))
	for _, r := range repos {
		byID[r.ID] = r
	}
	runs, err := d.GetActiveRuns()
	if err != nil {
		return nil, fmt.Errorf("list runs in validation: %w", err)
	}
	me := ShortBranch(branch)
	seen := map[string]bool{}
	var lanes []Lane
	for _, run := range runs {
		if _, ok := byID[run.RepoID]; !ok {
			continue
		}
		b := ShortBranch(run.Branch)
		if b == me || seen[b] {
			continue
		}
		seen[b] = true
		lanes = append(lanes, Lane{
			RunID:   run.ID,
			RepoID:  run.RepoID,
			Branch:  b,
			HeadSHA: run.HeadSHA,
			Parked:  run.AwaitingAgentSince != nil,
			GateDir: p.RepoDir(run.RepoID),
		})
	}
	return lanes, nil
}
