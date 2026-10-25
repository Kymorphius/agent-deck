package ui

// matrix/hardened: Shift+number status views anchor on the selected project.
// When the selected row is filtered out, the cursor lands on that project's
// header (or nearest visible ancestor) instead of whatever row now sits at the
// old index, and the anchor keeps its screen row so the list does not jump.

import (
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type matrixProjectAnchor struct {
	path     string
	remote   string
	isRemote bool
}

func (h *Home) matrixCaptureProjectAnchor() matrixProjectAnchor {
	if h.cursor < 0 || h.cursor >= len(h.flatItems) {
		return matrixProjectAnchor{}
	}
	item := h.flatItems[h.cursor]
	switch item.Type {
	case session.ItemTypeRemoteGroup, session.ItemTypeRemoteSession:
		return matrixProjectAnchor{path: item.Path, remote: item.RemoteName, isRemote: true}
	case session.ItemTypeWindow:
		if item.Path == "" {
			if inst := h.getInstanceByID(item.WindowSessionID); inst != nil {
				return matrixProjectAnchor{path: inst.GroupPath}
			}
		}
	case session.ItemTypeSession:
		if item.Path == "" && item.Session != nil {
			return matrixProjectAnchor{path: item.Session.GroupPath}
		}
	}
	return matrixProjectAnchor{path: item.Path}
}

func (a matrixProjectAnchor) parent(path string) string {
	if a.isRemote {
		return remoteGroupParentPath(path)
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[:idx]
	}
	return ""
}

// matrixRestoreProjectAnchor prefers the project header, then any row inside
// the project, walking up to ancestor groups when the project has no matches.
func (h *Home) matrixRestoreProjectAnchor(a matrixProjectAnchor) bool {
	for p := a.path; p != ""; p = a.parent(p) {
		for i, item := range h.flatItems {
			header := (!a.isRemote && item.Type == session.ItemTypeGroup) ||
				(a.isRemote && item.Type == session.ItemTypeRemoteGroup && item.RemoteName == a.remote)
			if header && item.Path == p {
				h.cursor = i
				return true
			}
		}
		for i, item := range h.flatItems {
			row := (!a.isRemote && item.Type == session.ItemTypeSession) ||
				(a.isRemote && item.Type == session.ItemTypeRemoteSession && item.RemoteName == a.remote)
			if row && (item.Path == p || strings.HasPrefix(item.Path, p+"/")) {
				h.cursor = i
				return true
			}
		}
	}
	return false
}

func (h *Home) matrixRebuildAnchoringProject(identity selectedItemIdentity) {
	if h.groupViewMode == session.GroupViewRecent {
		h.matrixRebuildAfterViewChange(identity)
		return
	}
	anchor := h.matrixCaptureProjectAnchor()
	screenRow := max(0, h.cursor-h.viewOffset)
	h.rebuildFlatItems()
	if !h.restoreSelectedItemIdentity(identity) && !h.matrixRestoreProjectAnchor(anchor) && len(h.flatItems) > 0 {
		h.cursor = max(0, min(h.cursor, len(h.flatItems)-1))
	}
	h.viewOffset = max(0, h.cursor-screenRow)
	h.syncViewport()
}
