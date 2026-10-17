package ui

import (
	"fmt"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: only an explicit open action creates this command. Preview,
// navigation and monitor polling never resume a saved conversation.
type matrixAutoWokeMsg struct {
	id     string
	legacy bool
	err    error
}

func (h *Home) matrixRecordOpenInteraction(inst *session.Instance) error {
	if h.interactionAccess != nil {
		return h.interactionAccess(inst)
	}
	if err := inst.MarkAccessedForInteraction(); err != nil {
		return fmt.Errorf("cannot open session before recording access: %w", err)
	}
	return nil
}

func (h *Home) matrixWakeSelectedSession(inst *session.Instance, legacy bool) tea.Cmd {
	if h.hasActiveAnimation(inst.ID) {
		return nil
	}
	h.resumingSessions[inst.ID] = time.Now()
	id := inst.ID
	return func() tea.Msg {
		current := h.getInstanceByID(id)
		if current == nil {
			return matrixAutoWokeMsg{id: id, legacy: legacy, err: fmt.Errorf("session no longer exists")}
		}
		_, err := session.WakeAutoSleepingSession(h.storage, current)
		return matrixAutoWokeMsg{id: id, legacy: legacy, err: err}
	}
}

func (h *Home) matrixAutoWakeFinished(msg matrixAutoWokeMsg) (tea.Model, tea.Cmd) {
	delete(h.resumingSessions, msg.id)
	if msg.err != nil {
		h.setError(fmt.Errorf("failed to wake sleeping session: %w", msg.err))
		return h, nil
	}
	h.invalidatePreviewCache(msg.id)
	h.refreshSessionRenderSnapshot(nil)
	// A slow resume must not open a different row the operator navigated to.
	if h.cursor < 0 || h.cursor >= len(h.flatItems) {
		return h, nil
	}
	item := h.flatItems[h.cursor]
	if item.Session == nil || item.Session.ID != msg.id {
		return h, nil
	}
	// matrix/hardened: do not run the ordinary stopped-row restart fallback
	// after wake. A concurrent manual stop or a post-wake exit stays stopped.
	if !item.Session.Exists() {
		h.setError(fmt.Errorf("resumed session is no longer running"))
		return h, nil
	}
	if msg.legacy || !h.embeddedLayout {
		return h, h.attachSession(item.Session)
	}
	if h.enterEmbeddedMode() {
		return h, h.startEmbeddedModeCmd()
	}
	return h, nil
}

func matrixSessionStatusLabel(status session.Status, sleeping, pending bool) string {
	if pending {
		return "sleep pending"
	}
	if sleeping {
		return "sleeping"
	}
	return string(status)
}

// matrix/hardened: retained history is a passive read in both sleeping and
// resumed sessions. A failed verification must stay visible in the pager.
func matrixScrollbackWithSavedHistory(inst *session.Instance, capture func() (string, error)) (string, error) {
	if inst.IsAutoSleeping() || inst.AutoSleepPending() {
		return session.ReadAutoSleepScrollback(inst)
	}
	current, err := capture()
	if err != nil || !inst.HasAutoSleepHistory() {
		return current, err
	}
	saved, err := session.ReadAutoSleepScrollback(inst)
	if err != nil {
		return "", err
	}
	return saved + "\n\n--- Current terminal after resume ---\n\n" + current, nil
}
