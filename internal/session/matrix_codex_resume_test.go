package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: LocateCodexResume must find a found Codex thread's rollout
// in the Codex home a new session launches with, start it in the thread's
// last directory, and refuse what `codex resume` cannot continue.

const matrixCodexThread = "01a12172-c424-7502-b2c3-5e250fde3050"

// writeMatrixRollout writes a rollout for thread under codexHome with a
// session_meta (source, first cwd) and one turn_context per further cwd.
func writeMatrixRollout(t *testing.T, codexHome, thread, threadSource string, cwds ...string) string {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `{"timestamp":"2026-10-09T16:15:32.038Z","ordinal":0,"type":"session_meta","payload":{"id":%q,"parent_thread_id":"01a11bb1-20c4-7ca0-bc1d-5e336f0d2612","cwd":%q,"thread_source":%q,"base_instructions":{"text":"x"}}}`+"\n", thread, cwds[0], threadSource)
	b.WriteString(`{"timestamp":"2026-10-09T16:15:33.000Z","ordinal":1,"type":"response_item","payload":{"type":"function_call_output","output":"{\"cwd\":\"/elsewhere\"}"}}` + "\n")
	for n, cwd := range cwds[1:] {
		fmt.Fprintf(&b, `{"timestamp":"2026-10-09T16:15:34.000Z","ordinal":%d,"type":"turn_context","payload":{"turn_id":"t%d","cwd":%q}}`+"\n", n+2, n, cwd)
	}
	path := filepath.Join(dir, "rollout-2026-10-09T09-15-31-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMatrixLocateCodexResume_LastCWDInLaunchHome(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	t.Setenv("CODEX_HOME", codexHome)
	first, last := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{first, last} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rollout := writeMatrixRollout(t, codexHome, matrixCodexThread, "user", first, first, last)

	loc, err := LocateCodexResume("", matrixCodexThread)
	if err != nil {
		t.Fatalf("LocateCodexResume: %v", err)
	}
	if loc.Dir != last || loc.Rollout != rollout || loc.Home != codexHome {
		t.Fatalf("got %+v, want dir %q rollout %q home %q", loc, last, rollout, codexHome)
	}
}

func TestMatrixLocateCodexResume_Refusals(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	t.Setenv("CODEX_HOME", codexHome)
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := LocateCodexResume("", "not-a-uuid"); err == nil {
		t.Fatal("non-uuid thread id accepted")
	}
	if _, err := LocateCodexResume("", matrixCodexThread); err == nil || !strings.Contains(err.Error(), "nothing to resume") {
		t.Fatalf("missing rollout: %v", err)
	}
	// A rollout in another home than the launch one is not resumable: the
	// launch would drop the id and start a fresh thread.
	writeMatrixRollout(t, filepath.Join(root, "other"), matrixCodexThread, "user", proj)
	if _, err := LocateCodexResume("", matrixCodexThread); err == nil || !strings.Contains(err.Error(), codexHome) {
		t.Fatalf("rollout in another home: %v", err)
	}

	writeMatrixRollout(t, codexHome, matrixCodexThread, "subagent", proj)
	if _, err := LocateCodexResume("", matrixCodexThread); err == nil || !strings.Contains(err.Error(), "resume its parent 01a11bb1-20c4-7ca0-bc1d-5e336f0d2612") {
		t.Fatalf("subagent thread: %v", err)
	}

	gone := filepath.Join(root, "gone")
	writeMatrixRollout(t, codexHome, matrixCodexThread, "user", proj, gone)
	if _, err := LocateCodexResume("", matrixCodexThread); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("vanished dir: %v", err)
	}
}
