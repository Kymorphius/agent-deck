package ui

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/stretchr/testify/require"
)

// matrix/hardened: a Claude or Codex session in /fast shows a "fast" tag on its row in
// both layouts, selected or not, and the tag never pushes the title out.
func TestFastBadgeRendersOnClaudeAndCodexRows(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) { checkFastBadge(t, tool) })
	}
}

func checkFastBadge(t *testing.T, tool string) {
	inst := &session.Instance{ID: "fast-row", Title: "burning-tokens", Tool: tool, Status: session.StatusIdle}
	item := session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "work", IsLastInGroup: true}
	for _, fast := range []bool{false, true} {
		state := sessionRenderState{status: session.StatusIdle, tool: tool, title: inst.Title, fastMode: fast}
		h := NewHome()
		h.width, h.height = 240, 40
		for _, selected := range []bool{false, true} {
			var b strings.Builder
			h.renderSessionItem(&b, item, selected, map[string]sessionRenderState{inst.ID: state}, 240)
			row := stripAnsi(b.String())
			require.Contains(t, row, "burning-tokens")
			require.Equal(t, fast, strings.Contains(row, "[fast]"), "classic row selected=%v", selected)

			b.Reset()
			h.renderEmbeddedSessionItem(&b, item, selected, state, 48)
			card := stripAnsi(b.String())
			require.Contains(t, card, "burning-tokens")
			require.Equal(t, fast, strings.Contains(card, "fast burning-tokens"), "embedded card selected=%v", selected)
		}
	}
}
