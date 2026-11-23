package triage

import "testing"

func TestInternalOnly(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		files []string
		want  bool
	}{
		"internal production and tests":    {files: []string{"internal/pipeline/executor.go", "internal/pipeline/executor_test.go"}, want: true},
		"nested internal package":          {files: []string{"svc/internal/store/db.go"}, want: true},
		"tests only":                       {files: []string{"internal/x/x_test.go", "internal/x/testdata/in.txt"}, want: true},
		"empty change has no signal":       {files: nil, want: false},
		"blank lines only":                 {files: []string{"", "  "}, want: false},
		"docs touched":                     {files: []string{"internal/a/a.go", "docs/reference/x.md"}, want: false},
		"readme touched":                   {files: []string{"internal/a/a.go", "README.md"}, want: false},
		"agent guidance touched":           {files: []string{"internal/a/a.go", ".agents/skills/x/SKILL.md"}, want: false},
		"cli package":                      {files: []string{"internal/cli/axi.go"}, want: false},
		"command entrypoint":               {files: []string{"cmd/no-mistakes/main.go"}, want: false},
		"config schema":                    {files: []string{"internal/config/config.go"}, want: false},
		"schema file":                      {files: []string{"internal/db/schema.go"}, want: false},
		"help text":                        {files: []string{"internal/x/help.go"}, want: false},
		"api package":                      {files: []string{"internal/api/routes.go"}, want: false},
		"proto definition":                 {files: []string{"internal/wire/msg.proto"}, want: false},
		"public package outside internal/": {files: []string{"pkg/client/client.go"}, want: false},
		"non-Go source has no signal":      {files: []string{"src/app.ts"}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := InternalOnly(tc.files); got != tc.want {
				t.Fatalf("InternalOnly(%q) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}
