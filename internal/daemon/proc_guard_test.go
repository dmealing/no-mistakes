package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procguard"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestProcGuardEnvironmentMarksTheRunAndPutsTheGuardFirstOnPath(t *testing.T) {
	if !procguard.Supported() {
		t.Skip("the process guard is installed on Linux only")
	}
	p := paths.WithRoot(t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")

	got := procGuardEnvironment(p, "01RUN")

	if got.Set[procguard.MarkerEnv] != "01RUN" {
		t.Fatalf("%s = %q, want the run ID", procguard.MarkerEnv, got.Set[procguard.MarkerEnv])
	}
	dir := filepath.Join(p.Root(), "proc-guard", "bin")
	if want := dir + string(os.PathListSeparator) + "/usr/bin:/bin"; got.Set["PATH"] != want {
		t.Fatalf("PATH = %q, want %q", got.Set["PATH"], want)
	}
	for _, tool := range procguard.Tools {
		if info, err := os.Stat(filepath.Join(dir, tool)); err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("guard wrapper %s missing or not executable: %v", tool, err)
		}
	}
}

func TestProcGuardEnvironmentNeedsARun(t *testing.T) {
	if got := procGuardEnvironment(paths.WithRoot(t.TempDir()), ""); !got.Empty() {
		t.Fatalf("overlay without a run = %v, want empty", got)
	}
}

func TestWithProcGuardWinsOverTheBaseOverlay(t *testing.T) {
	base := runenv.Overlay{Set: map[string]string{"PATH": "/profile/bin", "GH_CONFIG_DIR": "/profiles/a"}, Unset: []string{"GH_TOKEN"}}
	guard := runenv.Overlay{Set: map[string]string{"PATH": "/guard:/usr/bin", procguard.MarkerEnv: "01RUN"}}

	got := withProcGuard(base, guard)

	if got.Set["PATH"] != "/guard:/usr/bin" || got.Set[procguard.MarkerEnv] != "01RUN" {
		t.Fatalf("guard lost to the base overlay: %v", got.Set)
	}
	if got.Set["GH_CONFIG_DIR"] != "/profiles/a" || len(got.Unset) != 1 {
		t.Fatalf("base overlay not kept: %v", got)
	}
	if base.Set["PATH"] != "/profile/bin" {
		t.Fatal("withProcGuard mutated its base")
	}
	if same := withProcGuard(base, runenv.Overlay{}); same.Set["PATH"] != "/profile/bin" {
		t.Fatalf("an empty guard changed the base: %v", same)
	}
}

// TestPipelineAgentProcessSeesTheProcGuard is the production wiring: the agent
// process newPipelineAgent spawns carries the run marker, and pkill resolves
// to the guard's wrapper.
func TestPipelineAgentProcessSeesTheProcGuard(t *testing.T) {
	if !procguard.Supported() {
		t.Skip("the process guard is installed on Linux only")
	}
	p := paths.WithRoot(t.TempDir())
	dir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "env.txt")
	const response = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`
	piBin := filepath.Join(dir, "pi")
	script := "#!/bin/sh\ncat >/dev/null\n{ echo \"marker=$" + procguard.MarkerEnv + "\"; echo \"pkill=$(command -v pkill)\"; } >" + shellQuoteForTest(capture) + "\nprintf '%s\\n' '" + response + "'\n"
	if err := os.WriteFile(piBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	env := withProcGuard(runenv.Overlay{}, procGuardEnvironment(p, "01RUN"))
	cfg := &config.Config{Agent: types.AgentPi, AgentPathOverride: map[string]string{"pi": piBin}}
	ag, _, err := newPipelineAgent(context.Background(), cfg, filepath.Join(t.TempDir(), "evidence"), fakeLookPath, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if _, err := ag.Run(context.Background(), agent.RunOpts{Prompt: "hello", CWD: dir, Purpose: "test-evidence"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "marker=01RUN\n") {
		t.Errorf("agent process lacks the run marker:\n%s", got)
	}
	if want := "pkill=" + filepath.Join(p.Root(), "proc-guard", "bin", "pkill") + "\n"; !strings.Contains(got, want) {
		t.Errorf("agent process does not resolve pkill to the guard (want %q):\n%s", want, got)
	}
}
