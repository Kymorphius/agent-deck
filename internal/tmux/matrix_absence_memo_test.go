package tmux

import (
	"context"
	"crypto/rand"
	"testing"
	"time"
)

// matrix/hardened: a completed absence verdict is repeated without forking
// tmux, and the per-pass positive session cache revives it immediately.
func TestMatrixAbsenceMemoSkipsReprobesUntilPositiveEvidence(t *testing.T) {
	skipIfNoTmuxBinary(t)
	matrixMetadataCacheCleanup(t)
	live := matrixQueryFixture(t, "absence-memo")
	SetDefaultSocketName(live.SocketName)
	RefreshSessionCache()
	gone := &Session{Name: "matrix-absent-row", SocketName: live.SocketName}
	if gone.Exists() {
		t.Fatal("absent session reported alive")
	}
	if !gone.KnownAbsent() {
		t.Fatal("completed has-session absence did not seed the memo")
	}
	has, panes := matrixMetadataStarts("has-session"), matrixMetadataStarts("list-panes")
	for n := 0; n < 20; n++ {
		if gone.Exists() {
			t.Fatal("memoized session reported alive")
		}
		if _, ok := gone.PaneDeadExitStatus(); ok {
			t.Fatal("memoized session reported an exit status")
		}
		if gone.IsPaneDead() {
			t.Fatal("memoized session reported a dead pane")
		}
	}
	if got := matrixMetadataStarts("has-session"); got != has {
		t.Fatalf("memoized absence forked has-session %d times", got-has)
	}
	if got := matrixMetadataStarts("list-panes"); got != panes {
		t.Fatalf("memoized absence forked list-panes %d times", got-panes)
	}
	// One missing pane must not fail a capture batch for live neighbours.
	batches := matrixMetadataStarts("batch")
	prefetchMatrixCapturePanes(context.Background(), []*Session{gone, live}, rand.Reader)
	if got := matrixMetadataStarts("batch"); got != batches {
		t.Fatalf("known-absent pane joined a capture batch (%d starts)", got-batches)
	}

	// Another process creates the session: the next cache refresh wins.
	matrixMetadataNativeRun(t, live.SocketName, "new-session", "-d", "-s", gone.Name, "sleep 120")
	RefreshSessionCache()
	if gone.KnownAbsent() || !gone.Exists() {
		t.Fatal("positive session cache did not clear the absence memo")
	}
	if alive, err := gone.ProbeExists(); !alive || err != nil {
		t.Fatalf("revived ProbeExists = %v, %v", alive, err)
	}
	// ProbeExists stays authoritative: it never answers from the memo.
	gone.matrixNoteAbsent()
	if alive, err := gone.ProbeExists(); !alive || err != nil || gone.KnownAbsent() {
		t.Fatalf("ProbeExists answered from the memo: %v, %v", alive, err)
	}

	// Backoff grows and is capped; a name change or Start discards it.
	for n := 0; n < 10; n++ {
		gone.matrixNoteAbsent()
	}
	gone.absentMu.Lock()
	backoff := gone.absent.backoff
	gone.absentMu.Unlock()
	if backoff != matrixAbsentBackoffMax {
		t.Fatalf("backoff = %s, want cap %s", backoff, matrixAbsentBackoffMax)
	}
	renamed := &Session{Name: "matrix-absent-other", SocketName: live.SocketName}
	renamed.matrixNoteAbsent()
	renamed.Name = "matrix-absent-renamed"
	if renamed.KnownAbsent() {
		t.Fatal("memo survived a session name change")
	}
	expired := &Session{Name: "matrix-absent-expired", SocketName: live.SocketName}
	expired.matrixNoteAbsent()
	expired.absentMu.Lock()
	expired.absent.until = time.Now().Add(-time.Second)
	expired.absentMu.Unlock()
	if expired.KnownAbsent() {
		t.Fatal("expired memo still suppressed the probe")
	}
}
