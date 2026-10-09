package ui

// matrix/hardened: vim-style line numbers and ":N" jump for the session list.
//
// Every list row except view-mode dividers gets a number (group headers too,
// so ":N" can land on a group). ":" opens a one-line command prompt in place of
// the help bar: ":12<Enter>" moves the cursor to row 12, ":set nonu" /
// ":set nu" hide or show the numbers for this run. Numbers are drawn only in
// the classic one-line-per-row list; the embedded sidebar's card heights
// depend on width, so it keeps its full width (":N" still works there).

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

const matrixCmdMaxLen = 32

// matrixRowNumbers maps flatItems indices to 1-based line numbers (0 for
// dividers) and returns the highest number.
func (h *Home) matrixRowNumbers() ([]int, int) {
	nums := make([]int, len(h.flatItems))
	n := 0
	for i, it := range h.flatItems {
		if it.Type == session.ItemTypeDivider {
			continue
		}
		n++
		nums[i] = n
	}
	return nums, n
}

// matrixLineNumberWidth is the number column's width including its trailing
// space, or 0 when numbers are not drawn.
func (h *Home) matrixLineNumberWidth(total int) int {
	if h.navCount.noNumbers || h.embeddedLayout || total == 0 {
		return 0
	}
	return len(strconv.Itoa(total)) + 1
}

// matrixNumberRow prefixes a rendered row with its line number (blank for
// num 0 and for continuation lines) and clips each line to width.
func matrixNumberRow(row string, num, numW, width int, current bool) string {
	style := DimStyle
	if current {
		style = lipgloss.NewStyle().Foreground(ColorYellow).Bold(true)
	}
	lines := strings.Split(row, "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			break // trailing newline
		}
		prefix := strings.Repeat(" ", numW)
		if i == 0 && num > 0 {
			prefix = style.Render(fmt.Sprintf("%*d", numW-1, num)) + " "
		}
		line = prefix + line
		if cellWidth(line) > width {
			line = cellTruncate(line, width, "")
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// matrixJumpToLine moves the cursor to line n; false when n is out of range.
func (h *Home) matrixJumpToLine(n int) bool {
	nums, _ := h.matrixRowNumbers()
	for i, num := range nums {
		if num == n {
			h.jumpToIndex(i)
			return true
		}
	}
	return false
}

// matrixHandleCmdKey handles keys while the ":" prompt is open.
func (h *Home) matrixHandleCmdKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		h.matrixResetNav()
		return h, nil
	case tea.KeyBackspace:
		if h.navCount.cmdBuf == "" {
			h.matrixResetNav()
			return h, nil
		}
		r := []rune(h.navCount.cmdBuf)
		h.navCount.cmdBuf = string(r[:len(r)-1])
		return h, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(h.navCount.cmdBuf)
		h.matrixResetNav()
		return h, h.matrixRunCmd(text)
	case tea.KeySpace:
		h.matrixCmdAppend(" ")
		return h, nil
	case tea.KeyRunes:
		h.matrixCmdAppend(string(msg.Runes))
		return h, nil
	}
	return h, nil
}

func (h *Home) matrixCmdAppend(s string) {
	if len(h.navCount.cmdBuf)+len(s) <= matrixCmdMaxLen {
		h.navCount.cmdBuf += s
	}
}

func (h *Home) matrixRunCmd(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	if n, err := strconv.Atoi(text); err == nil {
		if !h.matrixJumpToLine(n) {
			_, total := h.matrixRowNumbers()
			h.setError(fmt.Errorf("no line %d (list has %d)", n, total))
			return nil
		}
		return h.fetchSelectedPreview()
	}
	switch text {
	case "set nu", "set number":
		h.navCount.noNumbers = false
	case "set nonu", "set nonumber":
		h.navCount.noNumbers = true
	default:
		h.setError(fmt.Errorf("unknown command :%s (use :N, :set nu, :set nonu)", text))
	}
	return nil
}

// matrixRenderCmdBar replaces the help bar while the ":" prompt is open.
func (h *Home) matrixRenderCmdBar() string {
	border := lipgloss.NewStyle().Foreground(ColorBorder).Render(strings.Repeat("─", max(0, h.width)))
	prompt := lipgloss.NewStyle().Foreground(ColorText).Render(":" + h.navCount.cmdBuf + "█")
	hint := lipgloss.NewStyle().Foreground(ColorComment).Render("   N⏎ jump to line · set nu / set nonu · Esc cancel")
	return lipgloss.JoinVertical(lipgloss.Left, border, lipgloss.NewStyle().MaxWidth(h.width).Render(prompt+hint))
}
