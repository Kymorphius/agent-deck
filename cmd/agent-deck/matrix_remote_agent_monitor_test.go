package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: a fake host monitor; counts reads so the retry window and
// the "never start one" contract stay observable.
type fakeRemoteAgentRuntime struct {
	calls    int
	snapshot func() (*session.RuntimeProfileSnapshot, error)
}

func (f *fakeRemoteAgentRuntime) Snapshot(context.Context, string) (*session.RuntimeProfileSnapshot, error) {
	f.calls++
	return f.snapshot()
}
func (f *fakeRemoteAgentRuntime) Refresh(context.Context, string) error { return nil }
func (f *fakeRemoteAgentRuntime) Observations(context.Context, string, string, uint64) ([]session.RuntimeProfileSnapshot, error) {
	return nil, nil
}

const remoteAgentMonitorTestProfile = "ra_monitor_test"

// remoteAgentMonitorFixture saves two never-started sessions (the local status
// pass reports them idle without tmux) and returns each one's launch identity.
func remoteAgentMonitorFixture(t *testing.T) map[string]string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTDECK_PROFILE", remoteAgentMonitorTestProfile)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	storage, err := session.NewStorageWithProfile(remoteAgentMonitorTestProfile)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	var rows []*session.Instance
	for _, id := range []string{"mon-observed", "mon-unobserved"} {
		rows = append(rows, &session.Instance{ID: id, Title: id, ProjectPath: home, GroupPath: "work", Tool: "shell", Status: session.StatusIdle, CreatedAt: time.Now()})
	}
	if err := storage.SaveWithGroups(rows, nil); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]string{}
	for _, inst := range loaded {
		identities[inst.ID] = inst.RuntimeIdentity()
	}
	return identities
}

func remoteAgentMonitorSnapshot(at time.Time, rows ...session.RuntimeSessionSnapshot) *session.RuntimeProfileSnapshot {
	s := &session.RuntimeProfileSnapshot{Protocol: session.RuntimeProtocolVersion, Epoch: "epoch-1", Revision: 7, Profile: remoteAgentMonitorTestProfile, ObservedAt: at}
	for _, row := range rows {
		row.Profile, row.Epoch, row.Revision = s.Profile, s.Epoch, s.Revision
		if row.ObservedAt.IsZero() {
			row.ObservedAt = at
		}
		s.Sessions = append(s.Sessions, row)
	}
	return s
}

type remoteAgentListedRow struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	StatusSource string `json:"status_source"`
	Substate     string `json:"substate"`
}

func remoteAgentListed(t *testing.T, listJSON string) map[string]remoteAgentListedRow {
	t.Helper()
	var rows []remoteAgentListedRow
	if err := json.Unmarshal([]byte(listJSON), &rows); err != nil {
		t.Fatalf("listing is not JSON: %v\n%s", err, listJSON)
	}
	out := map[string]remoteAgentListedRow{}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out
}

// Rows with a current monitor verdict are listed from it; rows without one
// take the local status pass exactly as before; `list --json` polls get the
// same rows in the CLI's bytes plus its stats line.
func TestRemoteAgentLister_UsesMonitorVerdicts(t *testing.T) {
	ids := remoteAgentMonitorFixture(t)
	fake := &fakeRemoteAgentRuntime{}
	fake.snapshot = func() (*session.RuntimeProfileSnapshot, error) {
		return remoteAgentMonitorSnapshot(time.Now(), session.RuntimeSessionSnapshot{
			ID: "mon-observed", Identity: ids["mon-observed"], Status: session.StatusRunning, Substate: session.SubstateRunning,
		}), nil
	}
	lister, err := newRemoteAgentLister(remoteAgentMonitorTestProfile, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer lister.close()
	list, groups, err := lister.probe()
	if err != nil {
		t.Fatal(err)
	}
	rows := remoteAgentListed(t, list)
	if got := rows["mon-observed"]; got.Status != "running" || got.Substate != string(session.SubstateRunning) || got.StatusSource != "live" {
		t.Fatalf("observed row = %+v, want the monitor's running verdict", got)
	}
	if got := rows["mon-unobserved"]; got.Status != "idle" || got.StatusSource != "live" {
		t.Fatalf("unobserved row = %+v, want the local pass's idle", got)
	}
	if !strings.Contains(groups, `"running": 1`) {
		t.Fatalf("group counts ignore the monitor verdict:\n%s", groups)
	}

	stdout, stderr, ok := lister.answerList(true)
	if !ok || stdout != list {
		t.Fatalf("list poll answer ok=%v differs from the probe listing:\n%s\n---\n%s", ok, stdout, list)
	}
	if !strings.HasPrefix(stderr, session.ListStatsPrefix) || !strings.Contains(stderr, `"sessions":2`) {
		t.Fatalf("stats line = %q", stderr)
	}
	if _, stderr, _ := lister.answerList(false); stderr != "" {
		t.Fatalf("plain list poll wrote stderr %q", stderr)
	}
	if fake.calls != 3 {
		t.Fatalf("monitor reads = %d, want one per listing", fake.calls)
	}
}

// A verdict whose probe began before the registry's last edit cannot override
// it: the row takes the local pass.
func TestRemoteAgentLister_RegistryEditFencesOlderVerdict(t *testing.T) {
	ids := remoteAgentMonitorFixture(t)
	fake := &fakeRemoteAgentRuntime{snapshot: func() (*session.RuntimeProfileSnapshot, error) {
		return remoteAgentMonitorSnapshot(time.Now(), session.RuntimeSessionSnapshot{
			ID: "mon-observed", Identity: ids["mon-observed"], Status: session.StatusRunning,
			ObservedAt: time.Now().Add(-time.Hour),
		}), nil
	}}
	lister, err := newRemoteAgentLister(remoteAgentMonitorTestProfile, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer lister.close()
	list, _, err := lister.probe()
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteAgentListed(t, list)["mon-observed"]; got.Status != "idle" {
		t.Fatalf("pre-edit verdict applied: %+v", got)
	}
}

// Without a usable monitor the listing is the local pass's, the monitor is
// left alone for the retry window, list polls go to the CLI, and the failure
// is logged once.
func TestRemoteAgentLister_FallsBackWithoutMonitor(t *testing.T) {
	ids := remoteAgentMonitorFixture(t)
	local, closeLocal, err := newRemoteAgentProbe(remoteAgentMonitorTestProfile)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLocal()
	want, _, err := local()
	if err != nil {
		t.Fatal(err)
	}
	stale := remoteAgentMonitorSnapshot(time.Now().Add(-time.Minute), session.RuntimeSessionSnapshot{ID: "mon-observed", Identity: ids["mon-observed"], Status: session.StatusRunning})
	wrong := remoteAgentMonitorSnapshot(time.Now(), session.RuntimeSessionSnapshot{ID: "mon-observed", Identity: ids["mon-observed"], Status: session.StatusRunning})
	wrong.Profile = "other"
	failed := remoteAgentMonitorSnapshot(time.Now())
	failed.Error = "collector failed"
	for name, source := range map[string]func() (*session.RuntimeProfileSnapshot, error){
		"unreachable": func() (*session.RuntimeProfileSnapshot, error) { return nil, errors.New("dial unix: no such file") },
		"stale":       func() (*session.RuntimeProfileSnapshot, error) { return stale, nil },
		"profile":     func() (*session.RuntimeProfileSnapshot, error) { return wrong, nil },
		"error":       func() (*session.RuntimeProfileSnapshot, error) { return failed, nil },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRemoteAgentRuntime{snapshot: source}
			lister, err := newRemoteAgentLister(remoteAgentMonitorTestProfile, fake)
			if err != nil {
				t.Fatal(err)
			}
			defer lister.close()
			var logs []string
			lister.logf = func(format string, args ...any) { logs = append(logs, format) }
			now := time.Now()
			lister.now = func() time.Time { return now }
			got, _, err := lister.probe()
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("fallback listing differs from the local probe:\n%s\n---\n%s", got, want)
			}
			if _, _, ok := lister.answerList(true); ok {
				t.Fatal("list poll answered in-process without the monitor")
			}
			if fake.calls != 1 {
				t.Fatalf("monitor asked %d times inside the retry window", fake.calls)
			}
			now = now.Add(remoteAgentMonitorRetry + time.Second)
			if _, _, err := lister.probe(); err != nil {
				t.Fatal(err)
			}
			if fake.calls != 2 || len(logs) != 1 {
				t.Fatalf("after the window: reads=%d logs=%d, want 2 and 1", fake.calls, len(logs))
			}
		})
	}
}

func TestRemoteAgentListPoll(t *testing.T) {
	for _, tc := range []struct {
		args            []string
		withStats, isIt bool
	}{
		{[]string{"list", "--json"}, false, true},
		{[]string{"list", "--json", "--stats"}, true, true},
		{[]string{"list"}, false, false},
		{[]string{"list", "--json", "--all"}, false, false},
		{[]string{"list", "--stats", "--json"}, false, false},
		{[]string{"group", "list", "--json"}, false, false},
	} {
		withStats, ok := remoteAgentListPoll(tc.args)
		if withStats != tc.withStats || ok != tc.isIt {
			t.Errorf("%v: (%v, %v), want (%v, %v)", tc.args, withStats, ok, tc.withStats, tc.isIt)
		}
	}
}

func remoteAgentOpenFixtureDB(t *testing.T) *session.Storage {
	t.Helper()
	storage, err := session.NewStorageWithProfile(remoteAgentMonitorTestProfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return storage
}

// One hydration serves every probe until the registry rows, its edit
// watermark or the user config change; commits to other tables (the TUI
// heartbeat, metadata) only cost a raw-row comparison.
func TestRemoteAgentLister_ReusesHydrationUntilRegistryChanges(t *testing.T) {
	remoteAgentMonitorFixture(t)
	lister, err := newRemoteAgentLister(remoteAgentMonitorTestProfile, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lister.close()
	if lister.observer == nil {
		t.Fatal("no registry observer")
	}
	probe := func() string {
		t.Helper()
		list, _, err := lister.probe()
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	first := probe()
	if second := probe(); second != first || lister.registryLoads != 1 || lister.registryHits != 1 {
		t.Fatalf("unchanged registry: loads=%d hits=%d same=%v", lister.registryLoads, lister.registryHits, second == first)
	}
	peer := remoteAgentOpenFixtureDB(t)
	if err := peer.GetDB().SetMeta("remote_agent_test_noise", time.Now().String()); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != first || lister.registryLoads != 1 || lister.registryHits != 2 {
		t.Fatalf("unrelated commit rehydrated: loads=%d hits=%d", lister.registryLoads, lister.registryHits)
	}
	if applied, err := peer.GetDB().UpdateTitleIfUnlocked("mon-observed", "renamed-elsewhere"); err != nil || !applied {
		t.Fatalf("rename: %v %v", applied, err)
	}
	if got := probe(); !strings.Contains(got, "renamed-elsewhere") || lister.registryLoads != 2 {
		t.Fatalf("registry edit not picked up: loads=%d\n%s", lister.registryLoads, got)
	}
	configPath, err := session.GetUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("# remote-agent registry reuse test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe()
	if lister.registryLoads != 3 {
		t.Fatalf("config change did not rehydrate: loads=%d", lister.registryLoads)
	}
}

// Kept instances hold monitor verdicts between probes; when the monitor goes
// away they are dropped, so the local pass really samples every row again.
func TestRemoteAgentLister_FallbackDropsKeptVerdicts(t *testing.T) {
	ids := remoteAgentMonitorFixture(t)
	up := true
	now := time.Now()
	fake := &fakeRemoteAgentRuntime{snapshot: func() (*session.RuntimeProfileSnapshot, error) {
		if !up {
			return nil, errors.New("monitor stopped")
		}
		return remoteAgentMonitorSnapshot(now, session.RuntimeSessionSnapshot{
			ID: "mon-observed", Identity: ids["mon-observed"], Status: session.StatusRunning,
		}), nil
	}}
	lister, err := newRemoteAgentLister(remoteAgentMonitorTestProfile, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer lister.close()
	lister.logf = func(string, ...any) {}
	lister.now = func() time.Time { return now }
	status := func() string {
		t.Helper()
		list, _, err := lister.probe()
		if err != nil {
			t.Fatal(err)
		}
		return remoteAgentListed(t, list)["mon-observed"].Status
	}
	if got := status(); got != "running" {
		t.Fatalf("with monitor: %q", got)
	}
	up = false
	now = now.Add(remoteAgentMonitorRetry + time.Second)
	if got := status(); got != "idle" {
		t.Fatalf("monitor gone, kept verdict still listed: %q", got)
	}
	up = true
	now = now.Add(remoteAgentMonitorRetry + time.Second)
	if got := status(); got != "running" {
		t.Fatalf("monitor back: %q", got)
	}
	// One hydration, plus the clean one taken when the monitor went away.
	if lister.registryLoads != 2 {
		t.Fatalf("registry hydrated %d times, want 2", lister.registryLoads)
	}
}

// The group poll returns the body the change feed pushes, and only with the
// monitor (otherwise the CLI runs).
func TestRemoteAgentLister_AnswersGroupPoll(t *testing.T) {
	ids := remoteAgentMonitorFixture(t)
	up := true
	fake := &fakeRemoteAgentRuntime{snapshot: func() (*session.RuntimeProfileSnapshot, error) {
		if !up {
			return nil, errors.New("monitor stopped")
		}
		return remoteAgentMonitorSnapshot(time.Now(), session.RuntimeSessionSnapshot{
			ID: "mon-observed", Identity: ids["mon-observed"], Status: session.StatusRunning,
		}), nil
	}}
	lister, err := newRemoteAgentLister(remoteAgentMonitorTestProfile, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer lister.close()
	lister.logf = func(string, ...any) {}
	_, pushed, err := lister.probe()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := lister.answerGroups()
	if !ok || got != pushed {
		t.Fatalf("group poll ok=%v differs from the pushed body:\n%s\n---\n%s", ok, got, pushed)
	}
	up = false
	lister.retryAt = time.Time{}
	if _, ok := lister.answerGroups(); ok {
		t.Fatal("group poll answered in-process without the monitor")
	}
	for args, want := range map[string]bool{"group list --json": true, "group list": false, "group list --json --all": false, "list --json": false} {
		if got := remoteAgentGroupPoll(strings.Fields(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}
