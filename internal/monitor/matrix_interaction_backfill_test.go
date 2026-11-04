package monitor

// matrix/hardened: stopped/archived conversations still contribute verified
// historical user input; no terminal or running process is needed to backfill it.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestMatrixInteractionBackfillForClosedConversation(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprintf("archived=%v", archived), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CODEX_HOME", root)
			session.ClearUserConfigCache()
			t.Cleanup(session.ClearUserConfigCache)
			const id = "11111111-2222-7333-8444-555555555555"
			at := time.Now().Add(-2 * time.Hour).UTC()
			path := filepath.Join(root, "sessions", "2026", "11", "04", "rollout-2026-11-04T00-00-00-"+id+".jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"source\":\"cli\",\"thread_source\":\"user\"}}\n{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"fixture question\"}}\n{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\",\"message\":\"fixture response\"}}\n", id, at.Format(time.RFC3339Nano), time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			inst := session.NewInstanceWithGroupAndTool("closed conversation", root, "fixture", "codex")
			inst.CodexSessionID, inst.Status, inst.LastAccessedAt = id, session.StatusStopped, time.Now()
			if archived {
				inst.ArchivedAt, inst.Status = time.Now(), session.StatusIdle
			}
			server := &Server{workers: make(chan struct{}, 1)}
			server.workers <- struct{}{}
			record := &observedInstance{instance: inst, done: make(chan session.RuntimeSessionSnapshot, 1)}
			server.probe(newProfile(session.DefaultProfile, "fixture-epoch"), record, nil, time.Now(), nil)
			row := <-record.done
			if !row.LastInteractionAt.Equal(at) {
				t.Fatalf("closed conversation interaction = %v, want user input %v", row.LastInteractionAt, at)
			}
			if row.Status != inst.Status {
				t.Fatal("historical backfill changed the closed conversation's status")
			}
		})
	}
}
