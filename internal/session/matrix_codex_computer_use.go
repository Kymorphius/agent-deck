package session

// matrix/hardened: Codex Computer Use freshness for local Codex sessions.
//
// The desktop app installs its bundled Computer Use plugin (MCP server
// cua_repl) into CODEX_HOME when it updates. A Codex process that was already
// running keeps the MCP servers it loaded at start, so its sessions silently
// lose desktop control. Restarting the TUI is not enough when the thread lives
// in the shared app-server daemon: the daemon keeps the thread loaded and a
// resume just reattaches to the stale session.
//
// The host of a thread is the process holding its writer lock
// ($CODEX_HOME/thread-writer-locks/<id>.lock): the pane's own codex, or the
// shared daemon recorded in $CODEX_HOME/app-server-daemon/daemon.pid. A host is
// stale when it runs no Computer Use server of the installed version and it
// either started before the bundle was installed or runs an older version.
//
// Refreshing restarts the host. Every thread it serves must belong to an idle
// Agent Deck session or be a finished subagent; anything else (for example a
// thread the desktop app has open) refuses the refresh. Nothing here runs
// unless the user asks for it or enables lifecycle.auto_refresh_computer_use.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type CodexComputerUseState string

const (
	CodexComputerUseOK CodexComputerUseState = "ok"
	// Stale: the host started before the bundle and runs no Computer Use server.
	CodexComputerUseStale CodexComputerUseState = "stale"
	// Outdated: the host runs a Computer Use server from an older bundle.
	CodexComputerUseOutdated CodexComputerUseState = "outdated"
	// Missing: a host newer than the bundle runs no server. Reported, never
	// refreshed automatically, because a restart is not known to help.
	CodexComputerUseMissing CodexComputerUseState = "missing"
	CodexComputerUseUnknown CodexComputerUseState = "unknown"
)

// CodexComputerUseHost is the process that holds a thread's writer lock.
type CodexComputerUseHost struct {
	PID       int       `json:"pid"`
	Daemon    bool      `json:"daemon"`
	StartedAt time.Time `json:"started_at"`
	Versions  []string  `json:"plugin_versions,omitempty"`
	Threads   []string  `json:"threads,omitempty"`
}

func (h *CodexComputerUseHost) key(home string) string {
	return fmt.Sprintf("%s\x00%d\x00%d", home, h.PID, h.StartedAt.Unix())
}

type CodexComputerUseFinding struct {
	ID       string                `json:"id"`
	Title    string                `json:"title"`
	ThreadID string                `json:"thread_id"`
	Home     string                `json:"codex_home"`
	State    CodexComputerUseState `json:"state"`
	Reason   string                `json:"reason,omitempty"`
	Host     *CodexComputerUseHost `json:"host,omitempty"`
}

// NeedsRefresh reports whether restarting the host is expected to restore Computer Use.
func (f CodexComputerUseFinding) NeedsRefresh() bool {
	return f.Host != nil && (f.State == CodexComputerUseStale || f.State == CodexComputerUseOutdated)
}

type CodexComputerUseReport struct {
	Bundles  map[string]aiupdate.ComputerUseBundle `json:"bundles"`
	Findings []CodexComputerUseFinding             `json:"sessions"`
}

// NeedingRefresh lists the sessions whose host should restart.
func (r CodexComputerUseReport) NeedingRefresh() []CodexComputerUseFinding {
	var out []CodexComputerUseFinding
	for _, f := range r.Findings {
		if f.NeedsRefresh() {
			out = append(out, f)
		}
	}
	return out
}

// codexComputerUseProbes are read-only observations plus the lifecycle actions
// a refresh performs. Tests replace the package value; production never does.
type codexComputerUseProbes struct {
	bundle        func(home string) (aiupdate.ComputerUseBundle, error)
	exists        func(*Instance) bool
	panePIDs      func(*Instance) ([]int, error)
	openPaths     func(pid int) ([]string, error)
	daemon        func(home string) (int, time.Time, error)
	started       func(pid int) (time.Time, error)
	children      func(pid int) ([]int, error)
	argv          func(pid int) ([]string, error)
	refreshStatus func(*Instance)
	pane          func(*Instance, bool) error
	pending       func(*Instance) error
	stop          func(*Instance) error
	restart       func(*Instance) error
	restartDaemon func(ctx context.Context, home string) error
	now           func() time.Time
	sleep         func(context.Context, time.Duration)
}

var codexComputerUseProbe = defaultCodexComputerUseProbes()

func defaultCodexComputerUseProbes() codexComputerUseProbes {
	return codexComputerUseProbes{
		bundle:    aiupdate.ReadComputerUseBundle,
		exists:    (*Instance).Exists,
		panePIDs:  (*Instance).collectCodexProcessCandidates,
		openPaths: codexComputerUseOpenPaths,
		daemon:    codexComputerUseDaemon,
		started:   codexComputerUseProcessStart,
		children:  codexComputerUseChildren,
		argv: func(pid int) ([]string, error) {
			command, err := matrixAutoSleepCommandProbe(pid)
			if err != nil {
				return nil, err
			}
			return append([]string{command.Executable}, command.Args...), nil
		},
		refreshStatus: func(inst *Instance) { _ = inst.UpdateStatus() },
		pane:          codexComputerUsePane,
		pending:       codexComputerUsePending,
		stop:          (*Instance).KillAndWait,
		restart:       (*Instance).Restart,
		restartDaemon: codexComputerUseRestartDaemon,
		now:           time.Now,
		sleep: func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
	}
}

func codexComputerUseOpenPaths(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		return procFDLinkTargets(pid)
	}
	return procfdOpenVnodePaths(pid)
}

// codexComputerUseProcessStart reads the wall-clock start of pid at one-second resolution.
func codexComputerUseProcessStart(pid int) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // G204: fixed binary and argv, no shell; pid is an integer
	if err != nil {
		return time.Time{}, fmt.Errorf("process %d start time unavailable", pid)
	}
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(string(out)), " "), time.Local)
}

func codexComputerUseChildren(pid int) ([]int, error) {
	root, err := ownershipProber.Inspect(pid)
	if err != nil {
		return nil, err
	}
	tree, err := ownershipProber.Descendants(root)
	if err != nil {
		return nil, err
	}
	var children []int
	for _, p := range tree {
		if p.PPID == pid && !p.IsZombie() {
			children = append(children, p.PID)
		}
	}
	return children, nil
}

// codexComputerUseDaemon returns the live shared daemon of home, proven by the
// start time recorded beside its PID. A missing record or a reused PID is no daemon.
func codexComputerUseDaemon(home string) (int, time.Time, error) {
	data, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if os.IsNotExist(err) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	var record struct {
		PID      int `json:"pid"`
		Identity struct {
			StartSeconds int64 `json:"startSeconds"`
		} `json:"processIdentity"`
	}
	if json.Unmarshal(data, &record) != nil || record.PID <= 1 || record.Identity.StartSeconds <= 0 {
		return 0, time.Time{}, errors.New("Codex daemon PID record is malformed")
	}
	recorded := time.Unix(record.Identity.StartSeconds, 0)
	started, err := codexComputerUseProcessStart(record.PID)
	if err != nil || started.Sub(recorded).Abs() > time.Second {
		return 0, time.Time{}, nil
	}
	return record.PID, recorded, nil
}

func codexComputerUseCanonicalHome(home string) string {
	home = ExpandPath(home)
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		return resolved
	}
	return filepath.Clean(home)
}

// codexComputerUseCandidate admits running-capable local Codex sessions bound to a thread.
func codexComputerUseCandidate(inst *Instance) bool {
	if inst == nil || inst.Tool != "codex" || inst.IsSSH() || inst.IsSandboxed() || inst.UsesTerminalBackend() {
		return false
	}
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return inst.ArchivedAt.IsZero() && IsBareCodexThreadID(inst.CodexSessionID)
}

// codexHeldThreads lists the thread ids whose writer locks under home appear in paths.
func codexHeldThreads(home string, paths []string) []string {
	dirs := []string{filepath.Join(home, "thread-writer-locks")}
	if resolved, err := filepath.EvalSymlinks(dirs[0]); err == nil && resolved != dirs[0] {
		dirs = append(dirs, resolved)
	}
	var threads []string
	for _, path := range paths {
		dir, name := filepath.Split(strings.TrimSuffix(path, " (deleted)"))
		id, ok := strings.CutSuffix(name, ".lock")
		if ok && IsBareCodexThreadID(id) && slices.Contains(dirs, filepath.Clean(dir)) && !slices.Contains(threads, id) {
			threads = append(threads, id)
		}
	}
	slices.Sort(threads)
	return threads
}

// findCodexComputerUseHost locates the process holding sid's writer lock.
func findCodexComputerUseHost(inst *Instance, home, sid string, bundle aiupdate.ComputerUseBundle) (*CodexComputerUseHost, error) {
	p := codexComputerUseProbe
	var host *CodexComputerUseHost
	pids, probeErr := p.panePIDs(inst)
	for _, pid := range pids {
		paths, err := p.openPaths(pid)
		if err != nil {
			probeErr = errors.Join(probeErr, err)
			continue
		}
		if threads := codexHeldThreads(home, paths); slices.Contains(threads, sid) {
			started, err := p.started(pid)
			if err != nil {
				return nil, err
			}
			host = &CodexComputerUseHost{PID: pid, StartedAt: started, Threads: threads}
			break
		}
	}
	if host == nil {
		pid, started, err := p.daemon(home)
		if err != nil {
			return nil, err
		}
		if pid > 0 {
			paths, err := p.openPaths(pid)
			if err != nil {
				return nil, err
			}
			if threads := codexHeldThreads(home, paths); slices.Contains(threads, sid) {
				host = &CodexComputerUseHost{PID: pid, Daemon: true, StartedAt: started, Threads: threads}
			}
		}
	}
	if host == nil {
		if probeErr != nil {
			return nil, fmt.Errorf("thread host unverified: %w", probeErr)
		}
		return nil, errors.New("no live Codex process holds this thread")
	}
	children, err := p.children(host.PID)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		argv, err := p.argv(child)
		if err != nil {
			continue // exited or inaccessible children cannot be Computer Use evidence
		}
		if version, ok := bundle.ComputerUsePluginVersion(argv); ok && !slices.Contains(host.Versions, version) {
			host.Versions = append(host.Versions, version)
		}
	}
	slices.Sort(host.Versions)
	return host, nil
}

func classifyCodexComputerUse(host *CodexComputerUseHost, bundle aiupdate.ComputerUseBundle) (CodexComputerUseState, string) {
	installed := time.Unix(bundle.MaterializedAt, 0)
	kind := "Codex process"
	if host.Daemon {
		kind = "shared Codex daemon"
	}
	switch {
	case slices.Contains(host.Versions, bundle.PluginVersion):
		return CodexComputerUseOK, ""
	case len(host.Versions) > 0:
		return CodexComputerUseOutdated, fmt.Sprintf("%s %d runs Computer Use %s; the desktop app installed %s", kind, host.PID, strings.Join(host.Versions, ","), bundle.PluginVersion)
	case host.StartedAt.Before(installed):
		return CodexComputerUseStale, fmt.Sprintf("%s %d started %s, before Computer Use %s was installed %s", kind, host.PID, host.StartedAt.Format(time.DateTime), bundle.PluginVersion, installed.Format(time.DateTime))
	default:
		return CodexComputerUseMissing, fmt.Sprintf("%s %d runs no Computer Use server although it started after the plugin was installed", kind, host.PID)
	}
}

// InspectCodexComputerUse reports Computer Use freshness for running local
// Codex sessions. It only reads files and process metadata.
func InspectCodexComputerUse(instances []*Instance) CodexComputerUseReport {
	p := codexComputerUseProbe
	report := CodexComputerUseReport{Bundles: map[string]aiupdate.ComputerUseBundle{}}
	bundleErrs := map[string]error{}
	for _, inst := range instances {
		if !codexComputerUseCandidate(inst) || !p.exists(inst) {
			continue
		}
		inst.mu.RLock()
		finding := CodexComputerUseFinding{ID: inst.ID, Title: inst.Title, ThreadID: inst.CodexSessionID}
		inst.mu.RUnlock()
		home := codexComputerUseCanonicalHome(CodexHomeDirForInstance(inst))
		finding.Home = home
		bundle, seen := report.Bundles[home]
		if err, failed := bundleErrs[home]; failed {
			finding.State, finding.Reason = CodexComputerUseUnknown, err.Error()
			report.Findings = append(report.Findings, finding)
			continue
		}
		if !seen {
			var err error
			if bundle, err = p.bundle(home); err != nil {
				bundleErrs[home] = err
				finding.State, finding.Reason = CodexComputerUseUnknown, err.Error()
				report.Findings = append(report.Findings, finding)
				continue
			}
			report.Bundles[home] = bundle
		}
		if !bundle.Active() {
			continue
		}
		host, err := findCodexComputerUseHost(inst, home, finding.ThreadID, bundle)
		if err != nil {
			finding.State, finding.Reason = CodexComputerUseUnknown, err.Error()
		} else {
			finding.Host = host
			finding.State, finding.Reason = classifyCodexComputerUse(host, bundle)
		}
		report.Findings = append(report.Findings, finding)
	}
	return report
}

// codexComputerUsePane requires one live pane whose composer is idle. strict
// (automatic) mode also refuses any interactive client attached to it.
func codexComputerUsePane(inst *Instance, strict bool) error {
	s := inst.GetTmuxSession()
	if s == nil {
		return errors.New("tmux session unavailable")
	}
	out, err := tmux.OutputBounded(inst.TmuxSocketName, "list-panes", "-s", "-t", "="+s.Name, "-F", "#{pane_dead}")
	if err != nil {
		return fmt.Errorf("pane inventory unavailable: %w", err)
	}
	if strings.TrimSpace(string(out)) != "0" {
		return errors.New("session has multiple, dead or unknown panes")
	}
	if strict {
		clients, err := tmux.OutputBounded(inst.TmuxSocketName, "list-clients", "-t", "="+s.Name, "-F", "#{client_control_mode}\t#{client_flags}")
		if err != nil {
			return fmt.Errorf("attached client inventory unavailable: %w", err)
		}
		if err := matrixAutoSleepClients(string(clients)); err != nil {
			return err
		}
	}
	frame, err := s.CapturePaneFresh()
	if err != nil {
		return fmt.Errorf("fresh composer capture unavailable: %w", err)
	}
	return codexComputerUseComposerIdle(frame)
}

// codexComputerUseComposerIdle reads the composer the way the fast switch
// does: send.ComposerDraft does not know the » composer and reads an older
// transcript row as a draft. Markers are only looked for in the status rows
// just above the composer, where Codex draws them, not in the transcript.
func codexComputerUseComposerIdle(frame string) error {
	st := readCodexFooter(frame)
	switch {
	case !st.known:
		return errors.New("the Codex composer and footer are not visible")
	case st.turnOn:
		return errors.New("a turn is running")
	case st.menu:
		return errors.New("a menu or approval is open")
	case st.draft != "":
		return errors.New("the composer holds an unsent draft")
	}
	lines := strings.Split(strings.ToLower(tmux.StripANSI(frame)), "\n")
	composer := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if codexComposerLine(lines[i]) {
			composer = i
			break
		}
	}
	status := strings.Join(lines[max(0, composer-8):], "\n")
	for _, marker := range []string{"esc to interrupt", "queued follow-up", "queued input", "to answer", "background terminal", "background task", "[image", "[attachment", "attached image", "attached file"} {
		if strings.Contains(status, marker) {
			return fmt.Errorf("the pane shows %q", marker)
		}
	}
	return nil
}

// codexComputerUsePending refuses a restart while a durable delivery waits for the session.
func codexComputerUsePending(inst *Instance) error {
	inst.mu.RLock()
	id, sid := inst.ID, inst.CodexSessionID
	dbPath := ""
	if inst.storageSnapshot != nil {
		dbPath = inst.storageSnapshot.dbPath
	}
	inst.mu.RUnlock()
	if !filepath.IsAbs(dbPath) {
		return errors.New("session's persisted profile identity is unknown")
	}
	_, err := matrixAutoSleepPendingProof(id, sid, filepath.Dir(dbPath))
	return err
}

// codexRolloutTurnClosed reports whether the last turn recorded in a rollout
// ended. Only the tail is read: the newest turn marker decides, and a tail
// without any marker fails closed unless it covers the whole file.
func codexRolloutTurnClosed(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("rollout is not a regular file")
	}
	const tail = 4 << 20
	start := max(0, info.Size()-tail)
	data, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return false, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	last := ""
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(`"event_msg"`)) {
			continue
		}
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil || record.Type != "event_msg" {
			continue
		}
		switch record.Payload.Type {
		case "task_started", "turn_started":
			last = "started"
		case "task_complete", "turn_complete", "turn_aborted":
			last = "ended"
		}
	}
	if last == "" {
		if start > 0 {
			return false, errors.New("the newest turn boundary is beyond the inspected rollout tail")
		}
		return true, nil
	}
	return last == "ended", nil
}

// codexComputerUseThreadIdle checks one thread a host serves. A thread with no
// rollout yet has never started a turn.
func codexComputerUseThreadIdle(home, thread string) (subagent bool, err error) {
	matches, err := exactCodexRolloutMatches(thread, home)
	if err != nil {
		return false, err
	}
	path, err := uniqueRegularArtifact(matches, "Codex thread "+thread)
	if errors.Is(err, errNoExactContextArtifact) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	subagent = readCodexRolloutThreadMeta(path).ThreadSource == "subagent"
	closed, err := codexRolloutTurnClosed(path)
	if err != nil {
		return subagent, fmt.Errorf("thread %s: %w", thread, err)
	}
	if !closed {
		return subagent, fmt.Errorf("thread %s has a turn in progress", thread)
	}
	return subagent, nil
}

type CodexComputerUseRefreshOptions struct {
	// Only limits the refresh to hosts serving these session IDs (empty: all due).
	Only map[string]bool
	// DryRun checks readiness without stopping anything.
	DryRun bool
	// Strict is automatic mode: no attached interactive client and IdleGrace of quiet.
	Strict    bool
	IdleGrace time.Duration
	// VerifyTimeout bounds waiting for the new Computer Use server (default 90s).
	VerifyTimeout time.Duration
}

type CodexComputerUseRefreshResult struct {
	HostPID   int      `json:"host_pid"`
	Daemon    bool     `json:"daemon"`
	Home      string   `json:"codex_home"`
	Sessions  []string `json:"sessions"`
	Titles    []string `json:"titles"`
	Ready     bool     `json:"ready"`
	Refreshed bool     `json:"refreshed"`
	Verified  bool     `json:"verified"`
	Reason    string   `json:"reason,omitempty"`
	hostKey   string
}

type codexComputerUsePlan struct {
	home    string
	host    CodexComputerUseHost
	bundle  aiupdate.ComputerUseBundle
	targets []*Instance
}

func planCodexComputerUseRefresh(instances []*Instance, report CodexComputerUseReport, only map[string]bool) []*codexComputerUsePlan {
	var plans []*codexComputerUsePlan
	byKey := map[string]*codexComputerUsePlan{}
	for _, f := range report.Findings {
		selected := f.NeedsRefresh()
		if len(only) > 0 {
			selected = only[f.ID] && f.Host != nil && f.State != CodexComputerUseOK && f.State != CodexComputerUseUnknown
		}
		if !selected {
			continue
		}
		key := f.Host.key(f.Home)
		if byKey[key] == nil {
			byKey[key] = &codexComputerUsePlan{home: f.Home, host: *f.Host, bundle: report.Bundles[f.Home]}
			plans = append(plans, byKey[key])
		}
	}
	// Every running session on a host's threads restarts with it, selected or not.
	for _, plan := range plans {
		for _, inst := range instances {
			if !codexComputerUseCandidate(inst) || codexComputerUseCanonicalHome(CodexHomeDirForInstance(inst)) != plan.home {
				continue
			}
			inst.mu.RLock()
			sid := inst.CodexSessionID
			inst.mu.RUnlock()
			if slices.Contains(plan.host.Threads, sid) && codexComputerUseProbe.exists(inst) {
				plan.targets = append(plan.targets, inst)
			}
		}
	}
	return plans
}

func (plan *codexComputerUsePlan) ready(opts CodexComputerUseRefreshOptions) error {
	p := codexComputerUseProbe
	managed := map[string]bool{}
	self := strings.TrimSpace(os.Getenv("AGENTDECK_INSTANCE_ID"))
	for _, inst := range plan.targets {
		if self != "" && inst.ID == self {
			return fmt.Errorf("%s is the session running this refresh; stopping it would end the refresh halfway", inst.Title)
		}
		p.refreshStatus(inst)
		inst.mu.RLock()
		title, status, sid := inst.Title, inst.Status, inst.CodexSessionID
		inst.mu.RUnlock()
		managed[sid] = true
		if status != StatusIdle && status != StatusWaiting {
			return fmt.Errorf("%s is %s", title, status)
		}
		if opts.Strict {
			if quiet := p.now().Sub(AutoSleepIdleSince(inst, AutoSleepEvidence{})); quiet < opts.IdleGrace {
				return fmt.Errorf("%s was active %s ago", title, quiet.Round(time.Second))
			}
		}
		if err := p.pane(inst, opts.Strict); err != nil {
			return fmt.Errorf("%s: %w", title, err)
		}
		if err := p.pending(inst); err != nil {
			return fmt.Errorf("%s: %w", title, err)
		}
	}
	for _, thread := range plan.host.Threads {
		subagent, err := codexComputerUseThreadIdle(plan.home, thread)
		if err != nil {
			return err
		}
		if !managed[thread] && !subagent {
			return fmt.Errorf("the host also serves thread %s, which no running Agent Deck session owns (it may be open in the Codex app)", thread)
		}
	}
	return nil
}

func (plan *codexComputerUsePlan) verified(ctx context.Context, timeout time.Duration) bool {
	p := codexComputerUseProbe
	deadline := p.now().Add(timeout)
	for {
		ok := true
		for _, inst := range plan.targets {
			inst.mu.RLock()
			sid := inst.CodexSessionID
			inst.mu.RUnlock()
			host, err := findCodexComputerUseHost(inst, plan.home, sid, plan.bundle)
			if err != nil || !slices.Contains(host.Versions, plan.bundle.PluginVersion) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
		if ctx.Err() != nil || !p.now().Before(deadline) {
			return false
		}
		p.sleep(ctx, 3*time.Second)
	}
}

// RefreshCodexComputerUse restarts stale hosts whose every thread is idle. A
// shared daemon is restarted between stopping and resuming its sessions, so
// each resume loads the thread fresh with the installed plugins.
func RefreshCodexComputerUse(ctx context.Context, instances []*Instance, report CodexComputerUseReport, opts CodexComputerUseRefreshOptions) []CodexComputerUseRefreshResult {
	p := codexComputerUseProbe
	if opts.VerifyTimeout <= 0 {
		opts.VerifyTimeout = 90 * time.Second
	}
	var results []CodexComputerUseRefreshResult
	for _, plan := range planCodexComputerUseRefresh(instances, report, opts.Only) {
		result := CodexComputerUseRefreshResult{HostPID: plan.host.PID, Daemon: plan.host.Daemon, Home: plan.home, hostKey: plan.host.key(plan.home)}
		for _, inst := range plan.targets {
			result.Sessions = append(result.Sessions, inst.ID)
			result.Titles = append(result.Titles, inst.Title)
		}
		if err := plan.ready(opts); err != nil {
			result.Reason = err.Error()
			results = append(results, result)
			continue
		}
		result.Ready = true
		if opts.DryRun || ctx.Err() != nil {
			results = append(results, result)
			continue
		}
		var failures []error
		var stopped []*Instance
		for _, inst := range plan.targets {
			if err := p.stop(inst); err != nil {
				failures = append(failures, fmt.Errorf("stop %s: %w", inst.Title, err))
				break
			}
			stopped = append(stopped, inst)
		}
		if len(failures) == 0 && plan.host.Daemon {
			if err := p.restartDaemon(ctx, plan.home); err != nil {
				failures = append(failures, fmt.Errorf("restart shared daemon: %w", err))
			}
		}
		// Resume whatever was stopped even after a failure: a stopped session
		// is worse than one that still needs another refresh.
		for _, inst := range stopped {
			if err := p.restart(inst); err != nil {
				failures = append(failures, fmt.Errorf("resume %s: %w", inst.Title, err))
			}
		}
		result.Refreshed = len(failures) == 0
		if result.Refreshed {
			result.Verified = plan.verified(ctx, opts.VerifyTimeout)
			if !result.Verified {
				result.Reason = "sessions resumed, but the new Computer Use server was not confirmed in time"
			}
		} else {
			result.Reason = errors.Join(failures...).Error()
		}
		sessionLog.Info("codex_computer_use_refresh",
			slog.Int("host_pid", result.HostPID), slog.Bool("daemon", result.Daemon),
			slog.Any("sessions", result.Sessions), slog.Bool("refreshed", result.Refreshed),
			slog.Bool("verified", result.Verified), slog.String("reason", result.Reason))
		results = append(results, result)
	}
	return results
}

// codexComputerUseRestartDaemon restarts the daemon with its own managed
// package, so the daemon and its sessions keep one Codex version.
func codexComputerUseRestartDaemon(ctx context.Context, home string) error {
	binary := filepath.Join(home, "packages", "app-server-daemon", "current", "bin", "codex")
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("managed Codex daemon package unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	_, err := aiupdate.New(binary, home).Restart(ctx)
	return err
}

const (
	codexComputerUseAutoIdle   = 10 * time.Minute
	codexComputerUseAutoRetry  = 30 * time.Minute
	codexComputerUseAutoVerify = 2 * time.Minute
)

// CodexComputerUseWatcher is the opt-in automatic refresh. It inspects
// processes only while some home's bundle differs from the last clean scan,
// and waits for each stale host's sessions to be quiet before restarting it.
type CodexComputerUseWatcher struct {
	mu        sync.Mutex
	cleanKey  string
	attempted map[string]time.Time
}

func NewCodexComputerUseWatcher() *CodexComputerUseWatcher {
	return &CodexComputerUseWatcher{attempted: map[string]time.Time{}}
}

func (w *CodexComputerUseWatcher) Tick(ctx context.Context, instances []*Instance) []CodexComputerUseRefreshResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := codexComputerUseProbe
	homes := map[string]bool{}
	for _, inst := range instances {
		if codexComputerUseCandidate(inst) {
			homes[codexComputerUseCanonicalHome(CodexHomeDirForInstance(inst))] = true
		}
	}
	var keys []string
	for home := range homes {
		bundle, err := p.bundle(home)
		if err != nil {
			return nil
		}
		keys = append(keys, home+"\x00"+bundle.Key())
	}
	slices.Sort(keys)
	key := strings.Join(keys, "\x01")
	if key == w.cleanKey {
		return nil
	}
	report := InspectCodexComputerUse(instances)
	due := map[string]bool{}
	pending := false
	for _, f := range report.NeedingRefresh() {
		pending = true
		if last, ok := w.attempted[f.Host.key(f.Home)]; !ok || p.now().Sub(last) >= codexComputerUseAutoRetry {
			due[f.ID] = true
		}
	}
	if !pending {
		w.cleanKey = key
		return nil
	}
	if len(due) == 0 {
		return nil
	}
	results := RefreshCodexComputerUse(ctx, instances, report, CodexComputerUseRefreshOptions{Only: due, Strict: true, IdleGrace: codexComputerUseAutoIdle, VerifyTimeout: codexComputerUseAutoVerify})
	for _, result := range results {
		// Not-yet-quiet hosts are checked again next minute; attempted ones back off.
		if result.Ready {
			w.attempted[result.hostKey] = p.now()
		}
	}
	return results
}
