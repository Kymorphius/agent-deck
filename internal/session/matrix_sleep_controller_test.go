package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// matrix/hardened: exercise the lifecycle policy through its real config file
// and watcher boundaries, without issuing signals or launching an AI tool.
func matrixSleepTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	if err := SaveUserConfig(&UserConfig{Lifecycle: LifecycleSettings{AutoSleep: true, SleepAfter: "1h"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixSleepSettingsRejectInvalidPeriodWithoutWriting(t *testing.T) {
	matrixSleepTestConfig(t)
	path, err := GetUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"bad", "0", "-1h", "30s", "721h"} {
		if _, _, err := SetConfigValue("lifecycle.sleep_after", value); err == nil {
			t.Errorf("accepted %q", value)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("invalid setting changed configuration")
		}
	}
	if _, value, err := SetConfigValue("lifecycle.sleep_after", "2h"); err != nil || value != "2h" {
		t.Fatalf("roundtrip: %v %v", value, err)
	}
	if _, value, err := SetConfigValue("lifecycle.auto_sleep", "false"); err != nil || value != false {
		t.Fatalf("disable: %v %v", value, err)
	}
}

func TestMatrixSleepWatcherRequiresStableEvidenceAndRecentAccessProtection(t *testing.T) {
	matrixSleepTestConfig(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	inst := &Instance{ID: "candidate", Tool: "codex", Status: StatusWaiting, CreatedAt: now.Add(-3 * time.Hour), LastAccessedAt: now.Add(-2 * time.Hour)}
	proof := AutoSleepEvidence{ThreadID: "thread", TurnID: "turn", Fingerprint: "one", CompletedAt: now.Add(-2 * time.Hour)}
	stops := 0
	w := NewAutoSleepWatcher(AutoSleepWatcherConfig{Now: func() time.Time { return now }, Refresh: func(*Instance) {}, StoredIdle: func(*Storage, *Instance, time.Time, time.Duration) error { return nil }, Inspect: func(*Instance, []*Instance) (AutoSleepEvidence, error) { return proof, nil }, Sleep: func(_ *Storage, _ *Instance, recheck func() error) error {
		if err := recheck(); err != nil {
			return err
		}
		stops++
		return nil
	}})
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	if stops != 0 {
		t.Fatal("first observation stopped session")
	}
	now = now.Add(time.Minute)
	proof.Fingerprint = "two"
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	if stops != 0 {
		t.Fatal("changed evidence stopped session")
	}
	now = now.Add(time.Minute)
	inst.LastAccessedAt = now
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	if stops != 0 {
		t.Fatal("recently opened session stopped")
	}
	now = now.Add(2 * time.Hour)
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	now = now.Add(time.Minute)
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	if stops != 1 {
		t.Fatalf("safely stable candidate stopped %d times", stops)
	}
}

func TestMatrixSleepWatcherFinalCheckRejectsNewWorkAndDisable(t *testing.T) {
	matrixSleepTestConfig(t)
	now := time.Now()
	inst := &Instance{ID: "candidate", Tool: "codex", Status: StatusWaiting, CreatedAt: now.Add(-3 * time.Hour)}
	proof := AutoSleepEvidence{Fingerprint: "one", CompletedAt: now.Add(-2 * time.Hour)}
	blocked := false
	stops := 0
	inspect := func(*Instance, []*Instance) (AutoSleepEvidence, error) {
		if blocked {
			return AutoSleepEvidence{}, errors.New("new work")
		}
		return proof, nil
	}
	w := NewAutoSleepWatcher(AutoSleepWatcherConfig{Now: func() time.Time { return now }, Refresh: func(*Instance) {}, StoredIdle: func(*Storage, *Instance, time.Time, time.Duration) error { return nil }, Inspect: inspect, Sleep: func(_ *Storage, _ *Instance, recheck func() error) error {
		blocked = true
		if err := recheck(); err == nil {
			stops++
		}
		return errors.New("skipped")
	}})
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	now = now.Add(time.Minute)
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	if stops != 0 {
		t.Fatal("new work passed final check")
	}
	blocked = false
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	now = now.Add(time.Minute)
	w.cfg.Sleep = func(_ *Storage, _ *Instance, recheck func() error) error {
		if _, _, err := SetConfigValue("lifecycle.auto_sleep", "false"); err != nil {
			t.Fatal(err)
		}
		if err := recheck(); err == nil {
			stops++
		}
		return errors.New("disabled")
	}
	w.Tick(context.Background(), nil, []*Instance{inst}, time.Hour)
	if stops != 0 {
		t.Fatal("disabled policy passed final check")
	}
}

func TestMatrixSleepLeaseDeduplicatesAndRejectsSymlink(t *testing.T) {
	matrixSleepTestConfig(t)
	first, err := tryAutoSleepLease("sleep-test")
	if err != nil || first == nil {
		t.Fatalf("first lease: %v", err)
	}
	defer first.Close()
	second, err := tryAutoSleepLease("sleep-test")
	if err != nil || second != nil {
		if second != nil {
			second.Close()
		}
		t.Fatalf("duplicate lease: %v", err)
	}
	dir, err := resolveLocksDirForSpawnLock()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "auto-sleep-symlink.lock")); err != nil {
		t.Fatal(err)
	}
	if lease, err := tryAutoSleepLease("symlink"); err == nil || lease != nil {
		t.Fatal("accepted symlink lease")
	}
}

func TestMatrixSleepStoredAccessCancelsDetachedCandidate(t *testing.T) {
	matrixSleepTestConfig(t)
	storage, err := NewStorageWithProfile("sleep-access")
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	now := time.Now()
	inst := &Instance{ID: "sleep-access", Title: "sleep access", Tool: "codex", Status: StatusWaiting, ProjectPath: t.TempDir(), CreatedAt: now.Add(-3 * time.Hour), LastAccessedAt: now.Add(-2 * time.Hour)}
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	if err := CheckAutoSleepStoredIdle(storage, inst, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Another management process can touch the DB without mutating this old model.
	if err := storage.GetDB().WriteLastAccessed(inst.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := CheckAutoSleepStoredIdle(storage, inst, now, time.Hour); err == nil {
		t.Fatal("ignored concurrent durable access")
	}
}
