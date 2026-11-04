package ui

import (
	"sort"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: only confirmed operator prompting advances the interaction clock.
type matrixRecentPromptResultMsg struct {
	instance *session.Instance
	target   *tmux.Session
	err      error
}

// matrix/hardened: aggregate views have no visible parent group to collapse.
func (h *Home) matrixStandaloneSessionView() bool {
	return h.groupViewMode == session.GroupViewPinnedOnly || h.groupViewMode == session.GroupViewRecent
}

// matrix/hardened: explicit navigation into Recent starts at the newest match.
// Refreshes still use identity restoration so background updates cannot move
// the user's selection. Other views retain their existing selection behavior.
func (h *Home) matrixRebuildAfterViewChange(identity selectedItemIdentity) {
	if h.groupViewMode != session.GroupViewRecent {
		h.rebuildFlatItemsPreservingSelection(identity)
		return
	}
	h.cursor, h.viewOffset = 0, 0
	h.pendingCursorRestore = nil
	// matrix/hardened: explicit view entry resets to newest; ordinary rebuilds
	// preserve identity, so bypass that policy for this deliberate navigation.
	h.rebuildFlatItemsAt(time.Now())
}

// matrix/hardened: Recent means submitted interaction, not viewing, background
// output or status transitions. Read the complete tree so folds cannot hide a chat.
func matrixRecentItemsFromTree(tree *session.GroupTree) []session.Item {
	if tree == nil {
		return nil
	}
	var items []session.Item
	for _, group := range tree.GroupList {
		if group == nil {
			continue
		}
		for _, inst := range group.Sessions {
			if inst != nil && !inst.LastInteractionAt().IsZero() {
				items = append(items, session.Item{Type: session.ItemTypeSession, Session: inst, Path: group.Path})
			}
		}
	}
	return items
}

// matrix/hardened: older remotes without a submitted-interaction time are unknown;
// their viewing/attach timestamp must not masquerade as actual input.
func matrixRemoteAccessTime(info session.RemoteSessionInfo) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, info.LastInteractionAt)
	return at
}

func (h *Home) matrixListActivityTime(inst *session.Instance) time.Time {
	if h.groupViewMode == session.GroupViewRecent { // matrix/hardened: use one clock for sorting and recency windows.
		return inst.LastInteractionAt()
	}
	return inst.DisplayLastActivityTime()
}

// matrix/hardened: merge host lists into one timeline, ignoring pins and manual
// group order. Sort blocks so expanded windows stay beside their owning session.
func matrixSortRecentItems(items []session.Item) []session.Item {
	type block struct {
		at   time.Time
		key  string
		rows []session.Item
	}
	var blocks []block
	for _, item := range items {
		switch {
		case item.Type == session.ItemTypeSession && item.Session != nil:
			blocks = append(blocks, block{item.Session.LastInteractionAt(), "local/" + item.Session.ID, []session.Item{item}})
		case item.Type == session.ItemTypeRemoteSession && item.RemoteSession != nil:
			blocks = append(blocks, block{matrixRemoteAccessTime(*item.RemoteSession), "remote/" + item.RemoteName + "/" + item.RemoteSession.ID, []session.Item{item}})
		case item.Type == session.ItemTypeWindow && len(blocks) > 0:
			last := &blocks[len(blocks)-1]
			if parent := last.rows[0].Session; parent != nil && parent.ID == item.WindowSessionID {
				last.rows = append(last.rows, item)
			}
		}
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].at.Equal(blocks[j].at) {
			return blocks[i].key < blocks[j].key
		}
		return blocks[i].at.After(blocks[j].at)
	})
	out := make([]session.Item, 0, len(items))
	for _, b := range blocks {
		out = append(out, b.rows...)
	}
	return out
}

func (h *Home) matrixRecentEmptyState() EmptyStateConfig {
	// matrix/hardened: explain zero history and composed filters without dropping
	// the selected recent view or pretending this is a new workspace.
	key := h.actionKey(hotkeyToggleRecentView)
	if key == "" {
		key = h.actionKey(hotkeyCycleGroupView)
	}
	hints := []string{}
	if key != "" {
		hints = append(hints, key+" back to normal view")
	}
	if h.statusFilter != "" {
		hints = append(hints, "0 clear status filter")
	}
	return EmptyStateConfig{
		Icon: "◷", Title: "最近交互 / Recent interactions", Subtitle: "No confirmed interactions matching these filters. Send a message in a session to see it here.",
		ShowSubtitleMinimal: true, ShowAllHintsMinimal: true, Hints: hints,
	}
}
