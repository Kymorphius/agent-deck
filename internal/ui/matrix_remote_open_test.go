package ui

import (
	"context"
	"errors"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type matrixFakeOpenRunner struct {
	calls  int
	result session.RemoteOpenResult
	err    error
}

func (r *matrixFakeOpenRunner) PrepareOpen(context.Context, string) (session.RemoteOpenResult, error) {
	r.calls++
	return r.result, r.err
}

func matrixRemoteOpenHome(t *testing.T) (*Home, *matrixFakeOpenRunner, *int) {
	t.Helper()
	h := armHomeWithOneRemoteSession(t)
	h.flatItems[0].RemoteSession.Status = string(session.StatusError)
	r := &matrixFakeOpenRunner{result: session.RemoteOpenResult{ID: "remote-id-xyz", Disposition: "resumed", Status: "starting"}}
	h.matrixRemoteOpenRunner = func(string, session.RemoteConfig) matrixRemoteOpenRunner { return r }
	ready := new(int)
	h.matrixRemoteOpenReady = func(matrixRemoteOpenMode, string, string) tea.Cmd { *ready++; return nil }
	return h, r, ready
}

// matrix/hardened: an old exited row, a stale error row with a live pane, and
// every interaction surface share server-authoritative preparation.
func TestMatrixRemoteOpenAllEntryPoints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout bool
		key    string
		mode   matrixRemoteOpenMode
	}{
		{"legacy enter", false, "enter", matrixRemoteOpenLegacy},
		{"embedded enter", true, "enter", matrixRemoteOpenEmbedded},
		{"legacy o", false, "o", matrixRemoteOpenLegacy},
		{"embedded o", true, "o", matrixRemoteOpenEmbedded},
		{"alt enter", true, "alt+enter", matrixRemoteOpenLegacy},
		{"insert", true, "I", matrixRemoteOpenInsert},
		{"new window", true, "shift+enter", matrixRemoteOpenWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r, ready := matrixRemoteOpenHome(t)
			h.embeddedLayout = tc.layout
			_, cmd := h.handleMainKey(keyMsgForMatrixRemote(tc.key))
			if cmd == nil || *ready != 0 || h.insertMode {
				t.Fatal("must prepare before entering")
			}
			msg, ok := cmd().(matrixRemoteOpenMsg)
			if !ok {
				t.Fatalf("message %T", cmd())
			}
			if msg.intent.mode != tc.mode {
				t.Fatalf("mode %v", msg.intent.mode)
			}
			h.applyMatrixRemoteOpen(msg)
			if r.calls != 1 || *ready != 1 {
				t.Fatalf("calls %d ready %d", r.calls, *ready)
			}
		})
	}
}
func keyMsgForMatrixRemote(key string) tea.KeyMsg {
	switch key {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "alt+enter":
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "shift+enter":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{shiftEnterMarker}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func TestMatrixRemoteOpenOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, status, disposition string
		err                       error
		want                      int
	}{
		{"error resumed", "error", "resumed", nil, 1},
		{"cached error live", "error", "existing", nil, 1},
		{"capped error", "error", "queued", nil, 0},
		{"new queued", "queued", "queued", nil, 0},
		{"old error", "error", "", session.ErrRemotePrepareOpenUnsupported, 0},
		{"old live", "waiting", "", session.ErrRemotePrepareOpenUnsupported, 1},
		{"old unknown", "", "", session.ErrRemotePrepareOpenUnsupported, 0},
		{"auth", "error", "", errors.New("permission denied"), 0},
		{"timeout", "waiting", "", context.DeadlineExceeded, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r, ready := matrixRemoteOpenHome(t)
			h.flatItems[0].RemoteSession.Status = tc.status
			r.result.Disposition = tc.disposition
			r.err = tc.err
			msg := h.beginMatrixRemoteOpen(matrixRemoteOpenEmbedded)().(matrixRemoteOpenMsg)
			h.applyMatrixRemoteOpen(msg)
			if *ready != tc.want || h.insertMode {
				t.Fatalf("ready %d want %d insert %v", *ready, tc.want, h.insertMode)
			}
		})
	}
}

func TestMatrixRemoteOpenLateReplyCannotStealFocus(t *testing.T) {
	for _, scenario := range []string{"cancel", "target", "host", "profile", "path", "new intent", "reconnect"} {
		t.Run(scenario, func(t *testing.T) {
			h, _, ready := matrixRemoteOpenHome(t)
			msg := h.beginMatrixRemoteOpen(matrixRemoteOpenEmbedded)().(matrixRemoteOpenMsg)
			switch scenario {
			case "cancel":
				h.matrixRemoteOpenPending = nil
			case "target":
				h.flatItems[0].RemoteSession.ID = "other"
			case "host", "profile", "path":
				rc := msg.intent.config
				switch scenario {
				case "host":
					rc.Host = "other"
				case "profile":
					rc.Profile = "other"
				case "path":
					rc.AgentDeckPath = "other"
				}
				h.remotePollConfig = map[string]session.RemoteConfig{"lab": rc}
			case "new intent":
				h.beginMatrixRemoteOpen(matrixRemoteOpenInsert)
			case "reconnect":
				msg.intent.channel = &session.RemoteChannel{}
			}
			h.applyMatrixRemoteOpen(msg)
			if *ready != 0 {
				t.Fatal("late result opened session")
			}
		})
	}
}

func TestMatrixRemoteOpenPendingInputIsNotDashboardAction(t *testing.T) {
	h, _, ready := matrixRemoteOpenHome(t)
	msg := h.beginMatrixRemoteOpen(matrixRemoteOpenInsert)().(matrixRemoteOpenMsg)
	for _, key := range []string{"a", "d", "R", "h", "j", "k", "l", "tab", "enter"} {
		if !h.cancelMatrixRemoteOpenForKey(keyMsgForMatrixRemote(key)) {
			t.Fatalf("typed %q must be consumed", key)
		}
		if h.matrixRemoteOpenPending == nil || h.matrixRemoteOpenInputPending == 0 || *ready != 0 {
			t.Fatalf("typed %q changed opening intent", key)
		}
	}
	h.applyMatrixRemoteOpen(msg)
	if *ready != 1 || h.matrixRemoteOpenInputPending != 0 {
		t.Fatal("readiness did not settle opening")
	}

	for _, key := range []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyEsc}} {
		h, _, ready = matrixRemoteOpenHome(t)
		msg = h.beginMatrixRemoteOpen(matrixRemoteOpenInsert)().(matrixRemoteOpenMsg)
		if h.cancelMatrixRemoteOpenForKey(key) {
			t.Fatal("explicit navigation/Esc should retain usual action")
		}
		h.applyMatrixRemoteOpen(msg)
		if *ready != 0 || h.matrixRemoteOpenPending != nil || h.matrixRemoteOpenInputPending != 0 {
			t.Fatal("cancelled reply opened session")
		}
	}
}

func TestMatrixRemoteOpenChannelFirstConnectAndReconnect(t *testing.T) {
	channel := &session.RemoteChannel{}
	for _, tc := range []struct {
		name    string
		intent  matrixRemoteOpenIntent
		current *session.RemoteChannel
		gen     uint64
		want    bool
	}{
		{"initial nil remains nil", matrixRemoteOpenIntent{}, nil, 0, true},
		{"initial nil first connect", matrixRemoteOpenIntent{}, channel, 1, true},
		{"initial nil reconnect", matrixRemoteOpenIntent{}, channel, 2, false},
		{"first connect pending", matrixRemoteOpenIntent{channel: channel}, channel, 1, true},
		{"existing unchanged", matrixRemoteOpenIntent{channel: channel, channelGen: 2}, channel, 2, true},
		{"existing reconnect", matrixRemoteOpenIntent{channel: channel, channelGen: 2}, channel, 3, false},
		{"channel replaced", matrixRemoteOpenIntent{channel: channel, channelGen: 2}, &session.RemoteChannel{}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matrixRemoteOpenChannelMatches(tc.intent, tc.current, tc.gen); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMatrixRemoteOpenEmbeddedFallbackStillAttaches(t *testing.T) {
	h, _, _ := matrixRemoteOpenHome(t)
	h.matrixRemoteOpenReady = nil
	h.insertOpenKeySender = func(insertTargetRef) (insertKeySender, error) { return nil, errInsertNoRemoteConfig }
	msg := h.beginMatrixRemoteOpen(matrixRemoteOpenEmbedded)().(matrixRemoteOpenMsg)
	cmd := h.applyMatrixRemoteOpen(msg)
	defer h.isAttaching.Store(false)
	if cmd == nil || !h.isAttaching.Load() || h.embeddedMode || h.insertMode {
		t.Fatal("unavailable embedded mode must retain full-screen attach fallback")
	}
}
