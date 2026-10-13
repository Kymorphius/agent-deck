// matrix/hardened: exercise keyboard actions through the real service boundary.
package nativeui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type mutation struct{ id, runID string }

type fakeService struct {
	mu        sync.Mutex
	items     []terminalhost.SessionInfo
	lists     int
	creates   []terminalhost.SessionInfo
	starts    []string
	stops     []mutation
	removes   []mutation
	renames   []struct{ id, name string }
	attaches  []terminalhost.SessionInfo
	createErr error
	startErr  error
	listErr   error
	autoStart bool
	attach    func() *exec.Cmd
}

func (s *fakeService) List(context.Context) ([]terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	return append([]terminalhost.SessionInfo(nil), s.items...), s.listErr
}

func (s *fakeService) Create(_ context.Context, name, tool, dir string) (terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := terminalhost.SessionInfo{ID: fmt.Sprintf("new-%d", len(s.creates)), Name: name, Tool: tool, Dir: dir, State: "stopped"}
	if s.autoStart {
		info.State, info.RunID = "running", "created-run"
	}
	s.creates = append(s.creates, info)
	s.items = append(s.items, info)
	return info, s.createErr
}

func (s *fakeService) Start(_ context.Context, id string) (terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, id)
	if s.startErr != nil {
		return terminalhost.SessionInfo{}, s.startErr
	}
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].State, s.items[i].RunID = "running", "new-run"
			return s.items[i], nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("missing session")
}

func (s *fakeService) Stop(_ context.Context, id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id && s.items[i].RunID == runID {
			s.stops = append(s.stops, mutation{id, runID})
			s.items[i].State = "stopped"
			return nil
		}
	}
	return errors.New("stale instance")
}

func (s *fakeService) Rename(_ context.Context, id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renames = append(s.renames, struct{ id, name string }{id, name})
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Name = name
			return nil
		}
	}
	return errors.New("missing session")
}

func (s *fakeService) Remove(_ context.Context, id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id && s.items[i].RunID == runID {
			s.removes = append(s.removes, mutation{id, runID})
			s.items = append(s.items[:i], s.items[i+1:]...)
			return nil
		}
	}
	return errors.New("stale instance")
}

func (s *fakeService) AttachCommand(info terminalhost.SessionInfo) (*exec.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attaches = append(s.attaches, info)
	if s.attach != nil {
		return s.attach(), nil
	}
	return exec.Command("native-test-attach"), nil
}

func (s *fakeService) replace(items ...terminalhost.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]terminalhost.SessionInfo(nil), items...)
}

func info(id, name, state, runID string) terminalhost.SessionInfo {
	return terminalhost.SessionInfo{ID: id, Name: name, State: state, RunID: runID, Tool: "shell", Dir: `C:\项目\原生终端`}
}

func modelWith(s *fakeService) *Model {
	m := New(s, `C:\项目\原生终端`).(*Model)
	m.Update(m.refresh()())
	return m
}

func key(value string) tea.KeyMsg {
	switch value {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
	}
}

func press(m *Model, value string) tea.Cmd {
	_, cmd := m.Update(key(value))
	return cmd
}

func runOperation(t *testing.T, m *Model, cmd tea.Cmd) operationResultMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected operation command")
	}
	msg, ok := cmd().(operationResultMsg)
	if !ok {
		t.Fatal("command did not perform service operation")
	}
	m.Update(msg)
	return msg
}

func TestSelectionUsesStableUUIDAcrossRefreshes(t *testing.T) {
	a, b, c := info("a", "相同名称", "running", "a1"), info("b", "相同名称", "running", "b1"), info("c", "第三个", "stopped", "")
	s := &fakeService{items: []terminalhost.SessionInfo{a, b, c}}
	m := modelWith(s)
	press(m, "j")
	s.replace(c, a, b)
	m.Update(m.refresh()())
	if m.selectedID != "b" || m.cursor != 2 {
		t.Fatalf("reorder changed selection: id=%s cursor=%d", m.selectedID, m.cursor)
	}
	press(m, "up")
	if m.selectedID != "a" {
		t.Fatalf("up selected %q", m.selectedID)
	}
	s.replace(c, b)
	m.Update(m.refresh()())
	if m.selectedID != "b" || m.cursor != 1 {
		t.Fatalf("removed row did not retain nearest selection: id=%s cursor=%d", m.selectedID, m.cursor)
	}
	s.replace()
	m.Update(m.refresh()())
	if m.selectedID != "" || m.cursor != 0 {
		t.Fatal("empty list retained dangling selection")
	}
}

func TestTextInputsDoNotInvokeListShortcuts(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", "原始", "running", "run")}}
	m := modelWith(s)
	press(m, "n")
	m.name.SetValue("")
	for _, char := range "qnsdrjko中文" {
		press(m, string(char))
	}
	if m.quitting || m.mode != createMode || m.name.Value() != "qnsdrjko中文" {
		t.Fatalf("text became an action: mode=%v name=%q quit=%v", m.mode, m.name.Value(), m.quitting)
	}
	press(m, "tab")
	m.dir.SetValue("")
	press(m, "qndsr")
	if m.dir.Value() != "qndsr" || len(s.creates)+len(s.stops)+len(s.removes)+len(s.renames)+len(s.attaches) != 0 {
		t.Fatal("path input invoked a list operation")
	}
	press(m, "esc")
	if m.mode != listMode {
		t.Fatal("escape did not cancel form")
	}
}

func TestCreateKeyboardFlowStartsAndAttachesChosenTool(t *testing.T) {
	s := &fakeService{}
	m := modelWith(s)
	press(m, "n")
	if m.dir.Value() != m.defaultCwd || m.name.Value() == "" {
		t.Fatal("new form lacks usable defaults")
	}
	m.name.SetValue("  原生 Codex  ")
	m.dir.SetValue(`C:\开发\项目`)
	press(m, "tab")
	press(m, "tab")
	press(m, "right")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || m.mode != attachMode {
		t.Fatalf("create did not attach: err=%v mode=%v", result.err, m.mode)
	}
	if len(s.creates) != 1 || s.creates[0].Name != "原生 Codex" || s.creates[0].Tool != "codex" || s.creates[0].Dir != `C:\开发\项目` {
		t.Fatalf("incorrect create arguments: %+v", s.creates)
	}
	if len(s.starts) != 1 || s.starts[0] != "new-0" || len(s.attaches) != 1 || s.attaches[0].RunID != "new-run" {
		t.Fatalf("create/start/attach boundary broken: starts=%v attaches=%+v", s.starts, s.attaches)
	}
	m.Update(attachReturnedMsg{generation: m.generation})
	if m.mode != listMode || m.selectedID != "new-0" || len(s.stops)+len(s.removes) != 0 {
		t.Fatal("return from attach lost selection or stopped process")
	}
}

func TestCreatedDescriptorSurvivesStartFailure(t *testing.T) {
	s := &fakeService{startErr: errors.New("codex missing")}
	m := modelWith(s)
	press(m, "n")
	result := runOperation(t, m, press(m, "enter"))
	if result.err == nil || m.mode != listMode || m.selectedID != "new-0" || len(s.items) != 1 || len(s.attaches) != 0 {
		t.Fatalf("start failure lost descriptor or attached: %+v mode=%v", result, m.mode)
	}
}

func TestObservationErrorStillAllowsExactOwnedRunCleanup(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("owned", "异常会话", "error", "exact-run")}}
	m := modelWith(s)
	press(m, "s")
	if m.mode != confirmStopMode {
		t.Fatal("observation error hid owned run cleanup")
	}
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.stops) != 1 || s.stops[0] != (mutation{"owned", "exact-run"}) || len(s.removes) != 0 {
		t.Fatalf("error cleanup lost exact run or removed descriptor: result=%+v stops=%+v removes=%+v", result, s.stops, s.removes)
	}
}

func TestPersistedCreateErrorSelectsDescriptorWithoutStartRetry(t *testing.T) {
	createErr := errors.New("auto-start failed")
	s := &fakeService{createErr: createErr}
	m := modelWith(s)
	press(m, "n")
	result := press(m, "enter")().(operationResultMsg)
	_, refresh := m.Update(result)
	if !errors.Is(result.err, createErr) || result.selectedID != "new-0" || m.mode != listMode || m.selectedID != "new-0" {
		t.Fatalf("persisted creation error lost selected descriptor: result=%+v mode=%v selected=%q", result, m.mode, m.selectedID)
	}
	if !m.noticeErr || !strings.HasPrefix(m.notice, "会话已创建，启动失败：") || len(s.creates) != 1 || len(s.items) != 1 || len(s.starts)+len(s.attaches) != 0 {
		t.Fatalf("auto-start error was retried or mislabelled: notice=%q creates=%d descriptors=%d starts=%v attaches=%v", m.notice, len(s.creates), len(s.items), s.starts, s.attaches)
	}
	// Execute the actual list refresh command after returning from the form.
	batch := refresh().(tea.BatchMsg)
	m.Update(batch[0]())
	m.Update(m.refresh()())
	if len(m.items) != 1 || m.selectedID != "new-0" || m.items[0].ID != "new-0" || len(s.starts)+len(s.attaches) != 0 {
		t.Fatalf("refresh changed selection or retried failed start: selected=%q items=%+v starts=%v attaches=%v", m.selectedID, m.items, s.starts, s.attaches)
	}
}

func TestAlreadyRunningCreateDoesNotRestartHost(t *testing.T) {
	s := &fakeService{autoStart: true}
	m := modelWith(s)
	press(m, "n")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.starts) != 0 || len(s.attaches) != 1 || s.attaches[0].RunID != "created-run" {
		t.Fatalf("auto-started creation was restarted: %+v starts=%v attaches=%+v", result, s.starts, s.attaches)
	}
}

func TestOpenStartsStoppedOrExitedAndPreservesRunning(t *testing.T) {
	for _, state := range []string{"stopped", "exited", "running", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s := &fakeService{items: []terminalhost.SessionInfo{info("target", "终端", state, "original")}}
			m := modelWith(s)
			result := runOperation(t, m, press(m, "o"))
			if state == "unknown" {
				if result.err == nil || len(s.starts)+len(s.attaches) != 0 {
					t.Fatal("unknown process state launched a new host")
				}
				return
			}
			if result.err != nil || len(s.attaches) != 1 || s.attaches[0].ID != "target" || m.mode != attachMode {
				t.Fatalf("failed to open target: %+v attaches=%v", result, s.attaches)
			}
			if state == "running" && len(s.starts) != 0 {
				t.Fatal("opening running process restarted it")
			}
			if state != "running" && (len(s.starts) != 1 || s.starts[0] != "target") {
				t.Fatal("stopped process was not explicitly started")
			}
		})
	}
}

func TestStopAndRemoveConfirmationsBindUUIDAndRunID(t *testing.T) {
	for _, action := range []string{"s", "d"} {
		t.Run(action, func(t *testing.T) {
			a, b := info("a", "重复名称", "running", "a-run"), info("b", "重复名称", "running", "b-run")
			s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
			m := modelWith(s)
			press(m, "j")
			press(m, action)
			if m.target.ID != "b" || m.target.RunID != "b-run" {
				t.Fatal("confirmation lost original binding")
			}
			press(m, "esc")
			if len(s.stops)+len(s.removes) != 0 {
				t.Fatal("cancel mutated service")
			}
			// Finish the refresh started when closing the dialog.
			m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: []terminalhost.SessionInfo{a, b}})
			press(m, action)
			result := runOperation(t, m, press(m, "enter"))
			if result.err != nil {
				t.Fatal(result.err)
			}
			mutations := s.stops
			if action == "d" {
				mutations = s.removes
			}
			if len(mutations) != 1 || mutations[0] != (mutation{"b", "b-run"}) {
				t.Fatalf("wrong mutation target: %+v", mutations)
			}
		})
	}
}

func TestConfirmationRejectsRestartedInstanceInAnotherWindow(t *testing.T) {
	for _, action := range []string{"s", "d"} {
		t.Run(action, func(t *testing.T) {
			original := info("same-id", "终端", "running", "old-run")
			s := &fakeService{items: []terminalhost.SessionInfo{original}}
			m := modelWith(s)
			press(m, action)
			changed := original
			changed.RunID = "new-run"
			s.replace(changed)
			result := runOperation(t, m, press(m, "enter"))
			if result.err == nil || len(s.stops)+len(s.removes) != 0 || m.mode != listMode {
				t.Fatalf("stale confirmation reached mutation: err=%v stops=%v removes=%v", result.err, s.stops, s.removes)
			}
		})
	}
}

func TestConfirmationRejectsChangedLocalSelection(t *testing.T) {
	a, b := info("a", "一", "running", "a1"), info("b", "二", "running", "b1")
	s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
	m := modelWith(s)
	press(m, "s")
	m.setItems([]terminalhost.SessionInfo{b})
	press(m, "enter")
	if m.mode != listMode || len(s.stops) != 0 {
		t.Fatal("confirmation followed replacement selection")
	}
}

func TestRenameSavesThroughService(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", "原名", "running", "a1")}}
	m := modelWith(s)
	press(m, "r")
	m.name.SetValue("新名称")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.renames) != 1 || s.renames[0].id != "a" || s.renames[0].name != "新名称" || s.items[0].Name != "新名称" {
		t.Fatalf("rename not persisted: %+v %+v", result, s.renames)
	}
}

func TestStaleAsyncResultsCannotOverwriteDialogsOrOperations(t *testing.T) {
	a := info("a", "原名", "running", "a1")
	s := &fakeService{items: []terminalhost.SessionInfo{a}}
	m := modelWith(s)
	oldRefresh := m.refresh()
	oldResult := oldRefresh().(listResultMsg)
	press(m, "r")
	m.name.SetValue("刚保存的名字")
	m.Update(oldResult)
	if m.mode != renameMode || m.name.Value() != "刚保存的名字" {
		t.Fatal("pending list replaced active rename form")
	}
	runOperation(t, m, press(m, "enter"))
	m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: s.items})
	m.Update(oldResult)
	if len(m.items) != 1 || m.items[0].Name != "刚保存的名字" {
		t.Fatal("old refresh overwrote successful rename")
	}
	press(m, "n")
	m.Update(operationResultMsg{generation: m.generation - 1, notice: "stale"})
	if m.mode != createMode || m.notice == "stale" {
		t.Fatal("old operation replaced newer modal")
	}
}

func TestPollOnlyRunsWhileListVisibleAndAllowsOneFetch(t *testing.T) {
	s := &fakeService{}
	m := modelWith(s)
	token := m.pollToken
	_, cmd := m.Update(pollMsg{token: token})
	if cmd == nil || !m.listInFlight {
		t.Fatal("visible list did not schedule refresh")
	}
	request := m.listRequest
	m.Update(pollMsg{token: token})
	if m.listRequest != request {
		t.Fatal("overlapping poll scheduled another list fetch")
	}
	press(m, "n")
	_, cmd = m.Update(pollMsg{token: token})
	if cmd != nil || m.listInFlight {
		t.Fatal("dialog continued polling list")
	}
	press(m, "esc")
	newRequest := m.listRequest
	_, cmd = m.Update(pollMsg{token: token})
	if cmd != nil || m.listRequest != newRequest {
		t.Fatal("obsolete tick restarted another polling chain")
	}
}

func TestRecoveredListClearsTransientFailureAndKeepsOperationNotice(t *testing.T) {
	s := &fakeService{listErr: errors.New("temporarily unavailable")}
	m := modelWith(s)
	if !m.noticeErr || !m.listFailure {
		t.Fatal("list error was not displayed")
	}
	s.listErr = nil
	m.Update(m.refresh()())
	if m.noticeErr || m.notice != "" {
		t.Fatal("successful refresh retained obsolete failure")
	}
	m.setNotice("会话名称已保存。", false)
	m.Update(m.refresh()())
	if m.notice != "会话名称已保存。" {
		t.Fatal("routine refresh cleared an operation result")
	}
}

func TestSeparateModelsHaveIndependentSelectionAndForms(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", "一", "running", "a1"), info("b", "二", "running", "b1")}}
	first, second := modelWith(s), modelWith(s)
	press(first, "j")
	press(first, "n")
	first.name.SetValue("窗口一")
	if second.selectedID != "a" || second.mode != listMode || second.name.Value() == "窗口一" {
		t.Fatal("UI state leaked between windows")
	}
}

func TestQuitNeverStopsOrRemovesHostedProcesses(t *testing.T) {
	for _, currentMode := range []mode{listMode, createMode, renameMode, confirmStopMode, confirmRemoveMode, workingMode, attachMode} {
		t.Run(fmt.Sprint(currentMode), func(t *testing.T) {
			s := &fakeService{items: []terminalhost.SessionInfo{info("a", "一", "running", "a1")}}
			m := modelWith(s)
			m.mode = currentMode
			cmd := press(m, "ctrl+c")
			if cmd == nil || !m.quitting {
				t.Fatal("Ctrl+C did not quit this UI mode")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok || len(s.stops)+len(s.removes) != 0 {
				t.Fatal("UI quit stopped native process")
			}
		})
	}
}

func TestViewBoundsChineseNamesAndAllModalSizes(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", strings.Repeat("中文会话🐱", 30)+"\x1b[31m\rInjected", "running", "a1")}}
	m := modelWith(s)
	m.target = s.items[0]
	m.name.SetValue(strings.Repeat("名称", 100))
	m.dir.SetValue(strings.Repeat("路径", 100))
	for _, currentMode := range []mode{listMode, createMode, renameMode, confirmStopMode, confirmRemoveMode, workingMode, attachMode} {
		for _, size := range [][2]int{{80, 24}, {40, 12}, {20, 6}, {5, 3}, {1, 1}} {
			m.mode = currentMode
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) > size[1] {
				t.Fatalf("mode=%d size=%v height=%d", currentMode, size, len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("mode=%d size=%v width=%d line=%q", currentMode, size, ansi.StringWidth(line), line)
				}
				if strings.ContainsAny(ansi.Strip(line), "\r\x1b") {
					t.Fatalf("unsafe control character rendered: %q", line)
				}
			}
		}
	}
}

func TestStateLabelsContainOnlyProcessFacts(t *testing.T) {
	for state, want := range map[string]string{"running": "正在运行", "exited": "已退出", "stopped": "已停止", "error": "异常", "waiting": "未知", "idle": "未知", "": "未知"} {
		if stateLabel(state) != want {
			t.Fatalf("state %q labelled %q, want %q", state, stateLabel(state), want)
		}
	}
}

// TestNativeUIAttachChild is the harmless child executed by the real Tea loop.
func TestNativeUIAttachChild(t *testing.T) {}

func TestActualExecProcessReturnsToList(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeService{
		items: []terminalhost.SessionInfo{info("target", "终端", "running", "run")},
		attach: func() *exec.Cmd {
			return exec.Command(executable, "-test.run=^TestNativeUIAttachChild$")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var program *tea.Program
	var opened, returned bool
	var filterErr error
	// A concrete EOF reader survives Tea's ReleaseTerminal/RestoreTerminal
	// cycle; nil input cannot be reinitialized by this pinned Tea version.
	program = tea.NewProgram(New(s, "."), tea.WithContext(ctx), tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		switch msg := msg.(type) {
		case listResultMsg:
			if !opened {
				opened = true
				go program.Send(key("enter"))
			} else if returned {
				go program.Send(key("q"))
			}
		case attachReturnedMsg:
			returned, filterErr = true, msg.err
		}
		return msg
	}))
	final, err := program.Run()
	if err != nil {
		t.Fatal(err)
	}
	m := final.(*Model)
	if !returned || filterErr != nil || m.mode != listMode || m.selectedID != "target" {
		t.Fatalf("ExecProcess failed to resume list: returned=%v error=%v mode=%d selected=%s", returned, filterErr, m.mode, m.selectedID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.attaches) != 1 || len(s.starts)+len(s.stops)+len(s.removes) != 0 {
		t.Fatalf("attach affected hosted process: attaches=%d starts=%d stops=%d removes=%d", len(s.attaches), len(s.starts), len(s.stops), len(s.removes))
	}
}
