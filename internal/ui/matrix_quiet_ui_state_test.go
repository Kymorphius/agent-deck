package ui

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the periodic autosave must not commit an unchanged state
// (each commit wakes every state-DB watcher); a changed one is written, and an
// explicit save still writes unconditionally.
func TestPeriodicUIStateSaveSkipsUnchangedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	storage, err := session.NewStorageWithProfile("_quiet_ui_state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	db := storage.GetDB()
	h := NewHome()
	h.storage = storage
	rows := []*session.Instance{{ID: "a", Title: "a", GroupPath: "g", Status: session.StatusIdle}}
	h.groupTree = session.NewGroupTree(rows)
	h.instances = rows
	h.rebuildFlatItems()
	written := func() string {
		t.Helper()
		v, err := db.GetMeta("ui_state")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	h.saveUIStateIfChanged()
	first := written()
	if first == "" {
		t.Fatal("first periodic save wrote nothing")
	}
	// Another writer (a second TUI) replaces the row; an unchanged local state
	// is not re-asserted by the periodic save.
	if err := db.SetMeta("ui_state", "other-instance"); err != nil {
		t.Fatal(err)
	}
	h.saveUIStateIfChanged()
	if got := written(); got != "other-instance" {
		t.Fatalf("unchanged state was rewritten: %q", got)
	}
	// A real change is saved.
	h.statusFilter = FilterModeActive
	h.saveUIStateIfChanged()
	if got := written(); got == "other-instance" || got == first {
		t.Fatalf("changed state was not saved: %q", got)
	}
	// Explicit saves stay unconditional.
	if err := db.SetMeta("ui_state", "other-instance"); err != nil {
		t.Fatal(err)
	}
	if err := h.saveUIStateErr(); err != nil {
		t.Fatal(err)
	}
	if got := written(); got == "other-instance" {
		t.Fatal("explicit save skipped an unchanged state")
	}
}
