package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestMatrixQueuedAttachFreshStateRejectsStopped(t *testing.T) {
	// matrix/hardened: A queued model loaded before the lock cannot revive
	// a session another operation has since stopped in the registry.
	f := matrixNewQueuedAttachFixture(t, 0)
	if _, err := f.storage.GetDB().DB().Exec("UPDATE instances SET status=? WHERE id=?", string(session.StatusStopped), f.target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSessionPaneForAttach("queued-attach-test", f.storage, f.target); err == nil || !strings.Contains(err.Error(), "state changed to stopped") {
		t.Fatalf("stale queued attach = %v, want fresh stopped refusal", err)
	}
	if got := f.stored(f.target.ID).Status; got != session.StatusStopped {
		t.Fatalf("stopped state changed to %s", got)
	}
	if _, err := os.Stat(f.spawns); !os.IsNotExist(err) {
		t.Fatalf("stale queued attach started a process: %v", err)
	}
}

func TestMatrixQueuedAttachProbeErrorDoesNotStart(t *testing.T) {
	// matrix/hardened: An inconclusive authoritative probe is not absence.
	// A local tmux shim exercises this refusal without delaying or contacting
	// any real socket, while all fixture state remains isolated.
	f := matrixNewQueuedAttachFixture(t, 0)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' 'temporary I/O failure' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := ensureSessionPaneForAttach("queued-attach-test", f.storage, f.target); err == nil || !strings.Contains(err.Error(), "cannot confirm") {
		t.Fatalf("inconclusive attach probe = %v, want fail-closed refusal", err)
	}
	if got := f.stored(f.target.ID).Status; got != session.StatusQueued {
		t.Fatalf("queued state changed to %s", got)
	}
	if _, err := os.Stat(f.spawns); !os.IsNotExist(err) {
		t.Fatalf("inconclusive attach probe started a process: %v", err)
	}
}
