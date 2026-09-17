package daemon

import (
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// applyCIMode enforces the configured CI mode on an executor. Local mode skips
// the CI step on every run the daemon starts or recovers, whatever the launch
// path or skip list, and records why.
func applyCIMode(executor *pipeline.Executor, mode config.CIMode) {
	if mode.Local() {
		executor.SkipStepWithReason(types.StepCI, config.LocalCISkipReason)
	}
}
