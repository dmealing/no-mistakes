package update

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalForkBuildIsWiredIntoDefaultUpdater(t *testing.T) {
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !u.localFork {
		t.Fatal("default updater must know this is a local fork build")
	}
}

func TestLocalForkBuildRefusesUpdate(t *testing.T) {
	stdout := new(bytes.Buffer)
	u := &updater{
		appName:        "no-mistakes",
		currentVersion: "v1.72.0-local.1",
		manifestURL:    "http://127.0.0.1:1/unreachable",
		stdout:         stdout,
		localFork:      true,
	}
	err := u.run(context.Background())
	if err == nil {
		t.Fatal("update of a local fork build must refuse")
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") || !strings.Contains(msg, "local fork build") || !strings.Contains(msg, "local branch") {
		t.Fatalf("refusal = %q, want one line naming the local fork build and its local branch", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("refusal wrote stdout: %q", stdout.String())
	}
}

func TestLocalForkBuildSuppressesUpdateBanner(t *testing.T) {
	t.Setenv(noUpdateCheckEnv, "")
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := writeCache(cachePath, &checkCache{CheckedAt: time.Now().Add(-48 * time.Hour), LatestVersion: "v1.77.0"}); err != nil {
		t.Fatal(err)
	}
	stderr := new(bytes.Buffer)
	spawned := false
	u := &updater{
		appName:         "no-mistakes",
		currentVersion:  "v1.72.0-local.1",
		cachePath:       cachePath,
		stderr:          stderr,
		now:             time.Now,
		spawnBackground: func(string) error { spawned = true; return nil },
		localFork:       true,
	}
	u.maybeNotifyAndCheck([]string{"status"})
	if stderr.Len() != 0 {
		t.Fatalf("local fork build printed an update banner: %q", stderr.String())
	}
	if spawned {
		t.Fatal("local fork build spawned a background update check")
	}
	if got := u.cachedLatestVersion(); got != "" {
		t.Fatalf("cachedLatestVersion() = %q, want empty for a local fork build", got)
	}
}
