//go:build !windows

package web

import (
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// matrix/hardened: real PTY readback proves the first input fragment remains
// queued while cold resume waits, and following fragments retain their order.
func TestMatrixSleepingWSFirstInputSurvivesWake(t *testing.T) {
	requireTmuxForWebIntegration(t)
	name := fmt.Sprintf("agentdeck_sleep_ws_%d", time.Now().UnixNano())
	if output, err := exec.Command("tmux", "new-session", "-d", "-s", name).CombinedOutput(); err != nil {
		t.Skipf("tmux unavailable: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
	var calls atomic.Int32
	_, conn := matrixSleepingWSServer(t, false, &matrixSleepingMutator{wake: func(string) (string, string, bool, error) {
		calls.Add(1)
		time.Sleep(150 * time.Millisecond)
		return name, "", true, nil
	}})
	for _, data := range []string{"printf 'FIRST_FRAGMENT_", "PRESERVED\\n'\r"} {
		if err := conn.WriteJSON(wsClientMessage{Type: "input", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readBinaryUntilContains(conn, "FIRST_FRAGMENT_PRESERVED", 5*time.Second)
	if err != nil || !strings.Contains(got, "FIRST_FRAGMENT_PRESERVED") {
		t.Fatalf("first fragmented input missing after wake: %v: %q", err, got)
	}
	if calls.Load() != 1 {
		t.Fatalf("each fragment started a new wake: %d", calls.Load())
	}
}
