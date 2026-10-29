package ui

// matrix/hardened: opening a recovered archive must follow its identity out
// of the archived filter and enter it without another search or key press.
import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestMatrixArchivedOpenContinuesAfterRestart(t *testing.T) {
	for _, embedded := range []bool{true, false} {
		for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'o'}}} {
			t.Run(key.String()+"/embedded="+map[bool]string{true: "yes", false: "no"}[embedded], func(t *testing.T) {
				h, source := matrixOpenSessionHome(t, "local")
				h.interactionAccess = func(*session.Instance) error { return nil }
				h.embeddedLayout = embedded
				source.ArchivedAt = time.Now()
				h.statusFilter = FilterModeArchived
				h.groupTree = session.NewGroupTree(h.instances)
				h.rebuildFlatItems()
				if !h.locateSearchSession(source, false) {
					t.Fatal("could not locate archived search target")
				}
				_, cmd := h.Update(key)
				if cmd == nil || h.resumingSessions[source.ID].IsZero() || h.embeddedMode {
					t.Fatal("archived open did not start recovery before attaching")
				}
				// The worker has durably restored and restarted this exact session.
				// Do not launch a real AI process in the input-router test.
				source.ArchivedAt = time.Time{}
				_, cmd = h.Update(sessionRestartedMsg{sessionID: source.ID, unarchived: true, openAfterRestart: true})
				if cmd == nil {
					t.Fatal("successful recovery did not continue opening")
				}
				if h.statusFilter != "" || h.flatItems[h.cursor].Session != source {
					t.Fatalf("recovered target lost: filter=%q cursor=%d", h.statusFilter, h.cursor)
				}
				if embedded && (!h.embeddedMode || !h.insertMode || h.insertModeSessionID != source.ID) {
					t.Fatal("recovery did not enter the original session")
				}
			})
		}
	}
}

func TestMatrixArchivedOpenFailureKeepsTarget(t *testing.T) {
	h, source := matrixOpenSessionHome(t, "local")
	h.interactionAccess = func(*session.Instance) error { return nil }
	source.ArchivedAt = time.Now()
	h.statusFilter = FilterModeArchived
	h.groupTree = session.NewGroupTree(h.instances)
	if !h.locateSearchSession(source, false) {
		t.Fatal("could not locate archive")
	}
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("no restart requested")
	}
	_, cmd = h.Update(sessionRestartedMsg{sessionID: source.ID, err: errors.New("spawn failed"), openAfterRestart: true})
	if cmd != nil || h.embeddedMode || h.statusFilter != FilterModeArchived || !source.IsArchived() {
		t.Fatal("failed recovery entered or hid the archive")
	}
	if h.flatItems[h.cursor].Session != source || h.hasActiveAnimation(source.ID) {
		t.Fatal("failed recovery lost selection or prevented retry")
	}
}

func TestMatrixArchivedOpenCompletionCarriesIntent(t *testing.T) {
	h, source := matrixOpenSessionHome(t, "local")
	h.interactionAccess = func(*session.Instance) error { return nil }
	// Removal before execution must fail without launching a process, while
	// retaining the explicit opening route on the completion message.
	delete(h.instanceByID, source.ID)
	msg := h.restartArchivedForOpen(source, true)().(sessionRestartedMsg)
	if msg.err == nil || !msg.openAfterRestart || !msg.legacyOpen || msg.sessionID != source.ID {
		t.Fatalf("lost archive opening intent: %+v", msg)
	}
}
