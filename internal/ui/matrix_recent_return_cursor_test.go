package ui

// matrix/hardened: a visit may be sorted by an ordinary fleet/row refresh
// before detach messages arrive. Every stage must follow identity, not a row.

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func matrixRecentReturnHome(t *testing.T) *Home {
	t.Helper()
	home := matrixRecentCursorHome(t)
	home.width, home.height = 80, 10
	home.storage = nil // This test applies in-memory fetch results, never a live fetch.
	home.groupViewMode = session.GroupViewRecent
	home.rebuildFlatItems()
	return home
}

func matrixAssertRecentReturnIdentity(t *testing.T, home *Home, want selectedItemIdentity, stage string) {
	t.Helper()
	if got := home.captureSelectedItemIdentity(); got != want {
		t.Fatalf("%s selected %+v, want %+v (cursor=%d offset=%d)", stage, got, want, home.cursor, home.viewOffset)
	}
	if home.cursor < home.viewOffset {
		t.Fatalf("%s kept selected row %d above viewport %d", stage, home.cursor, home.viewOffset)
	}
	budget, width := home.sidebarLineBudget()
	if home.viewOffset > 0 {
		budget--
	}
	if height := home.sidebarItemsHeight(home.flatItems, home.viewOffset, home.cursor+1, width); height > max(1, budget) {
		t.Fatalf("%s left selected row outside viewport: %d lines, budget %d", stage, height, budget)
	}
}

func TestMatrixRecentReturnLocalSelectionSurvivesFleetPollBeforeDetach(t *testing.T) {
	home := matrixRecentReturnHome(t)
	inst := matrixPinnedInstance(t, home, "b1")
	home.cursor = sessionIndexByTitle(home, inst.Title)
	home.viewOffset = home.cursor
	want, oldRow := home.captureSelectedItemIdentity(), home.cursor
	if oldRow == 0 {
		t.Fatal("fixture must open an older visit below the newest row")
	}
	home.isAttaching.Store(true)
	home.markSessionVisited(inst)
	matrixRecentInteraction(t, inst, time.Now()) // Actual submitted input, beyond viewing.
	// A poll unrelated to this local session can land before tea.Exec's return
	// callback. It must not leave the cursor on the old ordinal after sorting.
	home.Update(remoteSessionsFetchedMsg{sessions: home.remoteSessions, pushed: true})
	matrixAssertRecentReturnIdentity(t, home, want, "fleet poll during attach")
	if home.cursor != 0 || home.viewOffset != 0 {
		t.Fatalf("visited session did not move into the first visible row: cursor=%d offset=%d", home.cursor, home.viewOffset)
	}
	home.Update(statusUpdateMsg{attachedSessionID: inst.ID})
	matrixAssertRecentReturnIdentity(t, home, want, "detach callback")
	home.Update(attachReturnSyncedMsg{})
	matrixAssertRecentReturnIdentity(t, home, want, "delayed return reconciliation")
}

func TestMatrixRecentReturnRemoteSelectionFollowsHostAndID(t *testing.T) {
	for _, host := range []string{"dev", "pro"} {
		t.Run(host, func(t *testing.T) {
			home := matrixRecentReturnHome(t)
			// IDs are not fleet-global: this local row and both remotes have
			// the same ID, but each is a different selectable conversation.
			id := matrixPinnedInstance(t, home, "b1").ID
			old := time.Now().Add(-3 * time.Hour).Format(time.RFC3339Nano)
			home.remoteSessions = map[string][]session.RemoteSessionInfo{
				"dev": {{ID: id, Title: "dev conversation", Status: "running", LastInteractionAt: old}},
				"pro": {{ID: id, Title: "pro conversation", Status: "running", LastInteractionAt: old}},
			}
			home.rebuildFlatItems()
			want := selectedItemIdentity{remoteName: host, remoteSessionID: id, windowIndex: -1}
			if !home.restoreSelectedItemIdentity(want) || home.cursor == 0 {
				t.Fatal("fixture did not select an older remote conversation")
			}
			home.viewOffset = home.cursor
			fresh := append([]session.RemoteSessionInfo(nil), home.remoteSessions[host]...)
			fresh[0].LastInteractionAt = time.Now().Format(time.RFC3339Nano)
			other := "dev"
			if host == other {
				other = "pro"
			}
			home.Update(remoteSessionsFetchedMsg{
				sessions: map[string][]session.RemoteSessionInfo{host: fresh},
				failed:   map[string]bool{other: true}, pushed: true,
			})
			matrixAssertRecentReturnIdentity(t, home, want, "remote visit fetch")
			if home.cursor != 0 || home.viewOffset != 0 {
				t.Fatalf("remote visit is not first: cursor=%d offset=%d", home.cursor, home.viewOffset)
			}
			// Remote attach returns no local attachedSessionID; the identity
			// retained by the earlier poll must still identify its host.
			home.Update(statusUpdateMsg{})
			matrixAssertRecentReturnIdentity(t, home, want, "remote detach callback")
			home.Update(attachReturnSyncedMsg{})
			matrixAssertRecentReturnIdentity(t, home, want, "remote delayed reconciliation")
		})
	}
}

func TestMatrixRecentReturnWindowSelectionSurvivesOrdinaryRebuild(t *testing.T) {
	home := matrixRecentReturnHome(t)
	inst := matrixPinnedInstance(t, home, "b1")
	tmux.SeedWindowCacheForTest(t, map[string][]tmux.WindowInfo{
		inst.GetTmuxSession().Name: {{ID: "@0", Index: 0, Name: "first"}, {ID: "@1", Index: 1, Name: "second"}},
	})
	home.rebuildFlatItems()
	want := selectedItemIdentity{windowSessionID: inst.ID, windowIndex: 1}
	if !home.restoreSelectedItemIdentity(want) || home.cursor <= 2 {
		t.Fatal("fixture did not select a window under an older conversation")
	}
	home.viewOffset = home.cursor
	matrixRecentInteraction(t, inst, time.Now())
	// The ordinary rebuild also serves title reconciliation and embedded
	// focus/return paths, without the attach-return-specific wrapper.
	home.rebuildFlatItems()
	matrixAssertRecentReturnIdentity(t, home, want, "window parent visit rebuild")
	if home.cursor != 2 {
		t.Fatalf("window did not follow the newest parent block: cursor=%d, want 2", home.cursor)
	}
	home.Update(statusUpdateMsg{})
	matrixAssertRecentReturnIdentity(t, home, want, "window detach callback")
	home.Update(attachReturnSyncedMsg{})
	matrixAssertRecentReturnIdentity(t, home, want, "window delayed reconciliation")
	// A removed/collapsed window falls back to its own parent, never the
	// neighbor that happens to occupy its old numbered row.
	home.windowsCollapsed[inst.ID] = true
	home.rebuildFlatItems()
	want = selectedItemIdentity{sessionID: inst.ID, windowIndex: -1}
	matrixAssertRecentReturnIdentity(t, home, want, "collapsed window fallback")
}

// matrix/hardened: focus and detach still update viewing/lifecycle time, but
// must neither promote an older row nor create a new confirmed interaction.
func TestMatrixRecentReturnWithoutInteractionDoesNotReorder(t *testing.T) {
	home := matrixRecentReturnHome(t)
	inst := matrixPinnedInstance(t, home, "b1")
	home.cursor = sessionIndexByTitle(home, inst.Title)
	home.viewOffset = home.cursor
	want, oldRow, interactionAt := home.captureSelectedItemIdentity(), home.cursor, inst.LastInteractionAt()
	home.markSessionVisited(inst)
	inst.MarkAccessed()
	home.Update(remoteSessionsFetchedMsg{sessions: home.remoteSessions, pushed: true})
	home.Update(statusUpdateMsg{attachedSessionID: inst.ID})
	home.Update(attachReturnSyncedMsg{})
	matrixAssertRecentReturnIdentity(t, home, want, "view-only return")
	if home.cursor != oldRow || !inst.LastInteractionAt().Equal(interactionAt) {
		t.Fatalf("view-only return promoted a conversation: row %d -> %d, interaction %s -> %s", oldRow, home.cursor, interactionAt, inst.LastInteractionAt())
	}
	if top := home.flatItems[0].Session; top == nil || top.Title != "a1" {
		t.Fatal("view-only return changed the latest interaction")
	}
}
