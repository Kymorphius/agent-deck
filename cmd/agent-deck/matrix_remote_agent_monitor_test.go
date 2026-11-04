package main

import (
	"context"
	"encoding/json"
	"errors"
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
