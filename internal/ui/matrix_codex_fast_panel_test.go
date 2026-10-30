package ui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// drain runs a command (and any batch) and feeds host results back.
func drainCodexFast(p *CodexFastPanel, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			drainCodexFast(p, c)
		}
	case codexFastHostMsg:
		p.handleHost(msg)
	}
}

func TestCodexFastPanelDryRunConfirmAndApply(t *testing.T) {
	var calls [][]string
	runner := func(target string) adoptRunFunc {
		return func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{target}, args...))
			if target == "old" {
				return nil, errors.New(`remote old ssh command failed: verb not allowed over the channel`)
			}
			if target == "broken" {
				return []byte("Error: unknown command"), errors.New("exit status 1")
			}
			apply := !strings.Contains(strings.Join(args, " "), "--dry-run")
			res := []session.CodexFastResult{
				{Title: target + "-busy", WasFast: false, WasRunning: true, Outcome: session.CodexFastPlanned},
				{Title: target + "-idle", WasFast: false, Outcome: session.CodexFastPlanned},
				{Title: target + "-draft", Outcome: session.CodexFastSkipped, Reason: "composer has an unsent draft"},
			}
			if apply {
				res[0].Outcome, res[0].Fast, res[0].Interrupted, res[0].Continued = session.CodexFastSwitched, true, true, true
				res[1].Outcome, res[1].Fast = session.CodexFastSwitched, true
			}
			out, _ := json.Marshal(map[string]any{"results": res, "summary": session.CodexFastSummary(res)})
			return out, nil
		}
	}
	p := NewCodexFastPanel()
	p.SetSize(140, 50)
	drainCodexFast(p, p.Open([]string{"", "pro", "old", "broken"}, runner))
	require.Len(t, calls, 4)
	for _, c := range calls {
		require.Contains(t, c, "--dry-run", "opening the panel must only read")
	}
	view := stripAnsi(p.View())
	require.Contains(t, view, "remote pro")
	require.Contains(t, view, "pro-busy")
	require.Contains(t, view, "skip: composer has an unsent draft")
	require.Contains(t, view, "remote old agent-deck there is too old for codex-fast; skipped", "an old remote is a quiet note")
	require.NotContains(t, view, "verb not allowed")
	require.Contains(t, view, "older than codex-fast", "any other unreadable reply keeps its error")

	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	view = stripAnsi(p.View())
	require.Contains(t, view, "2 running + 2 idle")
	require.Contains(t, view, "继续")

	calls = nil
	p, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	drainCodexFast(p, cmd)
	require.Len(t, calls, 4)
	for _, c := range calls {
		require.NotContains(t, c, "--dry-run")
		require.Equal(t, "on", c[2])
	}
	view = stripAnsi(p.View())
	require.Contains(t, view, "✓ switched, continued")
	require.Equal(t, "", p.running)

	// Declining sends nothing.
	calls = nil
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	p, cmd = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	require.Nil(t, cmd)
	require.Empty(t, calls)
}

func TestCodexFastHotkeyIsAltX(t *testing.T) {
	require.Equal(t, "alt+x", defaultHotkeyBindings[hotkeyCodexFast])
}
