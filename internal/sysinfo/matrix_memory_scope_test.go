package sysinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: host Active/Inactive counters must not replace the guest's
// own total/available budget, even when they exceed that budget.
func TestMemoryGuestScopeFromExplicitProcInterface(t *testing.T) {
	root := t.TempDir()
	content := "MemTotal: 1048576 kB\nMemAvailable: 262144 kB\nActive: 8388608 kB\nInactive: 4194304 kB\n"
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	regular := collectMemLinuxFrom(root)
	if !regular.Available || regular.Scope != "" || regular.TotalBytes != 1<<30 || regular.UsedBytes != 768<<20 || regular.UsagePercent != 75 {
		t.Fatalf("ordinary proc snapshot = %+v", regular)
	}
	if err := os.Mkdir(filepath.Join(root, "ish"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ish", "version"), []byte("test guest\n"), 0600); err != nil {
		t.Fatal(err)
	}
	guest := collectMemLinuxFrom(root)
	if guest.Scope != MemoryScopeGuest {
		t.Fatalf("guest scope = %q", guest.Scope)
	}
	guest.Scope = ""
	if guest != regular {
		t.Fatalf("scope changed the budget: guest=%+v ordinary=%+v", guest, regular)
	}
}

// matrix/hardened: malformed/partial budgets must never wrap or look 100% used.
func TestMemoryInvalidSnapshotsUnavailable(t *testing.T) {
	cases := map[string]string{
		"missing available": "MemTotal: 1024 kB\n",
		"missing total":     "MemAvailable: 512 kB\n",
		"zero total":        "MemTotal: 0 kB\nMemAvailable: 0 kB\n",
		"underflow":         "MemTotal: 1024 kB\nMemAvailable: 1025 kB\n",
		"bad total":         "MemTotal: unknown kB\nMemAvailable: 512 kB\n",
		"bad available":     "MemTotal: 1024 kB\nMemAvailable: unknown kB\n",
		"negative":          "MemTotal: 1024 kB\nMemAvailable: -1 kB\n",
		"wrong units":       "MemTotal: 1024 MB\nMemAvailable: 512 kB\n",
		"missing units":     "MemTotal: 1024\nMemAvailable: 512 kB\n",
		"byte overflow":     "MemTotal: 18014398509481984 kB\nMemAvailable: 0 kB\n",
		"integer overflow":  "MemTotal: 18446744073709551616 kB\nMemAvailable: 0 kB\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ParseMeminfo(content); got.Available {
				t.Fatalf("invalid snapshot was available: %+v", got)
			}
		})
	}
	for _, content := range []string{
		"MemTotal: 1024 kB\nMemAvailable: 0 kB\n",
		"MemTotal: 1024 kB\nMemAvailable: 1024 kB\n",
		"MemTotal: 18014398509481983 kB\nMemAvailable: 0 kB\n",
	} {
		if got := ParseMeminfo(content); !got.Available || got.UsedBytes > got.TotalBytes || got.UsagePercent < 0 || got.UsagePercent > 100 {
			t.Fatalf("valid boundary snapshot = %+v", got)
		}
	}
}

// matrix/hardened: all header styles retain provenance; normal hosts stay stable.
func TestMemoryScopeSurvivesEveryHeaderStyle(t *testing.T) {
	stat := MemStat{Available: true, UsedBytes: 768 << 20, TotalBytes: 1 << 30, UsagePercent: 75}
	ordinary := map[string]string{
		"full":    "RAM: 768.0M/1.0G (75%)",
		"compact": "⛁ 768.0M/1.0G",
		"minimal": "768.0M/1.0G",
	}
	for style, want := range ordinary {
		if got := Format(Stats{Memory: stat}, style, []string{"ram"}); got != want {
			t.Errorf("legacy %s = %q, want %q", style, got, want)
		}
		guest := stat
		guest.Scope = MemoryScopeGuest
		if got := Format(Stats{Memory: guest}, style, []string{"ram"}); !strings.Contains(got, "guest") || !strings.Contains(got, "768.0M/1.0G") {
			t.Errorf("guest %s lost scope/budget: %q", style, got)
		}
	}
}
