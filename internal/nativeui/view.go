// matrix/hardened: cell-width clipping and process-only labels for native lists.
package nativeui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func plain(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
}

func clipped(value string, width int) string {
	return ansi.Truncate(value, max(1, width), "…")
}

func stateLabel(state string) string {
	switch state {
	case "running":
		return "正在运行"
	case "exited":
		return "已退出"
	case "stopped":
		return "已停止"
	case "error":
		return "异常"
	default:
		return "未知"
	}
}

func toolLabel(tool string) string {
	if tool == "codex" {
		return "Codex"
	}
	if tool == "shell" {
		return "Shell"
	}
	return plain(tool)
}

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	var lines []string
	switch m.mode {
	case createMode, renameMode:
		lines = m.formLines()
	case confirmStopMode, confirmRemoveMode:
		lines = m.confirmationLines()
	case workingMode, attachMode:
		lines = []string{"Agent Deck · Windows 原生会话", "", m.busyLabel, "", "Ctrl+C 退出界面（会话继续运行）"}
	default:
		lines = m.listLines()
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i := range lines {
		lines[i] = clipped(lines[i], m.width)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) listLines() []string {
	header := fmt.Sprintf("Agent Deck · Windows 原生会话 · %d 个", len(m.items))
	toolbar := "n 新建 · Enter/o 打开 · s 停止 · r 改名 · d 移除"
	footer := "↑↓/j/k 选择 · q/Ctrl+C 退出界面（会话继续运行）"
	path := "目录："
	if selected, ok := m.selected(); ok {
		path += plain(selected.Dir)
	}
	message := plain(m.notice)
	if m.noticeErr {
		message = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(message)
	}
	if m.height < 6 {
		// Very small windows retain an action line before optional detail.
		lines := []string{toolbar + " · q 退出"}
		if selected, ok := m.selected(); ok {
			lines = append(lines, "› "+plain(selected.Name)+" · "+stateLabel(selected.State))
		} else {
			lines = append(lines, "没有会话；按 n 新建。")
		}
		return append(lines, path, message, footer)
	}
	rows := m.height - 5
	lines := []string{header, toolbar}
	if len(m.items) == 0 {
		lines = append(lines, "没有会话；按 n 新建 Shell 或 Codex。")
		for len(lines) < 2+rows {
			lines = append(lines, "")
		}
	} else {
		start := max(0, m.cursor-rows+1)
		// Keep the selected row visible while retaining previous rows as context.
		end := min(len(m.items), start+rows)
		for i := start; i < end; i++ {
			info := m.items[i]
			prefix := "  "
			if info.ID == m.selectedID {
				prefix = "› "
			}
			name, tool, state := plain(info.Name), toolLabel(info.Tool), stateLabel(info.State)
			row := prefix + name + " · " + tool + " · " + state
			if m.width >= 64 {
				nameWidth := m.width - 28
				name = clipped(name, nameWidth)
				row = prefix + name + strings.Repeat(" ", max(0, nameWidth-ansi.StringWidth(name))) + "  " + tool + "  " + state
			}
			if info.ID == m.selectedID {
				row = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render(row)
			}
			lines = append(lines, row)
		}
		for len(lines) < 2+rows {
			lines = append(lines, "")
		}
	}
	return append(lines, path, message, footer)
}

func (m *Model) formLines() []string {
	title := "新建会话"
	if m.mode == renameMode {
		title = "修改会话名称"
	}
	namePrefix, dirPrefix, toolPrefix := "  ", "  ", "  "
	switch m.focus {
	case 0:
		namePrefix = "› "
	case 1:
		dirPrefix = "› "
	case 2:
		toolPrefix = "› "
	}
	lines := []string{title, namePrefix + "名称：" + m.name.View()}
	if m.mode == createMode {
		tool := "[Shell]   Codex"
		if m.tool == "codex" {
			tool = " Shell   [Codex]"
		}
		lines = append(lines, dirPrefix+"目录："+m.dir.View(), toolPrefix+"工具："+tool)
	}
	lines = append(lines, plain(m.notice), "Tab/↑↓ 切换 · ←→ 选工具 · Enter 保存 · Esc 取消")
	return lines
}

func (m *Model) confirmationLines() []string {
	title, detail := "停止此会话？", "将停止这次运行的进程；列表记录和项目文件保留。"
	if m.mode == confirmRemoveMode {
		title, detail = "移除此会话？", "移除列表记录，不删除项目文件。"
		if m.target.State == "running" {
			detail = "会先停止此会话，再移除列表记录；项目文件保留。"
		}
	}
	return []string{
		title, plain(m.target.Name) + " · " + stateLabel(m.target.State),
		detail, "目录：" + plain(m.target.Dir), "Enter 确认 · Esc 取消 · Ctrl+C 退出界面",
	}
}
