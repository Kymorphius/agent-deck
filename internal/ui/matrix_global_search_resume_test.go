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

// matrix/hardened: the G import resumes from the transcript's launch dir via
// session.LocateClaudeResumeDir, never from result.CWD (the conversation's
// last cwd), so a picked conversation is not restarted empty elsewhere.
func TestMatrixGlobalSearchImport_ResumesFromLaunchDir(t *testing.T) {
	src, err := os.ReadFile("home.go")
	if err != nil {
		t.Fatalf("read home.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (h *Home) createSessionFromGlobalSearch(")
	if start < 0 {
		t.Fatal("createSessionFromGlobalSearch not found")
	}
	fn := body[start:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "session.LocateClaudeResumeDir(") || !strings.Contains(fn, "LiveFor(") {
		t.Error("createSessionFromGlobalSearch no longer locates the launch dir / checks for a live conversation")
	}
	if strings.Contains(fn, "result.CWD") {
		t.Error("createSessionFromGlobalSearch uses result.CWD (the last cwd) again")
	}
}
