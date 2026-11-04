package web

// matrix/hardened: full menu loads and live status publications carry actual
// interaction time without using the visit clock or regressing newer input.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestMatrixInteractionMenuProjection(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 20, 30, 987654321, time.UTC)
	inst := session.NewInstanceWithTool("fixture", "/fixture", "codex")
	inst.LastAccessedAt = at.Add(time.Hour)
	if err := inst.RecordInteractionAt(at); err != nil {
		t.Fatal(err)
	}
	row := toMenuSession(inst)
	if !row.LastInteractionAt.Equal(at) || !row.LastAccessedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("menu projection mixed timestamps: %+v", row)
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["lastInteractionAt"] != at.Format(time.RFC3339Nano) {
		t.Fatalf("menu JSON lost interaction clock: %s", data)
	}
	viewed := session.NewInstanceWithTool("viewed", "/fixture", "codex")
	viewed.LastAccessedAt = at
	if got := toMenuSession(viewed).LastInteractionAt; !got.IsZero() {
		t.Fatalf("view-only menu row acquired interaction %s", got)
	}
}

func TestMatrixInteractionMenuLiveStateMonotonic(t *testing.T) {
	first := time.Date(2026, 10, 17, 10, 20, 30, 0, time.UTC)
	latest := first.Add(time.Minute)
	store := NewMemoryMenuData(nil)
	store.SetSnapshot(&MenuSnapshot{Items: []MenuItem{{Type: MenuItemTypeSession, Session: &MenuSession{
		ID: "fixture", LastAccessedAt: first.Add(time.Hour), LastInteractionAt: first,
	}}}})
	for _, at := range []time.Time{latest, first, {}} {
		store.UpdateSessionStates(map[string]MenuSessionState{"fixture": {
			Status: session.StatusWaiting, LastInteractionAt: at,
		}}, time.Now())
		menu, err := store.LoadMenuSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		row := menu.Items[0].Session
		if !row.LastInteractionAt.Equal(latest) || !row.LastAccessedAt.Equal(first.Add(time.Hour)) {
			t.Fatalf("live menu clock regressed or reused viewing: %+v", row)
		}
	}
}
