package session

// matrix/hardened: a cache miss reads metadata and user events separately;
// changing the file during those reads cannot establish a new interaction.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatrixInteractionClockRejectsChangingTranscript(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			body := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"fixture-native\",\"thread_source\":\"user\"}}\n{\"type\":\"event_msg\",\"timestamp\":%q,\"payload\":{\"type\":\"user_message\",\"message\":\"fixture request\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"reason\":\"fixture interruption\"}}\n", at)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cache := newMatrixTurnInterruptFileCache(1)
			cache.parse = func(tool, file string) matrixTurnInterruptFileResult {
				result := matrixParseTurnInterruptFile(tool, file)
				if result.interactionAt.IsZero() || !result.found {
					t.Fatal("fixture did not provide both native user and interruption facts")
				}
				if replace {
					next := file + ".replacement"
					if err := os.WriteFile(next, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(next, file); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(file, []byte(body+"{\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\"}}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return result
			}
			result := cache.load("codex", path)
			if !result.interactionAt.IsZero() || result.interactionID != "" {
				t.Fatal("a changing transcript published an interaction with unverified ownership")
			}
			if !result.found || result.verdict.Kind != "turn_aborted" {
				t.Fatal("interaction guard changed the legacy interruption result")
			}
		})
	}
}
