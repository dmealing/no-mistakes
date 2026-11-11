package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiCorrectIntentRefusesOtherInputsBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--action", "correct-intent"}, "requires --intent"},
		{[]string{"--action", "correct-intent", "--intent", "x", "--yes"}, "takes only the corrected intent"},
		{[]string{"--action", "correct-intent", "--intent", "x", "--findings", "r1"}, "takes only the corrected intent"},
		{[]string{"--action", "correct-intent", "--intent", "x", "--reason", "why"}, "takes only the corrected intent"},
		{[]string{"--action", "correct-intent", "--intent", "   "}, "must not be empty"},
		{[]string{"--action", "approve", "--intent", "x"}, "apply only to --action correct-intent"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("NM_HOME", t.TempDir())
			chdir(t, t.TempDir())
			out, err := executeCmd(append([]string{"axi", "respond"}, tc.args...)...)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("err=%v output=%s, want a refusal containing %q", err, out, tc.want)
			}
		})
	}
}

func TestAxiCorrectIntentSendsTheCorrectedIntentToTheReview(t *testing.T) {
	var got map[string]any
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, err
		}
		responded.Store(true)
		return &ipc.RespondResult{OK: true}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		run := fx.awaiting()
		if err := json.Unmarshal([]byte(`{"id":"review-step","step_name":"review","findings_json":"{\"findings\":[]}"}`), &run.Steps[0]); err != nil {
			return nil, err
		}
		return run, nil
	})
	intentFile := filepath.Join(t.TempDir(), "intent.txt")
	corrected := "Do X, and also Y, which the first intent left out.\n"
	if err := os.WriteFile(intentFile, []byte(corrected), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "respond", "--step", "review", "--action", "correct-intent", "--intent-file", intentFile, "--wait", "3s")
	if err != nil {
		t.Fatalf("correct-intent response failed: %v\n%s", err, out)
	}
	if got["action"] != "correct-intent" || got["corrected_intent"] != corrected || got["step"] != "review" {
		t.Fatalf("correct-intent response = %+v, want the corrected intent for the review step", got)
	}
	if got["finding_ids"] != nil || got["ignore_finding_ids"] != nil || got["added_findings"] != nil || got["coverage"] != nil {
		t.Fatalf("correct-intent response manufactured a finding decision: %+v", got)
	}
}
