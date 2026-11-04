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
	"github.com/asheshgoplani/agent-deck/internal/statedb"
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
	// observer and cache reuse one hydration per registry generation (see
	// matrix_remote_agent_registry.go); a nil observer hydrates every time.
	observer     *statedb.RegistryObserver
	cache        *remoteAgentRegistry
	loadRegistry func() ([]*session.Instance, []*session.GroupData, error)
	// registryLoads and registryHits count hydrations and reuses (tests).
	registryLoads, registryHits int
}

func newRemoteAgentLister(profile string, runtime session.RuntimeObservationSource) (*remoteAgentLister, error) {
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		return nil, err
	}
	l := &remoteAgentLister{
		storage:      storage,
		runtime:      runtime,
		now:          time.Now,
		logf:         func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) },
		loadRegistry: storage.LoadWithGroups,
	}
	if db := storage.GetDB(); db != nil {
		if observer, err := db.NewRegistryObserver(); err == nil {
			l.observer = observer
		}
	}
	return l, nil
}

func (l *remoteAgentLister) close() {
	if l.observer != nil {
		_ = l.observer.Close()
	}
	_ = l.storage.Close()
}

// probe builds the change feed's `list --json` and `group list --json`
// bodies. Without usable monitor verdicts it runs the CLI's status pass.
func (l *remoteAgentLister) probe() (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reg, cached, err := l.prepare(l.observed())
	if err != nil {
		return "", "", err
	}
	list, err := buildListJSON(l.storage.Profile(), reg.instances, cached)
	if err != nil {
		return "", "", err
	}
	groupJSON, err := buildGroupListJSON(session.NewGroupTreeWithGroups(reg.instances, reg.groups))
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
	reg, cached, err := l.prepare(snapshot)
	if err != nil {
		return "", "", false
	}
	// The rows `list --json` prints: archived cross-harness sources hidden.
	instances := defaultListInstances(reg.instances)
	if len(instances) == 0 {
		return "[]\n", "", true
	}
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

// answerGroups answers the channel's `group list --json` poll with the body
// the change feed pushes, under the same monitor condition as answerList.
func (l *remoteAgentLister) answerGroups() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := l.observed()
	if snapshot == nil {
		return "", false
	}
	reg, _, err := l.prepare(snapshot)
	if err != nil {
		return "", false
	}
	groupJSON, err := buildGroupListJSON(session.NewGroupTreeWithGroups(reg.instances, reg.groups))
	if err != nil {
		return "", false
	}
	return string(groupJSON), true
}

// prepare returns the current hydration with every row's status settled and
// the cached map for buildListJSON. Caller holds l.mu.
func (l *remoteAgentLister) prepare(snapshot *session.RuntimeProfileSnapshot) (*remoteAgentRegistry, map[*session.Instance]bool, error) {
	var fence time.Time
	if snapshot != nil {
		var err error
		if fence, err = l.storage.GetUpdatedAt(); err != nil {
			snapshot = nil
		}
	}
	reg, err := l.registry()
	if err != nil {
		return nil, nil, err
	}
	if snapshot == nil && len(reg.observed) > 0 {
		// Applied verdicts also mirror into public fields such as Status; only
		// a fresh hydration gives the local pass the stored state it expects.
		l.cache = nil
		if reg, err = l.registry(); err != nil {
			return nil, nil, err
		}
	}
	return reg, l.resolve(reg, snapshot, fence), nil
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
// cached map. Rows with a monitor verdict are consumers; the rest take the
// CLI's status pass, as every row does without a snapshot (prepare then hands
// over a hydration holding no verdicts). A kept row the current publication
// cannot update (a probe in flight, a gone pane the monitor no longer
// re-probes) keeps its last verdict, as in the TUI. Caller holds l.mu.
func (l *remoteAgentLister) resolve(reg *remoteAgentRegistry, snapshot *session.RuntimeProfileSnapshot, fence time.Time) map[*session.Instance]bool {
	pending := reg.instances
	if snapshot != nil {
		pending = applyRemoteAgentObservations(snapshot, l.storage.Profile(), reg.instances, fence, reg.observed)
	}
	refresh, cached := session.CLIStatusCandidates(pending)
	live := make([]*session.Instance, 0, len(refresh))
	for _, inst := range refresh {
		// The listing just proved these panes absent (or there never was
		// one): their verdict needs only their hook evidence, not the tmux
		// title caches the CLI warms for live panes.
		if sess := inst.GetTmuxSession(); sess == nil || sess.KnownAbsent() {
			if !reg.hooksLoaded[inst] {
				if session.HookStatusTool(inst.Tool) {
					session.ReloadHookStatus(inst)
				}
				reg.hooksLoaded[inst] = true
			}
			continue
		}
		live = append(live, inst)
	}
	session.RefreshInstancesForCLIStatus(live)
	return cached
}

// applyRemoteAgentObservations applies each row's current verdict, records it
// in observed, and returns the rows that hold none: absent from the
// publication, observed before the registry's last edit, or refused by
// ApplyRuntimeSnapshot (pending, stale or a different launch) without an
// earlier verdict. Those still probe locally.
func applyRemoteAgentObservations(snapshot *session.RuntimeProfileSnapshot, profile string, instances []*session.Instance, fence time.Time, observed map[*session.Instance]bool) []*session.Instance {
	rows := make(map[string]session.RuntimeSessionSnapshot, len(snapshot.Sessions))
	for _, row := range snapshot.Sessions {
		rows[row.ID] = row
	}
	var pending []*session.Instance
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if row, ok := rows[inst.ID]; ok && !row.ObservedAt.Before(fence) && inst.ApplyRuntimeSnapshot(profile, row) {
			observed[inst] = true
			continue
		}
		if !observed[inst] {
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

// remoteAgentGroupPoll recognizes the exact `group list --json` request the
// controller sends with each poll.
func remoteAgentGroupPoll(args []string) bool {
	return len(args) == 3 && args[0] == "group" && args[1] == "list" && args[2] == "--json"
}
