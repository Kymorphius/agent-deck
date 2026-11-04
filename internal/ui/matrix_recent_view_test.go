package ui

// matrix/hardened: recent is one timeline of confirmed interactions across the fleet,
// independent of group folds, background work, pins, and manual row order.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func matrixRecentInteraction(t *testing.T, inst *session.Instance, at time.Time) {
	t.Helper()
	if err := inst.RecordInteractionAt(at); err != nil {
		t.Fatal(err)
	}
}

func matrixRecentRows(t *testing.T, h *Home) []string {
	t.Helper()
	rows := make([]string, 0, len(h.flatItems))
	for _, item := range h.flatItems {
		if item.Level != 0 || item.IsSubSession || item.ParentIsLastInGroup {
			t.Fatalf("recent row retained an invisible tree parent: %+v", item)
		}
		switch {
		case item.Type == session.ItemTypeSession && item.Session != nil && item.CreatingID == "":
			rows = append(rows, "local/"+item.Session.Title)
		case item.Type == session.ItemTypeRemoteSession && item.RemoteSession != nil:
			rows = append(rows, item.RemoteName+"/"+item.RemoteSession.ID)
		default:
			t.Fatalf("recent timeline retained a header or placeholder: %+v", item)
		}
	}
	return rows
}

func matrixAssertRecentRows(t *testing.T, h *Home, want ...string) {
	t.Helper()
	if got := matrixRecentRows(t, h); !reflect.DeepEqual(got, append([]string{}, want...)) {
		t.Fatalf("recent timeline = %v, want %v", got, want)
	}
}

func matrixRecentBackgroundActivity(t *testing.T, inst *session.Instance, at time.Time) {
	t.Helper()
	if !inst.ApplyRuntimeSnapshot(session.DefaultProfile, session.RuntimeSessionSnapshot{
		ID: inst.ID, Profile: session.DefaultProfile, Identity: inst.RuntimeIdentity(),
		Epoch: "recent-view-test", Revision: 1, ObservedAt: at,
		Status: inst.Status, ActivityAt: at, ObservedActivityAt: at, ObservedActivityKnown: true,
	}) {
		t.Fatal("could not install a background activity observation")
	}
}

func TestMatrixRecentViewCombinesCollapsedFleetByInteractionTime(t *testing.T) {
	home := matrixPinnedHome(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 500, time.UTC)
	oldest := matrixPinnedInstance(t, home, "a1")
	oldest.Pin, oldest.Status = session.PinTop, session.StatusError
	matrixRecentInteraction(t, oldest, now.Add(-6*time.Hour))
	matrixPinnedInstance(t, home, "a2").LastAccessedAt = now // Viewing alone is not an interaction.
	child := matrixPinnedInstance(t, home, "a3")
	child.ParentSessionID, child.GroupPath = matrixPinnedInstance(t, home, "a2").ID, "alpha/deep"
	child.Status = session.StatusIdle
	matrixRecentInteraction(t, child, now.Add(-2*time.Nanosecond))
	matrixRecentInteraction(t, matrixPinnedInstance(t, home, "b1"), now.Add(-3*time.Hour))
	archived := matrixPinnedInstance(t, home, "b2")
	archived.ArchivedAt = now
	matrixRecentInteraction(t, archived, now)
	home.groupTree = session.NewGroupTree(home.instances)
	groupFolds := make(map[string]bool)
	for path, group := range home.groupTree.Groups {
		group.Expanded, home.groupTree.Expanded[path], groupFolds[path] = false, false, false
	}
	home.remoteSessions = map[string][]session.RemoteSessionInfo{
		"dev": {
			{ID: "old-pin", Title: "old pin", Group: "work", Status: "error", Pin: matrixRemotePinPointer(session.PinTop), LastInteractionAt: now.Add(-4 * time.Hour).Format(time.RFC3339Nano)},
			{ID: "child", Title: "remote child", Group: "work/deep", ParentSessionID: "unknown-parent", Status: "idle", LastInteractionAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)},
			{ID: "legacy", LastAccessedAt: now.Format(time.RFC3339Nano), LastActivityAt: now.Format(time.RFC3339Nano)},
			{ID: "malformed", LastInteractionAt: "not-a-timestamp", LastActivityAt: now.Format(time.RFC3339Nano)},
			{ID: "zero", LastInteractionAt: time.Time{}.Format(time.RFC3339Nano)},
			{ID: "archived", Archived: true, LastInteractionAt: now.Format(time.RFC3339Nano)},
		},
		"other": {{ID: "newest", Title: "newest", Status: "running", LastInteractionAt: now.Add(-time.Nanosecond).Format(time.RFC3339Nano)}},
	}
	home.remoteSessionOrder = remoteOrder{"dev": {"work": {"old-pin", "child"}}}
	home.remoteGroupsCollapsed = map[string]bool{"remotes/dev": true, "remotes/dev/work": true, "remotes/other": true}
	remoteFolds := map[string]bool{"remotes/dev": true, "remotes/dev/work": true, "remotes/other": true}
	home.creatingSessions["recent-pending"] = &CreatingSession{ID: "recent-pending", Title: "pending", Tool: "claude", GroupPath: "alpha"}
	home.groupViewMode = session.GroupViewRecent
	home.rebuildFlatItemsAt(now)

	matrixAssertRecentRows(t, home, "other/newest", "local/a3", "dev/child", "local/b1", "dev/old-pin", "local/a1")
	if !reflect.DeepEqual(home.groupTree.Expanded, groupFolds) || !reflect.DeepEqual(home.remoteGroupsCollapsed, remoteFolds) {
		t.Fatal("recent view changed saved local or remote folds")
	}
	for path, group := range home.groupTree.Groups {
		if group.Expanded {
			t.Fatalf("recent view expanded group %q", path)
		}
	}
	if child.ParentSessionID == "" || matrixRemotePinInfo(t, home, "dev", "child").ParentSessionID != "unknown-parent" {
		t.Fatal("standalone presentation changed a persisted parent relationship")
	}
}

func TestMatrixRecentViewFiltersUseInteractionClockAndStayExact(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status session.Status
		time   session.TimeFilterMode
		scope  string
		want   []string
	}{
		{"local running recent", session.StatusRunning, session.TimeFilter7Days, "alpha", []string{"local/a1"}},
		{"local idle", session.StatusIdle, session.TimeFilterAll, "alpha", []string{"local/a2"}},
		{"all running recent", session.StatusRunning, session.TimeFilter7Days, "", []string{"local/a1", "dev/fresh-visit"}},
		{"remote running recent", session.StatusRunning, session.TimeFilter7Days, "remotes/dev/work", []string{"dev/fresh-visit"}},
		{"empty status", session.StatusStopped, session.TimeFilterAll, "", nil},
		{"empty time", session.StatusRunning, session.TimeFilter7Days, "beta", nil},
		{"archived partition", FilterModeArchived, session.TimeFilter7Days, "beta", []string{"local/b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			for _, inst := range home.instances {
				inst.Status = session.StatusRunning
				matrixRecentInteraction(t, inst, now.AddDate(0, 0, -30))
				matrixRecentBackgroundActivity(t, inst, now)
			}
			matrixRecentInteraction(t, matrixPinnedInstance(t, home, "a1"), now.Add(-2*time.Hour))
			idle := matrixPinnedInstance(t, home, "a2")
			idle.Status = session.StatusIdle
			matrixRecentInteraction(t, idle, now.Add(-time.Hour))
			// Keep the replica consistent with the idle status used by the filter.
			idle.ClearRuntimeSnapshot()
			archived := matrixPinnedInstance(t, home, "b2")
			archived.ArchivedAt = now
			matrixRecentInteraction(t, archived, now.Add(-time.Hour))
			home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {
				{ID: "fresh-visit", Group: "work", Status: "running", LastInteractionAt: now.Add(-3 * time.Hour).Format(time.RFC3339Nano), LastActivityAt: now.AddDate(0, 0, -30).Format(time.RFC3339Nano)},
				{ID: "background-only", Group: "work", Status: "running", LastInteractionAt: now.AddDate(0, 0, -30).Format(time.RFC3339Nano), LastActivityAt: now.Format(time.RFC3339Nano)},
			}}
			home.groupViewMode, home.statusFilter, home.timeFilter, home.groupScope = session.GroupViewRecent, tc.status, tc.time, tc.scope
			home.keepEmptyFilter = false
			home.rebuildFlatItemsAt(now)
			matrixAssertRecentRows(t, home, tc.want...)
			if home.groupViewMode != session.GroupViewRecent || home.statusFilter != tc.status || home.timeFilter != tc.time || home.groupScope != tc.scope {
				t.Fatalf("recent rebuild relaxed a filter: mode=%v status=%q time=%v scope=%q", home.groupViewMode, home.statusFilter, home.timeFilter, home.groupScope)
			}
		})
	}
}

func TestMatrixRecentViewActivityAndViewingDoNotMoveSessionButInteractionDoes(t *testing.T) {
	home := matrixPinnedHome(t)
	now := time.Now()
	older, newer := matrixPinnedInstance(t, home, "a1"), matrixPinnedInstance(t, home, "b1")
	matrixRecentInteraction(t, older, now.Add(-8*time.Hour))
	matrixRecentInteraction(t, newer, now.Add(-2*time.Hour))
	home.groupViewMode = session.GroupViewRecent
	home.rebuildFlatItems()
	matrixAssertRecentRows(t, home, "local/b1", "local/a1")
	matrixRecentBackgroundActivity(t, older, now)
	if !older.DisplayLastActivityTime().After(newer.LastInteractionAt()) {
		t.Fatal("test did not install newer background activity")
	}
	home.rebuildFlatItems()
	matrixAssertRecentRows(t, home, "local/b1", "local/a1")
	home.markSessionVisited(older)
	older.MarkAccessed() // Returning from a view is not a submitted message.
	home.rebuildFlatItems()
	matrixAssertRecentRows(t, home, "local/b1", "local/a1")
	matrixRecentInteraction(t, older, time.Now())
	home.rebuildFlatItems()
	matrixAssertRecentRows(t, home, "local/a1", "local/b1")
}

func TestMatrixRecentViewSortKeepsWindowBlocksAndStableTies(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	older := &session.Instance{ID: "older"}
	newer := &session.Instance{ID: "a"}
	tied := &session.Instance{ID: "z"}
	remote := session.RemoteSessionInfo{ID: "same-id", LastInteractionAt: at.Format(time.RFC3339Nano)}
	matrixRecentInteraction(t, older, at.Add(-time.Hour))
	matrixRecentInteraction(t, newer, at)
	matrixRecentInteraction(t, tied, at)
	items := []session.Item{
		{Type: session.ItemTypeSession, Session: older},
		{Type: session.ItemTypeWindow, WindowSessionID: older.ID, WindowID: "old-0"},
		{Type: session.ItemTypeWindow, WindowSessionID: older.ID, WindowID: "old-1"},
		{Type: session.ItemTypeRemoteSession, RemoteName: "z-host", RemoteSession: &remote},
		{Type: session.ItemTypeSession, Session: tied},
		{Type: session.ItemTypeSession, Session: newer},
		{Type: session.ItemTypeWindow, WindowSessionID: newer.ID, WindowID: "new-0"},
		{Type: session.ItemTypeRemoteSession, RemoteName: "a-host", RemoteSession: &remote},
	}
	rows := func(sorted []session.Item) []string {
		var out []string
		for _, item := range sorted {
			switch item.Type {
			case session.ItemTypeSession:
				out = append(out, "local/"+item.Session.ID)
			case session.ItemTypeRemoteSession:
				out = append(out, item.RemoteName+"/"+item.RemoteSession.ID)
			case session.ItemTypeWindow:
				out = append(out, "window/"+item.WindowID)
			}
		}
		return out
	}
	want := []string{"local/a", "window/new-0", "local/z", "a-host/same-id", "z-host/same-id", "local/older", "window/old-0", "window/old-1"}
	got := matrixSortRecentItems(items)
	if !reflect.DeepEqual(rows(got), want) || !reflect.DeepEqual(rows(matrixSortRecentItems(got)), want) {
		t.Fatalf("recent sorting lost window ownership or stable host ties: %v", rows(got))
	}
}

func TestMatrixRecentViewHotkeyStartsNewestAndPreservesSelectionOnExit(t *testing.T) {
	home := matrixPinnedHome(t)
	now := time.Now()
	for i, inst := range home.instances {
		matrixRecentInteraction(t, inst, now.Add(-time.Duration(i)*time.Hour))
	}
	home.rebuildFlatItems()
	selected := matrixPinnedInstance(t, home, "b1")
	home.cursor = sessionIndexByTitle(home, "b1")
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if home.groupViewMode != session.GroupViewRecent || home.cursor != 0 || home.flatItems[home.cursor].Session.Title != "a1" {
		t.Fatal("Ctrl+E did not enter recent at the newest session")
	}
	home.cursor = sessionIndexByTitle(home, "b1")
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if home.groupViewMode != session.GroupViewNormal || home.flatItems[home.cursor].Session == nil || home.flatItems[home.cursor].Session.ID != selected.ID {
		t.Fatal("Ctrl+E did not return to normal with the same selected session")
	}
	home = matrixPinnedHome(t) // A fleet without any confirmed interaction.
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if home.groupViewMode != session.GroupViewRecent || len(home.flatItems) != 0 || home.cursor != 0 {
		t.Fatal("a never-visited fleet did not produce an empty recent view")
	}
	for _, key := range []tea.KeyType{tea.KeyHome, tea.KeyEnd, tea.KeyUp, tea.KeyDown} {
		home.Update(tea.KeyMsg{Type: key})
		if home.groupViewMode != session.GroupViewRecent || len(home.flatItems) != 0 || home.cursor != 0 {
			t.Fatal("navigation escaped the empty recent view or left an invalid cursor")
		}
	}
	state, filtered := home.filteredEmptyState()
	if !filtered || !strings.Contains(state.Title, "Recent") || !strings.Contains(strings.Join(state.Hints, " "), "ctrl+e back to normal view") {
		t.Fatalf("empty recent view lacks its identity or exit hint: %+v", state)
	}
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if home.groupViewMode != session.GroupViewNormal || sessionIndexByTitle(home, "a1") < 0 {
		t.Fatal("Ctrl+E could not leave an empty recent view")
	}
}

func TestMatrixRecentViewPreferenceSurvivesRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	storage, err := session.NewStorageWithProfile("_matrix_recent_view")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	before, _ := buildTwoGroupHome(t)
	t.Cleanup(before.cancel)
	before.storage, before.profile, before.storageWatcher = storage, "_matrix_recent_view", nil
	matrixRecentInteraction(t, matrixPinnedInstance(t, before, "a1"), time.Now())
	before.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	raw, err := storage.GetDB().GetMeta("ui_state")
	if err != nil {
		t.Fatal(err)
	}
	var state uiState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.GroupViewMode != int(session.GroupViewRecent) {
		t.Fatalf("persisted group_view_mode=%v, want recent", state.GroupViewMode)
	}
	after, _ := buildTwoGroupHome(t)
	t.Cleanup(after.cancel)
	after.storage, after.profile, after.storageWatcher = storage, "_matrix_recent_view", nil
	matrixRecentInteraction(t, matrixPinnedInstance(t, after, "a1"), time.Now())
	after.loadUIState()
	after.rebuildFlatItems()
	if after.groupViewMode != session.GroupViewRecent {
		t.Fatalf("restart restored mode %v, want recent", after.groupViewMode)
	}
	matrixAssertRecentRows(t, after, "local/a1")
}

func TestMatrixRecentViewRowsShowInteractionAgeWithoutOrphanConnectors(t *testing.T) {
	home := matrixPinnedHome(t)
	accessed := time.Now().Add(-2*time.Hour - 17*time.Minute - 5*time.Second)
	local := matrixPinnedInstance(t, home, "a1")
	local.CreatedAt = time.Now()
	matrixRecentInteraction(t, local, accessed)
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"d": {{ID: "r", Title: "r", Status: "idle", LastInteractionAt: accessed.Format(time.RFC3339Nano), LastActivityAt: time.Now().Format(time.RFC3339Nano)}}}
	home.groupViewMode, home.showSessionTimestamps = session.GroupViewRecent, false
	home.rebuildFlatItems()
	for _, embedded := range []bool{false, true} {
		home.embeddedLayout = embedded
		for _, width := range []int{30, 80, 120} {
			view := stripAnsi(home.renderSessionList(width, 24))
			if strings.Contains(view, "├─") || strings.Contains(view, "└─") {
				t.Fatalf("recent rows drew orphan connectors (embedded=%v width=%d):\n%s", embedded, width, view)
			}
			if strings.Count(view, formatRelativeTime(accessed)) != 2 {
				t.Fatalf("recent rows do not show both visit ages (embedded=%v width=%d):\n%s", embedded, width, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if cellWidth(line) > width {
					t.Fatalf("recent row overflows %d columns: %q", width, line)
				}
			}
		}
	}
}
