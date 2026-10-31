package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the CLI only formats and forwards; refresh needs --yes to act.
type fakeAIComputerUse struct {
	report  session.CodexComputerUseReport
	results []session.CodexComputerUseRefreshResult
	opts    *session.CodexComputerUseRefreshOptions
	closed  bool
}

func (f *fakeAIComputerUse) Inspect() session.CodexComputerUseReport { return f.report }

func (f *fakeAIComputerUse) Refresh(_ context.Context, opts session.CodexComputerUseRefreshOptions) []session.CodexComputerUseRefreshResult {
	f.opts = &opts
	return f.results
}

func (f *fakeAIComputerUse) Resolve(identifier string) (string, error) {
	if identifier == "missing" {
		return "", errors.New("session not found")
	}
	return "id-" + identifier, nil
}

func (f *fakeAIComputerUse) factory() (aiComputerUseService, func(), error) {
	return f, func() { f.closed = true }, nil
}

func staleComputerUseReport() session.CodexComputerUseReport {
	return session.CodexComputerUseReport{
		Bundles: map[string]aiupdate.ComputerUseBundle{"/h/.codex": {Present: true, Enabled: true, PluginVersion: "2", AppVersion: "2"}},
		Findings: []session.CodexComputerUseFinding{
			{ID: "a", Title: "Alpha", State: session.CodexComputerUseStale, Reason: "shared Codex daemon 7 started before", Host: &session.CodexComputerUseHost{PID: 7, Daemon: true}},
			{ID: "b", Title: "Beta", State: session.CodexComputerUseOK, Host: &session.CodexComputerUseHost{PID: 9}},
		},
	}
}

func TestAIComputerUseStatus(t *testing.T) {
	fake := &fakeAIComputerUse{report: staleComputerUseReport()}
	var out, diag bytes.Buffer
	if err := runAIComputerUse(context.Background(), []string{"computer-use"}, &out, &diag, fake.factory); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "stale") || !strings.Contains(text, "Alpha") || !strings.Contains(text, "1 session(s) need a refresh") || !fake.closed {
		t.Fatalf("output = %q", text)
	}
	if fake.opts != nil {
		t.Fatal("computer-use must not refresh")
	}
	if summary := formatAIComputerUse(fake.report, true); !strings.Contains(summary, "1 Codex session(s) predate") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestAIComputerUseRefreshIsDryRunWithoutYes(t *testing.T) {
	fake := &fakeAIComputerUse{results: []session.CodexComputerUseRefreshResult{{HostPID: 7, Daemon: true, Titles: []string{"Alpha"}, Ready: true}}}
	var out, diag bytes.Buffer
	if err := runAIComputerUse(context.Background(), []string{"refresh"}, &out, &diag, fake.factory); err != nil {
		t.Fatal(err)
	}
	if fake.opts == nil || !fake.opts.DryRun || !strings.Contains(out.String(), "Dry run") {
		t.Fatalf("opts = %+v output = %q", fake.opts, out.String())
	}
}

func TestAIComputerUseRefreshWithYesAndTargets(t *testing.T) {
	fake := &fakeAIComputerUse{results: []session.CodexComputerUseRefreshResult{{HostPID: 7, Ready: true, Refreshed: true, Verified: true}}}
	var out, diag bytes.Buffer
	if err := runAIComputerUse(context.Background(), []string{"refresh", "alpha", "--yes", "--json"}, &out, &diag, fake.factory); err != nil {
		t.Fatal(err)
	}
	if fake.opts == nil || fake.opts.DryRun || !fake.opts.Only["id-alpha"] || len(fake.opts.Only) != 1 {
		t.Fatalf("opts = %+v", fake.opts)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil || payload["success"] != true || payload["dry_run"] != false {
		t.Fatalf("payload = %s (%v)", out.String(), err)
	}
}

func TestAIComputerUseRefreshFailures(t *testing.T) {
	notReady := &fakeAIComputerUse{results: []session.CodexComputerUseRefreshResult{{HostPID: 7, Reason: "Alpha is running"}}}
	var out, diag bytes.Buffer
	if err := runAIComputerUse(context.Background(), []string{"refresh", "--yes"}, &out, &diag, notReady.factory); err == nil || !strings.Contains(out.String(), "not ready: Alpha is running") {
		t.Fatalf("a host that was not refreshed must fail: %v %q", err, out.String())
	}
	unknown := &fakeAIComputerUse{}
	out.Reset()
	if err := runAIComputerUse(context.Background(), []string{"refresh", "missing"}, &out, &diag, unknown.factory); err == nil || unknown.opts != nil {
		t.Fatalf("an unknown session must be refused before refreshing: %v", err)
	}
}

func TestAIComputerUseActionRouting(t *testing.T) {
	for args, want := range map[string]bool{"computer-use": true, "--json refresh": true, "status": false, "": false, "restart --yes": false} {
		if got := isAIComputerUseAction(strings.Fields(args)); got != want {
			t.Errorf("isAIComputerUseAction(%q) = %v", args, got)
		}
	}
	for args, want := range map[string]bool{"": true, "status": true, "status --json": false, "check": false, "--help": false} {
		if got := aiUpdateTextStatus(strings.Fields(args)); got != want {
			t.Errorf("aiUpdateTextStatus(%q) = %v", args, got)
		}
	}
}
