package ui

import (
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: use the live TUI object for explicit controls, allowing the
// core helper to reconcile a marker changed by another process before launch.
func (m *WebMutator) matrixWakeForExplicitStart(inst *session.Instance) (bool, error) {
	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return false, err
	}
	defer storage.Close()
	if err := inst.MarkAccessedForInteraction(); err != nil {
		return false, fmt.Errorf("cannot start before recording access: %w", err)
	}
	wasSleeping := inst.IsAutoSleeping() || inst.AutoSleepPending()
	woke, err := session.WakeAutoSleepingSession(storage, inst)
	return woke || (err == nil && wasSleeping && inst.Exists()), err
}

// matrix/hardened: the terminal's authenticated input path opts into wake. A
// fresh storage read also sees a sleep committed by another TUI process.
func (m *WebMutator) WakeSleepingSession(id string) (name, socket string, woke bool, err error) {
	unlock, err := m.beginHeadlessTx()
	if err != nil {
		return "", "", false, err
	}
	defer unlock()
	storage, err := session.NewStorageWithProfile(m.h.profile)
	if err != nil {
		return "", "", false, err
	}
	defer storage.Close()
	instances, _, err := storage.LoadWithGroups()
	if err != nil {
		return "", "", false, err
	}
	for _, inst := range instances {
		if inst.ID != id {
			continue
		}
		// matrix/hardened: reached only by a real, nonempty authenticated input.
		// Persist access before waiting for any competing lifecycle operation.
		if err := inst.MarkAccessedForInteraction(); err != nil {
			return "", "", false, fmt.Errorf("cannot resume before recording access: %w", err)
		}
		woke, err = session.WakeAutoSleepingSession(storage, inst)
		if err != nil {
			return "", "", false, err
		}
		if !inst.Exists() {
			return "", "", false, nil
		}
		if target := inst.GetTmuxSession(); target != nil {
			return target.Name, target.SocketName, woke, nil
		}
		return "", "", woke, nil
	}
	return "", "", false, fmt.Errorf("session not found: %s", id)
}
