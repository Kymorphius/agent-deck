// Package nativeui implements the independent Windows native session list.
// matrix/hardened: no tmux-backed UI, global state, or AI activity inference.
package nativeui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// Service owns persistence and native hosts. The UI never owns their lifetime.
// RunID fences mutations against a session restarted by another client.
type Service interface {
	List(context.Context) ([]terminalhost.SessionInfo, error)
	Create(context.Context, string, string, string) (terminalhost.SessionInfo, error)
	Start(context.Context, string) (terminalhost.SessionInfo, error)
	Stop(context.Context, string, string) error
	Rename(context.Context, string, string) error
	Remove(context.Context, string, string) error
	AttachCommand(terminalhost.SessionInfo) (*exec.Cmd, error)
}

type mode uint8

const (
	listMode mode = iota
	createMode
	renameMode
	confirmStopMode
	confirmRemoveMode
	workingMode
	attachMode
)

const operationTimeout = 30 * time.Second

// Model contains only one window's UI state. Construct a separate model for
// every list window, even when they share the same Service.
type Model struct {
	service     Service
	defaultCwd  string
	items       []terminalhost.SessionInfo
	selectedID  string
	cursor      int
	width       int
	height      int
	mode        mode
	notice      string
	noticeErr   bool
	listFailure bool
	busyLabel   string

	// matrix/hardened: closing/opening a dialog invalidates old list results.
	generation   uint64
	listRequest  uint64
	listInFlight bool
	pollToken    uint64
	quitting     bool

	target terminalhost.SessionInfo
	name   textinput.Model
	dir    textinput.Model
	focus  int
	tool   string
}

type listResultMsg struct {
	generation uint64
	request    uint64
	items      []terminalhost.SessionInfo
	err        error
}

type pollMsg struct{ token uint64 }

type operationResultMsg struct {
	generation uint64
	selectedID string
	command    *exec.Cmd
	notice     string
	err        error
}

type attachReturnedMsg struct {
	generation uint64
	err        error
}

// New creates the native list without starting or stopping any hosted process.
func New(service Service, defaultCwd string) tea.Model {
	name := textinput.New()
	name.CharLimit = 120
	name.Prompt = ""
	dir := textinput.New()
	dir.CharLimit = 4096
	dir.Prompt = ""
	return &Model{
		service: service, defaultCwd: defaultCwd,
		width: 80, height: 24, generation: 1, pollToken: 1,
		name: name, dir: dir, tool: "shell",
	}
}

// Run displays the list. Quitting this program leaves hosted sessions alive.
func Run(service Service, defaultCwd string) error {
	_, err := tea.NewProgram(New(service, defaultCwd), tea.WithAltScreen()).Run()
	return err
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), pollAfter(m.pollToken))
}

func pollAfter(token uint64) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return pollMsg{token} })
}

func (m *Model) refresh() tea.Cmd {
	if m.mode != listMode || m.listInFlight || m.quitting {
		return nil
	}
	m.listRequest++
	m.listInFlight = true
	generation, request, service := m.generation, m.listRequest, m.service
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		defer cancel()
		if service == nil {
			return listResultMsg{generation: generation, request: request, err: errors.New("会话服务不可用")}
		}
		items, err := service.List(ctx)
		return listResultMsg{generation: generation, request: request, items: items, err: err}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.resizeInputs()
		return m, nil
	case listResultMsg:
		if m.mode != listMode || msg.generation != m.generation || msg.request != m.listRequest {
			return m, nil
		}
		m.listInFlight = false
		if msg.err != nil {
			m.setNotice("读取列表失败："+msg.err.Error(), true)
			m.listFailure = true
			return m, nil
		}
		if m.listFailure {
			m.setNotice("", false)
		}
		m.setItems(msg.items)
		return m, nil
	case pollMsg:
		if m.mode != listMode || msg.token != m.pollToken {
			return m, nil
		}
		return m, tea.Batch(m.refresh(), pollAfter(m.pollToken))
	case operationResultMsg:
		if m.mode != workingMode || msg.generation != m.generation {
			return m, nil
		}
		if msg.selectedID != "" {
			m.selectedID = msg.selectedID
		}
		if msg.err != nil {
			m.setNotice(msg.err.Error(), true)
			return m, m.showList()
		}
		if msg.command != nil {
			m.mode = attachMode
			generation := m.generation
			return m, tea.ExecProcess(msg.command, func(err error) tea.Msg {
				return attachReturnedMsg{generation: generation, err: err}
			})
		}
		m.setNotice(msg.notice, false)
		return m, m.showList()
	case attachReturnedMsg:
		if m.mode != attachMode || msg.generation != m.generation {
			return m, nil
		}
		if msg.err != nil {
			m.setNotice("返回列表："+msg.err.Error(), true)
		} else {
			m.setNotice("已返回列表；正在运行的会话继续保留。", false)
		}
		return m, m.showList()
	case tea.KeyMsg:
		// matrix/hardened: Ctrl+C always exits the UI, including every modal.
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
		if m.mode == createMode || m.mode == renameMode {
			return m, m.updateForm(msg)
		}
		if m.mode == confirmStopMode || m.mode == confirmRemoveMode {
			return m, m.updateConfirmation(msg)
		}
		if m.mode == listMode {
			return m, m.updateListKey(msg)
		}
		return m, nil
	}
	// textinput cursor blink belongs to the active form, never to list commands.
	if m.mode == createMode || m.mode == renameMode {
		return m, m.updateInput(msg)
	}
	return m, nil
}

func (m *Model) setNotice(notice string, failure bool) {
	m.notice, m.noticeErr = notice, failure
	m.listFailure = false
}

func (m *Model) invalidate() {
	m.generation++
	m.pollToken++
	m.listInFlight = false
}

func (m *Model) showList() tea.Cmd {
	m.invalidate()
	m.mode = listMode
	m.name.Blur()
	m.dir.Blur()
	return tea.Batch(m.refresh(), pollAfter(m.pollToken))
}

func (m *Model) setItems(items []terminalhost.SessionInfo) {
	m.items = append([]terminalhost.SessionInfo(nil), items...)
	for i := range m.items {
		if m.items[i].ID == m.selectedID {
			m.cursor = i
			return
		}
	}
	if len(m.items) == 0 {
		m.cursor, m.selectedID = 0, ""
		return
	}
	m.cursor = min(max(0, m.cursor), len(m.items)-1)
	m.selectedID = m.items[m.cursor].ID
}

func (m *Model) selected() (terminalhost.SessionInfo, bool) {
	if m.cursor < 0 || m.cursor >= len(m.items) || m.items[m.cursor].ID != m.selectedID {
		return terminalhost.SessionInfo{}, false
	}
	return m.items[m.cursor], true
}

func (m *Model) updateListKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q":
		m.quitting = true
		return tea.Quit
	case "up", "k":
		if len(m.items) > 0 {
			m.cursor = max(0, m.cursor-1)
			m.selectedID = m.items[m.cursor].ID
		}
	case "down", "j":
		if len(m.items) > 0 {
			m.cursor = min(len(m.items)-1, m.cursor+1)
			m.selectedID = m.items[m.cursor].ID
		}
	case "n":
		return m.openCreate()
	case "enter", "o":
		if info, ok := m.selected(); ok {
			return m.openSession(info)
		}
	case "s", "d", "r":
		info, ok := m.selected()
		if !ok {
			return nil
		}
		if msg.String() == "s" && info.State != "running" && !(info.State == "error" && info.RunID != "") {
			m.setNotice("只有正在运行的会话需要停止。", false)
			return nil
		}
		m.invalidate()
		m.target = info
		m.setNotice("", false)
		switch msg.String() {
		case "s":
			m.mode = confirmStopMode
		case "d":
			m.mode = confirmRemoveMode
		case "r":
			m.mode, m.focus = renameMode, 0
			m.name.SetValue(info.Name)
			m.resizeInputs()
			return m.name.Focus()
		}
	}
	return nil
}

func (m *Model) openCreate() tea.Cmd {
	m.invalidate()
	m.mode, m.focus, m.tool = createMode, 0, "shell"
	m.setNotice("", false)
	defaultName := filepath.Base(m.defaultCwd)
	if defaultName == "." || defaultName == string(filepath.Separator) || defaultName == "" {
		defaultName = "新会话"
	}
	m.name.SetValue(defaultName)
	m.dir.SetValue(m.defaultCwd)
	m.resizeInputs()
	m.dir.Blur()
	return m.name.Focus()
}

func (m *Model) resizeInputs() {
	width := max(1, m.width-8)
	m.name.Width, m.dir.Width = width, width
}

func (m *Model) updateForm(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.showList()
	case "tab", "down":
		return m.moveFocus(1)
	case "shift+tab", "up":
		return m.moveFocus(-1)
	case "enter":
		name := strings.TrimSpace(m.name.Value())
		if name == "" {
			m.setNotice("请输入会话名称。", true)
			return nil
		}
		if m.mode == renameMode {
			return m.renameSession(name)
		}
		dir := strings.TrimSpace(m.dir.Value())
		if dir == "" {
			m.setNotice("请输入项目目录。", true)
			return nil
		}
		return m.createSession(name, m.tool, dir)
	}
	if m.mode == createMode && m.focus == 2 {
		switch msg.String() {
		case "left", "right", " ":
			if m.tool == "shell" {
				m.tool = "codex"
			} else {
				m.tool = "shell"
			}
		}
		return nil
	}
	return m.updateInput(msg)
}

func (m *Model) moveFocus(direction int) tea.Cmd {
	count := 1
	if m.mode == createMode {
		count = 3
	}
	m.focus = (m.focus + direction + count) % count
	m.name.Blur()
	m.dir.Blur()
	switch m.focus {
	case 0:
		return m.name.Focus()
	case 1:
		return m.dir.Focus()
	}
	return nil
}

func (m *Model) updateInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m.focus {
	case 0:
		m.name, cmd = m.name.Update(msg)
	case 1:
		m.dir, cmd = m.dir.Update(msg)
	}
	return cmd
}

func (m *Model) updateConfirmation(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.showList()
	case "enter":
		// Confirmations stay bound to the row and instance originally displayed.
		selected, ok := m.selected()
		if !ok || selected.ID != m.target.ID || selected.RunID != m.target.RunID {
			m.setNotice("会话已改变，请刷新后重新确认。", true)
			return m.showList()
		}
		return m.mutateSession(m.mode, m.target)
	}
	return nil
}

func (m *Model) beginOperation(label string, operation func(context.Context) operationResultMsg) tea.Cmd {
	m.invalidate()
	m.mode, m.busyLabel = workingMode, label
	m.name.Blur()
	m.dir.Blur()
	generation, service := m.generation, m.service
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		defer cancel()
		if service == nil {
			return operationResultMsg{generation: generation, err: errors.New("会话服务不可用")}
		}
		result := operation(ctx)
		result.generation = generation
		return result
	}
}

// currentInstance is a second check against changes made in another UI window.
// Stop/Remove also receive RunID so the service can close the remaining race.
func currentInstance(ctx context.Context, service Service, target terminalhost.SessionInfo) (terminalhost.SessionInfo, error) {
	items, err := service.List(ctx)
	if err != nil {
		return terminalhost.SessionInfo{}, fmt.Errorf("核对会话失败：%w", err)
	}
	for _, item := range items {
		if item.ID == target.ID {
			if item.RunID != target.RunID {
				return terminalhost.SessionInfo{}, errors.New("会话已在其他窗口重新启动，请刷新后重试。")
			}
			return item, nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("会话已被移除，请刷新后重试。")
}

func attachResult(service Service, info terminalhost.SessionInfo) operationResultMsg {
	command, err := service.AttachCommand(info)
	if err == nil && command == nil {
		err = errors.New("未能打开会话终端。")
	}
	return operationResultMsg{selectedID: info.ID, command: command, err: err}
}

func (m *Model) openSession(target terminalhost.SessionInfo) tea.Cmd {
	service := m.service
	return m.beginOperation("正在打开会话…", func(ctx context.Context) operationResultMsg {
		info, err := currentInstance(ctx, service, target)
		if err != nil {
			return operationResultMsg{err: err}
		}
		switch info.State {
		case "stopped", "exited":
			info, err = service.Start(ctx, target.ID)
			if err != nil {
				return operationResultMsg{err: fmt.Errorf("启动失败：%w", err)}
			}
		case "running":
		default:
			return operationResultMsg{err: errors.New("当前进程状态未知，请稍后刷新再打开。")}
		}
		return attachResult(service, info)
	})
}

func (m *Model) createSession(name, tool, dir string) tea.Cmd {
	service := m.service
	return m.beginOperation("正在创建会话…", func(ctx context.Context) operationResultMsg {
		info, err := service.Create(ctx, name, tool, dir)
		if err != nil {
			if info.ID != "" {
				// matrix/hardened: creation may persist before auto-start fails.
				// Preserve the descriptor and never automatically retry Start.
				return operationResultMsg{selectedID: info.ID, err: fmt.Errorf("会话已创建，启动失败：%w", err)}
			}
			return operationResultMsg{err: fmt.Errorf("创建失败：%w", err)}
		}
		selectedID := info.ID
		switch info.State {
		case "stopped", "exited":
			info, err = service.Start(ctx, selectedID)
			if err != nil {
				return operationResultMsg{selectedID: selectedID, err: fmt.Errorf("会话已创建，启动失败：%w", err)}
			}
		case "running":
		default:
			return operationResultMsg{selectedID: selectedID, err: errors.New("会话已创建，进程状态未知，请稍后刷新再打开。")}
		}
		return attachResult(service, info)
	})
}

func (m *Model) renameSession(name string) tea.Cmd {
	service, target := m.service, m.target
	return m.beginOperation("正在保存名称…", func(ctx context.Context) operationResultMsg {
		if _, err := currentInstance(ctx, service, target); err != nil {
			return operationResultMsg{err: err}
		}
		if err := service.Rename(ctx, target.ID, name); err != nil {
			return operationResultMsg{err: fmt.Errorf("重命名失败：%w", err)}
		}
		return operationResultMsg{selectedID: target.ID, notice: "会话名称已保存。"}
	})
}

func (m *Model) mutateSession(action mode, target terminalhost.SessionInfo) tea.Cmd {
	service := m.service
	label := "正在停止会话…"
	if action == confirmRemoveMode {
		label = "正在移除会话…"
	}
	return m.beginOperation(label, func(ctx context.Context) operationResultMsg {
		if _, err := currentInstance(ctx, service, target); err != nil {
			return operationResultMsg{err: err}
		}
		if action == confirmStopMode {
			if err := service.Stop(ctx, target.ID, target.RunID); err != nil {
				return operationResultMsg{err: fmt.Errorf("停止失败：%w", err)}
			}
			return operationResultMsg{selectedID: target.ID, notice: "会话已停止。"}
		}
		if err := service.Remove(ctx, target.ID, target.RunID); err != nil {
			return operationResultMsg{err: fmt.Errorf("移除失败：%w", err)}
		}
		return operationResultMsg{notice: "会话已从列表移除；项目文件保留。"}
	})
}
