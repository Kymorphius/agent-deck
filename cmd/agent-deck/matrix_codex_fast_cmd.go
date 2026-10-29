package main

// matrix/hardened: `agent-deck codex-fast on|off` switches every live Codex
// session of this profile on this host to /fast or back (see
// internal/session/matrix_codex_fast_switch.go). The TUI runs it here and on
// each remote, so both report the same per-session outcome.

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func handleCodexFast(profile string, args []string) {
	fs := flag.NewFlagSet("codex-fast", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	dryRun := fs.Bool("dry-run", false, "Only report what would change")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck codex-fast on|off [--dry-run] [--json]")
		fmt.Println()
		fmt.Println("Switch every live Codex session on this host to /fast (on) or back (off).")
		fmt.Println("A running turn is interrupted, switched, and sent \"" + session.CodexFastContinuePrompt + "\";")
		fmt.Println("an idle session is only switched. Sessions with an unsent draft, an open")
		fmt.Println("menu or an unreadable footer are skipped. Codex saves the last switch to")
		fmt.Println("config.toml (service_tier), so new Codex sessions follow it.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "on" && fs.Arg(0) != "off") {
		fs.Usage()
		os.Exit(2)
	}
	want := fs.Arg(0) == "on"
	out := NewCLIOutput(*jsonOutput, false)

	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		out.Error(fmt.Sprintf("load storage: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	instances, _, err := storage.LoadWithGroups()
	if err != nil {
		out.Error(fmt.Sprintf("load sessions: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}

	results := session.SwitchCodexFast(instances, want, *dryRun)
	summary := session.CodexFastSummary(results)
	failed := false
	var b strings.Builder
	for _, r := range results {
		if r.Outcome == session.CodexFastFailed {
			failed = true
		}
		line := fmt.Sprintf("%-8s %s", r.Outcome, r.Title)
		if r.Continued {
			line += " (interrupted, continued)"
		} else if r.Interrupted {
			line += " (interrupted)"
		}
		if r.Reason != "" {
			line += ": " + r.Reason
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(summary + "\n")
	data := map[string]any{"success": !failed, "fast": want, "dry_run": *dryRun, "summary": summary, "results": results}
	if failed {
		out.ErrorWithData(b.String(), ErrCodeInvalidOperation, data)
		os.Exit(1)
	}
	out.Print(b.String(), data)
}
