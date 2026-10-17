package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/sysinfo"
)

// matrix/hardened: narrow previews retain the qualifier before clipping values.
func TestMatrixGuestMemoryRendering(t *testing.T) {
	stats := sysinfo.Stats{Memory: sysinfo.MemStat{
		Available: true, Scope: sysinfo.MemoryScopeGuest,
		UsedBytes: 1 << 30, TotalBytes: 4 << 30, UsagePercent: 25,
	}}
	for _, width := range []int{20, 30, 60, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			got := stripAnsi(renderSystemStatsSnapshot(stats, []string{"ram"}, width))
			if !strings.Contains(got, "RAM guest") {
				t.Fatalf("guest qualifier lost: %q", got)
			}
			assertFrameFits(t, got, width)
			if width >= 30 && !strings.Contains(got, "1.0G/4.0G") {
				t.Fatalf("memory values lost at width %d: %q", width, got)
			}
		})
	}
	stats.Memory.Scope = ""
	if got := stripAnsi(renderSystemStatsSnapshot(stats, []string{"ram"}, 60)); strings.Contains(got, "guest") || !strings.Contains(got, "RAM") {
		t.Fatalf("legacy label changed: %q", got)
	}
}

func TestMatrixRemoteGuestMemoryRendering(t *testing.T) {
	stats := session.RemoteHostStats{
		Ok: true, MemAvailable: true, MemScope: sysinfo.MemoryScopeGuest,
		MemUsedBytes: 1 << 30, MemTotalBytes: 4 << 30,
	}
	fields := []string{session.PreviewFieldMemory}
	state := session.RemoteVersionState{Found: true, Version: "1.16.26"}
	if got := remoteHostLoadLineFiltered(stats, fields, state, "1.16.26"); got != "RAM guest 1.0G/4.0G" {
		t.Fatalf("remote header lost scope: %q", got)
	}
	for _, width := range []int{20, 30, 60} {
		lines := remotePreviewFieldLines(state, "1.16.26", nil, remoteHostStatsResult{Stats: stats}, true, fields, time.Now(), previewLayout{width: width, rows: 1})
		got := strings.Join(lines, "\n")
		if !strings.Contains(got, "RAM guest") {
			t.Fatalf("remote guest qualifier lost at width %d: %q", width, got)
		}
		assertFrameFits(t, got, width)
	}
	stats.MemScope = ""
	if got := remoteHostLoadLineFiltered(stats, fields, state, "1.16.26"); got != "1.0G/4.0G" {
		t.Fatalf("legacy remote header changed: %q", got)
	}
}
