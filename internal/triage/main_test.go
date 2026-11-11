package triage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Fixtures must not read the machine's system or global git config (a
	// commit.gpgsign=true there makes every fixture commit ask gpg to sign).
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir, err := os.MkdirTemp("", "no-mistakes-triage-tests-")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	identity := "[core]\n\tautocrlf = false\n[user]\n\temail = test@example.com\n\tname = Test User\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(identity), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
