package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func TestCLIStatusCandidatesStoppedBudget(t *testing.T) {
	for _, tc := range []struct {
		count int
		limit time.Duration
	}{{300, 300 * time.Millisecond}, {1000, time.Second}} {
		t.Run(fmt.Sprint(tc.count), func(t *testing.T) {
			instances := make([]*Instance, tc.count)
			for i := range instances {
				inst := &Instance{Title: fmt.Sprintf("stopped-%d", i), Status: StatusStopped}
				inst.tmuxSession = tmux.ReconnectSessionLazy(fmt.Sprintf("absent-%d", i), inst.Title, inst.ProjectPath, "", "inactive")
				inst.tmuxSession.SocketName = "list-fast-budget-absent"
				instances[i] = inst
			}
			start := time.Now()
			refresh, cached := CLIStatusCandidates(instances)
			elapsed := time.Since(start)
			if len(refresh) != 0 || len(cached) != tc.count {
				t.Fatalf("refresh=%d cached=%d, want 0 and %d", len(refresh), len(cached), tc.count)
			}
			if elapsed >= tc.limit {
				t.Fatalf("%d stopped rows took %s, limit %s", tc.count, elapsed, tc.limit)
			}
		})
	}
}

// matrix/hardened: a non-stopped row missing from a complete listing of its
// own socket is still refreshed, but its status pass reaches the absence
// verdict without one has-session fork per row (fresh `list --json` processes
// and remote-agent probes previously forked once per long-dead error row). A
// row present in the listing is left to the ordinary probe.
func TestCLIStatusCandidatesNotesListingAbsence(t *testing.T) {
	skipIfNoTmuxBinary(t)
	const rows = 40
	instances := make([]*Instance, 0, rows+1)
	for i := 0; i < rows; i++ {
		inst := &Instance{Title: fmt.Sprintf("error-%d", i), Status: StatusError}
		inst.tmuxSession = tmux.ReconnectSessionLazy(fmt.Sprintf("absent-error-%d", i), inst.Title, inst.ProjectPath, "", "inactive")
		instances = append(instances, inst)
	}
	live := &Instance{Title: "live", Status: StatusError}
	live.tmuxSession = tmux.ReconnectSessionLazy(bootstrapSessionName, live.Title, live.ProjectPath, "", "inactive")
	instances = append(instances, live)

	refresh, cached := CLIStatusCandidates(instances)
	if len(refresh) != rows+1 || len(cached) != 0 {
		t.Fatalf("refresh=%d cached=%d, want %d and 0", len(refresh), len(cached), rows+1)
	}
	if live.tmuxSession.KnownAbsent() {
		t.Fatal("a session present in the listing was noted absent")
	}
	hasSession := func() uint64 {
		for _, metric := range tmux.SubprocessMetrics().Categories {
			if metric.Category == "has-session" {
				return metric.Started
			}
		}
		return 0
	}
	before := hasSession()
	for _, inst := range instances[:rows] {
		if !inst.tmuxSession.KnownAbsent() {
			t.Fatalf("%s: listing absence was not noted", inst.Title)
		}
		if inst.tmuxSession.Exists() {
			t.Fatalf("%s: absent row reported alive", inst.Title)
		}
	}
	if got := hasSession() - before; got != 0 {
		t.Fatalf("absent rows forked has-session %d times", got)
	}
}
