package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
)

// matrix/hardened: a fake process world. Locks, children and argv are what the
// real probes read from the OS; lifecycle actions are recorded, never executed.
type cuWorld struct {
	t         *testing.T
	home      string
	bundle    aiupdate.ComputerUseBundle
	now       time.Time
	panes     map[string][]int
	locks     map[int][]string
	starts    map[int]time.Time
	kids      map[int][]int
	argvs     map[int][]string
	daemonPID int
	running   map[string]bool
	paneErr   map[string]error
	daemonErr error
	actions   []string
	inspects  int
	nextPID   int
}

const (
	cuThreadA = "01a18660-0000-7000-8000-00000000000a"
	cuThreadB = "01a18660-0000-7000-8000-00000000000b"
	cuThreadC = "01a18660-0000-7000-8000-00000000000c"
	cuThreadD = "01a18660-0000-7000-8000-00000000000d"
	cuThreadE = "01a18660-0000-7000-8000-00000000000e"
	cuThreadS = "01a18660-0000-7000-8000-0000000000aa"
	cuThreadU = "01a18660-0000-7000-8000-0000000000bb"
)

func newCUWorld(t *testing.T) *cuWorld {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("AGENTDECK_INSTANCE_ID", "")
	home = codexComputerUseCanonicalHome(home)
	now := time.Now().Truncate(time.Second)
	w := &cuWorld{
		t: t, home: home, now: now,
		bundle: aiupdate.ComputerUseBundle{Present: true, Enabled: true, PluginVersion: "2", MaterializedAt: now.Add(-time.Hour).Unix()},
		panes:  map[string][]int{}, locks: map[int][]string{}, starts: map[int]time.Time{}, kids: map[int][]int{},
		argvs: map[int][]string{}, running: map[string]bool{}, paneErr: map[string]error{}, nextPID: 5000,
	}
	old := codexComputerUseProbe
	t.Cleanup(func() { codexComputerUseProbe = old })
	codexComputerUseProbe = codexComputerUseProbes{
		bundle: func(string) (aiupdate.ComputerUseBundle, error) { return w.bundle, nil },
		exists: func(inst *Instance) bool { return w.running[inst.ID] },
		panePIDs: func(inst *Instance) ([]int, error) {
			w.inspects++
			return w.panes[inst.ID], nil
		},
		openPaths: func(pid int) ([]string, error) {
			var paths []string
			for _, id := range w.locks[pid] {
				paths = append(paths, filepath.Join(w.home, "thread-writer-locks", id+".lock"))
			}
			return paths, nil
		},
		daemon: func(string) (int, time.Time, error) { return w.daemonPID, w.starts[w.daemonPID], nil },
		started: func(pid int) (time.Time, error) {
			if s, ok := w.starts[pid]; ok {
				return s, nil
			}
			return time.Time{}, errors.New("no such process")
		},
		children:      func(pid int) ([]int, error) { return w.kids[pid], nil },
		argv:          func(pid int) ([]string, error) { return w.argvs[pid], nil },
		refreshStatus: func(*Instance) {},
		pane:          func(inst *Instance, strict bool) error { return w.paneErr[inst.ID] },
		pending:       func(*Instance) error { return nil },
		stop: func(inst *Instance) error {
			w.actions = append(w.actions, "stop:"+inst.ID)
			return nil
		},
		restart: func(inst *Instance) error {
			w.actions = append(w.actions, "restart:"+inst.ID)
			// An in-process host comes back as a fresh process with the plugin.
			for _, pid := range w.panes[inst.ID] {
				if slices.Contains(w.locks[pid], inst.CodexSessionID) {
					w.panes[inst.ID] = []int{w.host(inst.CodexSessionID, now, "2")}
					delete(w.locks, pid)
				}
			}
			return nil
		},
		restartDaemon: func(context.Context, string) error {
			w.actions = append(w.actions, "daemon")
			if w.daemonErr != nil {
				return w.daemonErr
			}
			threads := w.locks[w.daemonPID]
			delete(w.locks, w.daemonPID)
			w.daemonPID = w.host("", now, "2")
			w.locks[w.daemonPID] = threads
			return nil
		},
		now:   func() time.Time { return w.now },
		sleep: func(context.Context, time.Duration) {},
	}
	return w
}

// host creates a process holding thread (if any) with a Computer Use child of version (if any).
func (w *cuWorld) host(thread string, started time.Time, version string) int {
	w.nextPID += 10
	pid := w.nextPID
	w.starts[pid] = started
	if thread != "" {
		w.locks[pid] = []string{thread}
	}
	if version != "" {
		kid := pid + 1
		w.kids[pid] = []int{kid}
		w.argvs[kid] = []string{"/opt/cua/node", "--import", "file:///h/plugins/cache/openai-bundled/unified-computer-use/" + version + "/require-browser-access.mjs"}
	}
	return pid
}

func (w *cuWorld) session(id, thread string) *Instance {
	old := w.now.Add(-time.Hour)
	inst := &Instance{ID: id, Title: "title-" + id, Tool: "codex", Command: "codex", Status: StatusWaiting, CodexSessionID: thread, CreatedAt: old, LastAccessedAt: old}
	w.running[id] = true
	return inst
}

func (w *cuWorld) rollout(thread, source string, events ...string) {
	w.t.Helper()
	dir := filepath.Join(w.home, "sessions", "2026", "10", "30")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	lines := []string{`{"type":"session_meta","payload":{"id":"` + thread + `","thread_source":"` + source + `"}}`}
	for _, event := range events {
		lines = append(lines, `{"type":"event_msg","payload":{"type":"`+event+`"}}`)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-30T00-00-00-"+thread+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func cuFinding(t *testing.T, report CodexComputerUseReport, id string) CodexComputerUseFinding {
	t.Helper()
	for _, f := range report.Findings {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("no finding for %s in %+v", id, report.Findings)
	return CodexComputerUseFinding{}
}

func TestInspectCodexComputerUseClassifiesHosts(t *testing.T) {
	w := newCUWorld(t)
	before, after := w.now.Add(-2*time.Hour), w.now.Add(-time.Minute)
	stale, ok, daemonSession, missing, outdated, stopped := w.session("stale", cuThreadA), w.session("ok", cuThreadB), w.session("daemon", cuThreadC), w.session("missing", cuThreadD), w.session("outdated", cuThreadE), w.session("stopped", cuThreadS)
	w.panes["stale"] = []int{w.host(cuThreadA, before, "")}
	w.panes["ok"] = []int{w.host(cuThreadB, before, "2")}
	w.panes["daemon"] = []int{w.host("", after, "")} // a thin client holds no lock
	w.daemonPID = w.host(cuThreadC, before, "")
	w.panes["missing"] = []int{w.host(cuThreadD, after, "")}
	w.panes["outdated"] = []int{w.host(cuThreadE, after, "1")}
	w.running["stopped"] = false

	report := InspectCodexComputerUse([]*Instance{stale, ok, daemonSession, missing, outdated, stopped})
	want := map[string]CodexComputerUseState{"stale": CodexComputerUseStale, "ok": CodexComputerUseOK, "daemon": CodexComputerUseStale, "missing": CodexComputerUseMissing, "outdated": CodexComputerUseOutdated}
	if len(report.Findings) != len(want) {
		t.Fatalf("findings = %+v", report.Findings)
	}
	for id, state := range want {
		if f := cuFinding(t, report, id); f.State != state {
			t.Errorf("%s: state %s (%s), want %s", id, f.State, f.Reason, state)
		}
	}
	if f := cuFinding(t, report, "daemon"); !f.Host.Daemon || f.Host.PID != w.daemonPID {
		t.Errorf("daemon session host = %+v", f.Host)
	}
	var due []string
	for _, f := range report.NeedingRefresh() {
		due = append(due, f.ID)
	}
	slices.Sort(due)
	if !slices.Equal(due, []string{"daemon", "outdated", "stale"}) {
		t.Errorf("needing refresh = %v", due)
	}
}

func TestInspectCodexComputerUseIgnoresInactiveBundle(t *testing.T) {
	w := newCUWorld(t)
	w.bundle.Enabled = false
	inst := w.session("a", cuThreadA)
	w.panes["a"] = []int{w.host(cuThreadA, w.now.Add(-2*time.Hour), "")}
	if report := InspectCodexComputerUse([]*Instance{inst}); len(report.Findings) != 0 {
		t.Fatalf("a disabled plugin must not produce findings: %+v", report.Findings)
	}
}

// daemonWorld: two sessions and one finished subagent on a stale shared daemon.
func daemonWorld(t *testing.T) (*cuWorld, []*Instance) {
	w := newCUWorld(t)
	a, b := w.session("a", cuThreadA), w.session("b", cuThreadB)
	w.panes["a"] = []int{w.host("", w.now, "")}
	w.panes["b"] = []int{w.host("", w.now, "")}
	w.daemonPID = w.host("", w.now.Add(-2*time.Hour), "")
	w.locks[w.daemonPID] = []string{cuThreadA, cuThreadB, cuThreadS}
	w.rollout(cuThreadA, "user", "task_started", "task_complete")
	w.rollout(cuThreadB, "user", "task_started", "turn_aborted")
	w.rollout(cuThreadS, "subagent", "task_started", "task_complete")
	return w, []*Instance{a, b}
}

func TestRefreshCodexComputerUseRestartsDaemonBetweenStopAndResume(t *testing.T) {
	w, instances := daemonWorld(t)
	results := RefreshCodexComputerUse(context.Background(), instances, InspectCodexComputerUse(instances), CodexComputerUseRefreshOptions{})
	if len(results) != 1 || !results[0].Ready || !results[0].Refreshed || !results[0].Verified || !results[0].Daemon {
		t.Fatalf("results = %+v", results)
	}
	want := []string{"stop:a", "stop:b", "daemon", "restart:a", "restart:b"}
	if !slices.Equal(w.actions, want) {
		t.Fatalf("actions = %v, want %v", w.actions, want)
	}
}

func TestRefreshCodexComputerUseDryRunStopsNothing(t *testing.T) {
	w, instances := daemonWorld(t)
	results := RefreshCodexComputerUse(context.Background(), instances, InspectCodexComputerUse(instances), CodexComputerUseRefreshOptions{DryRun: true})
	if len(results) != 1 || !results[0].Ready || results[0].Refreshed || len(w.actions) != 0 {
		t.Fatalf("results = %+v actions = %v", results, w.actions)
	}
}

func TestRefreshCodexComputerUseRefusesUnsafeHosts(t *testing.T) {
	cases := map[string]func(*cuWorld, []*Instance){
		"session running":              func(w *cuWorld, in []*Instance) { in[0].Status = StatusRunning },
		"refresh runs inside a target": func(w *cuWorld, in []*Instance) { w.t.Setenv("AGENTDECK_INSTANCE_ID", "b") },
		"turn in progress":             func(w *cuWorld, in []*Instance) { w.rollout(cuThreadB, "user", "task_complete", "task_started") },
		"subagent still running":       func(w *cuWorld, in []*Instance) { w.rollout(cuThreadS, "subagent", "task_started") },
		"composer busy":                func(w *cuWorld, in []*Instance) { w.paneErr["a"] = errors.New("composer is busy") },
		"thread outside agent deck": func(w *cuWorld, in []*Instance) {
			w.locks[w.daemonPID] = append(w.locks[w.daemonPID], cuThreadU)
			w.rollout(cuThreadU, "user", "task_started", "task_complete")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w, instances := daemonWorld(t)
			mutate(w, instances)
			results := RefreshCodexComputerUse(context.Background(), instances, InspectCodexComputerUse(instances), CodexComputerUseRefreshOptions{})
			if len(results) != 1 || results[0].Ready || results[0].Reason == "" || len(w.actions) != 0 {
				t.Fatalf("results = %+v actions = %v", results, w.actions)
			}
		})
	}
}

func TestRefreshCodexComputerUseStrictWaitsForQuietSessions(t *testing.T) {
	w, instances := daemonWorld(t)
	instances[1].LastAccessedAt = w.now.Add(-time.Minute)
	opts := CodexComputerUseRefreshOptions{Strict: true, IdleGrace: 10 * time.Minute}
	results := RefreshCodexComputerUse(context.Background(), instances, InspectCodexComputerUse(instances), opts)
	if len(results) != 1 || results[0].Ready || len(w.actions) != 0 {
		t.Fatalf("a recently used session must hold the refresh: %+v %v", results, w.actions)
	}
}

func TestRefreshCodexComputerUseResumesAfterDaemonFailure(t *testing.T) {
	w, instances := daemonWorld(t)
	w.daemonErr = errors.New("restart refused")
	results := RefreshCodexComputerUse(context.Background(), instances, InspectCodexComputerUse(instances), CodexComputerUseRefreshOptions{})
	if len(results) != 1 || results[0].Refreshed || !strings.Contains(results[0].Reason, "restart refused") {
		t.Fatalf("results = %+v", results)
	}
	if !slices.Equal(w.actions, []string{"stop:a", "stop:b", "daemon", "restart:a", "restart:b"}) {
		t.Fatalf("stopped sessions must be resumed: %v", w.actions)
	}
}

func TestRefreshCodexComputerUseInProcessHost(t *testing.T) {
	w := newCUWorld(t)
	inst := w.session("a", cuThreadA)
	w.panes["a"] = []int{w.host(cuThreadA, w.now.Add(-2*time.Hour), "")}
	w.rollout(cuThreadA, "user", "task_started", "task_complete")
	results := RefreshCodexComputerUse(context.Background(), []*Instance{inst}, InspectCodexComputerUse([]*Instance{inst}), CodexComputerUseRefreshOptions{})
	if len(results) != 1 || !results[0].Verified || results[0].Daemon {
		t.Fatalf("results = %+v", results)
	}
	if !slices.Equal(w.actions, []string{"stop:a", "restart:a"}) {
		t.Fatalf("an in-process host restarts without touching the daemon: %v", w.actions)
	}
}

func TestCodexComputerUseWatcherBacksOffAndRemembersCleanBundle(t *testing.T) {
	w, instances := daemonWorld(t)
	instances[0].Status = StatusRunning
	watcher := NewCodexComputerUseWatcher()
	if results := watcher.Tick(context.Background(), instances); len(results) != 1 || results[0].Ready || len(w.actions) != 0 {
		t.Fatalf("busy host must wait: %+v %v", results, w.actions)
	}
	instances[0].Status = StatusWaiting
	if results := watcher.Tick(context.Background(), instances); len(results) != 1 || !results[0].Verified {
		t.Fatalf("quiet host must refresh: %+v", results)
	}
	w.inspects = 0
	if results := watcher.Tick(context.Background(), instances); len(results) != 0 {
		t.Fatalf("refreshed host must not restart again: %+v", results)
	}
	before := w.inspects
	watcher.Tick(context.Background(), instances)
	if w.inspects != before {
		t.Fatalf("an unchanged clean bundle must not probe processes again (%d probes)", w.inspects-before)
	}
	w.bundle.MaterializedAt++
	watcher.Tick(context.Background(), instances)
	if w.inspects == before {
		t.Fatal("a new bundle must be inspected again")
	}
}

func TestCodexComputerUseWatcherRetryBackoff(t *testing.T) {
	w, instances := daemonWorld(t)
	w.daemonErr = errors.New("restart refused")
	watcher := NewCodexComputerUseWatcher()
	watcher.Tick(context.Background(), instances)
	if n := len(w.actions); n == 0 {
		t.Fatal("first attempt must run")
	}
	attempts := len(w.actions)
	w.daemonErr = nil
	// The failed daemon restart kept the old daemon: still stale, but within backoff.
	watcher.Tick(context.Background(), instances)
	if len(w.actions) != attempts {
		t.Fatalf("a failed host must back off: %v", w.actions)
	}
	w.now = w.now.Add(codexComputerUseAutoRetry)
	watcher.Tick(context.Background(), instances)
	if len(w.actions) == attempts {
		t.Fatal("the host must be retried after the backoff")
	}
}

func TestCodexRolloutTurnClosed(t *testing.T) {
	write := func(t *testing.T, content string) string {
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	event := func(kind string) string { return `{"type":"event_msg","payload":{"type":"` + kind + `"}}` + "\n" }
	meta := `{"type":"session_meta","payload":{}}` + "\n"
	cases := map[string]struct {
		content string
		closed  bool
	}{
		"no turn yet":    {meta, true},
		"turn started":   {meta + event("task_started") + event("token_count"), false},
		"turn completed": {meta + event("task_started") + event("task_complete"), true},
		"turn aborted":   {meta + event("task_started") + event("turn_aborted"), true},
		"new turn":       {meta + event("task_complete") + event("task_started"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			closed, err := codexRolloutTurnClosed(write(t, tc.content))
			if err != nil || closed != tc.closed {
				t.Fatalf("closed = %v, %v; want %v", closed, err, tc.closed)
			}
		})
	}
	filler := strings.Repeat(`{"type":"response_item","payload":{"x":"`+strings.Repeat("y", 1000)+`"}}`+"\n", 5000)
	if _, err := codexRolloutTurnClosed(write(t, meta+event("task_started")+filler)); err == nil {
		t.Fatal("a tail without turn markers must not be called idle")
	}
	if closed, err := codexRolloutTurnClosed(write(t, meta+filler+event("task_complete"))); err != nil || !closed {
		t.Fatalf("the newest marker in the tail decides: %v %v", closed, err)
	}
}

func TestCurrentCodexDaemonPackageCommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "packages", "app-server-daemon")
	old := filepath.Join(root, "releases", "0.165.0-aarch64-apple-darwin", "bin", "codex")
	current := filepath.Join(root, "current", "bin", "codex")
	for _, path := range []string{old, current} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := currentCodexDaemonPackageCommand(old); got != current {
		t.Fatalf("release path = %q, want %q", got, current)
	}
	for _, command := range []string{"codex", "/usr/local/bin/codex", old + " --flag", filepath.Join(root, "releases", "x", "bin", "other")} {
		if got := currentCodexDaemonPackageCommand(command); got != command {
			t.Errorf("%q rewritten to %q", command, got)
		}
	}
	if got := (&Instance{Tool: "codex", Command: old}).resolveCodexCommand(old); got != current {
		t.Fatalf("a Codex launch must use the selected package: %q", got)
	}
	if got := (&Instance{Tool: "shell", Command: old}).resolveCodexCommand(old); got != old {
		t.Fatalf("other tools keep their command: %q", got)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if got := currentCodexDaemonPackageCommand(old); got != old {
		t.Fatalf("without a selected package the recorded release stays: %q", got)
	}
}

func TestWrappedCodexReleaseCommandLaunchesSelectedPackage(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "packages", "app-server-daemon")
	old := filepath.Join(root, "releases", "0.165.0-aarch64-apple-darwin", "bin", "codex")
	current := filepath.Join(root, "current", "bin", "codex")
	for _, path := range []string{old, current} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The shape of sessions recorded from inside a Codex process: the wrapper
	// carries the resume, the command is a bare release path.
	inst := &Instance{ID: "w", Title: "w", Tool: "codex", Command: old, Wrapper: "{command} resume " + cuThreadA + " -C /tmp"}
	launch, err := inst.applyWrapper(inst.buildCodexCommand(inst.Command))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(launch, current+" resume "+cuThreadA) || strings.Contains(launch, old) {
		t.Fatalf("launch = %q", launch)
	}
}

// Real composer shapes (see TestReadCodexFooterRealShapes): a » composer with
// a dim placeholder is idle; drafts, running turns and pending questions are not.
func TestCodexComputerUseComposerIdle(t *testing.T) {
	idle := "• Earlier answer.\n\n  Worked for 4m 10s\n\n\x1b[1m»\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n\n  GPT-6.1-Sol ultra · ~/x · task · Main [default]                Pursuing goal (1d)\n  ← for agents · ? for shortcuts                  ⚠ 2 warnings · f2 to view\n"
	if err := codexComputerUseComposerIdle(idle); err != nil {
		t.Fatalf("idle frame: %v", err)
	}
	// Status rows sit right above the composer; older transcript rows are ignored.
	// A marker word in the transcript just above it fails closed (refresh waits).
	transcript := "› an older prompt\n\n• the transcript mentions a background task\n" + strings.Repeat("  more answer text\n", 8) + "\n"
	if err := codexComputerUseComposerIdle(transcript + idle); err != nil {
		t.Fatalf("transcript rows must not count as drafts or markers: %v", err)
	}
	busy := map[string]string{
		"draft":      strings.Replace(idle, "\x1b[2mAsk Codex to do anything\x1b[0m", "keep this", 1),
		"running":    strings.Replace(idle, "  Worked for 4m 10s", "• Updating USB readback (7m 05s • esc to interrupt)", 1),
		"status row": strings.Replace(idle, "  Worked for 4m 10s", "◦ Updating USB readback (7m 05s • esc to interrupt)", 1),
		"question":   strings.Replace(idle, "  Worked for 4m 10s", "• Queued follow-up inputs\n  ? 1 question\n    ⇧← to answer", 1),
		"no footer":  "• just text\n",
	}
	for name, frame := range busy {
		if err := codexComputerUseComposerIdle(frame); err == nil {
			t.Errorf("%s frame was called idle", name)
		}
	}
}
