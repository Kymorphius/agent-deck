package web

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/gorilla/websocket"
)

// matrix/hardened: exercise the real authenticated WS read loop without a
// terminal process; passive viewers must never invoke the optional capability.
type matrixSleepingMutator struct {
	fakeMutator
	wake func(string) (string, string, bool, error)
}

func (m *matrixSleepingMutator) WakeSleepingSession(id string) (string, string, bool, error) {
	return m.wake(id)
}

func matrixSleepingWSServer(t *testing.T, readOnly bool, m *matrixSleepingMutator, pending ...bool) (*Server, *websocket.Conn) {
	t.Helper()
	srv := NewServer(Config{Profile: "work", ReadOnly: readOnly, Token: "sleep-test"})
	srv.menuData = &fakeMenuDataLoader{snapshot: &MenuSnapshot{Profile: "work", Items: []MenuItem{{
		Type: MenuItemTypeSession, Session: &MenuSession{ID: "sleep-1", Status: session.StatusStopped, Sleeping: true, SleepPending: len(pending) > 0 && pending[0], TmuxSession: "must-not-attach-passively"},
	}}}}
	srv.mutator = m
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(httpServer.URL, "/ws/session/sleep-1?token=sleep-test"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	expectWSStatusEvent(t, conn, "connected")
	expectWSStatusEvent(t, conn, "ready")
	var status wsServerMessage
	if err := conn.ReadJSON(&status); err != nil || status.Event != "sleeping" {
		t.Fatalf("sleep state missing: %v %+v", err, status)
	}
	if len(pending) > 0 && pending[0] && !strings.Contains(status.Message, "pending") {
		t.Fatalf("pending state presented as completed sleep: %+v", status)
	}
	return srv, conn
}

func TestMatrixPendingSleepWSAllowsExplicitSafeRecoveryOnly(t *testing.T) {
	var calls atomic.Int32
	_, conn := matrixSleepingWSServer(t, false, &matrixSleepingMutator{wake: func(string) (string, string, bool, error) {
		calls.Add(1)
		return "", "", false, errors.New("original processes are not proven exited")
	}}, true)
	for _, msg := range []wsClientMessage{{Type: "resize", Cols: 80, Rows: 24}, {Type: "input"}, {Type: "ping"}} {
		if err := conn.WriteJSON(msg); err != nil {
			t.Fatal(err)
		}
	}
	expectWSStatusEvent(t, conn, "pong")
	if calls.Load() != 0 {
		t.Fatal("passive pending observer attempted recovery")
	}
	if err := conn.WriteJSON(wsClientMessage{Type: "input", Data: "resume"}); err != nil {
		t.Fatal(err)
	}
	var got wsServerMessage
	if err := conn.ReadJSON(&got); err != nil || got.Code != "SESSION_WAKE_FAILED" || calls.Load() != 1 {
		t.Fatalf("pending explicit recovery swallowed or bypassed proof: %v %+v calls=%d", err, got, calls.Load())
	}
}

func TestMatrixPendingSleepMenuIsExplicitAndRecoverable(t *testing.T) {
	inst := session.NewInstanceWithTool("pending", t.TempDir(), "codex")
	inst.AutoSleep = &session.AutoSleepState{Version: 1, Phase: "pending"}
	got := toMenuSession(inst)
	if !got.Sleeping || !got.SleepPending {
		t.Fatalf("pending cleanup lost from Web menu: %+v", got)
	}
}

func TestMatrixSleepingWSPassiveMessagesDoNotWake(t *testing.T) {
	var calls atomic.Int32
	_, conn := matrixSleepingWSServer(t, false, &matrixSleepingMutator{wake: func(string) (string, string, bool, error) {
		calls.Add(1)
		return "", "", false, errors.New("unexpected wake")
	}})
	for _, msg := range []wsClientMessage{{Type: "resize", Cols: 80, Rows: 24}, {Type: "input"}, {Type: "ping"}} {
		if err := conn.WriteJSON(msg); err != nil {
			t.Fatal(err)
		}
	}
	expectWSStatusEvent(t, conn, "pong")
	if got := calls.Load(); got != 0 {
		t.Fatalf("passive viewer invoked wake %d times", got)
	}
}

func TestMatrixSleepingWSReadOnlyInputDoesNotWake(t *testing.T) {
	var calls atomic.Int32
	_, conn := matrixSleepingWSServer(t, true, &matrixSleepingMutator{wake: func(string) (string, string, bool, error) {
		calls.Add(1)
		return "", "", false, nil
	}})
	if err := conn.WriteJSON(wsClientMessage{Type: "input", Data: "hello"}); err != nil {
		t.Fatal(err)
	}
	var got wsServerMessage
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "READ_ONLY" || calls.Load() != 0 {
		t.Fatalf("readonly result=%+v wake calls=%d", got, calls.Load())
	}
}

func TestMatrixSleepingWSWakeFailureIsVisibleAndRetryable(t *testing.T) {
	var calls atomic.Int32
	_, conn := matrixSleepingWSServer(t, false, &matrixSleepingMutator{wake: func(string) (string, string, bool, error) {
		calls.Add(1)
		return "", "", false, errors.New("exact resume unavailable")
	}})
	for range 2 {
		if err := conn.WriteJSON(wsClientMessage{Type: "input", Data: "hello"}); err != nil {
			t.Fatal(err)
		}
		var got wsServerMessage
		if err := conn.ReadJSON(&got); err != nil {
			t.Fatal(err)
		}
		if got.Code != "SESSION_WAKE_FAILED" {
			t.Fatalf("wake failure was hidden: %+v", got)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("failed wake became a terminal-fatal connection")
	}
}
