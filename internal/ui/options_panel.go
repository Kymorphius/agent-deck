package ui

import tea "github.com/charmbracelet/bubbletea"

// OptionsPanel is the interface for tool-specific option panels in session dialogs.
// Implemented by ClaudeOptionsPanel and YoloOptionsPanel.
type OptionsPanel interface {
	Focus()
	Blur()
	IsFocused() bool
	AtTop() bool
	// AtBottom reports whether focus is on the panel's last control, so the
	// enclosing dialog knows when Tab/Enter/↓ should leave the panel instead
	// of moving within it.
	AtBottom() bool
	// FocusLast focuses the panel's last control, so a backward move (Shift+Tab
	// or ↑ from the row below) enters the panel at its bottom.
	FocusLast()
	// FocusedLine returns the zero-based logical line occupied by the focused
	// control in View, or -1 when the panel is not focused.
	FocusedLine() int
	// matrix/hardened: TextFocused reports whether one of the panel's text
	// fields currently has the focus, as opposed to a toggle or a picker. The
	// input-method policy needs it to tell typing from pure operation inside
	// the panel; the enclosing dialog only knows that the panel as a whole is
	// focused.
	TextFocused() bool
	Update(tea.Msg) tea.Cmd
	View() string
}
