package cli

import (
	"strings"
	"testing"
)

// This local fork build refuses self-update whatever the flags, so an agent
// obeying an upstream release cannot replace it and restore forge check waiting.
func TestUpdateCommandRefusesLocalForkBuild(t *testing.T) {
	for _, args := range [][]string{{"update"}, {"update", "--beta"}, {"update", "-y"}, {"update", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateUpdateCommand(t)

			out, err := executeCmd(args...)
			if err == nil {
				t.Fatalf("%v succeeded, want local fork refusal\noutput: %s", args, out)
			}
			if msg := err.Error(); strings.Contains(msg, "\n") || !strings.Contains(msg, "local fork build") {
				t.Fatalf("%v error = %q, want one-line local fork refusal", args, msg)
			}
		})
	}
}

func isolateUpdateCommand(t *testing.T) {
	t.Helper()
	t.Setenv("NM_HOME", t.TempDir())
}
