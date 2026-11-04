package session

// matrix/hardened: only durable main-conversation user records advance the
// interaction clock; visits, model output and injected context cannot do so.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func matrixInteractionClaudeRecord(id, stamp, content string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":%q,"message":{"role":"user","content":%s}}`, id, stamp, content)
}

func matrixInteractionCodexMeta(id string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"thread_source":"user","source":"cli"}}`, id)
}

func matrixInteractionCodexRecord(stamp string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"fixture prompt"}}`, stamp)
}

func TestMatrixInteractionClaudeEvidenceSkipsOutputAndSyntheticUsers(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC()
	stamp := at.Format(time.RFC3339Nano)
	user := matrixInteractionClaudeRecord("fixture", stamp, `"first prompt"`)
	newer := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name  string
		later string
	}{
		{"assistant", tiAssistant},
		{"tool result", matrixInteractionClaudeRecord("fixture", newer, `[{"type":"tool_result","content":"output"}]`)},
		{"sidechain", strings.Replace(matrixInteractionClaudeRecord("fixture", newer, `"child prompt"`), `"type":"user"`, `"type":"user","isSidechain":true`, 1)},
		{"compact summary", strings.Replace(matrixInteractionClaudeRecord("fixture", newer, `"summary"`), `"type":"user"`, `"type":"user","isCompactSummary":true`, 1)},
		{"meta instruction", strings.Replace(matrixInteractionClaudeRecord("fixture", newer, `"injected instruction"`), `"type":"user"`, `"type":"user","isMeta":true`, 1)},
		{"empty text", matrixInteractionClaudeRecord("fixture", newer, `"  "`)},
		{"missing timestamp", matrixInteractionClaudeRecord("fixture", "", `"undated prompt"`)},
		{"future timestamp", matrixInteractionClaudeRecord("fixture", time.Now().Add(time.Hour).Format(time.RFC3339Nano), `"future prompt"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := matrixWriteTranscript(t, user, tiAssistant, tc.later, tiDuration)
			_, _, complete, got, id := matrixScanLatestClaudeTurnFacts(path)
			if !complete || !got.Equal(at) || id != "fixture" {
				t.Fatalf("latest real user = %v/%q (complete=%v), want %v/fixture", got, id, complete, at)
			}
		})
	}
	for _, content := range []string{
		`[{"type":"image","source":{"type":"base64","data":"fixture"}}]`,
		`[{"type":"document","source":{"type":"base64","data":"fixture"}}]`,
	} {
		path := matrixWriteTranscript(t, matrixInteractionClaudeRecord("fixture", stamp, content), tiLost)
		verdict, interrupted, complete, got, _ := matrixScanLatestClaudeTurnFacts(path)
		if !complete || !got.Equal(at) || !interrupted || verdict.Kind != "server_error" {
			t.Fatalf("attachment-only prompt lost timestamp or changed interruption: at=%v interrupted=%v verdict=%+v", got, interrupted, verdict)
		}
	}
}

func TestMatrixInteractionClaudeEvidencePreservesLatestInterruptVerdict(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC()
	user := matrixInteractionClaudeRecord("fixture", at.Format(time.RFC3339Nano), `"real prompt"`)
	for _, tc := range []struct {
		name string
		tail []string
		want bool
	}{
		{"interrupted", []string{tiAssistant, tiLost, tiDuration}, true},
		{"finished", []string{tiLost, tiAssistant, tiDuration}, false},
		{"tool result after error", []string{tiLost, tiToolRes}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := matrixWriteTranscript(t, append([]string{user}, tc.tail...)...)
			_, interrupted, _, got, _ := matrixScanLatestClaudeTurnFacts(path)
			if interrupted != tc.want || !got.Equal(at) {
				t.Fatalf("interruption=%v clock=%v, want %v/%v", interrupted, got, tc.want, at)
			}
		})
	}
	partial := matrixWriteTranscript(t, user, tiAssistant)
	file, err := os.OpenFile(partial, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"type":"user","timestamp":"2099-01-01T00:00:00Z"`)
	_ = file.Close()
	_, _, cacheable, got, _ := matrixScanLatestClaudeTurnFacts(partial)
	if cacheable || !got.Equal(at) {
		t.Fatalf("partial tail fabricated input or became cacheable: clock=%v cacheable=%v", got, cacheable)
	}
	oversized := matrixWriteTranscript(t, user, `{"type":"assistant","message":{"content":"`+strings.Repeat("x", matrixTurnInterruptMaxLineBytes)+`"}}`)
	_, _, _, got, _ = matrixScanLatestClaudeTurnFacts(oversized)
	if !got.IsZero() {
		t.Fatal("looked through an unknown oversized record to fabricate latest evidence")
	}
}

func TestMatrixInteractionCodexRequiresNativeUserEvent(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC()
	stamp := at.Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name string
		tail []string
	}{
		{"assistant output and completion", []string{`{"type":"response_item","payload":{"role":"assistant","content":[]}}`, cxDone, cxTokens}},
		{"injected user response", []string{fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"instructions"}]}}`, time.Now().Format(time.RFC3339Nano))}},
		{"task started", []string{cxStarted}},
		{"invalid newer timestamp", []string{matrixInteractionCodexRecord("invalid")}},
		{"future newer timestamp", []string{matrixInteractionCodexRecord(time.Now().Add(time.Hour).Format(time.RFC3339Nano))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := append([]string{matrixInteractionCodexMeta("fixture"), matrixInteractionCodexRecord(stamp)}, tc.tail...)
			got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, lines...))
			if !got.interactionAt.Equal(at) {
				t.Fatalf("latest actual user time = %v, want %v", got.interactionAt, at)
			}
		})
	}
	for _, head := range []string{
		cxSubMeta,
		`{"type":"session_meta","payload":{"id":"fixture"}}`,
		`{"type":"session_meta","payload":{"id":"fixture","session_id":"foreign","thread_source":"user"}}`,
		`{"type":"snapshot"}`,
	} {
		got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, head, matrixInteractionCodexRecord(stamp)))
		if !got.interactionAt.IsZero() {
			t.Fatalf("non-main/unknown native metadata recorded input: %+v", got.meta)
		}
	}
}

func TestMatrixInteractionClaudeRefreshRequiresExactUserOwnership(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999997004"
	testRoot := t.TempDir()
	t.Setenv("HOME", testRoot)
	configDir := filepath.Join(testRoot, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	project := filepath.Join(testRoot, "fixture-project")
	dir := filepath.Join(configDir, "projects", ConvertToClaudeDirName(project))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).UTC()
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"owned prompt", []string{matrixInteractionClaudeRecord(sid, at.Format(time.RFC3339Nano), `"prompt"`), tiAssistant}, true},
		{"foreign prompt", []string{matrixInteractionClaudeRecord("other-native-id", at.Format(time.RFC3339Nano), `"prompt"`), tiAssistant}, false},
		{"output only", []string{tiAssistant, tiDuration}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, sid+".jsonl")
			matrixWriteInterruptCacheBody(t, path, matrixInterruptCacheBody(tc.lines...))
			inst := &Instance{Tool: "claude", ClaudeSessionID: sid, ProjectPath: project, Status: StatusStopped}
			inst.MarkAccessed()
			inst.RefreshInteractionEvidence()
			got := inst.LastInteractionAt()
			if tc.want && !got.Equal(at) || !tc.want && !got.IsZero() {
				t.Fatalf("interaction clock = %v, want known=%v time=%v", got, tc.want, at)
			}
			if inst.Status != StatusStopped {
				t.Fatal("historic interaction backfill changed the stopped lifecycle state")
			}
		})
	}
}

func TestMatrixInteractionEvidenceRefreshKeepsVisitsSeparateAndCaches(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999997001"
	testRoot := t.TempDir()
	at := time.Now().Add(-time.Hour).UTC()
	matrixSeedInterruptCacheRollout(t, testRoot, sid, matrixInteractionCodexMeta(sid), matrixInteractionCodexRecord(at.Format(time.RFC3339Nano)), cxDone)
	cache, count := matrixCountedInterruptCache(16)
	for n := 0; n < 3; n++ {
		inst := matrixInterruptCacheInstance(testRoot, sid)
		inst.MarkAccessed()
		if !inst.LastInteractionAt().IsZero() {
			t.Fatal("merely visiting established an interaction")
		}
		inst.matrixRefreshTurnInterruptWithCache(cache)
		if got := inst.LastInteractionAt(); !got.Equal(at) {
			t.Fatalf("fresh instance read input time %v, want %v", got, at)
		}
		inst.MarkAccessed()
		inst.matrixRefreshTurnInterruptWithCache(cache)
		if got := inst.LastInteractionAt(); !got.Equal(at) {
			t.Fatalf("a second visit reordered by access time: %v", got)
		}
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("unchanged transcript parsed %d times, want 1", got)
	}
	wrongRoot := t.TempDir()
	matrixSeedInterruptCacheRollout(t, wrongRoot, sid, matrixInteractionCodexMeta("another-thread"), matrixInteractionCodexRecord(at.Format(time.RFC3339Nano)))
	wrong := matrixInterruptCacheInstance(wrongRoot, sid)
	wrong.matrixRefreshTurnInterruptWithCache(cache)
	if !wrong.LastInteractionAt().IsZero() {
		t.Fatal("foreign native metadata established interaction for the filename's ID")
	}
}

func TestMatrixInteractionEvidenceRejectsInflightOldBinding(t *testing.T) {
	const oldID = "019f0000-0000-7000-8000-999999997002"
	const newID = "019f0000-0000-7000-8000-999999997003"
	testRoot := t.TempDir()
	at := time.Now().Add(-time.Hour).UTC()
	oldPath := matrixSeedInterruptCacheRollout(t, testRoot, oldID, matrixInteractionCodexMeta(oldID), matrixInteractionCodexRecord(at.Format(time.RFC3339Nano)))
	oldPath, _ = filepath.EvalSymlinks(oldPath)
	matrixSeedInterruptCacheRollout(t, testRoot, newID, matrixInteractionCodexMeta(newID), cxStarted)
	cache := newMatrixTurnInterruptFileCache(16)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	cache.parse = func(tool, path string) matrixTurnInterruptFileResult {
		result := matrixParseTurnInterruptFile(tool, path)
		if path == oldPath {
			close(entered)
			<-release
		}
		return result
	}
	inst := matrixInterruptCacheInstance(testRoot, oldID)
	done := make(chan struct{})
	go func() { inst.matrixRefreshTurnInterruptWithCache(cache); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old input scan did not start")
	}
	inst.mu.Lock()
	inst.CodexSessionID = newID
	inst.mu.Unlock()
	inst.matrixRefreshTurnInterruptWithCache(cache)
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old input scan did not finish")
	}
	if got := inst.LastInteractionAt(); !got.IsZero() {
		t.Fatalf("late old-native input poisoned new binding: %v", got)
	}
}
