package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: the ordinary delete dialog exposes a separate permanent
// preview; it does not execute permanent deletion on y/Enter automatically.
func TestPermanentDeleteDialogEntry(t *testing.T) {
	h := NewHome()
	h.confirmDialog.ShowDeleteSession("fixture", "fixture", false, false)
	if !strings.Contains(strings.ReplaceAll(h.confirmDialog.View(), "\u00a0", " "), "p permanent delete") {
		t.Fatal("permanent preview entry missing")
	}
	if h.confirmDialog.GetFocusedButton() != 1 {
		t.Fatal("ordinary delete no longer defaults to Cancel")
	}
	_, cmd := h.handleConfirmDialogKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd == nil || h.confirmDialog.IsVisible() {
		t.Fatal("p did not open child CLI preview")
	}
}
