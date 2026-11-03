package ui

// matrix/hardened: line numbers start at the top visible row by default
// ([ui] visible_line_numbers), and every number lookup agrees with them.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// scrolledTwoGroupHome scrolls the fixture so a1 is the top visible row.
func scrolledTwoGroupHome(t *testing.T) (*Home, map[string]int) {
	t.Helper()
	home, idx := buildTwoGroupHome(t)
	home.viewOffset = idx["a1"]
	home.cursor = idx["a1"]
	return home, idx
}

func TestMatrixVisibleLineNumbers_TopVisibleRowIsOne(t *testing.T) {
	home, idx := scrolledTwoGroupHome(t)

	nums, _ := home.matrixRowNumbers()
	if nums[idx["a1"]] != 1 || nums[idx["a2"]] != 2 || nums[idx["b2"]] != idx["b2"]-idx["a1"]+1 {
		t.Fatalf("visible numbering: a1=%d a2=%d b2=%d, want 1, 2, %d", nums[idx["a1"]], nums[idx["a2"]], nums[idx["b2"]], idx["b2"]-idx["a1"]+1)
	}
	if nums[0] != 0 {
		t.Fatalf("row scrolled off above has number %d, want 0 (no address)", nums[0])
	}

	lines := strings.Split(ansi.Strip(home.renderSessionList(60, 20)), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], "above") || !strings.HasPrefix(lines[1], "  1 ") || !strings.Contains(lines[1], "a1") { // matrix/hardened: stable number gutter.
		t.Fatalf("first visible row should be numbered 1 under the \"above\" marker:\n%s", strings.Join(lines, "\n"))
	}
}

func TestMatrixVisibleLineNumbers_LookupsMatchTheShownNumbers(t *testing.T) {
	home, idx := scrolledTwoGroupHome(t)

	h := matrixPressKeys(t, home, '2', 'l') // 2l: second visible row
	if h.cursor != idx["a2"] {
		t.Fatalf("2l: cursor on %s, want a2 (row 2 on screen)", describeCursor(h))
	}

	h, idx = scrolledTwoGroupHome(t)
	h = matrixTypeCmd(t, h, "3")
	if h.cursor != idx["a3"] {
		t.Fatalf(":3: cursor on %s, want a3 (row 3 on screen)", describeCursor(h))
	}
}

func TestMatrixVisibleLineNumbers_AbsoluteModeAndSetCommands(t *testing.T) {
	home, idx := scrolledTwoGroupHome(t)

	h := matrixTypeCmd(t, home, "set absnu")
	nums, _ := h.matrixRowNumbers()
	if nums[idx["a1"]] != idx["a1"]+1 {
		t.Fatalf(":set absnu: a1=%d, want whole-list number %d", nums[idx["a1"]], idx["a1"]+1)
	}
	h = matrixPressKeys(t, h, '3', 'j') // a count reset keeps the mode
	if !h.navCount.absNumbers {
		t.Fatal("absolute numbering lost after 3j")
	}
	h.viewOffset, h.cursor = idx["a1"], idx["a1"]
	h = matrixTypeCmd(t, h, "set visnu")
	if nums, _ := h.matrixRowNumbers(); nums[idx["a1"]] != 1 {
		t.Fatalf(":set visnu: a1=%d, want 1", nums[idx["a1"]])
	}
}

func TestMatrixVisibleLineNumbers_SettingDefaultAndPanel(t *testing.T) {
	if !(session.UISettings{}).GetVisibleLineNumbers() {
		t.Fatal("visible_line_numbers must default to true")
	}
	off := false
	home := NewHome()
	home.applySavedLayoutSettings(session.UISettings{VisibleLineNumbers: &off})
	if !home.navCount.absNumbers {
		t.Fatal("visible_line_numbers = false did not switch to whole-list numbering")
	}

	panel := NewSettingsPanel()
	panel.LoadConfig(&session.UserConfig{UI: session.UISettings{VisibleLineNumbers: &off}})
	if panel.visibleLineNumbers {
		t.Fatal("panel did not load visible_line_numbers = false")
	}
	panel.cursor = int(SettingVisibleLineNumbers)
	if !panel.toggleValue() {
		t.Fatal("toggle on SettingVisibleLineNumbers did nothing")
	}
	cfg := panel.GetConfig()
	if cfg.UI.VisibleLineNumbers == nil || !*cfg.UI.VisibleLineNumbers {
		t.Fatalf("panel saved %v, want visible_line_numbers = true", cfg.UI.VisibleLineNumbers)
	}
	shown := NewSettingsPanel()
	shown.SetSize(80, 20)
	shown.Show()
	shown.cursor = int(SettingVisibleLineNumbers) // scrolls the row into view
	if plain := ansi.Strip(shown.View()); !strings.Contains(plain, "Number visible rows") || !strings.Contains(plain, "SESSION LIST") {
		t.Fatalf("panel does not show the new row under SESSION LIST:\n%s", plain)
	}
}
