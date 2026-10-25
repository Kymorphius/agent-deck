package ui

// matrix/hardened: Shift+number views keep the cursor on the selected project.

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestMatrixStatusViewAnchorsProject(t *testing.T) {
	home, idx := matrixPinnedHome(t), map[string]int{}
	for _, inst := range home.instances {
		inst.Status = session.StatusIdle
	}
	matrixPinnedInstance(t, home, "a1").Status = session.StatusRunning
	matrixPinnedInstance(t, home, "b2").Status = session.StatusRunning
	home.rebuildFlatItems()
	for i, it := range home.flatItems {
		if it.Type == session.ItemTypeSession {
			idx[it.Session.Title] = i
		}
	}
	home.cursor = idx["b1"]
	home.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'!'}})
	if home.statusFilter != session.StatusRunning {
		t.Fatalf("filter=%q, want running", home.statusFilter)
	}
	got := home.flatItems[home.cursor]
	if got.Type != session.ItemTypeGroup || got.Path != "beta" {
		t.Fatalf("cursor on %+v, want beta header", got)
	}

	// The selected session stays selected when it is still visible.
	for i, it := range home.flatItems {
		if it.Type == session.ItemTypeSession && it.Session.Title == "b2" {
			home.cursor = i
		}
	}
	home.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'!'}})
	if got := home.flatItems[home.cursor]; got.Session == nil || got.Session.Title != "b2" {
		t.Fatalf("cursor on %+v, want b2", got)
	}
}
