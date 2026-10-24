package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// matrix/hardened: fake execution proves opening never uses cached restart or
// retries an uncertain mutation; no real remote or AI process is involved.
func TestMatrixRemotePrepareOpen(t *testing.T) {
	for _, disposition := range []string{"existing", "started", "resumed", "queued"} {
		t.Run(disposition, func(t *testing.T) {
			var calls [][]string
			r := &SSHRunner{runFn: func(_ context.Context, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if len(calls) == 1 {
					return []byte("prepare-open usage"), nil
				}
				return []byte(fmt.Sprintf(`{"id":"full-id","disposition":%q,"status":"error","group":"work","max_concurrent":1}`, disposition)), nil
			}}
			got, err := r.PrepareOpen(context.Background(), "full-id")
			if err != nil || got.Disposition != disposition {
				t.Fatalf("got %+v, %v", got, err)
			}
			want := [][]string{{"session", "prepare-open", "--help"}, {"session", "prepare-open", "--json", "full-id"}}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls %v", calls)
			}
		})
	}
}

func TestMatrixRemotePrepareOpenProbeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		unsupported  bool
	}{
		{"old stdout", "Error: unknown session command: prepare-open", true},
		{"old ssh stderr", "ssh command failed: exit status 1: Error: unknown session command: prepare-open", true},
		{"authentication", "ssh command failed: Permission denied (publickey)", false},
		{"timeout", "context deadline exceeded", false},
		{"generic unknown", "unknown command", false},
		{"unexpected suffix", "Error: unknown session command: prepare-open: permission denied", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := &SSHRunner{runFn: func(context.Context, ...string) ([]byte, error) { calls++; return nil, errors.New(tc.detail) }}
			_, err := r.PrepareOpen(context.Background(), "full-id")
			if errors.Is(err, ErrRemotePrepareOpenUnsupported) != tc.unsupported || calls != 1 {
				t.Fatalf("err %v calls %d", err, calls)
			}
		})
	}
}

func TestMatrixRemotePrepareOpenMutationNeverReplayed(t *testing.T) {
	for _, mutationErr := range []error{errChannelInterrupted, context.DeadlineExceeded} {
		calls := 0
		r := &SSHRunner{runFn: func(context.Context, ...string) ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return nil, mutationErr
		}}
		_, err := r.PrepareOpen(context.Background(), "full-id")
		if !errors.Is(err, mutationErr) || calls != 2 {
			t.Fatalf("err %v calls %d", err, calls)
		}
	}
	if !remoteVerbReadOnly([]string{"session", "prepare-open", "--help"}) {
		t.Fatal("help must be read only")
	}
	for _, args := range [][]string{{"session", "prepare-open", "--json", "full-id"}, {"session", "prepare-open", "--help", "full-id"}} {
		if remoteVerbReadOnly(args) {
			t.Fatalf("mutation classified read only: %v", args)
		}
	}
}

func TestMatrixRemotePrepareOpenInvalidReply(t *testing.T) {
	for _, body := range []string{`{"id":"different","disposition":"existing"}`, `{"id":"full-id","disposition":"unknown"}`, `{"id":"full-id","disposition":"existing"}`, `{"id":"full-id","disposition":"existing","status":"arbitrary"}`, `{"id":"full-id","disposition":"queued","status":"error","max_concurrent":0}`, `not json`} {
		calls := 0
		r := &SSHRunner{runFn: func(context.Context, ...string) ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return []byte(body), nil
		}}
		if _, err := r.PrepareOpen(context.Background(), "full-id"); err == nil || calls != 2 {
			t.Fatalf("body %s calls %d err %v", body, calls, err)
		}
	}
}

func TestMatrixRemoteConnectionGeneration(t *testing.T) {
	c := &RemoteChannel{gen: 3}
	if c.ConnectionGeneration() != 3 {
		t.Fatal("wrong generation")
	}
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
	if c.ConnectionGeneration() != 4 {
		t.Fatal("reconnection not observed")
	}
}
