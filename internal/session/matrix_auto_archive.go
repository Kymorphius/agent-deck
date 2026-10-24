package session

import (
	"context"
	"log/slog"
	"time"
)

// matrix/hardened: auto-archive hides local sessions that stayed dead (error
// or stopped, no tmux session) for longer than lifecycle.archive_after since
// their last use (created, accessed, activity, started or message sent). Archiving only sets archived_at; the conversation, tool ids
// and row are kept, and unarchive (TUI ^ then Shift+U, or `session unarchive`)
// restores the session for a normal restart.
//
// A candidate must match on two ticks at least autoArchiveConfirm apart with
// an unchanged fingerprint, its pane must be authoritatively absent, and the
// write only applies while the stored status is unchanged and unarchived.
// Pinned, conductor, SSH, sandboxed and auto-sleep sessions are never touched.
const autoArchiveConfirm = 50 * time.Second

type AutoArchiveWatcherConfig struct {
	Now     func() time.Time
	Probe   func(*Instance) (alive bool, err error)
	Archive func(*Storage, *Instance, Status, time.Time) (bool, error)
}

type autoArchiveObservation struct {
	fingerprint string
	firstSeen   time.Time
}

type AutoArchiveWatcher struct {
	cfg      AutoArchiveWatcherConfig
	observed map[string]autoArchiveObservation
}

func NewAutoArchiveWatcher(cfg AutoArchiveWatcherConfig) *AutoArchiveWatcher {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Probe == nil {
		cfg.Probe = autoArchiveProbe
	}
	if cfg.Archive == nil {
		cfg.Archive = func(storage *Storage, inst *Instance, status Status, at time.Time) (bool, error) {
			return storage.GetDB().ArchiveIfStatus(inst.ID, string(status), at)
		}
	}
	return &AutoArchiveWatcher{cfg: cfg, observed: make(map[string]autoArchiveObservation)}
}

func autoArchiveProbe(inst *Instance) (bool, error) {
	s := inst.GetTmuxSession()
	if s == nil {
		return false, nil
	}
	return s.ProbeExists()
}

// autoArchiveCandidate returns the fingerprint of an eligible dead session.
func autoArchiveCandidate(inst *Instance, now time.Time, after time.Duration) (string, Status, bool) {
	if inst == nil || inst.IsSSH() || inst.IsSandboxed() {
		return "", "", false
	}
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	status := inst.Status
	if !inst.ArchivedAt.IsZero() || (status != StatusError && status != StatusStopped) ||
		inst.Pin != PinNone || inst.IsConductor || inst.AutoSleep != nil || inst.SandboxContainer != "" {
		return "", "", false
	}
	lastUse := inst.CreatedAt
	for _, t := range []time.Time{inst.LastAccessedAt, inst.lastActivityAt, inst.LastStartedAt} {
		if t.After(lastUse) {
			lastUse = t
		}
	}
	if lastUse.IsZero() || now.Sub(lastUse) < after {
		return "", "", false
	}
	name := ""
	if inst.tmuxSession != nil {
		name = inst.tmuxSession.SocketName + "\x00" + inst.tmuxSession.Name
	}
	return string(status) + "\x00" + name + "\x00" + lastUse.UTC().Format(time.RFC3339Nano), status, true
}

// Tick archives confirmed candidates among freshly loaded instances and
// returns how many were archived.
func (w *AutoArchiveWatcher) Tick(ctx context.Context, storage *Storage, instances []*Instance, after time.Duration) int {
	now := w.cfg.Now()
	seen := make(map[string]bool, len(instances))
	archived := 0
	for _, inst := range instances {
		if ctx.Err() != nil {
			return archived
		}
		fingerprint, status, ok := autoArchiveCandidate(inst, now, after)
		if !ok {
			continue
		}
		seen[inst.ID] = true
		prior, known := w.observed[inst.ID]
		if !known || prior.fingerprint != fingerprint {
			w.observed[inst.ID] = autoArchiveObservation{fingerprint: fingerprint, firstSeen: now}
			continue
		}
		if now.Sub(prior.firstSeen) < autoArchiveConfirm {
			continue
		}
		// A message delivered to the session (send, conductor, Web) is use too.
		if sent, err := storage.GetDB().ReadLastSentAt(inst.ID); err != nil || (sent > 0 && now.Sub(time.Unix(sent, 0)) < after) {
			delete(w.observed, inst.ID)
			continue
		}
		if alive, err := w.cfg.Probe(inst); err != nil || alive {
			delete(w.observed, inst.ID)
			continue
		}
		applied, err := w.cfg.Archive(storage, inst, status, now.UTC())
		delete(w.observed, inst.ID)
		if err != nil {
			sessionLog.Warn("auto_archive_failed", slog.String("instance_id", inst.ID), slog.String("error", err.Error()))
			continue
		}
		if applied {
			archived++
			sessionLog.Info("auto_archive", slog.String("instance_id", inst.ID), slog.String("title", inst.Title), slog.String("status", string(status)))
		}
	}
	for id := range w.observed {
		if !seen[id] {
			delete(w.observed, id)
		}
	}
	return archived
}
