package ui

// matrix/hardened: use explicit UI focus instead of trying to infer modes from
// terminal output. Text keeps the operator's IME; navigation uses raw keys.
import (
	"time"

	"github.com/asheshgoplani/agent-deck/internal/inputmethod"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: a switch that could not run yet is retried on our own clock.
// Waiting for the next keystroke cannot work in navigation mode, because the
// IME consumes that key for composition before the TUI ever sees it. The
// bounded burst covers window activation; the 2s tickMsg chain then keeps the
// desired mode eventually consistent.
const (
	inputMethodRetryDelay = 150 * time.Millisecond
	inputMethodRetryBurst = 12
)

// inputMethodRetryMsg re-runs an unapplied switch without touching the model.
type inputMethodRetryMsg struct{}

type inputMethodController interface {
	// Sync reports whether the requested mode is in effect. reassert asks for
	// the text-input policy to be applied again, which only a real return of
	// keyboard focus does; plain typing must not override a source the
	// operator picked while editing.
	Sync(navigation bool, reassert bool) bool
	Restore()
	Blur()
}

// matrix/hardened: injectable without touching the desktop in startup tests.
var createInputMethodController = func(textPolicy string) inputMethodController {
	c := inputmethod.New(textPolicy)
	if c == nil {
		return nil
	}
	return c
}

// ConfigureInputMethod is called only on the interactive CLI startup path.
// Terminals report focus changes, not necessarily their initial focus state.
// Start with keyboard focus; the native backend still requires the owning app
// to be foreground, and subsequent terminal blur reports revoke this focus.
func (h *Home) ConfigureInputMethod() bool {
	config, err := session.LoadUserConfig()
	if err != nil {
		inputmethod.Trace("not configured: reading the user config failed: %v", err)
		return false
	}
	if !config.UI.AutoInputMethod {
		inputmethod.Trace("not configured: ui.auto_input_method is off")
		return false
	}
	controller := createInputMethodController(config.UI.InputMethodTextSource)
	if controller == nil {
		// The native backend already traced why it is unavailable.
		inputmethod.Trace("not configured: no native controller")
		return false
	}
	h.inputMethod = controller
	h.inputMethodFocused = true
	inputmethod.Trace("configured: text policy=%q, assuming keyboard focus until a blur report",
		config.UI.InputMethodTextSource)
	return true
}

// RestoreInputMethod is safe from both normal exit and the signal cleanup path.
func (h *Home) RestoreInputMethod() {
	if h.inputMethod != nil {
		h.inputMethod.Restore()
	}
}

// syncInputMethod reconciles the native input source with the current UI focus.
// It returns a command when the desired mode is not in effect yet, so the TUI
// retries on its own instead of waiting for input that the IME may swallow.
func (h *Home) syncInputMethod(msg tea.Msg) tea.Cmd {
	if h.inputMethod == nil {
		return nil
	}
	force, verify, reassert := false, false, false
	switch msg.(type) {
	case tea.FocusMsg:
		// Keyboard focus came back to this terminal tab: whatever another app
		// or tab left behind, this view decides the mode again.
		h.inputMethodFocused, force, reassert = true, true, true
	case tea.KeyMsg:
		h.inputMethodFocused = true
		force = true
	case tea.MouseMsg:
		// Hover reports do not prove keyboard focus (iTerm reports them even
		// for an inactive pane); a click does.
		if mouse := msg.(tea.MouseMsg); mouse.Action == tea.MouseActionPress && mouse.Button == tea.MouseButtonLeft {
			h.inputMethodFocused = true
			force = true
		}
	case tea.BlurMsg:
		h.inputMethodFocused = false
		h.inputMethod.Blur()
	case inputMethodRetryMsg:
		h.inputMethodRetryQueued = false
	case tickMsg:
		// The source can also change without any event reaching us: the
		// operator switches it by hand, another app restores its own
		// preference, or a sibling tab takes over. Re-check on the regular
		// tick so the list cannot sit in a composing IME indefinitely.
		verify = true
	}
	if !h.inputMethodFocused {
		return nil
	}
	navigation := h.inputMethodNavigation()
	if force || navigation != h.inputMethodNav {
		// A real event or a mode change earns a fresh retry budget.
		h.inputMethodRetries = 0
	}
	// Output/refresh frames still do no native work while the desired mode is
	// already in effect.
	if !force && !verify && h.inputMethodApplied && navigation == h.inputMethodNav {
		return nil
	}
	inputmethod.Trace("ui sync msg=%T nav=%v force=%v verify=%v reassert=%v applied=%v retries=%d",
		msg, navigation, force, verify, reassert, h.inputMethodApplied, h.inputMethodRetries)
	applied := h.inputMethod.Sync(navigation, reassert)
	h.inputMethodNav, h.inputMethodApplied = navigation, applied
	if applied {
		h.inputMethodRetries = 0
		return nil
	}
	if h.inputMethodRetryQueued || h.inputMethodRetries >= inputMethodRetryBurst {
		return nil
	}
	h.inputMethodRetries++
	h.inputMethodRetryQueued = true
	return tea.Tick(inputMethodRetryDelay, func(time.Time) tea.Msg { return inputMethodRetryMsg{} })
}

func (h *Home) inputMethodNavigation() bool {
	if h.isQuitting || h.isAttaching.Load() {
		return false
	}
	// Modal focus has priority over the session's insert mode underneath it.
	if h.search != nil && h.search.IsVisible() ||
		h.groupSearch != nil && h.groupSearch.IsVisible() || // matrix/hardened: group names are text input.
		h.globalSearch != nil && h.globalSearch.IsVisible() ||
		h.promptInputDialog != nil && h.promptInputDialog.IsVisible() ||
		h.zoxidePicker != nil && h.zoxidePicker.IsVisible() {
		return false
	}
	if d := h.newDialog; d != nil && d.IsVisible() {
		return !d.isTextInputFocused()
	}
	if d := h.groupDialog; d != nil && d.IsVisible() {
		return d.mode == GroupDialogMove || d.mode == GroupDialogCreate && d.focusIndex == 2
	}
	if d := h.editSessionDialog; d != nil && d.IsVisible() {
		return d.focusIndex < 0 || d.focusIndex >= len(d.fields) || d.fields[d.focusIndex].kind != editFieldText
	}
	if d := h.forkDialog; d != nil && d.IsVisible() {
		switch d.currentFocus() {
		case forkFocusName, forkFocusGroup, forkFocusBranch:
			return false
		default:
			return true
		}
	}
	if d := h.editPathsDialog; d != nil && d.IsVisible() {
		return !d.IsEditing()
	}
	if p := h.settingsPanel; p != nil && p.IsVisible() {
		return !p.editingText
	}
	if p := h.setupWizard; p != nil && p.IsVisible() {
		return !p.configDirInput.Focused()
	}
	if p := h.kanbanPanel; p != nil && p.IsVisible() {
		return !p.filterEditing
	}
	if h.notesEditing {
		return false
	}
	// These overlays consume shortcuts even if an embedded session is active.
	if h.helpOverlay != nil && h.helpOverlay.IsVisible() ||
		h.confirmDialog != nil && h.confirmDialog.IsVisible() ||
		h.hookTrustDialog != nil && h.hookTrustDialog.IsVisible() ||
		h.sessionSwitcher != nil && h.sessionSwitcher.IsVisible() ||
		h.scrollbackPager != nil && h.scrollbackPager.IsVisible() ||
		h.contextPager != nil && h.contextPager.IsVisible() ||
		h.sessionPickerDialog != nil && h.sessionPickerDialog.IsVisible() ||
		h.codeBlockDialog != nil && h.codeBlockDialog.IsVisible() ||
		h.mcpDialog != nil && h.mcpDialog.IsVisible() ||
		h.pluginDialog != nil && h.pluginDialog.IsVisible() ||
		h.skillDialog != nil && h.skillDialog.IsVisible() ||
		h.geminiModelDialog != nil && h.geminiModelDialog.IsVisible() ||
		h.worktreeFinishDialog != nil && h.worktreeFinishDialog.IsVisible() ||
		h.watcherPanel != nil && h.watcherPanel.IsVisible() ||
		h.agentsPanel != nil && h.agentsPanel.IsVisible() ||
		h.adoptPanel != nil && h.adoptPanel.IsVisible() ||
		h.webServicePanel != nil && h.webServicePanel.IsVisible() ||
		h.deadLetterPanel != nil && h.deadLetterPanel.IsVisible() ||
		h.toolVisibilityPanel != nil && h.toolVisibilityPanel.IsVisible() {
		return true
	}
	return !h.insertMode
}
