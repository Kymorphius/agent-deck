package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// matrix/hardened: the lifecycle controller owns its storage and lease. It can
// run in headless Web without turning the shared read-only monitor into a
// process mutator, and exits with its management host.
func RunAutoSleepController(ctx context.Context, profile string) {
	if ctx == nil {
		return
	}
	var storage *Storage
	var lease *os.File
	defer func() {
		if storage != nil {
			_ = storage.Close()
		}
		if lease != nil {
			_ = syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
			_ = lease.Close()
		}
	}()
	watcher := NewAutoSleepWatcher(AutoSleepWatcherConfig{})
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			cfg, err := LoadUserConfig()
			if err == nil && cfg != nil && cfg.Lifecycle.AutoSleep {
				after, err := cfg.Lifecycle.SleepDuration()
				if err == nil {
					if lease == nil {
						lease, err = tryAutoSleepLease(profile)
					}
					if err == nil && lease != nil {
						if storage == nil {
							storage, err = NewStorageWithProfile(profile)
						}
						if err == nil {
							instances, loadErr := storage.Load()
							if loadErr == nil {
								watcher.Tick(ctx, storage, instances, after)
							}
						}
					}
				}
			} else {
				// Unknown/disabled policy breaks the continuous observation window.
				watcher = NewAutoSleepWatcher(AutoSleepWatcherConfig{})
			}
			timer.Reset(time.Minute)
		}
	}
}

// matrix/hardened: a nonblocking per-profile lock prevents multiple TUI/Web
// hosts from multiplying probes or stopping the same instance.
func tryAutoSleepLease(profile string) (*os.File, error) {
	dir, err := resolveLocksDirForSpawnLock()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "auto-sleep-"+spawnLockSafeID(profile)+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		_ = f.Close()
		return nil, fmt.Errorf("unsafe auto-sleep lease")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 || int(st.Uid) != os.Getuid() {
		_ = f.Close()
		return nil, fmt.Errorf("unsafe auto-sleep lease ownership")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

type AutoSleepWatcherConfig struct {
	Now        func() time.Time
	Inspect    func(*Instance, []*Instance) (AutoSleepEvidence, error)
	Sleep      func(*Storage, *Instance, func() error) error
	Refresh    func(*Instance)
	StoredIdle func(*Storage, *Instance, time.Time, time.Duration) error
}

type autoSleepObservation struct {
	fingerprint string
	firstSeen   time.Time
}

// AutoSleepWatcher requires two matching observations at least one minute
// apart. Old completion timestamps alone never immediately stop a process.
type AutoSleepWatcher struct {
	mu       sync.Mutex
	cfg      AutoSleepWatcherConfig
	observed map[string]autoSleepObservation
	rotation int
}

func NewAutoSleepWatcher(cfg AutoSleepWatcherConfig) *AutoSleepWatcher {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Inspect == nil {
		cfg.Inspect = InspectAutoSleepCandidate
	}
	if cfg.Sleep == nil {
		cfg.Sleep = AutoSleepSession
	}
	if cfg.Refresh == nil {
		cfg.Refresh = func(inst *Instance) { inst.UpdateStatus() }
	}
	if cfg.StoredIdle == nil {
		cfg.StoredIdle = CheckAutoSleepStoredIdle
	}
	return &AutoSleepWatcher{cfg: cfg, observed: map[string]autoSleepObservation{}}
}

// AutoSleepIdleSince includes deliberate user access and recorded activity;
// a historical completion must not age a newly opened session into sleep.
func AutoSleepIdleSince(inst *Instance, proof AutoSleepEvidence) time.Time {
	at := proof.CompletedAt
	for _, candidate := range []time.Time{inst.CreatedAt, inst.LastAccessedAt, inst.LastActivityAt()} {
		if candidate.After(at) {
			at = candidate
		}
	}
	return at
}

func (w *AutoSleepWatcher) Tick(ctx context.Context, storage *Storage, instances []*Instance, after time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if after <= 0 || ctx.Err() != nil {
		return
	}
	now := w.cfg.Now()
	present := make(map[string]bool, len(instances))
	for _, inst := range instances {
		if inst != nil {
			present[inst.ID] = true
		}
	}
	for id := range w.observed {
		if !present[id] {
			delete(w.observed, id)
		}
	}
	if len(instances) == 0 {
		return
	}
	checked := 0
	for visited := 0; visited < len(instances) && checked < 4 && ctx.Err() == nil; visited++ {
		index := w.rotation % len(instances)
		w.rotation = (index + 1) % len(instances)
		inst := instances[index]
		if inst == nil {
			continue
		}
		status := inst.GetStatusThreadSafe()
		watchable := status == StatusIdle || status == StatusWaiting || status == StatusRunning
		if !IsCodexCompatible(inst.Tool) || inst.IsArchived() || inst.Pin != PinNone || inst.IsAutoSleeping() || inst.AutoSleepPending() || !watchable {
			delete(w.observed, inst.ID)
			continue
		}
		checked++
		// The registry status can be old while no browser is open. Refresh this
		// bounded candidate only; the normal display sampler remains unchanged.
		w.cfg.Refresh(inst)
		proof, err := w.cfg.Inspect(inst, instances)
		if err != nil || proof.Fingerprint == "" || proof.CompletedAt.IsZero() || now.Sub(AutoSleepIdleSince(inst, proof)) < after {
			delete(w.observed, inst.ID)
			continue
		}
		previous, ok := w.observed[inst.ID]
		if !ok || previous.fingerprint != proof.Fingerprint {
			w.observed[inst.ID] = autoSleepObservation{proof.Fingerprint, now}
			continue
		}
		if now.Sub(previous.firstSeen) < time.Minute {
			continue
		}
		recheck := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			cfg, err := LoadUserConfig()
			if err != nil || cfg == nil || !cfg.Lifecycle.AutoSleep {
				return fmt.Errorf("automatic sleep disabled")
			}
			currentAfter, err := cfg.Lifecycle.SleepDuration()
			if err != nil {
				return err
			}
			// Reload the registry under the stop locks: a newly registered child
			// or second owner of this thread must also block this last check.
			currentInst, currentInstances := inst, instances
			if storage != nil {
				currentInstances, err = storage.Load()
				if err != nil {
					return err
				}
				currentInst = nil
				for _, candidate := range currentInstances {
					if candidate.ID == inst.ID {
						currentInst = candidate
						break
					}
				}
				if currentInst == nil {
					return fmt.Errorf("session was removed before sleep")
				}
				w.cfg.Refresh(currentInst)
			}
			current, err := w.cfg.Inspect(currentInst, currentInstances)
			if err != nil {
				return err
			}
			if current.Fingerprint != proof.Fingerprint || w.cfg.Now().Sub(AutoSleepIdleSince(currentInst, current)) < currentAfter {
				return fmt.Errorf("session changed before sleep")
			}
			if err := w.cfg.StoredIdle(storage, inst, w.cfg.Now(), currentAfter); err != nil {
				return err
			}
			return nil
		}
		if err := w.cfg.Sleep(storage, inst, recheck); err != nil {
			idleLog.Debug("auto_sleep_skipped", slog.String("instance_id", inst.ID), slog.String("reason", err.Error()))
		} else {
			_ = WriteSessionLifecycleEvent(SessionLifecycleEvent{InstanceID: inst.ID, Action: "auto-sleep", Reason: "completed and unused", Timestamp: now.Unix()})
		}
		delete(w.observed, inst.ID)
	}
}

// CheckAutoSleepStoredIdle observes cross-process access/activity immediately
// before stop. A controller's detached Instance is not an access-time fence.
func CheckAutoSleepStoredIdle(storage *Storage, inst *Instance, now time.Time, after time.Duration) error {
	if storage == nil || storage.GetDB() == nil {
		return fmt.Errorf("auto-sleep storage unavailable")
	}
	row, err := storage.GetDB().LoadInstanceByID(inst.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("session was removed before sleep")
	}
	for _, at := range []time.Time{row.LastAccessed, ReadLastActivityAtFromToolData(row.ToolData)} {
		if !at.IsZero() && now.Sub(at) < after {
			return fmt.Errorf("session was accessed or active before sleep")
		}
	}
	return nil
}
