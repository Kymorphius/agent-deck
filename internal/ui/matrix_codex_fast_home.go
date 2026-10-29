package ui

// matrix/hardened: Home-side glue for the alt+x Codex fast panel. Kept out of
// home.go so upstream merges only meet one-line hooks there.

import tea "github.com/charmbracelet/bubbletea"

func (h *Home) openCodexFastPanel() tea.Cmd {
	if h.codexFastPanel == nil {
		h.codexFastPanel = NewCodexFastPanel()
	}
	h.codexFastPanel.SetSize(h.width, h.height)
	return h.codexFastPanel.Open(codexFastTargets(), h.adoptRunner)
}

// handleCodexFastMsg consumes the panel's async results. ok is false for any
// other message.
func (h *Home) handleCodexFastMsg(msg tea.Msg) (tea.Cmd, bool) {
	m, ok := msg.(codexFastHostMsg)
	if !ok {
		return nil, false
	}
	if h.codexFastPanel != nil {
		h.codexFastPanel.handleHost(m)
	}
	return nil, true
}
