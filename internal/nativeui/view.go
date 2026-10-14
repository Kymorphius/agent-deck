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
	if tool == "powershell" {
		return "PowerShell"
	}
	return plain(tool)
}

func groupLabel(group string) string {
	if group == "" {
		return "默认分组"
	}
	return plain(group)
}

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	var lines []string
	switch m.mode {
	case createMode, renameMode, moveMode:
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
	header := fmt.Sprintf("Agent Deck · Windows 原生会话 · %d/%d 个", len(m.items), len(m.allItems))
	toolbar := "n 新建 · Enter/o 打开 · s 停止 · r 改名 · g 分组 · d 移除"
	footer := "↑↓/j/k 选择 · / 搜索 · Tab 分组 · q/Ctrl+C 退出（会话继续运行）"
	group := "全部分组"
	if !m.allGroups {
		group = groupLabel(m.filterGroup)
	}
	filter := "分组：" + group + " · 搜索：" + plain(m.search.Value())
	if m.searching {
		// matrix/hardened: field padding must not push active keyboard hints
		// beyond the terminal edge; search consumes list shortcuts as text.
		toolbar = "Enter 保留 · Esc 清除 · Ctrl+C 退出"
		footer = toolbar + "（会话继续运行）"
		filter = "分组：" + group + " · 搜索：" + m.search.View()
	}
	path := "目录："
	if selected, ok := m.selected(); ok {
		path += plain(selected.Dir)
	}
	message := plain(m.notice)
	if m.noticeErr {
		message = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(message)
	}
	if m.height < 7 {
		// Very small windows retain an action line before optional detail.
		shortToolbar := toolbar
		if !m.searching {
			shortToolbar += " · q 退出"
		}
		lines := []string{shortToolbar}
		if selected, ok := m.selected(); ok {
			lines = append(lines, "› "+plain(selected.Name)+" · "+stateLabel(selected.State))
		} else {
			if m.searching || len(m.allItems) > 0 {
				lines = append(lines, "没有匹配的会话；Esc 清除搜索。")
			} else {
				lines = append(lines, "没有会话；按 n 新建。")
			}
		}
		return append(lines, filter, path, message, footer)
	}
	rows := m.height - 6
	lines := []string{header, toolbar, filter}
	if len(m.items) == 0 {
		empty := "没有会话；按 n 新建 Shell、Codex 或 PowerShell。"
		if m.searching {
			empty = "没有匹配的会话；Esc 清除搜索。"
		} else if len(m.allItems) > 0 {
			empty = "没有匹配的会话；Esc 清除筛选。"
		}
		lines = append(lines, empty)
		for len(lines) < 3+rows {
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
			name, tool, state, group := plain(info.Name), toolLabel(info.Tool), stateLabel(info.State), groupLabel(info.Group)
			row := prefix + name + " · " + group + " · " + tool + " · " + state
			if m.width >= 64 {
				nameWidth := m.width - 44
				name = clipped(name, nameWidth)
				group = clipped(group, 14)
				row = prefix + name + strings.Repeat(" ", max(0, nameWidth-ansi.StringWidth(name))) + "  " + group + strings.Repeat(" ", max(0, 14-ansi.StringWidth(group))) + "  " + tool + "  " + state
			}
			if info.ID == m.selectedID {
				row = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render(row)
			}
			lines = append(lines, row)
		}
		for len(lines) < 3+rows {
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
	if m.mode == moveMode {
		return formViewport([]string{"移动会话分组", plain(m.target.Name) + " · " + stateLabel(m.target.State), "› 分组：" + m.group.View(), "留空使用默认分组；当前进程继续运行。", plain(m.notice), "Enter 保存 · Esc 取消"}, 2, m.height)
	}
	namePrefix, dirPrefix, toolPrefix, groupPrefix := "  ", "  ", "  ", "  "
	switch m.focus {
	case 0:
		namePrefix = "› "
	case 1:
		dirPrefix = "› "
	case 2:
		toolPrefix = "› "
	case 3:
		groupPrefix = "› "
	}
	lines := []string{title, namePrefix + "名称：" + m.name.View()}
	if m.mode == createMode {
		choices := []string{"Shell", "Codex", "PowerShell"}
		for i, tool := range [...]string{"shell", "codex", "powershell"} {
			if m.tool == tool {
				choices[i] = "[" + choices[i] + "]"
			}
		}
		lines = append(lines, dirPrefix+"目录："+m.dir.View(), toolPrefix+"工具："+strings.Join(choices, "   "), groupPrefix+"分组："+m.group.View()+"（留空为默认分组）")
	}
	lines = append(lines, plain(m.notice), "Tab/↑↓ 切换 · ←→ 选工具 · Enter 保存 · Esc 取消")
	return formViewport(lines, m.focus+1, m.height)
}

// matrix/hardened: short forms keep the active field reachable when the group
// is below the ordinary viewport, without changing the four-field tab order.
func formViewport(lines []string, active, height int) []string {
	if len(lines) <= height {
		return lines
	}
	if height <= 1 {
		return []string{lines[active]}
	}
	footer := lines[len(lines)-1]
	if height == 2 {
		return []string{lines[active], footer}
	}
	rows := height - 2
	start := max(1, active-rows+1)
	end := min(len(lines)-1, start+rows)
	compact := append([]string{lines[0]}, lines[start:end]...)
	return append(compact, footer)
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
