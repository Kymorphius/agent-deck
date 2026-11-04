package ui

// matrix/hardened: reserving a Recent device column must change card height,
// mouse routing and pagination together at the card-width fallback boundary.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func matrixRecentGeometryHome(t *testing.T, width int, density string) *Home {
	t.Helper()
	home := matrixPinnedHome(t)
	home.width, home.height = width, 80
	home.embeddedLayout, home.compactSidebar = true, false
	home.sidebarDensity, home.groupViewMode = density, session.GroupViewRecent
	home.flatItems = nil
	for _, title := range []string{"a1", "a2", "a3"} {
		inst := matrixPinnedInstance(t, home, title)
		matrixRecentInteraction(t, inst, time.Now())
		home.flatItems = append(home.flatItems, session.Item{Type: session.ItemTypeSession, Session: inst})
	}
	home.cursor, home.viewOffset = 0, 0
	return home
}

func TestMatrixRecentDeviceColumnGeometryAtCardBoundary(t *testing.T) {
	for _, density := range []string{session.SidebarDensityCompact, session.SidebarDensityFull} {
		for _, width := range []int{27, 28, 32, 33, 34} {
			t.Run(fmt.Sprintf("%s/width=%d", density, width), func(t *testing.T) {
				home := matrixRecentGeometryHome(t, width, density)
				contentWidth := home.matrixRecentContentWidth(width)
				if contentWidth >= width {
					t.Fatal("Recent fixture did not reserve a device column")
				}
				wantHeight := 1
				if contentWidth >= embeddedCardMinWidth {
					wantHeight = sidebarDensityRowLines(density)
				}
				view := stripAnsi(home.renderSessionList(width, 60))
				lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
				if want := len(home.flatItems) * wantHeight; len(lines) != want {
					t.Fatalf("rendered %d lines, want %d (content width %d):\n%s", len(lines), want, contentWidth, view)
				}
				if got := home.sidebarItemsHeight(home.flatItems, 0, len(home.flatItems), width); got != len(lines) {
					t.Fatalf("measured list height %d, rendered %d", got, len(lines))
				}
				for index, item := range home.flatItems {
					if got := home.sidebarItemRenderHeightAtWidth(item, width); got != wantHeight {
						t.Errorf("item %d height = %d, want %d", index, got, wantHeight)
					}
					for line := 0; line < wantHeight; line++ {
						y := home.getListContentStartY() + index*wantHeight + line
						if got := home.mouseYToItemIndex(y); got != index {
							t.Errorf("rendered item %d line %d routes to item %d", index, line, got)
						}
					}
				}
				if got := home.pageStepItems(2*wantHeight, 1); got != 2 {
					t.Errorf("two rendered row heights page down %d rows, want 2", got)
				}
				home.cursor = 2
				if got := home.pageStepItems(2*wantHeight, -1); got != 2 {
					t.Errorf("two rendered row heights page up %d rows, want 2", got)
				}
			})
		}
	}
}

func TestMatrixRecentAutoDensityUsesReservedContentWidth(t *testing.T) {
	home := matrixRecentGeometryHome(t, 32, session.SidebarDensityAuto)
	home.height = 12 // Five available lines: enough for three fallback rows.
	budget, width := home.sidebarLineBudget()
	if want := sidebarAutoRowLinesFor(home.flatItems, home.matrixRecentContentWidth(width), budget); home.sidebarAutoRowLines() != want {
		t.Fatalf("auto density = %d, want %d for reserved content width", home.sidebarAutoRowLines(), want)
	}
	if got := home.sidebarAutoRowLines(); got != embeddedSessionRowLines {
		t.Fatalf("one-line fallback rows should leave full metadata density available, got %d", got)
	}
}

func TestMatrixRecentWindowColumnDoesNotWrap(t *testing.T) {
	home := matrixRecentGeometryHome(t, 40, session.SidebarDensityCompact)
	home.flatItems = append(home.flatItems[:1], session.Item{
		Type: session.ItemTypeWindow, Level: 1, WindowIndex: 1,
		WindowName: strings.Repeat("窗口", 20), IsLastWindow: true,
	})
	view := stripAnsi(home.renderSessionList(40, 60))
	lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
	for _, line := range lines {
		if cellWidth(line) > 40 {
			t.Fatalf("device padding made a window row wrap: %q", line)
		}
	}
	if got := home.sidebarItemsHeight(home.flatItems, 0, len(home.flatItems), 40); got != len(lines) {
		t.Fatalf("window list measured %d rows, rendered %d", got, len(lines))
	}
}
