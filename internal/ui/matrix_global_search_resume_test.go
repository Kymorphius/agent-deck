package ui

import (
	"os"
	"strings"
	"testing"
)

// matrix/hardened: createSessionFromGlobalSearch must not bake a resume
// command into inst.Command — Start's buildClaudeResumeCommand wraps any
// non-"claude" Command again and the pane dies on "exec exec claude ...".
// See internal/session/matrix_global_search_resume_test.go for the
// command-shape half; this guards the call site.
func TestMatrixGlobalSearchImport_DoesNotBakeResumeCommand(t *testing.T) {
	src, err := os.ReadFile("home.go")
	if err != nil {
		t.Fatalf("read home.go: %v", err)
	}
	for _, bad := range []string{`"exec claude --resume `, `"exec claude --session-id `} {
		if strings.Contains(string(src), bad) {
			t.Errorf("home.go builds a %s... command again; let Start build it from ClaudeSessionID", bad)
		}
	}
}
