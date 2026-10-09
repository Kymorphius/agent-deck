package worldstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeBridge struct {
	requests []map[string]any
	answer   func(req map[string]any) (string, error)
}

func (f *fakeBridge) runner() Runner {
	return func(_ context.Context, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		f.requests = append(f.requests, req)
		out, err := f.answer(req)
		return []byte(out), err
	}
}

const ownerJSON = `{"accountId":"acc","spaceId":"space"}`

func TestBoardFollowsPaginationAndDecodesLinkStates(t *testing.T) {
	f := &fakeBridge{answer: func(req map[string]any) (string, error) {
		if req["cursor"] == nil {
			return `{"version":1,"owner":` + ownerJSON + `,"tasks":[
				{"id":"a","name":"A","status":"待办","lane":"todo","revision":"r1"},
				{"id":"b","name":"B","status":"进行中","lane":"doing","revision":"r2","agentDeckLinks":[{"source":"agent-deck","deviceId":"host","sessionId":"s1","linkedAt":1}]}
			],"nextCursor":"b"}`, nil
		}
		return `{"version":1,"owner":` + ownerJSON + `,"tasks":[{"id":"c","name":"C","status":"暂停","lane":"paused","revision":"r3","agentDeckLinks":null}],"nextCursor":null}`, nil
	}}
	board, err := NewWithRunner(f.runner()).Board(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Tasks) != 3 || board.Owner.SpaceID != "space" {
		t.Fatalf("board = %+v", board)
	}
	if f.requests[1]["cursor"] != "b" || f.requests[1]["owner"] == nil {
		t.Fatalf("second page must carry cursor and owner: %v", f.requests[1])
	}
	if a := board.Find("a"); a.AgentDeckLinks != nil || a.LinksUnknown {
		t.Fatalf("absent links must be empty and known: %+v", a)
	}
	if b := board.Find("b"); !b.LinkedTo("host", "s1") {
		t.Fatalf("links not decoded: %+v", b)
	}
	if c := board.Find("c"); !c.LinksUnknown {
		t.Fatalf("null links must be unknown: %+v", c)
	}
}

func TestBridgeErrorsKeepTheirCode(t *testing.T) {
	f := &fakeBridge{answer: func(map[string]any) (string, error) {
		return `{"ok":false,"error":{"code":"CONFLICT","message":"任务已变化"}}`, errors.New("exit status 1")
	}}
	_, err := NewWithRunner(f.runner()).SetStatus(context.Background(), Owner{"acc", "space"}, Task{ID: "a", Revision: "r"}, "暂停")
	if CodeOf(err) != CodeConflict {
		t.Fatalf("err = %v", err)
	}
	req := f.requests[0]
	if req["op"] != "task.update" || req["revision"] != "r" || req["patch"].(map[string]any)["status"] != "暂停" {
		t.Fatalf("request = %v", req)
	}
}

func TestInterruptedWriteIsUncertainButInterruptedReadIsNot(t *testing.T) {
	f := &fakeBridge{answer: func(map[string]any) (string, error) { return "", errors.New("signal: killed") }}
	c := NewWithRunner(f.runner())
	if _, err := c.Link(context.Background(), Owner{"acc", "space"}, Task{ID: "a", Revision: "r"}, "host", "s"); CodeOf(err) != CodeWriteUncertain {
		t.Fatalf("write err = %v", err)
	}
	if _, err := c.Board(context.Background()); CodeOf(err) != CodeUnavailable {
		t.Fatalf("read err = %v", err)
	}
	// A bridge that never started cannot have written anything.
	f.answer = func(map[string]any) (string, error) {
		return "", &Error{Code: CodeUnavailable, Message: "did not start"}
	}
	if _, err := c.Complete(context.Background(), Owner{"acc", "space"}, Task{ID: "a"}, ""); CodeOf(err) != CodeUnavailable {
		t.Fatalf("start failure err = %v", err)
	}
}

func TestStatusChangeRefusesDoneAndUnknownWithoutCallingBridge(t *testing.T) {
	f := &fakeBridge{answer: func(map[string]any) (string, error) { return "{}", nil }}
	c := NewWithRunner(f.runner())
	for _, s := range []string{StatusDone, "已调整", ""} {
		if _, err := c.SetStatus(context.Background(), Owner{}, Task{ID: "a"}, s); CodeOf(err) != CodeBadRequest {
			t.Fatalf("%q: err = %v", s, err)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("bridge called: %v", f.requests)
	}
}

func TestLinkRequestsUseAgentDeckSourceAndConfirmation(t *testing.T) {
	f := &fakeBridge{answer: func(map[string]any) (string, error) {
		return `{"links":[{"source":"agent-deck","deviceId":"host","sessionId":"s","linkedAt":5}]}`, nil
	}}
	links, err := NewWithRunner(f.runner()).Link(context.Background(), Owner{"acc", "space"}, Task{ID: "a", Revision: "r"}, "host", "s")
	if err != nil || len(links) != 1 {
		t.Fatalf("links=%v err=%v", links, err)
	}
	req := f.requests[0]
	if req["source"] != Source || req["confirmed"] != true || req["op"] != "task.linkExecution" {
		t.Fatalf("request = %v", req)
	}
}

func TestNewValidatesBridgePath(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(Config{Bridge: "relative/bridge.cjs"}); err == nil {
		t.Fatal("relative path accepted")
	}
	dir := t.TempDir()
	if _, err := New(Config{Bridge: filepath.Join(dir, "missing.cjs")}); err == nil {
		t.Fatal("missing file accepted")
	}
	path := filepath.Join(dir, "bridge.cjs")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Bridge: path}); err != nil {
		t.Fatal(err)
	}
}

func TestColumnsOrderCapAndFilter(t *testing.T) {
	d := func(v int64) *int64 { return &v }
	tasks := []Task{
		{ID: "1", Name: "late", Lane: "todo", PlanDate: d(200)},
		{ID: "2", Name: "undated", Lane: "todo"},
		{ID: "3", Name: "early", Lane: "todo", PlanDate: d(100)},
		{ID: "4", Name: "gone", Lane: "cancelled"},
		{ID: "5", Name: "odd", Lane: "review", Status: "已调整"},
	}
	for i := 0; i < DoneLimit+3; i++ {
		tasks = append(tasks, Task{ID: "d", Name: "done", Lane: "done", CompletedAt: d(int64(i))})
	}
	cols := Columns(tasks, "")
	if len(cols) != 5 || cols[0].Lane != "doing" || cols[4].Lane != "done" {
		t.Fatalf("cols = %+v", cols)
	}
	todo := cols[1].Tasks
	if todo[0].Name != "early" || todo[1].Name != "late" || todo[2].Name != "undated" {
		t.Fatalf("todo order = %v", todo)
	}
	if len(cols[3].Tasks) != 1 {
		t.Fatalf("unknown statuses must stay visible in review: %+v", cols[3])
	}
	if len(cols[4].Tasks) != DoneLimit || cols[4].Hidden != 3 || *cols[4].Tasks[0].CompletedAt != int64(DoneLimit+2) {
		t.Fatalf("done = %d hidden %d", len(cols[4].Tasks), cols[4].Hidden)
	}
	for _, c := range cols {
		for _, task := range c.Tasks {
			if task.Lane == "cancelled" {
				t.Fatal("cancelled task shown")
			}
		}
	}
	if got := Columns(tasks, "EAR"); len(got[1].Tasks) != 1 || got[1].Tasks[0].Name != "early" {
		t.Fatalf("filter = %+v", got[1])
	}
}

func TestLaunchPromptCarriesTaskContext(t *testing.T) {
	p := LaunchPrompt(Task{Name: "修复登录", Category: "上班", PlanText: "先复现"})
	for _, want := range []string{"修复登录", "上班", "先复现"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q: %s", want, p)
		}
	}
}

func TestLaunchArgsPutFlagsBeforePath(t *testing.T) {
	args := LaunchArgs(LaunchRequest{Profile: "work", Path: "/repo", Group: "g/sub", Title: "T", Prompt: "p", TaskID: "task-1", NoParent: true})
	if args[0] != "-p" || args[1] != "work" || args[2] != "launch" || args[len(args)-1] != "/repo" {
		t.Fatalf("args = %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c claude", "--json", "-t T", "-g g/sub", "--hint worldstore_task=task-1", "-no-parent", "--message-file -"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %v", want, args)
		}
	}
}
