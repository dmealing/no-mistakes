package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIMode_DefaultsToLocalWhenKeyAbsent(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIMode != CIModeLocal {
		t.Fatalf("missing file ci_mode = %q, want %q", cfg.CIMode, CIModeLocal)
	}
	cfg, err = LoadGlobalFromBytes([]byte("log_level: info\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIMode != CIModeLocal {
		t.Fatalf("absent key ci_mode = %q, want %q", cfg.CIMode, CIModeLocal)
	}
	if merged := Merge(cfg, &RepoConfig{}); merged.CIMode != CIModeLocal {
		t.Fatalf("merged ci_mode = %q, want %q", merged.CIMode, CIModeLocal)
	}
}

func TestCIMode_ParsesGitHubAndRejectsUnknown(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("ci_mode: github\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIMode != CIModeGitHub {
		t.Fatalf("ci_mode = %q, want %q", cfg.CIMode, CIModeGitHub)
	}
	if merged := Merge(cfg, &RepoConfig{}); merged.CIMode != CIModeGitHub {
		t.Fatalf("merged ci_mode = %q, want %q", merged.CIMode, CIModeGitHub)
	}
	if _, err := LoadGlobalFromBytes([]byte("ci_mode: forge\n")); err == nil || !strings.Contains(err.Error(), "ci_mode") {
		t.Fatalf("unknown ci_mode error = %v, want ci_mode parse error", err)
	}
}

func TestCIMode_SkipSteps(t *testing.T) {
	got := CIModeLocal.SkipSteps([]types.StepName{types.StepReview, types.StepCI})
	if want := []types.StepName{types.StepReview, types.StepCI}; !reflect.DeepEqual(got, want) {
		t.Fatalf("local skips = %v, want %v (ci exactly once)", got, want)
	}
	if got := CIModeLocal.SkipSteps(nil); !reflect.DeepEqual(got, []types.StepName{types.StepCI}) {
		t.Fatalf("local skips from nil = %v, want [ci]", got)
	}
	if got := CIModeGitHub.SkipSteps([]types.StepName{types.StepReview}); !reflect.DeepEqual(got, []types.StepName{types.StepReview}) {
		t.Fatalf("github skips = %v, want [review] unchanged", got)
	}
	if got := CIMode("").SkipSteps(nil); !reflect.DeepEqual(got, []types.StepName{types.StepCI}) {
		t.Fatalf("unset mode skips = %v, want [ci] (local is the default)", got)
	}
	if got := CIModeGitHub.SkipSteps(nil); got != nil {
		t.Fatalf("github skips from nil = %v, want nil", got)
	}
}

func TestLaunchSkipSteps_ReadsGlobalConfigFromNMHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	if got := LaunchSkipSteps(nil); !reflect.DeepEqual(got, []types.StepName{types.StepCI}) {
		t.Fatalf("launch skips with no config = %v, want [ci]", got)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("ci_mode: github\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LaunchSkipSteps(nil); got != nil {
		t.Fatalf("launch skips in github mode = %v, want nil", got)
	}
}

func TestDefaultConfigYAML_DocumentsCIModeWithoutSettingIt(t *testing.T) {
	if !strings.Contains(defaultConfigYAML, "# ci_mode: local") {
		t.Fatal("default config template must document ci_mode")
	}
	cfg, err := LoadGlobalFromBytes([]byte(defaultConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIMode != CIModeLocal {
		t.Fatalf("template ci_mode = %q, want %q", cfg.CIMode, CIModeLocal)
	}
}
