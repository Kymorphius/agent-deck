package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/sysinfo"
)

// matrix/hardened: distinguish a guest budget without changing older remote payloads.
func TestMatrixSystemStatsMemoryScope(t *testing.T) {
	setupSystemStatsAccountsTest(t)
	for _, scope := range []string{"", sysinfo.MemoryScopeGuest} {
		t.Run("scope="+scope, func(t *testing.T) {
			stats := sysinfo.Stats{Memory: sysinfo.MemStat{
				Available: true, UsedBytes: 1 << 30, TotalBytes: 4 << 30, UsagePercent: 25, Scope: scope,
			}}
			text := captureStdout(t, func() { printSystemStats(stats, false) })
			label := "Memory"
			if scope != "" {
				label += " (guest)"
			}
			if !strings.Contains(text, label+": 1.0G/4.0G (25%)\n") {
				t.Fatalf("text lost memory scope or values: %q", text)
			}
			output := captureStdout(t, func() { printSystemStats(stats, true) })
			var wire struct {
				Memory map[string]json.RawMessage `json:"memory"`
			}
			if err := json.Unmarshal([]byte(output), &wire); err != nil {
				t.Fatal(err)
			}
			got, present := wire.Memory["scope"]
			if present != (scope != "") || (present && string(got) != `"guest"`) {
				t.Fatalf("scope = %s, present = %v; output %s", got, present, output)
			}
		})
	}
	output := captureStdout(t, func() {
		printSystemStats(sysinfo.Stats{Memory: sysinfo.MemStat{Scope: sysinfo.MemoryScopeGuest}}, true)
	})
	if strings.Contains(output, `"memory"`) || strings.Contains(output, `"scope"`) {
		t.Fatalf("unavailable guest memory must remain omitted: %s", output)
	}
}
