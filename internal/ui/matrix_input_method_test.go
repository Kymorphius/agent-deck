package ui

// matrix/hardened: model-level focus tests use a fake native controller.
import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/inputmethod"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type fakeInputMethod struct {
	modes     []bool
	reasserts []bool
	// matrix/hardened: deny reproduces a switch that cannot run yet (the
	// terminal app is not foreground), which must stay retryable.
	deny  bool
	blurs int
}

func (f *fakeInputMethod) Sync(nav bool, reassert bool) bool {
	f.modes = append(f.modes, nav)
	f.reasserts = append(f.reasserts, reassert)
	return !f.deny
}
func (f *fakeInputMethod) Restore() {}
func (f *fakeInputMethod) Blur()    { f.blurs++ }

func TestInputMethodModes(t *testing.T) {
	h := &Home{}
	check := func(want bool) {
		t.Helper()
		if got := h.inputMethodNavigation(); got != want {
			t.Fatalf("navigation=%v want=%v", got, want)
		}
	}
	check(true)
	h.insertMode = true
	check(false)
	h.sessionSwitcher = NewSessionSwitcher()
	h.sessionSwitcher.visible = true
	check(true)
	h.sessionSwitcher.visible = false
	h.insertMode = false
	h.search = NewSearch()
	h.search.visible = true
	check(false)
	h.search.visible = false
	h.groupDialog = NewGroupDialog()
	h.groupDialog.Show()
	check(false)
	h.groupDialog.mode = GroupDialogMove
	check(true)
	h.groupDialog.visible = false
	h.settingsPanel = NewSettingsPanel()
	h.settingsPanel.visible = true
	check(true)
	h.settingsPanel.editingText = true
	check(false)
	h.settingsPanel.visible = false
	h.notesEditing = true
	check(false)
	h.notesEditing = false
	h.isAttaching.Store(true)
	check(false)
	h.isAttaching.Store(false)
	h.isQuitting = true
	check(false)
}

func TestInputMethodFocusAndRefresh(t *testing.T) {
	f := &fakeInputMethod{}
	h := &Home{inputMethod: f}
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 0 {
		t.Fatal("startup without keyboard focus must not acquire the IME")
	}
	h.syncInputMethod(tea.FocusMsg{})
	if len(f.modes) != 1 || !f.modes[0] {
		t.Fatal("focused list did not enter navigation")
	}
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 1 {
		t.Fatal("unchanged refresh invoked the native controller")
	}
	h.notesEditing = true
	h.syncInputMethod(tea.KeyMsg{Type: tea.KeyEnter})
	if f.modes[len(f.modes)-1] {
		t.Fatal("text editor did not restore the IME")
	}
	h.syncInputMethod(tea.BlurMsg{})
	h.notesEditing = false
	h.syncInputMethod(tea.WindowSizeMsg{})
	h.syncInputMethod(tea.MouseMsg{Action: tea.MouseActionMotion})
	if len(f.modes) != 2 {
		t.Fatal("blurred background updates changed input mode")
	}
	h.syncInputMethod(tea.FocusMsg{})
	if len(f.modes) != 3 || !f.modes[2] {
		t.Fatal("returning to the list did not reapply navigation")
	}
}

func TestInputMethodHomeUpdateSearch(t *testing.T) {
	h, _ := buildFocusHome(t)
	f := &fakeInputMethod{}
	h.inputMethod = f
	h.Update(tea.FocusMsg{})
	h.Update(plainKeyMsg('/'))
	if !h.search.IsVisible() || len(f.modes) < 2 || f.modes[len(f.modes)-1] {
		t.Fatal("opening search through Home.Update did not restore text input")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	if got := h.search.input.Value(); got != "中文" {
		t.Fatalf("search text was changed by navigation handling: %q", got)
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.search.IsVisible() || !f.modes[len(f.modes)-1] {
		t.Fatal("closing search did not restore list navigation")
	}
}

func TestInputMethodFormFields(t *testing.T) {
	d := NewNewDialog()
	d.Show()
	h := &Home{newDialog: d}
	d.focusIndex = d.indexOf(focusName)
	if h.inputMethodNavigation() {
		t.Fatal("name input was treated as a shortcut picker")
	}
	d.focusIndex = d.indexOf(focusCreate)
	if !h.inputMethodNavigation() {
		t.Fatal("create button was treated as text input")
	}
	d.Hide()
	h.groupDialog = NewGroupDialog()
	h.groupDialog.Show()
	h.groupDialog.focusIndex = 2
	if !h.inputMethodNavigation() {
		t.Fatal("group target picker was treated as text input")
	}
	h.groupDialog.focusIndex = 0
	if h.inputMethodNavigation() {
		t.Fatal("group name was treated as navigation")
	}
}

func TestInputMethodConfiguredStartupWithoutFocusReport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	if err := session.SaveUserConfig(&session.UserConfig{UI: session.UISettings{AutoInputMethod: true}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeInputMethod{}
	previous := createInputMethodController
	createInputMethodController = func(string) inputMethodController { return f }
	t.Cleanup(func() { createInputMethodController = previous })
	h := &Home{}
	if !h.ConfigureInputMethod() {
		t.Fatal("explicitly enabled input method was not configured")
	}
	// Teft sends no initial FocusMsg when the foreground TUI re-execs.
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 1 || !f.modes[0] {
		t.Fatal("startup waited for a focus report before bypassing the IME")
	}
	h.syncInputMethod(tea.BlurMsg{})
	h.notesEditing = true
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 1 {
		t.Fatal("startup focus assumption overrode an actual blur report")
	}
}

// matrix/hardened: a switch that could not run must be retried by the TUI
// itself. In the list the IME swallows the next keystroke for composition, so
// waiting for input would leave the list in a composing IME indefinitely.
func TestInputMethodRetriesUnappliedSwitch(t *testing.T) {
	f := &fakeInputMethod{deny: true}
	h := &Home{inputMethod: f, inputMethodFocused: true}
	cmd := h.syncInputMethod(tea.FocusMsg{})
	if cmd == nil {
		t.Fatal("unapplied switch scheduled no retry")
	}
	if h.inputMethodApplied {
		t.Fatal("a denied switch was recorded as applied")
	}
	if msg := cmd(); msg != (inputMethodRetryMsg{}) {
		t.Fatalf("retry command produced %T", msg)
	}
	// The retry itself runs the sync again even though nothing else changed.
	if cmd = h.syncInputMethod(inputMethodRetryMsg{}); len(f.modes) != 2 {
		t.Fatalf("retry did not re-sync: modes=%v", f.modes)
	}
	if cmd == nil {
		t.Fatal("still-unapplied switch stopped retrying")
	}
	// Only one retry tick is outstanding at a time.
	h.inputMethodRetryQueued = true
	if again := h.syncInputMethod(tickMsg(time.Now())); again != nil {
		t.Fatal("a second retry tick was queued while one was pending")
	}
	// Once it succeeds, the retry chain stops and refresh frames do no work.
	f.deny = false
	if h.syncInputMethod(inputMethodRetryMsg{}) != nil {
		t.Fatal("applied switch kept the retry chain alive")
	}
	applied := len(f.modes)
	if h.syncInputMethod(tea.WindowSizeMsg{}) != nil || len(f.modes) != applied {
		t.Fatalf("refresh frame re-entered the native controller: modes=%v", f.modes)
	}
	if h.inputMethodRetries != 0 {
		t.Fatalf("retry budget was not released: %d", h.inputMethodRetries)
	}
}

// The retry burst is bounded: a terminal that stays in the background must not
// keep the TUI awake forever. The regular tick keeps verifying after that.
func TestInputMethodRetryBurstIsBounded(t *testing.T) {
	f := &fakeInputMethod{deny: true}
	h := &Home{inputMethod: f, inputMethodFocused: true}
	h.syncInputMethod(tea.FocusMsg{})
	for i := 0; i < inputMethodRetryBurst*2; i++ {
		h.inputMethodRetryQueued = false
		h.syncInputMethod(inputMethodRetryMsg{})
	}
	if h.inputMethodRetries != inputMethodRetryBurst {
		t.Fatalf("retries=%d want=%d", h.inputMethodRetries, inputMethodRetryBurst)
	}
	if cmd := h.syncInputMethod(inputMethodRetryMsg{}); cmd != nil {
		t.Fatal("exhausted burst kept scheduling retries")
	}
	// A real event earns a fresh budget.
	h.syncInputMethod(tea.FocusMsg{})
	if h.inputMethodRetries != 1 {
		t.Fatalf("a focus report did not reset the retry budget: %d", h.inputMethodRetries)
	}
}

// The periodic tick verifies the mode even with no event: the operator, another
// app or a sibling tab can change the source behind the TUI's back.
func TestInputMethodTickVerifiesMode(t *testing.T) {
	f := &fakeInputMethod{}
	h := &Home{inputMethod: f, inputMethodFocused: true}
	h.syncInputMethod(tea.FocusMsg{})
	before := len(f.modes)
	h.syncInputMethod(tickMsg(time.Now()))
	if len(f.modes) != before+1 {
		t.Fatalf("tick skipped verification: modes=%v", f.modes)
	}
	h.syncInputMethod(tea.BlurMsg{})
	h.syncInputMethod(tickMsg(time.Now()))
	if len(f.modes) != before+1 {
		t.Fatal("a blurred tab verified the desktop input source")
	}
	if f.blurs != 1 {
		t.Fatalf("blur reports forwarded: %d", f.blurs)
	}
}

// matrix/hardened: a refused configuration must leave a trace. "Silently off"
// is indistinguishable from "switching is broken" when looking at the desktop.
func TestInputMethodTracesRefusedConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	if err := session.SaveUserConfig(&session.UserConfig{}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	inputmethod.SetDebugWriter(&buf)
	t.Cleanup(func() { inputmethod.SetDebugWriter(nil) })
	h := &Home{}
	if h.ConfigureInputMethod() {
		t.Fatal("the disabled default was configured")
	}
	if got := buf.String(); !strings.Contains(got, "auto_input_method is off") {
		t.Fatalf("refusal was not traced: %q", got)
	}
	// A configured controller records that it starts out assuming focus.
	buf.Reset()
	if err := session.SaveUserConfig(&session.UserConfig{UI: session.UISettings{AutoInputMethod: true}}); err != nil {
		t.Fatal(err)
	}
	session.ClearUserConfigCache()
	previous := createInputMethodController
	createInputMethodController = func(string) inputMethodController { return &fakeInputMethod{} }
	t.Cleanup(func() { createInputMethodController = previous })
	if !h.ConfigureInputMethod() {
		t.Fatal("explicitly enabled input method was not configured")
	}
	if got := buf.String(); !strings.Contains(got, "configured") {
		t.Fatalf("configuration was not traced: %q", got)
	}
}

// matrix/hardened: only a real return of keyboard focus re-applies the text
// policy. Plain typing must not override a source the operator picked while
// editing, and refresh frames must not either.
func TestInputMethodReassertsOnlyOnFocus(t *testing.T) {
	f := &fakeInputMethod{}
	h := &Home{inputMethod: f, inputMethodFocused: true, notesEditing: true}
	h.syncInputMethod(tea.FocusMsg{})
	if len(f.reasserts) != 1 || !f.reasserts[0] {
		t.Fatalf("focus did not reassert the text policy: %v", f.reasserts)
	}
	h.syncInputMethod(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if len(f.reasserts) != 2 || f.reasserts[1] {
		t.Fatalf("typing reasserted the text policy: %v", f.reasserts)
	}
	h.syncInputMethod(tickMsg(time.Now()))
	if last := f.reasserts[len(f.reasserts)-1]; last {
		t.Fatal("a refresh tick reasserted the text policy")
	}
}

// matrix/hardened: the tool options panel inside the new-session dialog mixes
// text fields with toggles and pickers, and the worktree finish dialog mixes a
// typed merge target with checkboxes. Both used to be classified per dialog,
// which forced ASCII onto their text fields.
func TestInputMethodPanelAndDialogTextFields(t *testing.T) {
	d := NewNewDialog()
	d.Show()
	d.focusTargets = []focusTarget{focusOptions}
	d.focusIndex = 0
	h := &Home{newDialog: d}

	d.toolOptions = nil
	if !h.inputMethodNavigation() {
		t.Fatal("a tool without an options panel was treated as text input")
	}

	d.toolOptions = d.claudeOptions
	d.claudeOptions.resumeIDInput.Blur()
	d.claudeOptions.extraArgsInput.Blur()
	d.claudeOptions.startQueryInput.Blur()
	if !h.inputMethodNavigation() {
		t.Fatal("an options toggle was treated as text input")
	}
	for name, input := range map[string]*textinput.Model{
		"resume id":   &d.claudeOptions.resumeIDInput,
		"extra args":  &d.claudeOptions.extraArgsInput,
		"start query": &d.claudeOptions.startQueryInput,
	} {
		input.Focus()
		if h.inputMethodNavigation() {
			t.Fatalf("the %s field was treated as an operation", name)
		}
		input.Blur()
	}

	d.toolOptions = d.ompOptions
	if !h.inputMethodNavigation() {
		t.Fatal("an unfocused OMP panel was treated as text input")
	}
	d.ompOptions.inputs[0].Focus()
	if h.inputMethodNavigation() {
		t.Fatal("an OMP text field was treated as an operation")
	}

	// The worktree finish dialog: 0 = merge checkbox, 1 = target input, 2 = keep branch.
	w := &WorktreeFinishDialog{visible: true}
	h = &Home{worktreeFinishDialog: w}
	for _, index := range []int{0, 2} {
		w.focusIndex = index
		if !h.inputMethodNavigation() {
			t.Fatalf("worktree checkbox %d was treated as text input", index)
		}
	}
	w.focusIndex = 1
	if h.inputMethodNavigation() {
		t.Fatal("the worktree merge target was treated as an operation")
	}
}
