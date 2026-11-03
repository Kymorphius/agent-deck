package ui

// matrix/hardened: keep fleet origin separate from the recent session title.
// The number and jump-hint gutters retain their own positions and key mapping.

import (
	"strings"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func matrixRecentDeviceName(name string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, stripAnsi(name)))
}

func (h *Home) matrixRecentDeviceWidth(width int) int {
	if h.groupViewMode != session.GroupViewRecent {
		return 0
	}
	// matrix/hardened: retain a useful title on a narrow rail. Host labels are
	// cell-truncated; filtering and scrolling cannot change their column width.
	labelWidth := cellWidth("本机")
	for name := range h.remoteSessions {
		labelWidth = max(labelWidth, cellWidth(matrixRecentDeviceName(name)))
	}
	columnWidth := min(min(labelWidth, 10)+1, max(0, width-18))
	if columnWidth < cellWidth("本机")+1 {
		return 0
	}
	return columnWidth
}

func (h *Home) matrixRecentContentWidth(width int) int {
	return width - h.matrixRecentDeviceWidth(width)
}

func matrixRecentDeviceRow(row string, item session.Item, columnWidth, width int, selected bool) string {
	if columnWidth == 0 {
		return row
	}
	label := ""
	switch item.Type {
	case session.ItemTypeSession:
		label = "本机"
	case session.ItemTypeRemoteSession:
		label = matrixRecentDeviceName(item.RemoteName)
	}
	style := DimStyle
	if selected {
		style = SessionStatusSelStyle
	}
	lines := strings.Split(row, "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			break
		}
		prefix := strings.Repeat(" ", columnWidth)
		if i == 0 && label != "" {
			prefix = style.Render(fitCellWidth(cellTruncate(label, columnWidth-1, "…"), columnWidth-1)) + " "
		}
		// matrix/hardened: window rows have no internal width budget. Clip
		// after decoration too so their extra source padding cannot wrap.
		lines[i] = cellTruncate(prefix+line, width, "")
	}
	return strings.Join(lines, "\n")
}
