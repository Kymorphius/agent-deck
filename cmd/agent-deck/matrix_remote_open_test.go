package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type matrixRemoteOpenAttacher struct {
	prepared, attached int
	result             session.RemoteOpenResult
	err                error
}

func (r *matrixRemoteOpenAttacher) PrepareOpen(context.Context, string) (session.RemoteOpenResult, error) {
	r.prepared++
	return r.result, r.err
}
func (r *matrixRemoteOpenAttacher) Attach(string) error { r.attached++; return nil }

// matrix/hardened: public remote attach cannot mutate before TTY validation,
// silently recover through an unsupported old CLI, or attach a capped target.
func TestMatrixRemoteCLIArchitecture(t *testing.T) {
	for _, tc := range []struct {
		name, status, disposition string
		interactive               bool
		err                       error
		prepared, attached        int
		wantErr                   bool
	}{
		{"no TTY", "error", "resumed", false, nil, 0, 0, true},
		{"exact resumed", "error", "resumed", true, nil, 1, 1, false},
		{"cached error live", "error", "existing", true, nil, 1, 1, false},
		{"cap reached", "error", "queued", true, nil, 1, 0, true},
		{"old active", "waiting", "", true, session.ErrRemotePrepareOpenUnsupported, 1, 1, false},
		{"old error", "error", "", true, session.ErrRemotePrepareOpenUnsupported, 1, 0, true},
		{"old unknown", "", "", true, session.ErrRemotePrepareOpenUnsupported, 1, 0, true},
		{"timeout", "waiting", "", true, context.DeadlineExceeded, 1, 0, true},
		{"auth failure", "waiting", "", true, errors.New("permission denied"), 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &matrixRemoteOpenAttacher{result: session.RemoteOpenResult{ID: "full-id", Status: "starting", Disposition: tc.disposition}, err: tc.err}
			var notices bytes.Buffer
			err := prepareAndAttachRemote(context.Background(), r, session.RemoteSessionInfo{ID: "full-id", Status: tc.status}, tc.interactive, &notices)
			if (err != nil) != tc.wantErr || r.prepared != tc.prepared || r.attached != tc.attached {
				t.Fatalf("err %v prepared %d attached %d", err, r.prepared, r.attached)
			}
		})
	}
}
