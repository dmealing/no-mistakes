package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CIMode selects whether the CI step monitors forge checks. It is a
// global-only, machine-local choice: this fork build defaults to local because
// the forge runs no CI (GitHub Actions is off), so a CI step that waits for
// forge checks would wait for checks that never arrive.
type CIMode string

const (
	// CIModeLocal skips the CI step on every run, whatever the launch path.
	// Validation is the local pipeline (review, test, lint); pull requests are
	// still opened on the forge.
	CIModeLocal CIMode = "local"
	// CIModeGitHub restores upstream forge check monitoring unchanged.
	CIModeGitHub CIMode = "github"

	DefaultCIMode = CIModeLocal

	// LocalCISkipReason is recorded on the CI step a local-mode run skips, so
	// status output says why the step did not run.
	LocalCISkipReason = "CI is local (ci_mode: local): forge checks are not monitored"
)

func parseCIMode(value string) (CIMode, error) {
	switch mode := CIMode(strings.TrimSpace(value)); mode {
	case "":
		return DefaultCIMode, nil
	case CIModeLocal, CIModeGitHub:
		return mode, nil
	default:
		return "", fmt.Errorf("parse ci_mode %q: want %q or %q", value, CIModeLocal, CIModeGitHub)
	}
}

// Local reports whether CI is local. Only an explicit github mode monitors
// forge checks, so an unset mode keeps the documented local default.
func (m CIMode) Local() bool {
	return m != CIModeGitHub
}

// SkipSteps returns skips plus the steps this mode always skips.
func (m CIMode) SkipSteps(skips []types.StepName) []types.StepName {
	if !m.Local() || slices.Contains(skips, types.StepCI) {
		return skips
	}
	return append(slices.Clone(skips), types.StepCI)
}

// ConfiguredCIMode reads the CI mode from the global config under NM_HOME.
// An unreadable config falls back to the default; the daemon reports the
// config error itself when it creates the run.
func ConfiguredCIMode() CIMode {
	if p, err := paths.New(); err == nil {
		if cfg, err := LoadGlobal(p.ConfigFile()); err == nil {
			return cfg.CIMode
		}
	}
	return DefaultCIMode
}

// LaunchSkipSteps applies the machine's configured CI mode to a run-launch
// request's skip list. Launch clients call it so local mode takes effect even
// against a daemon started before ci_mode existed; the daemon applies the same
// mode again at run creation.
func LaunchSkipSteps(skips []types.StepName) []types.StepName {
	return ConfiguredCIMode().SkipSteps(skips)
}
