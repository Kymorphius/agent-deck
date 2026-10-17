package session

import "testing"

// matrix/hardened: an updated remote's memory scope must survive the SSH wire.
func TestMatrixRemoteStatsMemoryScope(t *testing.T) {
	for _, tc := range []struct {
		name, memory, wantScope string
		available               bool
	}{
		{"guest", `"memory":{"scope":"guest","used_bytes":1024,"total_bytes":4096,"usage_percent":25}`, "guest", true},
		{"legacy", `"memory":{"used_bytes":1024,"total_bytes":4096,"usage_percent":25}`, "", true},
		{"unavailable", `"cpu":{"usage_percent":0}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := parseRemoteHostStats([]byte("{" + tc.memory + "}"))
			if err != nil {
				t.Fatal(err)
			}
			if !stats.Ok || stats.MemScope != tc.wantScope || stats.MemAvailable != tc.available {
				t.Fatalf("remote scope lost or invented: %+v", stats)
			}
			if tc.available && (stats.MemUsedBytes != 1024 || stats.MemTotalBytes != 4096 || stats.MemUsagePercent != 25) {
				t.Fatalf("scope handling changed remote values: %+v", stats)
			}
		})
	}
}
