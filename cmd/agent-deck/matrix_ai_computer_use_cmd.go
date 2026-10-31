package main

// matrix/hardened: Codex Computer Use freshness. Inspection is read-only;
// refresh restarts processes only with --yes, after per-host readiness checks.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type aiComputerUseService interface {
	Inspect() session.CodexComputerUseReport
	Refresh(context.Context, session.CodexComputerUseRefreshOptions) []session.CodexComputerUseRefreshResult
	Resolve(identifier string) (string, error)
}

type localAIComputerUse struct{ instances []*session.Instance }

func (l localAIComputerUse) Inspect() session.CodexComputerUseReport {
	return session.InspectCodexComputerUse(l.instances)
}

func (l localAIComputerUse) Refresh(ctx context.Context, opts session.CodexComputerUseRefreshOptions) []session.CodexComputerUseRefreshResult {
	return session.RefreshCodexComputerUse(ctx, l.instances, session.InspectCodexComputerUse(l.instances), opts)
}

func (l localAIComputerUse) Resolve(identifier string) (string, error) {
	inst, message, _ := ResolveSession(identifier, l.instances)
	if inst == nil {
		return "", errors.New(message)
	}
	return inst.ID, nil
}

func newLocalAIComputerUse(profile string) (aiComputerUseService, func(), error) {
	storage, instances, _, err := loadSessionData(profile)
	if err != nil {
		return nil, nil, err
	}
	return localAIComputerUse{instances: instances}, func() { _ = storage.Close() }, nil
}

func isAIComputerUseAction(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg == "computer-use" || arg == "refresh"
		}
	}
	return false
}

func runAIComputerUse(ctx context.Context, args []string, stdout, stderr io.Writer, factory func() (aiComputerUseService, func(), error)) error {
	fs := flag.NewFlagSet("ai-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	yes := fs.Bool("yes", false, "Restart the stale Codex hosts (without it, refresh only checks readiness)")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		return err
	}
	fail := func(err error) error {
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"success": false, "error": err.Error(), "code": ErrCodeInvalidOperation})
		} else {
			fmt.Fprintf(stderr, "Error: %v\n", err)
		}
		return err
	}
	action, targets := fs.Arg(0), fs.Args()[1:]
	if action == "computer-use" && len(targets) > 0 {
		return fail(errors.New("computer-use takes no session arguments"))
	}
	svc, closeSvc, err := factory()
	if err != nil {
		return fail(err)
	}
	defer closeSvc()
	if action == "computer-use" {
		report := svc.Inspect()
		if *jsonOutput {
			return json.NewEncoder(stdout).Encode(map[string]any{"success": true, "action": action, "computer_use": report})
		}
		_, err := fmt.Fprint(stdout, formatAIComputerUse(report, false))
		return err
	}
	opts := session.CodexComputerUseRefreshOptions{DryRun: !*yes}
	if len(targets) > 0 {
		opts.Only = map[string]bool{}
		for _, target := range targets {
			id, err := svc.Resolve(target)
			if err != nil {
				return fail(err)
			}
			opts.Only[id] = true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	results := svc.Refresh(ctx, opts)
	failed := false
	for _, r := range results {
		if !r.Ready || (*yes && !r.Verified) {
			failed = true
		}
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"success": !failed, "action": action, "dry_run": opts.DryRun, "hosts": results}); err != nil {
			return err
		}
	} else {
		fmt.Fprint(stdout, formatAIComputerUseRefresh(results, opts.DryRun))
	}
	if failed {
		return errors.New("some Codex hosts were not refreshed")
	}
	return nil
}

func computerUseHostLabel(host *session.CodexComputerUseHost) string {
	if host == nil {
		return "host unknown"
	}
	if host.Daemon {
		return fmt.Sprintf("shared daemon %d", host.PID)
	}
	return fmt.Sprintf("process %d", host.PID)
}

// formatAIComputerUse renders the report; summary keeps only the totals for `status`.
func formatAIComputerUse(report session.CodexComputerUseReport, summary bool) string {
	var b strings.Builder
	homes := make([]string, 0, len(report.Bundles))
	for home := range report.Bundles {
		homes = append(homes, home)
	}
	sort.Strings(homes)
	active := false
	for _, home := range homes {
		bundle := report.Bundles[home]
		if !bundle.Present {
			continue
		}
		active = true
		state := "enabled"
		if !bundle.Enabled {
			state = "disabled in config.toml"
		}
		if !summary {
			fmt.Fprintf(&b, "Computer Use in %s: plugin %s from the Codex app %s, installed %s (%s)\n", home, bundle.PluginVersion, bundle.AppVersion, time.Unix(bundle.MaterializedAt, 0).Format(time.DateTime), state)
		}
	}
	due := report.NeedingRefresh()
	if summary {
		if !active {
			return ""
		}
		if len(due) == 0 {
			return "  Computer Use: running Codex sessions have the installed plugin.\n"
		}
		return fmt.Sprintf("  Computer Use: %d Codex session(s) predate the installed plugin. See `agent-deck ai-update computer-use`.\n", len(due))
	}
	if !active {
		return "No Codex app Computer Use plugin is installed for the running Codex sessions.\n"
	}
	for _, f := range report.Findings {
		reason := f.Reason
		if reason == "" {
			reason = computerUseHostLabel(f.Host)
		}
		fmt.Fprintf(&b, "  %-8s %s  %s\n", f.State, f.Title, reason)
	}
	if len(due) > 0 {
		fmt.Fprintf(&b, "%d session(s) need a refresh. Check with `agent-deck ai-update refresh`, apply with `--yes`.\n", len(due))
	}
	return b.String()
}

func formatAIComputerUseRefresh(results []session.CodexComputerUseRefreshResult, dryRun bool) string {
	if len(results) == 0 {
		return "No Codex session needs a Computer Use refresh.\n"
	}
	var b strings.Builder
	for _, r := range results {
		host := computerUseHostLabel(&session.CodexComputerUseHost{PID: r.HostPID, Daemon: r.Daemon})
		var state string
		switch {
		case !r.Ready:
			state = "not ready: " + r.Reason
		case dryRun:
			state = "ready"
		case r.Verified:
			state = "refreshed; Computer Use is running"
		default:
			state = "failed: " + r.Reason
		}
		fmt.Fprintf(&b, "%s (%s): %s\n", host, strings.Join(r.Titles, ", "), state)
	}
	if dryRun {
		b.WriteString("Dry run: nothing was stopped. Ready hosts restart with --yes; a shared daemon restart also resumes its sessions.\n")
	}
	return b.String()
}
