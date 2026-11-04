package ui

// matrix/hardened: search scope controls candidates independently from the
// presentation that must reveal the selected session.
import (
	"sort"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: search reads the already-loaded remote snapshot, without
// polling SSH or admitting remote projections to the local registry/storage.
func (h *Home) setSessionSearchItems() {
	h.instancesMu.RLock()
	items := make([]SessionSearchItem, 0, len(h.instances))
	for _, inst := range h.instances {
		items = append(items, SessionSearchItem{Instance: inst})
	}
	h.instancesMu.RUnlock()
	h.remoteSessionsMu.RLock()
	names := make([]string, 0, len(h.remoteSessions))
	for name := range h.remoteSessions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, info := range h.remoteSessions[name] {
			items = append(items, remoteSessionSearchItem(name, info))
		}
	}
	h.remoteSessionsMu.RUnlock()
	h.search.SetCandidates(items)
}

func remoteSessionSearchItem(name string, info session.RemoteSessionInfo) SessionSearchItem {
	// The projection is only fuzzy-match/render metadata; lifecycle operations
	// always resolve the typed origin against the latest remote snapshot.
	inst := &session.Instance{ID: info.ID, Title: info.Title, Tool: info.Tool, ProjectPath: info.Path, GroupPath: normalizeRemoteGroupPath(info.Group), Status: session.Status(info.Status)}
	return SessionSearchItem{Instance: inst, RemoteName: name, Remote: &info}
}

func (h *Home) openSessionSearch() {
	h.search.SetScopedGroup("")
	h.search.SetSize(h.width, h.height)
	h.setSessionSearchItems()
	h.search.Show()
}

func sessionSearchGroupPath(inst *session.Instance) string {
	if inst.GroupPath == "" {
		return session.DefaultGroupPath
	}
	return inst.GroupPath
}

func (h *Home) openViewSearch() {
	// Capture the chosen filters, not the rows currently exposed by folds.
	status, window, mode, scope := h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope
	labels := []string{h.groupViewMode.Label(), h.statusFilterLabel()}
	if window != session.TimeFilterAll {
		labels = append(labels, window.Label())
	}
	if scope != "" {
		labels = append(labels, scope)
	}
	localFilter := func(inst *session.Instance) bool {
		if inst == nil || !pathInScope(sessionSearchGroupPath(inst), scope) || inst.IsArchived() != (status == FilterModeArchived) {
			return false
		}
		if status != "" && status != FilterModeArchived && !h.matchesSessionFilter(status, inst) {
			return false
		}
		if mode == session.GroupViewPinnedOnly && !matrixSessionIsPinned(inst) {
			return false
		}
		activity := inst.DisplayLastActivityTime()
		if mode == session.GroupViewRecent {
			activity = inst.LastInteractionAt()
			if activity.IsZero() {
				return false
			}
		}
		return window.Matches(activity, time.Now())
	}
	h.search.SetCandidateViewScope(strings.Join(labels, " · "), func(item SessionSearchItem) bool {
		if item.RemoteName == "" {
			return localFilter(item.Instance)
		}
		info := item.Remote
		if info == nil || info.Archived != (status == FilterModeArchived) || !pathInScope("remotes/"+item.RemoteName+"/"+normalizeRemoteGroupPath(info.Group), scope) || !h.matrixMatchesRemoteFilter(status, *info) {
			return false
		}
		if mode == session.GroupViewPinnedOnly && !matrixRemoteSessionIsPinned(*info) {
			return false
		}
		activity, known := info.LastActivity()
		if mode == session.GroupViewRecent {
			activity = matrixRemoteAccessTime(*info)
			known = !activity.IsZero()
			if !known {
				return false
			}
		}
		return !known || window.Matches(activity, time.Now())
	})
	h.search.SetSize(h.width, h.height)
	h.setSessionSearchItems()
	h.search.Show()
}

func (h *Home) locateSearchSession(selected *session.Instance, preserveView bool) bool {
	if selected == nil || selected.ID == "" || h.groupTree == nil {
		return false
	}
	// A reload can replace or remove the selected object while search is open.
	var target *session.Instance
	h.instancesMu.RLock()
	for _, inst := range h.instances {
		if inst != nil && inst.ID == selected.ID {
			target = inst
			break
		}
	}
	h.instancesMu.RUnlock()
	if target == nil {
		return false
	}
	if preserveView {
		if !h.search.matchesView(SessionSearchItem{Instance: target}) {
			return false
		}
	} else {
		if h.search.IsVisible() && h.search.scopedGroup != "" && (h.search.scopedRemote != "" || sessionSearchGroupPath(target) != h.search.scopedGroup) {
			return false
		}
		h.statusFilter = ""
		h.keepEmptyFilter = false
		if target.IsArchived() {
			h.statusFilter = FilterModeArchived
		}
		h.timeFilter = session.TimeFilterAll
		h.groupViewMode = session.GroupViewNormal
		h.groupScopeMu.Lock()
		h.groupScope = ""
		h.groupScopeMu.Unlock()
	}
	h.groupTree.ExpandGroupWithParents(sessionSearchGroupPath(target))
	h.rebuildFlatItems()
	if index := h.flatItemIndexByID(target.ID); index >= 0 {
		h.jumpToIndex(index)
		h.saveGroupState()
		h.saveUIState()
		return true
	}
	return false
}

// matrix/hardened: remote selection is a locator only. Revalidate origin and
// scope before changing presentation, then reveal the current snapshot row.
func (h *Home) locateSearchItem(selected SessionSearchItem, preserveView bool) bool {
	if selected.RemoteName == "" {
		return h.locateSearchSession(selected.Instance, preserveView)
	}
	if selected.Instance == nil || selected.Instance.ID == "" {
		return false
	}
	var target *session.RemoteSessionInfo
	h.remoteSessionsMu.RLock()
	for _, info := range h.remoteSessions[selected.RemoteName] {
		if info.ID == selected.Instance.ID {
			copy := info
			target = &copy
			break
		}
	}
	h.remoteSessionsMu.RUnlock()
	if target == nil {
		return false
	}
	current := remoteSessionSearchItem(selected.RemoteName, *target)
	if preserveView {
		if !h.search.matchesView(current) {
			return false
		}
	} else if h.search.IsVisible() && h.search.scopedGroup != "" && (h.search.scopedRemote != selected.RemoteName || h.search.scopedGroup != normalizeRemoteGroupPath(target.Group)) {
		return false
	}
	if !preserveView {
		h.statusFilter = ""
		if target.Archived {
			h.statusFilter = FilterModeArchived
		}
		h.keepEmptyFilter = false
		h.timeFilter = session.TimeFilterAll
		h.groupViewMode = session.GroupViewNormal
		h.groupScopeMu.Lock()
		h.groupScope = ""
		h.groupScopeMu.Unlock()
	}
	if h.embeddedLayout {
		h.sidebarMode = sidebarGrouped
	}
	root := "remotes/" + selected.RemoteName
	for path := root + "/" + normalizeRemoteGroupPath(target.Group); ; path = groupParentPath(path) {
		delete(h.remoteGroupsCollapsed, path)
		if path == root {
			break
		}
	}
	h.rebuildFlatItems()
	for index, item := range h.flatItems {
		if item.Type == session.ItemTypeRemoteSession && item.RemoteName == selected.RemoteName && item.RemoteSession != nil && item.RemoteSession.ID == target.ID {
			h.jumpToIndex(index)
			h.saveGroupState()
			h.saveUIState()
			return true
		}
	}
	return false
}
