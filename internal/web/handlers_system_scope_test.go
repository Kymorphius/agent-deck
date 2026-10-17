package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/sysinfo"
)

// matrix/hardened: REST consumers retain guest scope; legacy and unavailable snapshots stay compatible.
func TestMatrixSystemStatsMemoryScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     string
		available bool
	}{
		{"guest", sysinfo.MemoryScopeGuest, true},
		{"legacy", "", true},
		{"unavailable", sysinfo.MemoryScopeGuest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{systemStats: systemStatsCache{collect: func() sysinfo.Stats {
				return sysinfo.Stats{Memory: sysinfo.MemStat{
					Available: tc.available, Scope: tc.scope,
					UsedBytes: 1 << 30, TotalBytes: 4 << 30, UsagePercent: 25,
				}}
			}}}
			w := httptest.NewRecorder()
			s.handleSystemStats(w, httptest.NewRequest(http.MethodGet, "/api/system/stats", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var body struct {
				Memory map[string]any `json:"memory"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !tc.available {
				if body.Memory != nil {
					t.Fatalf("unavailable memory emitted: %s", w.Body.String())
				}
				return
			}
			got, present := body.Memory["scope"]
			if present != (tc.scope != "") || (present && got != tc.scope) {
				t.Fatalf("scope changed: %s", w.Body.String())
			}
			if body.Memory["used_human"] != "1.0G" || body.Memory["total_human"] != "4.0G" || body.Memory["usage_percent"] != float64(25) {
				t.Fatalf("memory values changed: %s", w.Body.String())
			}
		})
	}
}
