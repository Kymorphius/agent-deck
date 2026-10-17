package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: an intentionally incomplete marker catches any accidental
// wake before validation; exact resume refuses it without spawning a process.
func matrixSetCLISleepMarker(t *testing.T, home, id string, state session.AutoSleepState) {
	matrixSetCLISleepToolData(t, home, id, map[string]any{"matrix_auto_sleep": state})
}

func matrixSetCLISleepToolData(t *testing.T, home, id string, data map[string]any) {
	t.Helper()
	db, err := sql.Open("sqlite", stateDBPath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE instances SET tool_data=?, status='stopped' WHERE id=?", string(raw), id); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixSleepingCLIValidatesBeforeWake(t *testing.T) {
	for _, phase := range []string{"asleep", "pending"} {
		home := t.TempDir()
		id := addSessionJSON(t, home, "sleep-validation", "codex")
		matrixSetCLISleepMarker(t, home, id, session.AutoSleepState{Version: 1, Phase: phase})
		for _, args := range [][]string{
			{"session", "send", id, "hello", "--queue", "--wait"},
			{"session", "send", id, "hello", "--stream"},
			{"session", "send", id, "hello", "--image", filepath.Join(home, "unused.png")},
			{"session", "attach", id},
		} {
			stdout, stderr, code := runAgentDeck(t, home, args...)
			if code == 0 || strings.Contains(stdout+stderr, "failed to wake") {
				t.Fatalf("invalid request invoked wake: %v code=%d %s %s", args, code, stdout, stderr)
			}
		}
		stdout, stderr, code := runAgentDeck(t, home, "session", "send", id, "hello", "--no-wait")
		if code == 0 || !strings.Contains(stdout+stderr, "failed to wake") {
			t.Fatalf("valid send bypassed exact wake: %d %s %s", code, stdout, stderr)
		}
	}
}

func TestMatrixSleepingCLIPaneOutputReadsSavedScrollback(t *testing.T) {
	home := t.TempDir()
	id := addSessionJSON(t, home, "sleep-output", "codex")
	dir := filepath.Join(filepath.Dir(stateDBPath(t, home)), "auto-sleep")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+"-fixture.log")
	content := "saved terminal output\nCLI_SAVED_SCROLLBACK\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	prior := session.AutoSleepState{Version: 1, Phase: "asleep", ScrollbackPath: path, ScrollbackBytes: int64(len(content)), ScrollbackSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
	for _, phase := range []string{"asleep", "pending"} {
		prior.Phase = phase
		matrixSetCLISleepMarker(t, home, id, prior)
		stdout, stderr, code := runAgentDeck(t, home, "session", "output", id, "--pane", "--json")
		if code != 0 || !strings.Contains(stdout, "CLI_SAVED_SCROLLBACK") {
			t.Fatalf("phase=%s pane output woke or lost saved output: %d %s %s", phase, code, stdout, stderr)
		}
	}
	// Clearing wake intent must not make its independently retained terminal
	// history unreachable. This route is explicit and never reads a live pane.
	matrixSetCLISleepToolData(t, home, id, map[string]any{"matrix_auto_sleep": nil, "matrix_auto_sleep_history": []session.AutoSleepState{prior}})
	stdout, stderr, code := runAgentDeck(t, home, "session", "output", id, "--saved-pane", "--json")
	if code != 0 || !strings.Contains(stdout, "CLI_SAVED_SCROLLBACK") || !strings.Contains(stdout, "saved-pane") {
		t.Fatalf("wake history was no longer readable: %d %s %s", code, stdout, stderr)
	}
}
