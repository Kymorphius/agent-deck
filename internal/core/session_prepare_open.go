package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/tmux"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: prepare-open records an explicit remote visit. It needs no
// terminal and never attaches, sends input, tears down a pane, or guesses an ID.
type SessionPrepareOpenIn struct {
	Profile string `json:"profile"`
	Session string `json:"session" doc:"Full session ID to open"`
}

type SessionPrepareOpenOut struct {
	ID             string         `json:"id"`
	Disposition    string         `json:"disposition" doc:"existing, started, resumed or queued"`
	Status         session.Status `json:"status"`
	Group          string         `json:"group,omitempty"`
	MaxConcurrent  int            `json:"max_concurrent,omitempty"`
	CodexSessionID string         `json:"codex_session_id,omitempty"`
}

func (deps Deps) sessionPrepareOpen(_ context.Context, in SessionPrepareOpenIn) (SessionPrepareOpenOut, error) {
	d, err := loadSessionData(in.Profile)
	if err != nil {
		return SessionPrepareOpenOut{}, err
	}
	defer d.storage.Close()
	var inst *session.Instance
	for _, candidate := range d.instances {
		if candidate.ID == in.Session {
			inst = candidate
			break
		}
	}
	if inst == nil {
		return SessionPrepareOpenOut{}, Errorf(CodeNotFound, "session '%s' not found; prepare-open requires the full ID", in.Session)
	}
	// matrix/hardened: embedded opens do not pass through session attach.
	// Publish the same durable visit fence before choosing any lifecycle path.
	if err := inst.MarkAccessedForInteraction(); err != nil {
		return SessionPrepareOpenOut{}, Errorf(CodeStorage, "cannot record explicit session visit: %v", err)
	}
	// A queued Codex start may discover an ID asynchronously. This operation
	// reports only the binding it actually validated, never an unsynchronized
	// discovery made while formatting the response.
	nativeID := inst.CodexSessionID
	out := func(disposition string, cap int) SessionPrepareOpenOut {
		return SessionPrepareOpenOut{ID: inst.ID, Disposition: disposition, Status: inst.GetStatusThreadSafe(), Group: inst.GroupPath, MaxConcurrent: cap, CodexSessionID: nativeID}
	}
	tm := inst.GetTmuxSession()
	if tm == nil {
		return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "session has no terminal target")
	}
	exists, err := tm.ProbeExists()
	if err != nil {
		return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "cannot establish terminal state before opening: %v", err)
	}
	if exists {
		// A cached error never authorizes restarting a recovered live pane.
		panes, err := tmux.OutputBounded(tm.SocketName, "display-message", "-p", "-t", "="+tm.Name+":0.0", "#{pane_dead}")
		if err != nil {
			return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "cannot establish primary pane liveness before opening: %v", err)
		}
		flag := strings.TrimSpace(string(panes))
		if flag != "0" && flag != "1" {
			return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "cannot establish primary pane liveness: empty or unknown terminal response")
		}
		dead := flag == "1"
		if dead {
			return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "session '%s' has a retained dead terminal; inspect it and use explicit R restart", inst.Title)
		}
		return out("existing", 0), nil
	}
	if !inst.IsAutoSleeping() && !inst.AutoSleepPending() && inst.Status != session.StatusQueued && inst.Status != session.StatusStopped && inst.Status != session.StatusError {
		return SessionPrepareOpenOut{}, Errorf(CodeNotRunning, "session '%s' has no terminal but its state is %s; inspect it before restarting", inst.Title, inst.Status)
	}
	if inst.Status != session.StatusQueued && !inst.IsAutoSleeping() && !inst.AutoSleepPending() && inst.Tool != "codex" {
		return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "automatic remote recovery supports ordinary Codex sessions only; use explicit R restart for '%s' (%s)", inst.Title, inst.Tool)
	}
	tree := session.NewGroupTreeWithGroups(d.instances, d.groups)
	max := session.GroupMaxConcurrent(tree, inst.GroupPath)
	if session.ShouldQueue(d.instances, inst.GroupPath, max) {
		// Do not discard the stopped/error/sleep recovery intent: a subsequent
		// visit must still require its strict native lifecycle, never fresh Start.
		return out("queued", max), nil
	}
	// Keep the established exact sleep wake lifecycle, including pending
	// ownership reconciliation. Every absent-pane spawn first passes the cap.
	if inst.IsAutoSleeping() || inst.AutoSleepPending() {
		if _, err := session.WakeAutoSleepingSession(d.storage, inst); err != nil {
			return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "%v", err)
		}
		return out("resumed", 0), nil
	}
	disposition := "started"
	if inst.Status == session.StatusQueued {
		err = inst.Start()
	} else {
		disposition = "resumed"
		err = session.ResumeExactCodexForOpen(d.storage, inst)
	}
	if err != nil {
		return SessionPrepareOpenOut{}, Errorf(CodeInvalid, "cannot prepare session '%s': %v", inst.Title, err)
	}
	if err = inst.VerifySpawned(SpawnVerifyWait); err != nil {
		inst.Status = session.StatusError
		failure := fmt.Sprintf("session '%s' spawn could not be confirmed: %v", inst.Title, err)
		if saveErr := d.storage.SaveWithGroups([]*session.Instance{inst}, nil); saveErr != nil {
			failure += fmt.Sprintf("; saving failure state also failed: %v", saveErr)
		}
		return SessionPrepareOpenOut{}, Errorf(CodeSpawnFailed, "%s", failure)
	}
	if err = d.storage.SaveWithGroups([]*session.Instance{inst}, nil); err != nil {
		return SessionPrepareOpenOut{}, Errorf(CodeStorage, "session '%s' was spawned but state persistence failed; its terminal was retained: %v", inst.Title, err)
	}
	return out(disposition, 0), nil
}
