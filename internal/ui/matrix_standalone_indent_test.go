package ui

// matrix/hardened: fleet timelines must use the same columns for local and
// remote rows, including selected rows and the local window-chevron slot.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func matrixStandaloneIndentRows(t *testing.T, h *Home, selected bool, width int) map[string][]string {
	t.Helper()
	plain := matrixPinnedInstance(t, h, "a1")
	windows := matrixPinnedInstance(t, h, "a2")
	plainTitle := "LOCAL"
	plain.Title, windows.Title = plainTitle, plainTitle
	plain.LastAccessedAt, windows.LastAccessedAt = time.Now(), time.Now()
	tmux.SeedWindowCacheForTest(t, map[string][]tmux.WindowInfo{
		windows.GetTmuxSession().Name: {{ID: "@0", Index: 0}, {ID: "@1", Index: 1}},
	})
	remote := &session.RemoteSessionInfo{
		ID: "remote", Title: "REMOTE", Status: "idle", Tool: "codex",
		LastAccessedAt: time.Now().Format(time.RFC3339Nano),
	}
	if h.groupViewMode == session.GroupViewPinnedOnly {
		plain.Pin, windows.Pin, remote.Pin = session.PinTop, session.PinTop, matrixRemotePinPointer(session.PinTop)
	}
	snapshot := map[string]sessionRenderState{}
	rows := make(map[string][]string)
	for name, inst := range map[string]*session.Instance{"local": plain, "local windows": windows} {
		snapshot[inst.ID] = sessionRenderState{status: session.StatusIdle, tool: "codex", title: plainTitle}
		var b strings.Builder
		h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst}, selected, snapshot, width)
		rows[name] = strings.Split(strings.TrimSuffix(stripAnsi(b.String()), "\n"), "\n")
	}
	var b strings.Builder
	h.renderRemoteSessionItemAtWidth(&b, session.Item{Type: session.ItemTypeRemoteSession, RemoteName: "dev", RemoteSession: remote}, selected, width)
	rows["remote"] = strings.Split(strings.TrimSuffix(stripAnsi(b.String()), "\n"), "\n")
	return rows
}

func matrixAssertStandaloneColumn(t *testing.T, line, marker string, want int) {
	t.Helper()
	if got := leadDisplayWidth(line, marker); got != want {
		t.Errorf("%q column = %d, want %d in %q", marker, got, want, line)
	}
}

func TestMatrixStandaloneClassicRowsShareColumns(t *testing.T) {
	for _, mode := range []session.GroupViewMode{session.GroupViewRecent, session.GroupViewPinnedOnly} {
		for _, embedded := range []bool{false, true} {
			for _, width := range []int{embeddedCardMinWidth - 1, 80} {
				if embedded && width >= embeddedCardMinWidth {
					continue // Wide cards have their own column contract below.
				}
				for _, selected := range []bool{false, true} {
					t.Run(fmt.Sprintf("mode=%v/embedded=%v/width=%d/selected=%v", mode, embedded, width, selected), func(t *testing.T) {
						h := matrixPinnedHome(t)
						h.groupViewMode, h.embeddedLayout = mode, embedded
						for name, lines := range matrixStandaloneIndentRows(t, h, selected, width) {
							if len(lines) != 1 {
								t.Fatalf("%s rendered %d classic lines, want 1", name, len(lines))
							}
							line := lines[0]
							matrixAssertStandaloneColumn(t, line, "○", leftGutterWidth+2)
							title := "LOCAL"
							if mode == session.GroupViewPinnedOnly {
								title = "📌"
							} else if name == "remote" {
								title = "REMOTE" // matrix/hardened: Recent renders the device in the list's source column.
							}
							matrixAssertStandaloneColumn(t, line, title, leftGutterWidth+4)
							if selected {
								matrixAssertStandaloneColumn(t, line, "▶", leftGutterWidth)
							}
							if name == "local windows" {
								matrixAssertStandaloneColumn(t, line, "▾", leftGutterWidth+1)
							}
							if strings.ContainsAny(line, "├└") || cellWidth(line) > width {
								t.Errorf("%s retained a tree parent or exceeded %d cells: %q", name, width, line)
							}
						}
					})
				}
			}
		}
	}
}

func TestMatrixStandaloneEmbeddedCardsShareColumns(t *testing.T) {
	for _, mode := range []session.GroupViewMode{session.GroupViewRecent, session.GroupViewPinnedOnly} {
		for _, compact := range []bool{false, true} {
			for _, density := range []string{session.SidebarDensityMinimal, session.SidebarDensityCompact, session.SidebarDensityFull} {
				for _, width := range []int{embeddedCardMinWidth, 80} {
					for _, selected := range []bool{false, true} {
						t.Run(fmt.Sprintf("mode=%v/compact=%v/%s/width=%d/selected=%v", mode, compact, density, width, selected), func(t *testing.T) {
							h := matrixPinnedHome(t)
							h.groupViewMode, h.embeddedLayout, h.compactSidebar, h.sidebarDensity = mode, true, compact, density
							gutter := leftGutterWidth
							if compact {
								gutter = 0
							}
							statusColumn, titleColumn, chevronColumn := gutter+4, gutter+6, gutter+2
							if mode == session.GroupViewRecent {
								// matrix/hardened: Recent cards use single-cell markers.
								statusColumn, titleColumn, chevronColumn = gutter+2, gutter+4, gutter+1
							}
							for name, lines := range matrixStandaloneIndentRows(t, h, selected, width) {
								line := lines[0]
								matrixAssertStandaloneColumn(t, line, "○", statusColumn)
								title, titleCol := "LOCAL", titleColumn
								if mode == session.GroupViewPinnedOnly {
									title = "📌"
								} else if name == "remote" {
									title = "REMOTE" // matrix/hardened: the title no longer repeats the source column.
								}
								if density == session.SidebarDensityMinimal {
									titleCol += cellWidth(sidebarToolIcon("codex")) + 1
								}
								matrixAssertStandaloneColumn(t, line, title, titleCol)
								if selected {
									matrixAssertStandaloneColumn(t, line, "▶", gutter)
								}
								if name == "local windows" {
									matrixAssertStandaloneColumn(t, line, "▾", chevronColumn)
								}
								if density != session.SidebarDensityMinimal {
									matrixAssertStandaloneColumn(t, lines[len(lines)-1], "╰", gutter+2)
								}
								for _, row := range lines {
									if cellWidth(row) > width {
										t.Errorf("%s exceeded %d cells: %q", name, width, row)
									}
								}
							}
						})
					}
				}
			}
		}
	}
}
