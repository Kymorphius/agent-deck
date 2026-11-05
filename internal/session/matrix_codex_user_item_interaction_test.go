package session

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: Codex also persists submitted input as native UserMessage
// items. Raw response-item user roles include injected context and are not proof.
func matrixCodexUserItem(stamp, threadID, content string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"item_completed","thread_id":%q,"turn_id":"fixture-turn","item":{"type":"UserMessage","id":"fixture-message","client_id":"fixture-client","content":%s}}}`, stamp, threadID, content)
}

func TestMatrixInteractionCodexUserItemSkipsOutputAndInjectedContext(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999996001"
	at := time.Now().Add(-time.Hour).UTC()
	newer := at.Add(time.Minute).Format(time.RFC3339Nano)
	user := matrixCodexUserItem(at.Format(time.RFC3339Nano), sid, `[{"type":"text","text":"fixture submitted prompt"}]`)
	for _, tail := range []struct {
		name string
		line string
	}{
		{"environment", fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>fixture</environment_context>"}]}}`, newer)},
		{"agents instructions", fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\nfixture"}]}}`, newer)},
		{"plain raw user role", fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"raw role alone"}]}}`, newer)},
		{"agent message", strings.Replace(matrixCodexUserItem(newer, sid, `[{"type":"text","text":"assistant output"}]`), `"UserMessage"`, `"AgentMessage"`, 1)},
		{"tool output", fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","aggregated_output":"fixture output"}}}`, newer)},
		{"reasoning", fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","summary_text":["fixture reasoning"]}}}`, newer)},
		{"task started", cxStarted},
	} {
		t.Run(tail.name, func(t *testing.T) {
			path := matrixWriteTranscript(t, matrixInteractionCodexMeta(sid), user, tail.line, cxDone, cxTokens)
			got := matrixParseTurnInterruptFile("codex", path)
			if !got.interactionAt.Equal(at) || got.found {
				t.Fatalf("interaction=%v interrupted=%v, want original submitted time %v and completed turn", got.interactionAt, got.found, at)
			}
		})
	}
}

func TestMatrixInteractionCodexUserItemValidatesContentTimeAndThread(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999996002"
	at := time.Now().Add(-time.Hour).UTC()
	stamp := at.Format(time.RFC3339Nano)
	old := matrixCodexUserItem(stamp, sid, `[{"type":"text","text":"original prompt"}]`)
	newer := at.Add(time.Minute).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name string
		line string
	}{
		{"empty content", matrixCodexUserItem(newer, sid, `[]`)},
		{"blank text", matrixCodexUserItem(newer, sid, `[{"type":"text","text":" \t "}]`)},
		{"unknown content", matrixCodexUserItem(newer, sid, `[{"type":"tool_result","text":"tool output"}]`)},
		{"empty image", matrixCodexUserItem(newer, sid, `[{"type":"image","image_url":""}]`)},
		{"empty local image", matrixCodexUserItem(newer, sid, `[{"type":"local_image","path":""}]`)},
		{"missing timestamp", matrixCodexUserItem("", sid, `[{"type":"text","text":"undated"}]`)},
		{"bad timestamp", matrixCodexUserItem("invalid", sid, `[{"type":"text","text":"invalid"}]`)},
		{"future timestamp", matrixCodexUserItem(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), sid, `[{"type":"text","text":"future"}]`)},
		{"foreign thread", matrixCodexUserItem(newer, "019f0000-0000-7000-8000-999999996099", `[{"type":"text","text":"foreign prompt"}]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, matrixInteractionCodexMeta(sid), old, tc.line))
			if !got.interactionAt.Equal(at) {
				t.Fatalf("invalid newer item changed interaction to %v, want %v", got.interactionAt, at)
			}
		})
	}
	for _, content := range []string{
		`[{"type":"image","image_url":"data:image/png;base64,Zml4dHVyZQ=="}]`,
		`[{"type":"local_image","path":"/fixture/image.png"}]`,
	} {
		got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, matrixInteractionCodexMeta(sid), matrixCodexUserItem(stamp, sid, content)))
		if !got.interactionAt.Equal(at) {
			t.Fatalf("attachment-only submission lost its original time: got %v, want %v", got.interactionAt, at)
		}
	}
	withoutThreadID := matrixCodexUserItem(stamp, "", `[{"type":"text","text":"fixture prompt"}]`)
	got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, matrixInteractionCodexMeta(sid), withoutThreadID))
	if !got.interactionAt.Equal(at) {
		t.Fatalf("native user item without optional thread_id lost main-header ownership: got %v, want %v", got.interactionAt, at)
	}
}

func TestMatrixInteractionCodexUserItemPreservesInterruptionSemantics(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999996003"
	at := time.Now().Add(-time.Hour).UTC()
	user := matrixCodexUserItem(at.Format(time.RFC3339Nano), sid, `[{"type":"text","text":"submitted prompt"}]`)
	foreign := matrixCodexUserItem(at.Add(time.Minute).Format(time.RFC3339Nano), "foreign-thread", `[{"type":"text","text":"other prompt"}]`)
	for _, tc := range []struct {
		name string
		body []string
		want bool
	}{
		{"later abort stays visible", []string{user, cxAborted}, true},
		{"new submission clears abort", []string{cxAborted, user}, false},
		{"foreign item cannot clear abort", []string{user, cxAborted, foreign}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := append([]string{matrixInteractionCodexMeta(sid)}, tc.body...)
			got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, lines...))
			if !got.interactionAt.Equal(at) || got.found != tc.want {
				t.Fatalf("interaction=%v interrupted=%v, want %v/%v", got.interactionAt, got.found, at, tc.want)
			}
		})
	}
}

func TestMatrixInteractionCodexUserItemStillRequiresOwnedMainThread(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999996004"
	at := time.Now().Add(-time.Hour).UTC()
	user := matrixCodexUserItem(at.Format(time.RFC3339Nano), sid, `[{"type":"text","text":"fixture prompt"}]`)
	for _, head := range []string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"thread_source":"subagent","parent_thread_id":"fixture-parent"}}`, sid),
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"thread_source":"user","parent_thread_id":"fixture-parent"}}`, sid),
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`, sid),
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":"foreign","thread_source":"user"}}`, sid),
	} {
		got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, head, user))
		if !got.interactionAt.IsZero() {
			t.Fatalf("unverified main-thread metadata accepted interaction: %+v", got.meta)
		}
	}
	home := t.TempDir()
	const foreignID = "019f0000-0000-7000-8000-999999996099"
	foreignUser := matrixCodexUserItem(at.Format(time.RFC3339Nano), foreignID, `[{"type":"text","text":"fixture prompt"}]`)
	matrixSeedInterruptCacheRollout(t, home, sid, matrixInteractionCodexMeta(foreignID), foreignUser)
	inst := matrixInterruptCacheInstance(home, sid)
	inst.matrixRefreshTurnInterruptWithCache(newMatrixTurnInterruptFileCache(4))
	if !inst.LastInteractionAt().IsZero() {
		t.Fatal("filename owner accepted a foreign native header")
	}
}

func TestMatrixInteractionCodexUserItemKeepsLegacyUserEvents(t *testing.T) {
	const sid = "019f0000-0000-7000-8000-999999996005"
	at := time.Now().Add(-time.Hour).UTC()
	legacy := matrixInteractionCodexRecord(at.Format(time.RFC3339Nano))
	item := matrixCodexUserItem(at.Format(time.RFC3339Nano), sid, `[{"type":"text","text":"same submitted prompt"}]`)
	for _, body := range [][]string{{legacy}, {legacy, item}, {item, legacy}} {
		lines := append([]string{matrixInteractionCodexMeta(sid)}, body...)
		got := matrixParseTurnInterruptFile("codex", matrixWriteTranscript(t, lines...))
		if !got.interactionAt.Equal(at) {
			t.Fatalf("legacy or duplicate event changed original submission time: got %v, want %v", got.interactionAt, at)
		}
	}
}
