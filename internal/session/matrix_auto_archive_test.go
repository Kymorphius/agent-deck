package session

import (
	"context"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: auto-archive hides only confirmed, long-dead local rows and
// loses every race to a status change, restart or manual archive.
func TestMatrixAutoArchiveConfirmsDeadSessionsOnly(t *testing.T) {
	storage := newTestStorage(t)
	old := time.Now().Add(-48 * time.Hour)
	mk := func(id string, status Status) *Instance {
		return &Instance{ID: id, Title: id, ProjectPath: "/tmp/x", GroupPath: "g", Tool: "shell", Status: status, CreatedAt: old}
	}
	dead, stopped, live := mk("dead", StatusError), mk("stopped", StatusStopped), mk("live", StatusIdle)
	recent := mk("recent", StatusError)
	recent.LastAccessedAt = time.Now().Add(-time.Hour)
	pinned := mk("pinned", StatusError)
	pinned.Pin = PinTop
	conductor := mk("conductor", StatusError)
	conductor.IsConductor = true
	remote := mk("remote", StatusError)
	remote.SSHHost = "dev"
	slept := mk("slept", StatusStopped)
	slept.AutoSleep = &AutoSleepState{Phase: "asleep"}
	alive := mk("alive", StatusError)
	raced := mk("raced", StatusError)
	all := []*Instance{dead, stopped, live, recent, pinned, conductor, remote, slept, alive, raced}
	if err := storage.Save(all); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	probes := 0
	w := NewAutoArchiveWatcher(AutoArchiveWatcherConfig{
		Now: func() time.Time { return now },
		Probe: func(inst *Instance) (bool, error) {
			probes++
			return inst.ID == "alive", nil
		},
	})
	ctx := context.Background()
	if got := w.Tick(ctx, storage, all, 24*time.Hour); got != 0 || probes != 0 {
		t.Fatalf("first observation archived %d, probed %d; want confirmation first", got, probes)
	}
	now = now.Add(30 * time.Second)
	if got := w.Tick(ctx, storage, all, 24*time.Hour); got != 0 {
		t.Fatalf("archived %d before the confirmation interval", got)
	}
	// Another writer restarts "raced" before the confirming tick.
	if err := storage.GetDB().PersistInstanceStatusesTx([]statedb.InstanceStatusUpdate{{ID: "raced", Status: "running"}}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if got := w.Tick(ctx, storage, all, 24*time.Hour); got != 2 {
		t.Fatalf("archived %d, want dead and stopped", got)
	}

	loaded, err := storage.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range loaded {
		want := inst.ID == "dead" || inst.ID == "stopped"
		if inst.IsArchived() != want {
			t.Errorf("%s archived = %v, want %v", inst.ID, inst.IsArchived(), want)
		}
	}
	// Unarchive is the revival path; the row and its fields are retained.
	if err := storage.GetDB().SetArchived("dead", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if applied, err := storage.GetDB().ArchiveIfStatus("stopped", "stopped", time.Now()); err != nil || applied {
		t.Fatalf("already-archived row re-archived: %v, %v", applied, err)
	}
}

func TestMatrixAutoArchiveDurationBounds(t *testing.T) {
	for in, ok := range map[string]bool{"": true, "1h": true, "720h": true, "8760h": true, "59m": false, "8761h": false, "x": false} {
		d, err := (LifecycleSettings{ArchiveAfter: in}).ArchiveDuration()
		if (err == nil) != ok {
			t.Errorf("ArchiveDuration(%q) = %v, %v", in, d, err)
		}
		if in == "" && d != 24*time.Hour {
			t.Errorf("default = %v, want 24h", d)
		}
	}
}
