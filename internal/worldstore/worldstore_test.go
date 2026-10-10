package worldstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeEntry struct {
	requests []map[string]any
	answer   func(req map[string]any) (string, error)
}

func (f *fakeEntry) client() *Client {
	c := NewWithRunner(func(_ context.Context, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		f.requests = append(f.requests, req)
		out, err := f.answer(req)
		return []byte(out), err
	})
	c.now = func() time.Time { return time.Unix(1791600000, 0) }
	return c
}

func ok(result string) string { return `{"ok":true,"result":` + result + `}` }

const snapshotJSON = `{"accountId":"acc","spaceId":"space","updatedAt":"u","pending":false,"tuiCanWrite":true,
 "availableFields":["任务｜状态","任务｜agent-deck 会话关联","任务｜执行分类"],
 "groups":{
  "tasks":[
   {"id":"t1","name":"写周报","imported":false,"fields":{"任务｜状态":"待办","任务｜计划日期":1790784000,"任务｜执行分类":"AI 执行"}},
   {"id":"t2","name":"修复登录","imported":false,"fields":{"任务｜状态":"待办","任务｜agent-deck 会话关联":"{\"version\":1,\"links\":[{\"source\":\"agent-deck\",\"deviceId\":\"host\",\"sessionId\":\"s1\",\"linkedAt\":1}]}"}},
   {"id":"t3","name":"整理行李","imported":false,"fields":{"任务｜状态":"待办"}},
   {"id":"t4","name":"理发","imported":false,"fields":{"任务｜状态":"周期进行中","任务｜agent-deck 会话关联":"free text"}},
   {"id":"t5","name":"旧任务","imported":true,"fields":{"任务｜状态":"已完成","任务｜完成时间":1790000000000}}
  ],
  "executions":[
   {"id":"e1","name":"执行｜修复登录","fields":{"执行｜状态":"进行中","执行｜关联任务":["t2"],"执行｜开始时间":1791500000}},
   {"id":"e2","name":"执行｜整理行李","fields":{"执行｜状态":"暂停","执行｜关联任务":["t3"],"执行｜开始时间":1791400000}},
   {"id":"e3","name":"执行｜写周报","fields":{"执行｜状态":"已完成","执行｜关联任务":["t1"]}}
  ]}}`

func boardEntry() *fakeEntry {
	return &fakeEntry{answer: func(req map[string]any) (string, error) {
		if req["op"] == "snapshot" {
			return ok(snapshotJSON), nil
		}
		return ok(`{"verified":true}`), nil
	}}
}

func TestBoardDerivesLanesFromStatusAndOpenExecutions(t *testing.T) {
	board, err := boardEntry().client().Board(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if board.Owner != (Owner{"acc", "space"}) || !board.LinksReady || !board.CanWrite {
		t.Fatalf("board = %+v", board)
	}
	want := map[string]string{"t1": "todo", "t2": "doing", "t3": "paused", "t4": "review", "t5": "done"}
	for id, lane := range want {
		if got := board.Find(id).Lane; got != lane {
			t.Errorf("%s lane = %s, want %s", id, got, lane)
		}
	}
	if board.Running == nil || board.Running.ID != "e1" || board.RunningTaskID != "t2" {
		t.Fatalf("running = %+v / %s", board.Running, board.RunningTaskID)
	}
	if *board.Find("t1").PlanDate != 1790784000 || *board.Find("t5").CompletedAt != 1790000000 {
		t.Fatal("dates must be Unix seconds (milliseconds normalised)")
	}
	if !board.Find("t2").LinkedTo("host", "s1") || board.Find("t2").LinksRaw == "" {
		t.Fatalf("links: %+v", board.Find("t2"))
	}
	if !board.Find("t4").LinksUnknown || !board.Find("t5").ReadOnly {
		t.Fatal("unknown link format and imported tasks must be flagged")
	}
}

func TestPendingOrUnconfirmedGroupsAreNotEmpty(t *testing.T) {
	for _, snap := range []string{
		`{"accountId":"a","spaceId":"s","pending":true,"groups":{"tasks":[],"executions":[]}}`,
		`{"accountId":"a","spaceId":"s","pending":false,"groups":{"tasks":null,"executions":[]}}`,
		`{"accountId":"","spaceId":"s","pending":false,"groups":{"tasks":[],"executions":[]}}`,
	} {
		f := &fakeEntry{answer: func(map[string]any) (string, error) { return ok(snap), nil }}
		if _, err := f.client().Board(context.Background()); CodeOf(err) != CodeSourceUnavailable {
			t.Fatalf("%s: err = %v", snap, err)
		}
	}
}

func TestActionsFollowWorldStoreFlow(t *testing.T) {
	f := boardEntry()
	c := f.client()
	board, _ := c.Board(context.Background())
	owner := board.Owner
	if err := c.Start(context.Background(), owner, *board.Find("t1"), "agentdeck-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := c.Pause(context.Background(), owner, *board.Find("t2")); err != nil {
		t.Fatal(err)
	}
	if err := c.Resume(context.Background(), owner, *board.Find("t3")); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(context.Background(), owner, *board.Find("t1"), "写完了"); err != nil {
		t.Fatal(err)
	}
	payloads := []map[string]any{}
	for _, r := range f.requests[1:] {
		if r["op"] != "task.action" || r["confirm"] != true || r["owner"] == nil {
			t.Fatalf("request = %v", r)
		}
		payloads = append(payloads, r["payload"].(map[string]any))
	}
	checks := []map[string]any{
		{"mode": "start", "taskId": "t1", "expectedTaskStatus": "待办", "operationId": "agentdeck-0123456789abcdef"},
		{"mode": "pause", "executionId": "e1", "expectedStatus": "进行中"},
		{"mode": "resume", "executionId": "e2", "expectedStatus": "暂停"},
		{"mode": "complete", "taskId": "t1", "expectedTaskStatus": "待办", "result": "写完了", "expectedResult": ""},
	}
	for i, want := range checks {
		for k, v := range want {
			if payloads[i][k] != v {
				t.Fatalf("payload %d %s = %v, want %v (%v)", i, k, payloads[i][k], v, payloads[i])
			}
		}
		if payloads[i]["now"] != float64(1791600000) {
			t.Fatalf("payload %d needs an explicit now: %v", i, payloads[i])
		}
	}
}

func TestActionsRefusedBeforeSending(t *testing.T) {
	f := boardEntry()
	c := f.client()
	board, _ := c.Board(context.Background())
	o := board.Owner
	for name, err := range map[string]error{
		"start with open execution": c.Start(context.Background(), o, *board.Find("t2"), "agentdeck-x1234567"),
		"start closed":              c.Start(context.Background(), o, *board.Find("t5"), "agentdeck-x1234567"),
		"pause without running":     c.Pause(context.Background(), o, *board.Find("t1")),
		"resume running":            c.Resume(context.Background(), o, *board.Find("t2")),
		"complete recurring":        c.Complete(context.Background(), o, *board.Find("t4"), ""),
		"complete done":             c.Complete(context.Background(), o, *board.Find("t5"), ""),
	} {
		if CodeOf(err) != CodeBadInput {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := c.Link(context.Background(), o, *board.Find("t4"), "host", "s"); CodeOf(err) != CodeBadInput {
		t.Errorf("link over unknown format: %v", err)
	}
	if len(f.requests) != 1 {
		t.Fatalf("only the snapshot may reach World Store: %v", f.requests)
	}
}

func TestWorldStoreErrorsKeepTheirCode(t *testing.T) {
	f := &fakeEntry{answer: func(map[string]any) (string, error) {
		return `{"ok":false,"error":{"code":"REDLINE_REVIEW_REQUIRED","message":"关联红线尚未核对"}}`, errors.New("exit status 1")
	}}
	err := f.client().Start(context.Background(), Owner{"a", "s"}, Task{ID: "t", Status: "待办"}, "agentdeck-x1234567")
	if CodeOf(err) != CodeRedlineReview {
		t.Fatalf("err = %v", err)
	}
}

func TestInterruptedWriteIsUncertainButReadIsNot(t *testing.T) {
	f := &fakeEntry{answer: func(map[string]any) (string, error) { return "", errors.New("signal: killed") }}
	c := f.client()
	err := c.Complete(context.Background(), Owner{"a", "s"}, Task{ID: "t", Status: "待办"}, "")
	if !Uncertain(err) {
		t.Fatalf("write err = %v", err)
	}
	if _, err := c.Board(context.Background()); CodeOf(err) != CodeUnavailable {
		t.Fatalf("read err = %v", err)
	}
	// Garbage instead of a JSON answer after a write is also uncertain.
	f.answer = func(map[string]any) (string, error) { return "half a line", nil }
	if err := c.Complete(context.Background(), Owner{"a", "s"}, Task{ID: "t", Status: "待办"}, ""); !Uncertain(err) {
		t.Fatalf("garbage err = %v", err)
	}
	// An entry that never started cannot have written anything.
	f.answer = func(map[string]any) (string, error) {
		return "", &Error{Code: CodeUnavailable, Message: "did not start"}
	}
	if err := c.Complete(context.Background(), Owner{"a", "s"}, Task{ID: "t", Status: "待办"}, ""); CodeOf(err) != CodeUnavailable {
		t.Fatalf("start failure err = %v", err)
	}
}

func TestLinkRequestsQuoteTheStoredBaseline(t *testing.T) {
	f := boardEntry()
	f.answer = func(req map[string]any) (string, error) {
		if req["op"] == "snapshot" {
			return ok(snapshotJSON), nil
		}
		return ok(`{"links":[]}`), nil
	}
	c := f.client()
	board, _ := c.Board(context.Background())
	t2 := *board.Find("t2")
	if _, err := c.Unlink(context.Background(), board.Owner, t2, "host", "s1"); err != nil {
		t.Fatal(err)
	}
	req := f.requests[1]
	payload, expected := req["payload"].(map[string]any), req["expected"].(map[string]any)
	if req["op"] != "task.unlink" || req["id"] != "t2" || req["confirm"] != true || payload["source"] != Source || payload["sessionId"] != "s1" || expected[LinkField] != t2.LinksRaw {
		t.Fatalf("request = %v", req)
	}
}

func TestOlderWorldStoreWithoutLinkOpsIsExplained(t *testing.T) {
	f := &fakeEntry{answer: func(map[string]any) (string, error) {
		return `{"ok":false,"error":{"code":"INVALID_REQUEST","message":"请求格式或参数无效"}}`, errors.New("exit status 1")
	}}
	_, err := f.client().Link(context.Background(), Owner{"a", "s"}, Task{ID: "t"}, "host", "s")
	if err == nil || !strings.Contains(err.Error(), "does not support agent-deck session links") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewValidatesEntryPath(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(Config{Bridge: "relative/worldstore-ai.cjs"}); err == nil {
		t.Fatal("relative path accepted")
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "multica-task-bridge.cjs")
	_ = os.WriteFile(old, nil, 0o600)
	if _, err := New(Config{Bridge: old}); err == nil || !strings.Contains(err.Error(), "worldstore-ai.cjs") {
		t.Fatalf("retired bridge accepted: %v", err)
	}
	if _, err := New(Config{Bridge: filepath.Join(dir, "missing.cjs")}); err == nil {
		t.Fatal("missing file accepted")
	}
	entry := filepath.Join(dir, "worldstore-ai.cjs")
	_ = os.WriteFile(entry, nil, 0o600)
	if _, err := New(Config{Bridge: entry}); err != nil {
		t.Fatal(err)
	}
}

func TestOperationIDsMatchWorldStoreFormat(t *testing.T) {
	a, b := NewOperationID(), NewOperationID()
	if a == b || len(a) < 8 || len(a) > 80 || strings.Trim(a, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		t.Fatalf("ids %q %q", a, b)
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

func TestExecCategoryProjectionFilterAndUpdate(t *testing.T) {
	f := boardEntry()
	c := f.client()
	board, err := c.Board(context.Background())
	if err != nil || !board.ExecReady {
		t.Fatalf("board=%+v err=%v", board, err)
	}
	on := map[string]bool{}
	for _, task := range board.Tasks {
		on[task.ID] = OnBoard(task, DefaultExecCategory)
	}
	// t1 carries the category, t2 has a linked session; the rest are neither.
	if !on["t1"] || !on["t2"] || on["t3"] || on["t4"] || on["t5"] {
		t.Fatalf("on board = %v", on)
	}
	if OnBoard(Task{ExecCategory: ""}, "") {
		t.Fatal("an empty category must not match every uncategorised task")
	}
	if err := c.SetExecCategory(context.Background(), board.Owner, *board.Find("t3"), DefaultExecCategory); err != nil {
		t.Fatal(err)
	}
	req := f.requests[len(f.requests)-1]
	if req["op"] != "task.update" || req["confirm"] != true || req["id"] != "t3" ||
		req["patch"].(map[string]any)[ExecCategoryField] != DefaultExecCategory || req["expected"].(map[string]any)[ExecCategoryField] != "" {
		t.Fatalf("request = %v", req)
	}
	if err := c.SetExecCategory(context.Background(), board.Owner, *board.Find("t5"), DefaultExecCategory); CodeOf(err) != CodeBadInput {
		t.Fatalf("read-only task: %v", err)
	}
}
