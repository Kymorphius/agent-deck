package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the remote list retains the visit clock with subsecond
// precision and does not invent visits from creation/background activity.
func TestMatrixRecentListWireTime(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 20, 30, 987654321, time.FixedZone("test", 8*60*60))
	instances := []*session.Instance{
		{ID: "visited", Title: "visited", LastAccessedAt: at, Status: session.StatusStopped},
		{ID: "unvisited", Title: "unvisited", CreatedAt: at, LastStartedAt: at, Status: session.StatusStopped},
	}
	cached := make(map[*session.Instance]bool)
	for _, inst := range instances {
		inst.SetTmuxSessionForTest(nil)
		cached[inst] = true
	}
	data, err := buildListJSON("_matrix_recent_wire", instances, cached)
	if err != nil {
		t.Fatal(err)
	}
	var rows []session.RemoteSessionInfo
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].LastAccessedAt != at.Format(time.RFC3339Nano) || rows[1].LastAccessedAt != "" {
		t.Fatalf("remote visit clocks lost or invented: %s", data)
	}
	var legacy session.RemoteSessionInfo
	if err := json.Unmarshal([]byte(`{"id":"legacy","last_activity_at":"2026-10-17T10:20:30Z"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.LastAccessedAt != "" {
		t.Fatal("older remote activity became a user visit")
	}
}

func TestMatrixRecentInteractionListWireTime(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 20, 30, 987654321, time.FixedZone("test", 8*60*60))
	interacted := &session.Instance{ID: "interacted", LastAccessedAt: at.Add(time.Hour), Status: session.StatusStopped}
	if err := interacted.RecordInteractionAt(at); err != nil {
		t.Fatal(err)
	}
	viewed := &session.Instance{ID: "viewed", LastAccessedAt: at.Add(2 * time.Hour), Status: session.StatusStopped}
	instances := []*session.Instance{interacted, viewed}
	cached := make(map[*session.Instance]bool)
	for _, inst := range instances {
		inst.SetTmuxSessionForTest(nil)
		cached[inst] = true
	}
	data, err := buildListJSON("_matrix_interaction_wire", instances, cached)
	if err != nil {
		t.Fatal(err)
	}
	var rows []session.RemoteSessionInfo
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].LastInteractionAt != at.UTC().Format(time.RFC3339Nano) || rows[1].LastInteractionAt != "" {
		t.Fatalf("remote interaction clocks lost or invented from a visit: %s", data)
	}
	if rows[0].LastAccessedAt == rows[0].LastInteractionAt {
		t.Fatal("visit replaced independent interaction clock")
	}
	var legacy session.RemoteSessionInfo
	if err := json.Unmarshal([]byte(`{"id":"legacy","last_accessed_at":"2026-10-17T10:20:30Z","last_activity_at":"2026-10-17T10:21:00Z"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.LastInteractionAt != "" {
		t.Fatal("old remote visits or output became a confirmed interaction")
	}
}

func TestMatrixInteractionChangeInvalidatesRemotePushHash(t *testing.T) {
	// matrix/hardened: background output remains scrubbed, but a newly
	// confirmed user submission must trigger the remote-agent changed push.
	original := `[{"id":"fixture","last_activity_at":"2026-10-17T10:20:30Z","last_interaction_at":"2026-10-17T10:00:00Z"}]`
	background := strings.Replace(original, "10:20:30Z", "10:21:30Z", 1)
	interaction := strings.Replace(original, "10:00:00Z", "10:22:00Z", 1)
	want := remoteAgentContentHash(original, "[]")
	if got := remoteAgentContentHash(background, "[]"); got != want {
		t.Fatal("background output changed the otherwise stable remote hash")
	}
	if got := remoteAgentContentHash(interaction, "[]"); got == want {
		t.Fatal("interaction-only update did not trigger remote push")
	}
}
