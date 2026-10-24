package ui

import (
	"context"
	"errors"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: every user opening path shares one server-authoritative
// prepare step; none may infer permission to restart from a cached row.
type matrixRemoteOpenMode uint8

const (
	matrixRemoteOpenLegacy matrixRemoteOpenMode = iota
	matrixRemoteOpenEmbedded
	matrixRemoteOpenInsert
	matrixRemoteOpenWindow
)

type matrixRemoteOpenIntent struct {
	generation uint64
	name, id   string
	status     string
	config     session.RemoteConfig
	mode       matrixRemoteOpenMode
	channel    *session.RemoteChannel
	channelGen uint64
}

type matrixRemoteOpenMsg struct {
	intent matrixRemoteOpenIntent
	result session.RemoteOpenResult
	err    error
}

type matrixRemoteOpenRunner interface {
	PrepareOpen(context.Context, string) (session.RemoteOpenResult, error)
}

func (h *Home) beginMatrixRemoteOpen(mode matrixRemoteOpenMode) tea.Cmd {
	if h.cursor < 0 || h.cursor >= len(h.flatItems) {
		return nil
	}
	item := h.flatItems[h.cursor]
	if item.Type != session.ItemTypeRemoteSession || item.RemoteSession == nil || item.RemoteName == "" || item.RemoteSession.ID == "" {
		return nil
	}
	cfg, err := session.LoadUserConfig()
	if err != nil || cfg == nil {
		h.setError(fmt.Errorf("cannot load remote configuration"))
		return nil
	}
	rc, ok := cfg.Remotes[item.RemoteName]
	if !ok || !h.remotePollConfigMatches(item.RemoteName, rc) {
		h.setError(fmt.Errorf("remote configuration changed; refresh before opening"))
		return nil
	}
	h.matrixRemoteOpenGeneration++
	h.cancelMatrixRemoteOpen()
	intent := matrixRemoteOpenIntent{generation: h.matrixRemoteOpenGeneration, name: item.RemoteName, id: item.RemoteSession.ID, status: item.RemoteSession.Status, config: rc, mode: mode}
	intent.channel = session.RemoteChannelFor(intent.name)
	if intent.channel != nil {
		intent.channelGen = intent.channel.ConnectionGeneration()
	}
	h.matrixRemoteOpenPending = &intent
	h.matrixRemoteOpenInputPending = intent.generation
	var runner matrixRemoteOpenRunner = session.NewSSHRunner(intent.name, rc)
	if h.matrixRemoteOpenRunner != nil {
		runner = h.matrixRemoteOpenRunner(intent.name, rc)
	}
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		result, err := runner.PrepareOpen(ctx, intent.id)
		return matrixRemoteOpenMsg{intent: intent, result: result, err: err}
	}
}

func (h *Home) cancelMatrixRemoteOpen() {
	// matrix/hardened: cancel only focus intent. An already-dispatched
	// start/resume must finish its own bounded operation and persistence.
	h.matrixRemoteOpenPending = nil
}

func matrixRemoteOpenChannelMatches(intent matrixRemoteOpenIntent, current *session.RemoteChannel, generation uint64) bool {
	if intent.channel != nil && current != intent.channel {
		return false
	}
	if intent.channelGen == 0 {
		// A first connection created by the capability probe is expected;
		// its subsequent reconnection retires this opening intention.
		return generation <= 1
	}
	return generation == intent.channelGen
}

// matrix/hardened: while opening, typed text must not become dashboard
// shortcuts. Navigation and Escape explicitly cancel and retain their usual
// meaning; other keys are consumed until readiness, without buffering input.
func (h *Home) cancelMatrixRemoteOpenForKey(key tea.KeyMsg) bool {
	if h.matrixRemoteOpenInputPending == 0 {
		return false
	}
	switch key.String() {
	case "esc", "up", "down", "left", "right", "pgup", "pgdown", "home", "end":
		h.cancelMatrixRemoteOpen()
		h.matrixRemoteOpenInputPending = 0
		return false
	default:
		h.setError(fmt.Errorf("remote session is opening; wait for readiness or press Esc to cancel"))
		return true
	}
}

func (h *Home) applyMatrixRemoteOpen(msg matrixRemoteOpenMsg) tea.Cmd {
	if h.matrixRemoteOpenInputPending == msg.intent.generation {
		h.matrixRemoteOpenInputPending = 0
	}
	pending := h.matrixRemoteOpenPending
	if pending == nil || pending.generation != msg.intent.generation {
		return nil
	}
	h.cancelMatrixRemoteOpen()
	// matrix/hardened: navigation, cancellation, host/profile/path changes,
	// and transport reconnection retire an asynchronous opening intention.
	if h.cursor < 0 || h.cursor >= len(h.flatItems) || h.isQuitting {
		return nil
	}
	item := h.flatItems[h.cursor]
	if item.Type != session.ItemTypeRemoteSession || item.RemoteSession == nil || item.RemoteName != msg.intent.name || item.RemoteSession.ID != msg.intent.id {
		return nil
	}
	cfg, err := session.LoadUserConfig()
	if err != nil || cfg == nil {
		return nil
	}
	rc, ok := cfg.Remotes[msg.intent.name]
	if !ok || rc != msg.intent.config || !h.remotePollConfigMatches(msg.intent.name, msg.intent.config) {
		return nil
	}
	channel := session.RemoteChannelFor(msg.intent.name)
	var channelGen uint64
	if channel != nil {
		channelGen = channel.ConnectionGeneration()
	}
	if !matrixRemoteOpenChannelMatches(msg.intent, channel, channelGen) {
		return nil
	}
	if session.IsRemoteInterrupted(msg.err) {
		return h.remoteOutcomeUnknown(msg.intent.name)
	}
	if errors.Is(msg.err, session.ErrRemotePrepareOpenUnsupported) {
		// A live old remote remains attachable. Error/stopped/unknown rows
		// require an update or a separate, explicit restart action.
		switch msg.intent.status {
		case string(session.StatusRunning), string(session.StatusWaiting), string(session.StatusIdle):
			h.setError(fmt.Errorf("%s uses an older Agent Deck; update it for automatic session recovery", msg.intent.name))
		default:
			h.setError(msg.err)
			return nil
		}
	} else if msg.err != nil {
		h.setError(fmt.Errorf("cannot open session on %s: %w", msg.intent.name, msg.err))
		return nil
	} else if msg.result.Disposition == "queued" {
		h.setError(fmt.Errorf("cannot open yet: group %q on %s has reached its concurrency limit (%d)", msg.result.Group, msg.intent.name, msg.result.MaxConcurrent))
		return nil
	}
	// The prepare reply is the only authorization to proceed. Entering an
	// input/PTY mode remains on the UI loop and never runs inside the worker.
	if h.matrixRemoteOpenReady != nil {
		return h.matrixRemoteOpenReady(msg.intent.mode, msg.intent.name, msg.intent.id)
	}
	switch msg.intent.mode {
	case matrixRemoteOpenEmbedded:
		if h.enterEmbeddedMode() {
			return h.startEmbeddedModeCmd()
		}
		return h.attachRemoteSession(msg.intent.name, msg.intent.id)
	case matrixRemoteOpenInsert:
		h.enterInsertMode()
	case matrixRemoteOpenWindow:
		if req, ok := buildRemoteAttachRequest(msg.intent.name, msg.intent.id, resolveITermOpenAs()); ok {
			if err := h.openInNewWindow(req, true); err != nil {
				h.setError(fmt.Errorf("open remote in new window: %w", err))
			}
		}
	default:
		return h.attachRemoteSession(msg.intent.name, msg.intent.id)
	}
	return nil
}
