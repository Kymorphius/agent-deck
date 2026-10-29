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

// matrix/hardened: a remote row shows the remote's fast_mode verdict in both
// layouts; an older remote that omits it shows nothing.
func TestFastBadgeRendersOnRemoteRows(t *testing.T) {
	for _, fast := range []bool{false, true} {
		rs := session.RemoteSessionInfo{ID: "remote-fast", Title: "pro-burning", Tool: "codex", Status: "waiting", RemoteName: "pro", FastMode: fast}
		item := session.Item{Type: session.ItemTypeRemoteSession, RemoteSession: &rs, RemoteName: "pro", Level: 1, IsLastInGroup: true}
		for _, embedded := range []bool{false, true} {
			for _, selected := range []bool{false, true} {
				h := NewHome()
				h.width, h.height = 240, 40
				h.embeddedLayout = embedded
				var b strings.Builder
				h.renderRemoteSessionItemAtWidth(&b, item, selected, 60)
				row := stripAnsi(b.String())
				require.Contains(t, row, "pro-burning")
				want := "[fast]"
				if embedded {
					want = "fast pro-burning"
				}
				require.Equal(t, fast, strings.Contains(row, want), "embedded=%v selected=%v row=%q", embedded, selected, row)
			}
		}
	}
}
