package statedb

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// matrix/hardened: sleep mutations preserve unrelated concurrent edits and CAS
// the native identity/marker rather than overwriting whole registry snapshots.
func TestMatrixAutoSleepCAS(t *testing.T) {
	db := newTestDB(t)
	row := &InstanceRow{ID: "sleep", Tool: "codex", Command: "codex", ProjectPath: "/tmp", Status: "waiting", CreatedAt: time.Now(), ToolData: json.RawMessage(`{"codex_session_id":"original","notes":"before"}`)}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE instances SET title='concurrent',tool_data=json_set(tool_data,'$.notes','after') WHERE id='sleep'`); err != nil {
		t.Fatal(err)
	}
	pending := json.RawMessage(`{"version":1,"phase":"pending","codex_session_id":"original"}`)
	if err := db.CompareAndSwapAutoSleep(row, nil, pending, ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadInstanceByID("sleep")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "concurrent" || got.Status != "waiting" {
		t.Fatalf("unrelated fields clobbered: %+v", got)
	}
	var td map[string]json.RawMessage
	_ = json.Unmarshal(got.ToolData, &td)
	if string(td["notes"]) != `"after"` {
		t.Fatalf("notes clobbered: %s", got.ToolData)
	}
	asleep := json.RawMessage(`{"version":1,"phase":"asleep","codex_session_id":"original"}`)
	if err = db.CompareAndSwapAutoSleep(row, nil, asleep, "stopped"); !errors.Is(err, ErrAutoSleepConflict) {
		t.Fatalf("stale marker must fail: %v", err)
	}
	if err = db.CompareAndSwapAutoSleep(row, pending, asleep, "stopped"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.LoadInstanceByID("sleep")
	if got.Status != "stopped" {
		t.Fatal(got.Status)
	}
	if _, err = db.DB().Exec(`UPDATE instances SET tool_data=json_set(tool_data,'$.codex_session_id','different') WHERE id='sleep'`); err != nil {
		t.Fatal(err)
	}
	if err = db.CompareAndSwapAutoSleep(row, asleep, nil, "waiting"); !errors.Is(err, ErrAutoSleepConflict) {
		t.Fatalf("changed native ID must fail: %v", err)
	}
}

func TestMatrixAutoSleepCASClearAndWakeAccess(t *testing.T) {
	db := newTestDB(t)
	asleep := json.RawMessage(`{"version":1,"phase":"asleep"}`)
	row := &InstanceRow{ID: "sleep", Tool: "codex", Command: "codex", Status: "stopped", CreatedAt: time.Now(), ToolData: json.RawMessage(`{"codex_session_id":"original","matrix_auto_sleep":{"version":1,"phase":"asleep"}}`)}
	if err := db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	if err := db.CompareAndSwapAutoSleep(row, asleep, nil, "waiting"); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadInstanceByID("sleep")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting" || time.Since(got.LastAccessed) > 2*time.Second {
		t.Fatalf("wake time/status missing: %+v", got)
	}
	var td map[string]json.RawMessage
	_ = json.Unmarshal(got.ToolData, &td)
	if string(td["matrix_auto_sleep"]) != "null" {
		t.Fatalf("marker not explicitly cleared: %s", got.ToolData)
	}
}
