package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: a remote-agent serves another device's TUI or Web. Its
// listings reuse this host's runtime monitor, which already samples every pane
// for the local TUI and Web, instead of every connected device's agent
// capturing every live pane again on each probe. The monitor is only read: the
// agent never starts one, so a host without a running monitor keeps the local
// status pass, and any monitor failure falls back to that pass for
// remoteAgentMonitorRetry. Verdicts are applied the way the headless Web
// applies them (registry first, then its edit watermark as the fence). Rows
// without an applicable verdict, mostly long-dead ones the monitor stops
// re-probing and publishes as stale, take the CLI's status pass, which no
// longer forks per absent row; they read exactly as before.
const (
	// remoteAgentMonitorTimeout bounds one snapshot read, including the
	// first pass of a monitor that was idle until this request.
	remoteAgentMonitorTimeout = 8 * time.Second
	// remoteAgentMonitorMaxAge rejects a publication from a stalled monitor
	// (it publishes every pass, every 2s while anyone asks).
	remoteAgentMonitorMaxAge = 15 * time.Second
	// remoteAgentMonitorRetry is how long a failed monitor is left alone.
	remoteAgentMonitorRetry = 30 * time.Second
)

// remoteAgentLister builds the agent's listings from one storage handle. Its
// methods are serialized: the change feed and request goroutines share it.
type remoteAgentLister struct {
	mu       sync.Mutex
	storage  *session.Storage
	runtime  session.RuntimeObservationSource
	now      func() time.Time
	retryAt  time.Time
	degraded bool
	logf     func(format string, args ...any)
}

func newRemoteAgentLister(profile string, runtime session.RuntimeObservationSource) (*remoteAgentLister, error) {
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		return nil, err
	}
	return &remoteAgentLister{
		storage: storage,
		runtime: runtime,
		now:     time.Now,
		logf:    func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) },
	}, nil
}

func (l *remoteAgentLister) close() { _ = l.storage.Close() }

// probe builds the change feed's `list --json` and `group list --json`
// bodies. Without usable monitor verdicts it runs the CLI's status pass.
func (l *remoteAgentLister) probe() (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := l.observed()
	instances, groups, err := l.storage.LoadWithGroups()
	if err != nil {
		return "", "", err
	}
	cached := l.resolve(instances, snapshot)
	list, err := buildListJSON(l.storage.Profile(), instances, cached)
	if err != nil {
		return "", "", err
	}
	groupJSON, err := buildGroupListJSON(session.NewGroupTreeWithGroups(instances, groups))
	if err != nil {
		return "", "", err
	}
	return string(list), string(groupJSON), nil
}

// answerList answers the channel's own `list --json [--stats]` poll from the
// monitor's verdicts, in the bytes the CLI prints for the same rows. ok is
// false whenever the monitor cannot be used; the caller then runs the CLI.
func (l *remoteAgentLister) answerList(withStats bool) (stdout, stderr string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	started, tmuxBefore := time.Now(), tmux.SubprocessStarts()
	snapshot := l.observed()
	if snapshot == nil {
		return "", "", false
	}
	instances, _, err := l.storage.LoadWithGroups()
	if err != nil {
		return "", "", false
	}
	// The rows `list --json` prints: archived cross-harness sources hidden.
	instances = defaultListInstances(instances)
	if len(instances) == 0 {
		return "[]\n", "", true
	}
	cached := l.resolve(instances, snapshot)
	list, err := buildListJSON(l.storage.Profile(), instances, cached)
	if err != nil {
		return "", "", false
	}
	if withStats {
		stats := session.ListStats{StatusPassMS: time.Since(started).Milliseconds(), TmuxCalls: tmux.SubprocessStarts() - tmuxBefore, Sessions: len(instances)}
		if encoded, err := json.Marshal(stats); err == nil {
			stderr = session.ListStatsPrefix + string(encoded) + "\n"
		}
	}
	return string(list), stderr, true
}

// observed returns the monitor's latest complete publication, or nil when it
// cannot be used now. Caller holds l.mu.
func (l *remoteAgentLister) observed() *session.RuntimeProfileSnapshot {
	if l.runtime == nil || l.now().Before(l.retryAt) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteAgentMonitorTimeout)
	defer cancel()
	profile := l.storage.Profile()
	snapshot, err := l.runtime.Snapshot(ctx, profile)
	if err == nil {
		err = validRemoteAgentObservation(snapshot, profile, l.now())
	}
	if err != nil {
		l.retryAt = l.now().Add(remoteAgentMonitorRetry)
		if !l.degraded {
			l.degraded = true
			l.logf("remote-agent: runtime monitor unavailable, sampling locally: %v\n", err)
		}
		return nil
	}
	if l.degraded {
		l.degraded = false
		l.logf("remote-agent: using runtime monitor observations again\n")
	}
	return snapshot
}

// resolve settles every row's status for buildListJSON and returns its
// cached map. Rows with a current monitor verdict become consumers; the rest
// (all of them without a snapshot, or when the registry's edit watermark
// cannot be read) take the CLI's status pass. Caller holds l.mu.
func (l *remoteAgentLister) resolve(instances []*session.Instance, snapshot *session.RuntimeProfileSnapshot) map[*session.Instance]bool {
	pending := instances
	if snapshot != nil {
		if fence, err := l.storage.GetUpdatedAt(); err == nil {
			pending = applyRemoteAgentObservations(snapshot, l.storage.Profile(), instances, fence)
		}
	}
	refresh, cached := session.CLIStatusCandidates(pending)
	session.RefreshInstancesForCLIStatus(refresh)
	return cached
}

// applyRemoteAgentObservations applies each row's verdict and returns the
// instances left without one: absent from the publication, observed before
// the registry's last edit, or refused by ApplyRuntimeSnapshot (pending,
// stale or a different launch). Those are untouched and still probe locally.
func applyRemoteAgentObservations(snapshot *session.RuntimeProfileSnapshot, profile string, instances []*session.Instance, fence time.Time) []*session.Instance {
	rows := make(map[string]session.RuntimeSessionSnapshot, len(snapshot.Sessions))
	for _, row := range snapshot.Sessions {
		rows[row.ID] = row
	}
	var pending []*session.Instance
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		row, ok := rows[inst.ID]
		if !ok || row.ObservedAt.Before(fence) || !inst.ApplyRuntimeSnapshot(profile, row) {
			pending = append(pending, inst)
		}
	}
	return pending
}

// validRemoteAgentObservation is the TUI's envelope check plus a freshness
// bound, so a stalled monitor cannot freeze the remote view.
func validRemoteAgentObservation(snapshot *session.RuntimeProfileSnapshot, profile string, now time.Time) error {
	if snapshot == nil || snapshot.Protocol != session.RuntimeProtocolVersion || snapshot.Profile != profile ||
		snapshot.Epoch == "" || snapshot.Revision == 0 || snapshot.ObservedAt.IsZero() {
		return errors.New("invalid runtime profile snapshot")
	}
	if snapshot.Error != "" {
		return fmt.Errorf("runtime monitor: %s", snapshot.Error)
	}
	if age := now.Sub(snapshot.ObservedAt); age > remoteAgentMonitorMaxAge {
		return fmt.Errorf("runtime monitor snapshot is %s old", age.Round(time.Second))
	}
	seen := make(map[string]bool, len(snapshot.Sessions))
	for _, row := range snapshot.Sessions {
		if row.ID == "" || row.Profile != snapshot.Profile || row.Epoch != snapshot.Epoch ||
			row.Revision != snapshot.Revision || row.ObservedAt.IsZero() || seen[row.ID] {
			return errors.New("invalid runtime session envelope")
		}
		seen[row.ID] = true
	}
	return nil
}

// remoteAgentListPoll recognizes the exact `list --json [--stats]` request
// SSHRunner.FetchSessions sends.
func remoteAgentListPoll(args []string) (withStats, ok bool) {
	switch {
	case len(args) == 2 && args[0] == "list" && args[1] == "--json":
		return false, true
	case len(args) == 3 && args[0] == "list" && args[1] == "--json" && args[2] == session.ListStatsFlag:
		return true, true
	}
	return false, false
}
