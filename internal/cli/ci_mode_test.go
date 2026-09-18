package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func setCIModeHome(t *testing.T, mode config.CIMode) {
	t.Helper()
	home := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", home)
	if mode == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("ci_mode: "+string(mode)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The post-receive hook runs `no-mistakes daemon notify-push`, so a plain
// `git push` to the gate reaches the daemon through this request.
func TestNotifyPushRequestsCISkipUnderDefaultLocalMode(t *testing.T) {
	for _, tc := range []struct {
		mode config.CIMode
		want []types.StepName
	}{
		{mode: "", want: []types.StepName{types.StepCI}},
		{mode: config.CIModeGitHub, want: nil},
	} {
		t.Run("mode="+string(tc.mode), func(t *testing.T) {
			setCIModeHome(t, tc.mode)
			p := paths.WithRoot(os.Getenv("NM_HOME"))
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			requests := make(chan ipc.PushReceivedParams, 1)
			srv.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.PushReceivedParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				requests <- params
				return &ipc.PushReceivedResult{RunID: "run-1"}, nil
			})
			done := make(chan error, 1)
			go func() { done <- srv.Serve(p.Socket()) }()
			t.Cleanup(func() { srv.Close(); <-done })
			deadline := time.Now().Add(3 * time.Second)
			for alive, _ := daemon.IsRunning(p); !alive; alive, _ = daemon.IsRunning(p) {
				if time.Now().After(deadline) {
					t.Fatal("test IPC server did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}

			cmd := newDaemonNotifyPushCmd()
			cmd.SetArgs([]string{"--gate", t.TempDir(), "--ref", "refs/heads/feature", "--old", strings.Repeat("0", 40), "--new", strings.Repeat("a", 40)})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := (<-requests).SkipSteps; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("skip_steps = %v, want %v", got, tc.want)
			}
		})
	}
}

func renderCompletedCIRun(t *testing.T, reason string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	prURL := "https://github.com/example/repo/pull/7"
	err := renderDriveResult(cmd, &ipc.RunInfo{
		ID: "local-ci-run", Status: types.RunCompleted, HeadSHA: strings.Repeat("b", 40), PRURL: &prURL,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepPR, Status: types.StepStatusCompleted},
			{StepName: types.StepCI, Status: types.StepStatusSkipped, SkipReason: reason},
		},
	}, false)
	return out.String(), err
}

func TestAxiCompletedRunExplainsLocalCISkip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		// Recorded by a daemon that knows ci_mode.
		{name: "recorded reason", reason: config.LocalCISkipReason},
		// A daemon started before ci_mode existed records no reason.
		{name: "legacy daemon", reason: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCIModeHome(t, "")
			out, err := renderCompletedCIRun(t, tc.reason)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Outcome string `toon:"outcome"`
				Run     struct {
					LocalCI        string             `toon:"local_ci"`
					AutomaticSkips []automaticSkipRow `toon:"automatic_skips"`
				} `toon:"run"`
				Help []string `toon:"help"`
			}
			if err := toon.UnmarshalString(out, &doc); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if doc.Outcome != "passed" {
				t.Fatalf("outcome = %q, want passed:\n%s", doc.Outcome, out)
			}
			if doc.Run.LocalCI != config.LocalCISkipReason {
				t.Fatalf("run.local_ci = %q, want %q:\n%s", doc.Run.LocalCI, config.LocalCISkipReason, out)
			}
			if len(doc.Run.AutomaticSkips) != 0 {
				t.Fatalf("local CI skip must not read as missing evidence: %+v", doc.Run.AutomaticSkips)
			}
			help := strings.Join(doc.Help, "\n")
			if !strings.Contains(help, "CI is local") {
				t.Fatalf("help does not explain the local CI skip:\n%s", help)
			}
			for _, forbidden := range []string{"wait for checks", "monitor checks", "CI verification did not run", "checks passed"} {
				if strings.Contains(help, forbidden) {
					t.Fatalf("help mentions %q:\n%s", forbidden, help)
				}
			}
		})
	}
}

func TestAxiGitHubModeKeepsUnexplainedCISkipUnlabelled(t *testing.T) {
	setCIModeHome(t, config.CIModeGitHub)
	out, err := renderCompletedCIRun(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "local_ci") || strings.Contains(out, "CI is local") {
		t.Fatalf("github mode labelled an explicit skip as local CI:\n%s", out)
	}
}
