package main

// matrix/hardened: Recent follows accepted input, not successful transport
// writes, pending inbox work or the access fence taken before a send.

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func matrixSubmissionClockFixture(t *testing.T) (*session.Storage, *session.Instance, time.Time) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	storage, err := session.NewStorageWithProfile("submitted-interaction")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := session.NewInstanceWithTool("confirmed input", root, "shell")
	old := time.Now().Add(-time.Hour).UTC()
	inst.LastAccessedAt = old.Add(-time.Minute)
	if err := inst.RecordInteractionAt(old); err != nil {
		t.Fatal(err)
	}
	if err := storage.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	rows, err := storage.Load()
	if err != nil || len(rows) != 1 {
		t.Fatalf("load fixture: rows=%d err=%v", len(rows), err)
	}
	return storage, rows[0], old
}

func TestMatrixSubmittedInteractionUsesTransportAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		message   string
		panes     []string
		sendErr   error
		confirmed bool
	}{
		{
			name: "shell accepted command", message: "echo RECENT_SUBMITTED",
			panes:     []string{"$ \n", "$ echo RECENT_SUBMITTED\nRECENT_SUBMITTED\n$ \n"},
			confirmed: true,
		},
		{
			name: "body delivered without acceptance", message: "sleep 30 && echo RECENT_PENDING",
			panes: []string{"$ \n", "$ sleep 30 && echo RECENT_PENDING\n"},
		},
		{
			name: "failed send", message: "echo RECENT_FAILED", panes: []string{"$ \n"},
			sendErr: errors.New("transport closed before delivery"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, inst, old := matrixSubmissionClockFixture(t)
			accessed := inst.LastAccessedAt
			target := &mockSendRetryTarget{statuses: []string{"waiting"}, panes: tc.panes, sendKeysErr: tc.sendErr}
			result, sendErr := executeSend(target, "shell", tc.message, false, sendExecTuning{
				retry: sendRetryOptions{maxRetries: 2, checkDelay: 0, verifyDelivery: true},
			})
			if (result.delivery == deliverySubmitted && sendErr == nil) != tc.confirmed {
				t.Fatalf("fixture acceptance = %q, %v, want confirmed=%v", result.delivery, sendErr, tc.confirmed)
			}
			at := time.Now().UTC()
			if err := recordSubmittedInteraction(inst, result, sendErr, at); err != nil {
				t.Fatal(err)
			}
			want := old
			if tc.confirmed {
				want = at
			}
			if !inst.LastInteractionAt().Equal(want) || !inst.LastAccessedAt.Equal(accessed) {
				t.Fatalf("interaction=%v access=%v, want %v and unchanged %v", inst.LastInteractionAt(), inst.LastAccessedAt, want, accessed)
			}
			loaded, err := storage.Load()
			if err != nil || len(loaded) != 1 || !loaded[0].LastInteractionAt().Equal(want) {
				t.Fatalf("durable interaction did not match transport acceptance: rows=%d err=%v", len(loaded), err)
			}
		})
	}
}

func TestMatrixSubmittedInteractionIgnoresPendingAndFailedOutcomes(t *testing.T) {
	storage, inst, old := matrixSubmissionClockFixture(t)
	for _, delivery := range []string{"", deliveryQueued, deliveryQueuedSocket, deliveryTypedNotSubmitted, deliveryUnverified} {
		if err := recordSubmittedInteraction(inst, sendDeliveryResult{delivery: delivery}, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// A provisional positive result cannot override a later error verdict.
	if err := recordSubmittedInteraction(inst, sendDeliveryResult{delivery: deliverySubmitted}, errors.New("submission refused"), time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := storage.Load()
	if err != nil || len(loaded) != 1 || !loaded[0].LastInteractionAt().Equal(old) {
		t.Fatalf("unsubmitted work changed durable interaction time: rows=%d err=%v", len(loaded), err)
	}
}
