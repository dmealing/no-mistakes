package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestTestStep_PromptsStopOnlyTheAgentsOwnProcesses pins the local fork's
// process rule: one run's Test agent stopped "its" suite with
// `pkill -f "bun test"` and then `kill` on pgrep output, and killed another
// run's `bun test --shard` processes. Both Test turns must forbid kills that
// select by name or pattern and give the process-group command instead.
func TestTestStep_PromptsStopOnlyTheAgentsOwnProcesses(t *testing.T) {
	fixPrompt, evidencePrompt := testStepPromptsForCIMode(t, config.CIModeLocal)
	for name, prompt := range map[string]string{"fix": fixPrompt, "evidence": evidencePrompt} {
		for _, want := range []string{
			"Stop only processes you started, by their PID or process group",
			"Never select processes to stop by name or pattern (pkill, killall, or kill on PIDs from pgrep, pidof, or ps | grep)",
			"`setsid <command> > <log> 2>&1 & echo $! > <pidfile>`",
			"`kill -TERM -\"$(cat <pidfile>)\"`",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt missing %q:\n%s", name, want, prompt)
			}
		}
	}
}
