package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/core"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: An interactive visit may start an unspawned queued session,
// while existing panes and manually stopped/error sessions keep their behavior.
// Call only after the TTY and automatic-sleep gates in handleSessionAttach.
func ensureSessionPaneForAttach(profile string, storage *session.Storage, inst *session.Instance) (*session.Instance, error) {
	exists, err := attachPaneExists(inst)
	if err != nil {
		return nil, err
	}
	if exists {
		return inst, nil
	}
	if inst.Status != session.StatusQueued {
		return nil, fmt.Errorf("session '%s' is not running", inst.Title)
	}

	// matrix/hardened: Reuse core's profile mutation lock without adding a CLI
	// command or nesting Start's own spawn lock. Reload, admission and the
	// targeted save finish before the interactive attach holds the terminal.
	const operation = "matrix.session.attach-queued"
	registry := core.NewRegistry()
	registry.MustRegister(core.Def{
		ID: operation, Class: core.Mutate, Remote: core.RemoteDeny,
		In: "", Out: &session.Instance{},
		Exec: core.Typed(func(_ context.Context, id string) (*session.Instance, error) {
			return startQueuedSessionForAttach(storage, id)
		}),
	})
	result := core.RunWithMutationLock(context.Background(), registry, operation, profile, inst.ID)
	if result.Err != nil {
		return nil, result.Err
	}
	return result.Out.(*session.Instance), nil
}

// matrix/hardened: Inconclusive tmux probes never authorize a new process.
func attachPaneExists(inst *session.Instance) (bool, error) {
	if inst == nil || inst.GetTmuxSession() == nil {
		return false, fmt.Errorf("session has no tmux target")
	}
	exists, err := inst.GetTmuxSession().ProbeExists()
	if err != nil {
		return false, fmt.Errorf("cannot confirm session '%s' before attach: %w", inst.Title, err)
	}
	return exists, nil
}

// matrix/hardened: The profile lock protects this fresh decision from another
// interactive opener or core start/stop. Save only this session's snapshot so
// unrelated session/group edits are never replayed by the attach path.
func startQueuedSessionForAttach(storage *session.Storage, id string) (*session.Instance, error) {
	instances, groups, err := storage.LoadWithGroups()
	if err != nil {
		return nil, fmt.Errorf("cannot reload session before attach: %w", err)
	}
	var inst *session.Instance
	for _, candidate := range instances {
		if candidate.ID == id {
			inst = candidate
			break
		}
	}
	if inst == nil {
		return nil, fmt.Errorf("session was removed before attach")
	}
	exists, err := attachPaneExists(inst)
	if err != nil {
		return nil, err
	}
	if exists {
		return inst, nil
	}
	if inst.Status != session.StatusQueued {
		return nil, fmt.Errorf("session '%s' is not running (state changed to %s)", inst.Title, inst.Status)
	}
	tree := session.NewGroupTreeWithGroups(instances, groups)
	max := session.GroupMaxConcurrent(tree, inst.GroupPath)
	if session.ShouldQueue(instances, inst.GroupPath, max) {
		return nil, fmt.Errorf("session '%s' is still queued: group '%s' is at its concurrency cap %d", inst.Title, inst.GroupPath, max)
	}
	if err := inst.Start(); err != nil {
		return nil, fmt.Errorf("failed to start queued session '%s': %w", inst.Title, err)
	}
	if err := inst.VerifySpawned(spawnVerifyWait); err != nil {
		inst.Status = session.StatusError
		failure := fmt.Errorf("failed to start queued session '%s': %w", inst.Title, err)
		if saveErr := storage.SaveWithGroups([]*session.Instance{inst}, nil); saveErr != nil {
			return nil, errors.Join(failure, fmt.Errorf("failed to save spawn failure: %w", saveErr))
		}
		return nil, failure
	}
	if err := storage.SaveWithGroups([]*session.Instance{inst}, nil); err != nil {
		return nil, fmt.Errorf("session '%s' was started but its state could not be saved: %w", inst.Title, err)
	}
	return inst, nil
}
