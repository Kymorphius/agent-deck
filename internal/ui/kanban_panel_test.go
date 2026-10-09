package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

// matrix/hardened: World Store task board panel.

type kanbanFakeBridge struct {
	ops    []string
	answer func(req map[string]any) (string, error)
}

func (f *kanbanFakeBridge) env() *kanbanEnv {
	run := func(_ context.Context, stdin []byte) ([]byte, error) {
		var req map[string]any
		_ = json.Unmarshal(stdin, &req)
		f.ops = append(f.ops, req["op"].(string))
		out, err := f.answer(req)
		return []byte(out), err
	}
	return &kanbanEnv{client: worldstore.NewWithRunner(run), device: "host"}
}

const kanbanBoardJSON = `{"version":1,"owner":{"accountId":"a","spaceId":"s"},"tasks":[
	{"id":"t1","name":"写周报","status":"待办","lane":"todo","revision":"r1"},
	{"id":"t2","name":"修复登录","status":"进行中","lane":"doing","revision":"r2","agentDeckLinks":[{"source":"agent-deck","deviceId":"host","sessionId":"sess-1","linkedAt":1}]},
	{"id":"t3","name":"历史导入","status":"待办","lane":"todo","revision":"r3","readOnly":true}
],"nextCursor":null}`

func kanbanKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// openKanban opens a panel on the fake bridge and delivers the first read.
func openKanban(t *testing.T, f *kanbanFakeBridge, ctx *kanbanSession) *KanbanPanel {
	t.Helper()
	p := NewKanbanPanel()
	p.SetSize(140, 30)
	lookup := func(id string) (kanbanSession, bool) {
		if id == "sess-1" {
			return kanbanSession{ID: id, Title: "login-fix", Status: session.StatusWaiting}, true
		}
		return kanbanSession{}, false
	}
	cmd := p.Open(f.env(), "", ctx, lookup)
	p.handleLoaded(cmd().(kanbanLoadedMsg))
	return p
}

func kanbanBoardBridge() *kanbanFakeBridge {
	return &kanbanFakeBridge{answer: func(req map[string]any) (string, error) {
		switch req["op"] {
		case "task.list":
			return kanbanBoardJSON, nil
		case "task.update":
			return `{"task":{"id":"t1","status":"暂停"}}`, nil
		case "task.linkExecution":
			return `{"links":[]}`, nil
		}
		return `{"ok":false,"error":{"code":"BAD_REQUEST","message":"unexpected"}}`, nil
	}}
}

func TestKanbanBoardRendersColumnsWithinTerminal(t *testing.T) {
	p := openKanban(t, kanbanBoardBridge(), nil)
	view := p.View()
	for _, want := range []string{"进行中 (1)", "待办 (2)", "修复登录", "写周报", "login-fix (waiting)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	lines := strings.Split(view, "\n")
	if len(lines) != 30 {
		t.Fatalf("view is %d lines, terminal is 30", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 140 {
			t.Fatalf("line wider than terminal (%d): %q", w, l)
		}
	}
	// Narrow terminals show one column with tabs.
	p.SetSize(60, 20)
	if v := p.View(); !strings.Contains(v, "[进行中 1]") || strings.Count(v, "\n") != 19 {
		t.Fatalf("narrow view:\n%s", v)
	}
}

func TestKanbanMoveNeedsConfirmationAndRereads(t *testing.T) {
	f := kanbanBoardBridge()
	p := openKanban(t, f, nil)
	p.Update(kanbanKey("l")) // todo column, first card 写周报
	if p.selected().ID != "t1" {
		t.Fatalf("selected %+v", p.selected())
	}
	p.Update(kanbanKey("m"))
	if p.confirm == nil || p.confirm.kind != "move" {
		t.Fatal("m must open a confirmation")
	}
	p.Update(kanbanKey("right")) // 进行中 → 暂停
	if got := p.confirm.choices[p.confirm.choice]; got != "暂停" {
		t.Fatalf("choice = %s", got)
	}
	p.Update(kanbanKey("esc"))
	if p.confirm != nil || len(f.ops) != 1 {
		t.Fatalf("esc must cancel without writing: %v", f.ops)
	}
	p.Update(kanbanKey("m"))
	p.Update(kanbanKey("right"))
	_, cmd := p.Update(kanbanKey("enter"))
	written := cmd().(kanbanWrittenMsg)
	if written.err != nil || f.ops[len(f.ops)-1] != "task.update" {
		t.Fatalf("written=%+v ops=%v", written, f.ops)
	}
	reload := p.handleWritten(written)
	if reload == nil {
		t.Fatal("a confirmed write must be followed by a fresh read")
	}
}

func TestKanbanUncertainWriteLocksUntilReread(t *testing.T) {
	f := kanbanBoardBridge()
	p := openKanban(t, f, &kanbanSession{ID: "sess-9", Title: "other"})
	f.answer = func(req map[string]any) (string, error) {
		if req["op"] == "task.list" {
			return kanbanBoardJSON, nil
		}
		return `{"ok":false,"error":{"code":"WRITE_UNCERTAIN","message":"原生写入未确认"}}`, nil
	}
	p.Update(kanbanKey("l"))
	p.Update(kanbanKey("L"))
	_, cmd := p.Update(kanbanKey("enter"))
	if follow := p.handleWritten(cmd().(kanbanWrittenMsg)); follow != nil {
		t.Fatal("an uncertain write must not trigger anything automatically")
	}
	if !p.locked {
		t.Fatal("uncertain write must lock")
	}
	p.Update(kanbanKey("x"))
	if p.confirm != nil {
		t.Fatal("writes must stay locked until a fresh read")
	}
	_, cmd = p.Update(kanbanKey("r"))
	p.handleLoaded(cmd().(kanbanLoadedMsg))
	if p.locked {
		t.Fatal("a fresh read must unlock")
	}
}

func TestKanbanRefusesReadOnlyAndUnlinkedActionsWithoutBridge(t *testing.T) {
	f := kanbanBoardBridge()
	p := openKanban(t, f, nil)
	p.Update(kanbanKey("l"))
	p.Update(kanbanKey("j")) // 历史导入 (read-only)
	p.Update(kanbanKey("m"))
	if p.confirm != nil || !strings.Contains(p.notice, "read-only") {
		t.Fatalf("read-only move allowed: %q", p.notice)
	}
	p.Update(kanbanKey("k"))
	p.Update(kanbanKey("L"))
	if p.confirm != nil {
		t.Fatal("link without a context session allowed")
	}
	if len(f.ops) != 1 {
		t.Fatalf("bridge called: %v", f.ops)
	}
}

func TestKanbanEnterJumpsToLinkedSession(t *testing.T) {
	p := openKanban(t, kanbanBoardBridge(), nil)
	// doing column is first; 修复登录 is linked to sess-1.
	_, cmd := p.Update(kanbanKey("enter"))
	if cmd == nil {
		t.Fatal("no jump")
	}
	if got := cmd().(kanbanJumpMsg).sessionID; got != "sess-1" {
		t.Fatalf("jump to %s", got)
	}
	if mark, _ := p.linkMarker(*p.selected()); mark != "●" {
		t.Fatalf("live linked session marker = %q", mark)
	}
}

func TestKanbanFilterNarrowsColumns(t *testing.T) {
	p := openKanban(t, kanbanBoardBridge(), nil)
	p.Update(kanbanKey("/"))
	for _, r := range "周报" {
		p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	p.Update(kanbanKey("enter"))
	total := 0
	for _, c := range p.cols {
		total += len(c.Tasks)
	}
	if total != 1 || p.selected().ID != "t1" {
		t.Fatalf("filter left %d tasks, selected %+v", total, p.selected())
	}
	p.Update(kanbanKey("esc"))
	if p.filter != "" || !p.IsVisible() {
		t.Fatal("first esc clears the filter and keeps the board open")
	}
}

func TestKanbanUnconfiguredShowsSetupHelp(t *testing.T) {
	p := NewKanbanPanel()
	p.SetSize(100, 20)
	if cmd := p.Open(nil, worldstore.ErrNotConfigured.Error(), nil, nil); cmd != nil {
		t.Fatal("unconfigured board must not try to read")
	}
	if v := p.View(); !strings.Contains(v, "not configured") {
		t.Fatalf("view:\n%s", v)
	}
}
