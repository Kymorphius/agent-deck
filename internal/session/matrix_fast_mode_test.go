package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

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

func TestFastModeCachedReadsRuntimeReplica(t *testing.T) {
	inst := &Instance{ID: "x"}
	inst.runtimeReplica.Store(&RuntimeSessionSnapshot{FastMode: true})
	if !inst.FastModeCached() {
		t.Fatal("consumer must report the monitor's fast verdict")
	}
	inst.runtimeReplica.Store(nil)
	inst.runtimeConsumer.Store(true)
	if inst.FastModeCached() {
		t.Fatal("consumer without a row must not claim fast")
	}
}

// matrix/hardened: Codex /fast is the priority service tier in the latest
// thread_settings_applied event (real rollout shape, trimmed).
func TestScanCodexFastTier(t *testing.T) {
	settings := func(tier string) string {
		return `{"timestamp":"2026-10-20T18:47:12.369Z","type":"event_msg","payload":{"type":"thread_settings_applied","thread_id":"t","thread_settings":{"model":"gpt-6.1-sol","service_tier":"` + tier + `","reasoning_effort":"ultra"}}}`
	}
	const (
		started = `{"timestamp":"2026-10-20T18:47:13.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"x"}}`
		quoted  = `{"type":"response_item","payload":{"type":"message","content":[{"type":"output_text","text":"thread_settings_applied priority"}]}}`
	)
	tests := []struct {
		name    string
		records []string
		want    bool
	}{
		{"priority then turn", []string{settings("default"), settings("priority"), started}, true},
		{"switched back to default", []string{settings("priority"), settings("default"), started}, false},
		{"no settings event", []string{started}, false},
		{"text mentioning the event is not an event", []string{settings("default"), quoted}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fast, complete := (&codexTierTracker{entries: map[string]*codexTierEntry{}}).fast(writeUsageLimitTranscript(t, tt.records...))
			if !complete || fast != tt.want {
				t.Fatalf("fast=%v complete=%v, want fast=%v", fast, complete, tt.want)
			}
		})
	}
}

func TestFastModeCachedCodexMemoFollowsBinding(t *testing.T) {
	inst := &Instance{ID: "c", Tool: "codex", CodexSessionID: "abc"}
	inst.matrixInterrupt = matrixTurnInterruptMemo{key: "codex:abc", codexFast: true}
	if !inst.FastModeCached() {
		t.Fatal("producer must report the cached Codex fast tier")
	}
	inst.CodexSessionID = "other"
	if inst.FastModeCached() {
		t.Fatal("a rebound Codex session must not inherit the old fast tier")
	}
}

func TestRemoteSessionInfoDecodesFastMode(t *testing.T) {
	var rows []RemoteSessionInfo
	if err := json.Unmarshal([]byte(`[{"id":"a","fast_mode":true},{"id":"b"}]`), &rows); err != nil {
		t.Fatal(err)
	}
	if !rows[0].FastMode || rows[1].FastMode {
		t.Fatalf("fast_mode decode = %v/%v, want true/false (older remotes omit it)", rows[0].FastMode, rows[1].FastMode)
	}
}

func codexSettingsLine(tier string) string {
	return `{"timestamp":"2026-10-29T18:59:47.478Z","type":"event_msg","payload":{"type":"thread_settings_applied","thread_id":"t","thread_settings":{"model":"gpt-6.1-sol","service_tier":"` + tier + `"}}}`
}

// matrix/hardened: real rollouts had a 7.9 MiB tool-output line, and 28 MiB of
// records, after the latest settings event; both hid /fast from the list.
func TestCodexTierSurvivesHugeOutputAfterSettings(t *testing.T) {
	big := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 3<<20) + `"}}`
	medium := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("y", 200<<10) + `"}}`
	records := []string{codexSettingsLine("default"), codexSettingsLine("priority"), big}
	for n := 0; n < 50; n++ { // ~10 MiB of ordinary records
		records = append(records, medium)
	}
	tracker := &codexTierTracker{entries: map[string]*codexTierEntry{}}
	fast, ok := tracker.fast(writeUsageLimitTranscript(t, records...))
	if !ok || !fast {
		t.Fatalf("fast=%v ok=%v, want the priority tier found behind the large output", fast, ok)
	}
}

func TestCodexTierFollowsAppendsAndRewrites(t *testing.T) {
	path := writeUsageLimitTranscript(t, codexSettingsLine("priority"))
	tracker := &codexTierTracker{entries: map[string]*codexTierEntry{}}
	if fast, ok := tracker.fast(path); !ok || !fast {
		t.Fatalf("initial: fast=%v ok=%v", fast, ok)
	}
	appendLine := func(line string, newline bool) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if newline {
			line += "\n"
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	// A partial line is not read until it is complete.
	half := codexSettingsLine("default")
	appendLine(half[:20], false)
	if fast, _ := tracker.fast(path); !fast {
		t.Fatal("a partial line must not change the tier")
	}
	appendLine(half[20:], true)
	if fast, ok := tracker.fast(path); !ok || fast {
		t.Fatalf("after /fast off: fast=%v ok=%v", fast, ok)
	}
	appendLine(`{"type":"event_msg","payload":{"type":"task_started"}}`, true)
	if fast, _ := tracker.fast(path); fast {
		t.Fatal("unrelated appends keep the last tier")
	}
	// A rewritten (shorter) file is read again from the end.
	if err := os.WriteFile(path, []byte(codexSettingsLine("priority")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fast, ok := tracker.fast(path); !ok || !fast {
		t.Fatalf("after rewrite: fast=%v ok=%v", fast, ok)
	}
}
