package core

// matrix/hardened: the core/remote-agent and CLI JSON transports must expose
// the same independent clock without inventing it from viewing or output.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestMatrixInteractionStaticSessionWire(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 20, 30, 987654321, time.UTC)
	inst := &session.Instance{ID: "interacted", LastAccessedAt: at.Add(time.Hour)}
	if err := inst.RecordInteractionAt(at); err != nil {
		t.Fatal(err)
	}
	row := staticSessionRow(inst, []*session.Instance{inst}, "fixture")
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var remote session.RemoteSessionInfo
	if err := json.Unmarshal(data, &remote); err != nil {
		t.Fatal(err)
	}
	if remote.LastInteractionAt != at.Format(time.RFC3339Nano) || remote.LastAccessedAt != at.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("core transport mixed visit and interaction: %s", data)
	}
	viewed := &session.Instance{ID: "viewed", LastAccessedAt: at}
	if got := staticSessionRow(viewed, []*session.Instance{viewed}, "fixture").LastInteractionAt; got != "" {
		t.Fatalf("view-only core row acquired interaction %q", got)
	}
}
