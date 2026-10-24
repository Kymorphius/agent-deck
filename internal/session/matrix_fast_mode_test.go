package session

import "testing"

// matrix/hardened: the list flags Claude /fast from usage.speed on the latest
// main-conversation assistant turn; sidechains and older turns do not count.
func TestScanLatestAssistantFastMode(t *testing.T) {
	const (
		user      = `{"type":"user","message":{"role":"user","content":"hi"}}`
		fast      = `{"type":"assistant","timestamp":"2026-10-23T10:00:00Z","message":{"role":"assistant","usage":{"speed":"fast"}}}`
		standard  = `{"type":"assistant","timestamp":"2026-10-23T10:00:00Z","message":{"role":"assistant","usage":{"speed":"standard"}}}`
		noUsage   = `{"type":"assistant","timestamp":"2026-10-23T10:00:00Z","message":{"role":"assistant"}}`
		sideFast  = `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","usage":{"speed":"fast"}}}`
		nullSpeed = `{"type":"assistant","message":{"role":"assistant","usage":{"speed":null}}}`
	)
	tests := []struct {
		name    string
		records []string
		want    bool
	}{
		{"latest fast", []string{standard, fast, user}, true},
		{"latest standard after fast", []string{fast, standard}, false},
		{"no usage", []string{noUsage}, false},
		{"null speed", []string{fast, nullSpeed}, false},
		{"sidechain fast ignored", []string{standard, sideFast}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanLatestAssistantUsageLimitFact(writeUsageLimitTranscript(t, tt.records...))
			if !got.formed || got.fact.fast != tt.want {
				t.Fatalf("formed=%v fast=%v, want fast=%v", got.formed, got.fact.fast, tt.want)
			}
		})
	}
}

func TestClaudeFastModeCachedReadsRuntimeReplica(t *testing.T) {
	inst := &Instance{ID: "x"}
	inst.runtimeReplica.Store(&RuntimeSessionSnapshot{FastMode: true})
	if !inst.ClaudeFastModeCached() {
		t.Fatal("consumer must report the monitor's fast verdict")
	}
	inst.runtimeReplica.Store(nil)
	inst.runtimeConsumer.Store(true)
	if inst.ClaudeFastModeCached() {
		t.Fatal("consumer without a row must not claim fast")
	}
}
