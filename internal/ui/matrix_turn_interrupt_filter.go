package ui

import (
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/lipgloss"
)

// matrix/hardened: a place to look for interrupted turns (see
// internal/session/matrix_turn_interrupt.go). The filter bar always carries a
// "↯ N" pill, dim at 0, and "~" lists only the sessions whose last turn stopped
// before it finished.

// FilterKeyInterrupted toggles the interrupted-turn filter.
const FilterKeyInterrupted = "~"

// FilterModeInterrupted is the filter value for interrupted turns (not a real
// session status).
const FilterModeInterrupted session.Status = "matrix-interrupted"

// matrixShowsTurnInterrupted is the one rule for "this row is an interrupted
// turn", shared by the row glyph, the pill count and the filter: the substate
// says so and the light is waiting or idle. A running row is left alone because
// the verdict is a few seconds old and a new turn may already be under way.
func matrixShowsTurnInterrupted(status session.Status, substate session.Substate) bool {
	return substate == session.SubstateTurnInterrupted &&
		(status == session.StatusWaiting || status == session.StatusIdle)
}

// matchesSessionFilter is matchesStatusFilter for a session, adding the
// interrupted-turn filter, which needs the substate as well as the status.
func (h *Home) matchesSessionFilter(filter session.Status, inst *session.Instance) bool {
	if filter != FilterModeInterrupted {
		return h.matchesStatusFilter(filter, inst.Status)
	}
	if inst.IsArchived() {
		return false
	}
	state, ok := h.getSessionRenderSnapshot()[inst.ID]
	if !ok {
		return matrixShowsTurnInterrupted(inst.GetStatusThreadSafe(), inst.CachedSubstate())
	}
	return matrixShowsTurnInterrupted(state.status, state.substate)
}

// matrixCountTurnInterrupted counts interrupted turns from the render snapshot
// (lock-free, like countSessionStatuses).
func (h *Home) matrixCountTurnInterrupted() int {
	n := 0
	for _, state := range h.getSessionRenderSnapshot() {
		if !state.matrixArchived && matrixShowsTurnInterrupted(state.status, state.substate) {
			n++
		}
	}
	return n
}

// matrixInterruptedPill renders the "↯ N" filter-bar pill: highlighted while
// the filter is on, red when there is something to see, dim at 0.
func (h *Home) matrixInterruptedPill(dim lipgloss.Style) string {
	label := fmt.Sprintf("↯ %d", h.matrixCountTurnInterrupted())
	switch {
	case h.statusFilter == FilterModeInterrupted:
		return lipgloss.NewStyle().Foreground(ColorBg).Background(ColorRed).Bold(true).Padding(0, 1).Render(label)
	case label != "↯ 0":
		return lipgloss.NewStyle().Foreground(ColorRed).Background(ColorSurface).Padding(0, 1).Render(label)
	default:
		return dim.Render(label)
	}
}

// matrixInterruptedEmptyState is the empty list under the interrupted filter.
func matrixInterruptedEmptyState() EmptyStateConfig {
	return EmptyStateConfig{
		Icon:                "↯",
		Title:               "No interrupted turns",
		Subtitle:            "A turn cut short by an API error or an abort shows here until you continue it",
		ShowSubtitleMinimal: true,
		ShowAllHintsMinimal: true,
		Hints:               []string{FilterKeyInterrupted + " or 0 show all"},
	}
}
