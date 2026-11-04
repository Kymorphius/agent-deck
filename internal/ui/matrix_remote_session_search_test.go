package ui

// matrix/hardened: session search keeps local and remote identities separate,
// even when all registries contain the same ID and title.
import (
	"fmt"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func matrixRemoteSearchHome(t *testing.T) *Home {
	h := newMatrixLocalSearchMatchingHome(t, 120, []*session.Instance{{ID: "shared", Title: "共同古琴", GroupPath: "team/course", Tool: "codex", Status: session.StatusIdle}})
	h.setHotkeys(resolveHotkeys(nil))
	now := time.Now().Format(time.RFC3339Nano)
	pin := session.PinTop
	h.remoteSessions = map[string][]session.RemoteSessionInfo{}
	for _, host := range []string{"east", "west"} {
		h.remoteSessions[host] = []session.RemoteSessionInfo{{ID: "shared", Title: "共同古琴", Group: "team/course", Tool: "claude", Path: "/work/course", Status: "idle", LastActivityAt: now, LastAccessedAt: now, LastInteractionAt: now, Pin: &pin}}
		for _, path := range []string{"remotes/" + host, "remotes/" + host + "/team", "remotes/" + host + "/team/course"} {
			h.remoteGroupsCollapsed[path] = true
		}
	}
	h.rebuildFlatItems()
	return h
}

func TestMatrixRemoteSessionSearch_IdentityAndLocate(t *testing.T) {
	h := matrixRemoteSearchHome(t)
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "古琴")
	if len(h.search.results) != 3 {
		t.Fatalf("got %d matches, want local plus both remotes", len(h.search.results))
	}
	frame := ansi.Strip(h.search.View())
	if !strings.Contains(frame, "east") || !strings.Contains(frame, "west") {
		t.Fatal("remote origins are not shown")
	}
	for i := range h.search.results {
		h.search.cursor = i
		if h.search.SelectedItem().RemoteName == "west" {
			break
		}
	}
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.isAttaching.Load() || h.embeddedMode {
		t.Fatal("search scheduled session startup or attach")
	}
	row := h.flatItems[h.cursor]
	if row.Type != session.ItemTypeRemoteSession || row.RemoteName != "west" || row.RemoteSession.ID != "shared" {
		t.Fatalf("wrong origin located: %+v", row)
	}
	for _, path := range []string{"remotes/west", "remotes/west/team", "remotes/west/team/course"} {
		if h.remoteGroupsCollapsed[path] {
			t.Fatalf("ancestor %s folded", path)
		}
	}
	if !h.remoteGroupsCollapsed["remotes/east"] {
		t.Fatal("other host was unfolded")
	}
}

func TestMatrixRemoteSessionSearch_StaleIdentityDoesNotChangeView(t *testing.T) {
	for _, change := range []string{"deleted", "moved", "renamed"} {
		t.Run(change, func(t *testing.T) {
			h := matrixRemoteSearchHome(t)
			h.remoteGroupsCollapsed["remotes/west"] = false
			h.remoteGroupsCollapsed["remotes/west/team"] = false
			h.remoteGroupsCollapsed["remotes/west/team/course"] = false
			h.rebuildFlatItems()
			for i, row := range h.flatItems {
				if row.Type == session.ItemTypeRemoteSession && row.RemoteName == "west" {
					h.cursor = i
					break
				}
			}
			h.openInGroupSearch()
			h.search.input.SetValue("古琴")
			h.search.updateResults()
			if len(h.search.results) != 1 {
				t.Fatalf("remote exact-group matches=%d", len(h.search.results))
			}
			if change == "deleted" {
				delete(h.remoteSessions, "west")
			} else if change == "moved" {
				h.remoteSessions["west"][0].Group = "elsewhere"
			} else {
				h.remoteSessions["west"][0].Title = "new title"
			}
			// Renaming an existing identity remains valid; a deleted/moved scoped row does not.
			before, cursor := slices.Clone(h.flatItems), h.cursor
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil {
				t.Fatal("locator scheduled remote work")
			}
			if change != "renamed" && (h.err == nil || h.cursor != cursor || !reflect.DeepEqual(before, h.flatItems)) {
				t.Fatal("stale scoped identity changed view")
			}
			if change == "renamed" && (h.err != nil || h.flatItems[h.cursor].RemoteSession.Title != "new title") {
				t.Fatal("latest metadata was not used")
			}
		})
	}
}

func TestMatrixRemoteSessionSearch_RefreshPreservesQueryScopeAndSelection(t *testing.T) {
	h := matrixRemoteSearchHome(t)
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "古琴")
	h.search.cursor = 2
	selected := h.search.SelectedItem()
	if selected.RemoteName != "west" {
		t.Fatal("fixture selection")
	}
	// Exercise the actual local registry reload message; it must not wash away
	// remote candidates while retaining the local search's typed identity.
	_, _ = h.Update(loadSessionsMsg{instances: []*session.Instance{{ID: "new-local", Title: "共同古琴", Tool: "shell", GroupPath: "team/course"}}})
	if h.search.input.Value() != "古琴" || len(h.search.results) != 3 || h.search.SelectedItem().RemoteName != "west" {
		t.Fatal("local reload lost remote corpus/query/selection")
	}
	// A real remote message inserts a match before the selected one.
	east := append([]session.RemoteSessionInfo{{ID: "added", Title: "共同古琴", Group: "team/course", Status: "idle"}}, h.remoteSessions["east"]...)
	_, _ = h.applyRemoteFetch(remoteSessionsFetchedMsg{sessions: map[string][]session.RemoteSessionInfo{"east": east, "west": h.remoteSessions["west"]}})
	if h.search.input.Value() != "古琴" || len(h.search.results) != 4 || h.search.SelectedItem().RemoteName != "west" || h.search.SelectedItem().Instance.ID != "shared" {
		t.Fatal("remote fetch reset the selected origin or query")
	}
	if len(h.instances) != 1 || h.instances[0].ID != "new-local" {
		t.Fatal("remote projection entered local registry")
	}
	// A scoped refresh must keep its exact host and group.
	h.search.SetRemoteGroup("west", "team/course")
	h.setSessionSearchItems()
	_, _ = h.applyRemoteFetch(remoteSessionsFetchedMsg{sessions: map[string][]session.RemoteSessionInfo{"east": east, "west": h.remoteSessions["west"]}})
	if len(h.search.results) != 1 || h.search.SelectedItem().RemoteName != "west" || h.search.input.Value() != "古琴" || h.search.scopedRemote != "west" {
		t.Fatal("fetch escaped exact remote group scope")
	}
}

func TestMatrixRemoteSessionSearch_ViewScopeMatchesRemoteRules(t *testing.T) {
	for _, kind := range []string{"status", "archived", "time", "unknown-time", "recent", "pinned", "scope"} {
		t.Run(kind, func(t *testing.T) {
			h := matrixRemoteSearchHome(t)
			row := h.remoteSessions["west"][0]
			switch kind {
			case "status":
				h.statusFilter = session.StatusRunning
				row.Status = "running"
			case "archived":
				h.statusFilter = FilterModeArchived
				row.Archived = true
			case "time":
				h.timeFilter = session.TimeFilter7Days
				h.remoteSessions["east"][0].LastActivityAt = time.Now().AddDate(0, 0, -60).Format(time.RFC3339Nano)
			case "unknown-time":
				h.timeFilter = session.TimeFilter7Days
				row.LastActivityAt = ""
				h.remoteSessions["east"][0].LastActivityAt = time.Now().AddDate(0, 0, -60).Format(time.RFC3339Nano)
			case "recent":
				h.groupViewMode = session.GroupViewRecent
				h.remoteSessions["east"][0].LastInteractionAt = ""
			case "pinned":
				h.groupViewMode = session.GroupViewPinnedOnly
				h.remoteSessions["east"][0].Pin = nil
			case "scope":
				h.groupScope = "remotes/west/team"
			}
			h.remoteSessions["west"][0] = row
			h.keepEmptyFilter = true
			h.rebuildFlatItems()
			h.openViewSearch()
			h.search.input.SetValue("古琴")
			h.search.updateResults()
			found := false
			for i := range h.search.results {
				h.search.cursor = i
				item := h.search.SelectedItem()
				if item.RemoteName == "east" {
					t.Fatal("view search leaked ineligible east session")
				}
				if item.RemoteName == "west" {
					found = true
				}
			}
			if !found {
				t.Fatal("view search omitted eligible west session")
			}
			for i := range h.search.results {
				h.search.cursor = i
				if h.search.SelectedItem().RemoteName == "west" {
					break
				}
			}
			before := matrixSearchScopeState(h)
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || h.flatItems[h.cursor].RemoteName != "west" || matrixSearchScopeState(h) != before {
				t.Fatal("view locator changed scope or started remote work")
			}
		})
	}
}

func TestMatrixRemoteSessionSearch_GroupHeaderAndDefaultGroup(t *testing.T) {
	for _, fromSession := range []bool{false, true} {
		t.Run(fmt.Sprint(fromSession), func(t *testing.T) {
			h := matrixRemoteSearchHome(t)
			h.remoteSessions["west"][0].Group = ""
			for _, path := range []string{"remotes/west", "remotes/west/" + session.DefaultGroupPath} {
				delete(h.remoteGroupsCollapsed, path)
			}
			h.remoteSessions["west"] = append(h.remoteSessions["west"], session.RemoteSessionInfo{ID: "child", Title: "共同古琴", Group: session.DefaultGroupPath + "/child"})
			h.rebuildFlatItems()
			found := false
			for i, row := range h.flatItems {
				if row.RemoteName != "west" {
					continue
				}
				if fromSession && row.Type == session.ItemTypeRemoteSession && row.RemoteSession.ID == "shared" || !fromSession && row.Type == session.ItemTypeRemoteGroup && row.Path == "remotes/west/"+session.DefaultGroupPath {
					h.cursor = i
					found = true
					break
				}
			}
			if !found {
				t.Fatal("fixture missing row")
			}
			h.openInGroupSearch()
			h.search.input.SetValue("古琴")
			h.search.updateResults()
			if len(h.search.results) != 1 || h.search.SelectedItem().RemoteName != "west" || h.search.SelectedItem().Instance.ID != "shared" {
				t.Fatal("remote group search leaked child/other host/local")
			}
		})
	}
}

func TestMatrixRemoteSessionSearch_StaleViewMembershipDoesNotChangePresentation(t *testing.T) {
	for _, change := range []string{"status", "archive", "scope", "removed"} {
		t.Run(change, func(t *testing.T) {
			h := matrixRemoteSearchHome(t)
			h.statusFilter = session.StatusIdle
			h.groupScope = "remotes/west/team"
			h.keepEmptyFilter = true
			h.rebuildFlatItems()
			h.openViewSearch()
			h.search.input.SetValue("古琴")
			h.search.updateResults()
			if len(h.search.results) != 1 {
				t.Fatal("fixture must select west alone")
			}
			switch change {
			case "status":
				h.remoteSessions["west"][0].Status = "running"
			case "archive":
				h.remoteSessions["west"][0].Archived = true
			case "scope":
				h.remoteSessions["west"][0].Group = "elsewhere"
			case "removed":
				delete(h.remoteSessions, "west")
			}
			before, cursor, view := slices.Clone(h.flatItems), h.cursor, matrixSearchScopeState(h)
			folds := maps.Clone(h.remoteGroupsCollapsed)
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || h.err == nil || h.cursor != cursor || !reflect.DeepEqual(before, h.flatItems) || matrixSearchScopeState(h) != view || !maps.Equal(folds, h.remoteGroupsCollapsed) {
				t.Fatalf("stale result cmd=%T err=%v cursor=%d/%d rowsEqual=%v view=%+v/%+v folds=%v/%v", cmd, h.err, h.cursor, cursor, reflect.DeepEqual(before, h.flatItems), matrixSearchScopeState(h), view, h.remoteGroupsCollapsed, folds)
			}
		})
	}
}

func TestMatrixRemoteSessionSearch_HostHeaderExplainsAllSessionFallback(t *testing.T) {
	h := matrixRemoteSearchHome(t)
	for i, row := range h.flatItems {
		if row.Type == session.ItemTypeRemoteGroup && row.Path == "remotes/west" {
			h.cursor = i
			break
		}
	}
	h.openInGroupSearch()
	if !h.search.IsVisible() || h.search.scopedRemote != "" || h.search.scopedGroup != "" || !strings.Contains(h.search.notice, "全部本机与远程") || len(h.search.results) != 3 {
		t.Fatal("host-only header must explain the all-session fallback")
	}
}

func TestMatrixRemoteSessionSearch_AllScopeRevealsArchivedRemote(t *testing.T) {
	h := matrixRemoteSearchHome(t)
	h.remoteSessions["west"][0].Archived = true
	h.statusFilter = session.StatusRunning
	h.timeFilter = session.TimeFilter7Days
	h.groupViewMode = session.GroupViewRecent
	h.groupScope = "unrelated"
	h.rebuildFlatItems()
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "古琴")
	if len(h.search.results) != 3 {
		t.Fatal("all-session search omitted archived or hidden sessions")
	}
	for i := range h.search.results {
		h.search.cursor = i
		if h.search.SelectedItem().RemoteName == "west" {
			break
		}
	}
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	row := h.flatItems[h.cursor]
	if cmd != nil || row.Type != session.ItemTypeRemoteSession || row.RemoteName != "west" || h.statusFilter != FilterModeArchived || h.timeFilter != session.TimeFilterAll || h.groupViewMode != session.GroupViewNormal || h.groupScope != "" || !h.remoteSessions["west"][0].Archived {
		t.Fatal("all-scope locator failed to reveal archived remote without changing archive data")
	}
}
