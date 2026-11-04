package statedb

// matrix/hardened: an actual-input clock survives independently loaded writers,
// old binaries omitting the extra, and snapshot reconciliation.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func interactionClockData(at time.Time) json.RawMessage {
	data, _ := json.Marshal(map[string]any{interactionClockKey: at.UnixNano(), "custom": "keep"})
	return data
}

func persistedInteractionClock(t *testing.T, db *StateDB, id string) int64 {
	t.Helper()
	row, err := db.LoadInstanceByID(id)
	require.NoError(t, err)
	require.NotNil(t, row)
	var values map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(row.ToolData, &values))
	return interactionClockStamp(values[interactionClockKey])
}

func TestMatrixInteractionClockTargetedWritesAreMonotonicAcrossConnections(t *testing.T) {
	db := newTestDB(t)
	accessed := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, db.SaveInstance(&InstanceRow{ID: "clock", Title: "kept", Status: "idle", LastAccessed: accessed, ToolData: json.RawMessage(`{"notes":"unchanged","custom":{"counter":7}}`)}))
	peer, err := Open(db.path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	at := time.Now().Add(-time.Minute)
	require.NoError(t, db.WriteLastInteractionAt("clock", at))
	require.NoError(t, peer.WriteLastInteractionAt("clock", at.Add(time.Nanosecond)))
	require.NoError(t, db.WriteLastInteractionAt("clock", at.Add(-time.Second)))
	require.Equal(t, at.Add(time.Nanosecond).UnixNano(), persistedInteractionClock(t, peer, "clock"))
	row, err := db.LoadInstanceByID("clock")
	require.NoError(t, err)
	require.Equal(t, "kept", row.Title)
	require.Equal(t, accessed.Unix(), row.LastAccessed.Unix())
	var values map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(row.ToolData, &values))
	require.JSONEq(t, `"unchanged"`, string(values["notes"]))
	require.JSONEq(t, `{"counter":7}`, string(values["custom"]))
}

func TestMatrixInteractionClockStaleFullSavesKeepLatest(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "bulk"}[bulk], func(t *testing.T) {
			db := newTestDB(t)
			old := time.Now().Add(-time.Hour)
			seed := &InstanceRow{ID: "clock", Title: "initial", ToolData: interactionClockData(old)}
			require.NoError(t, db.SaveInstance(seed))
			stale := CloneInstanceRow(seed)
			latest := old.Add(time.Minute)
			require.NoError(t, db.WriteLastInteractionAt(seed.ID, latest))
			stale.Title, stale.ToolData = "renamed", interactionClockData(old.Add(time.Second))
			if bulk {
				require.NoError(t, db.SaveInstances([]*InstanceRow{stale}))
			} else {
				require.NoError(t, db.SaveInstance(stale))
			}
			require.Equal(t, latest.UnixNano(), persistedInteractionClock(t, db, seed.ID))
			// A legacy writer knows no timestamp key at all.
			stale.ToolData = json.RawMessage(`{"custom":"keep"}`)
			require.NoError(t, db.SaveInstance(stale))
			require.Equal(t, latest.UnixNano(), persistedInteractionClock(t, db, seed.ID))
		})
	}
}

func TestMatrixInteractionClockSnapshotMergeKeepsMaximum(t *testing.T) {
	for _, desiredNewer := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed newer", true: "desired newer"}[desiredNewer], func(t *testing.T) {
			db := newTestDB(t)
			old := time.Now().Add(-time.Hour)
			require.NoError(t, db.SaveInstance(&InstanceRow{ID: "clock", Title: "initial", ToolData: interactionClockData(old)}))
			base, err := db.LoadInstanceByID("clock")
			require.NoError(t, err)
			committed, proposed := old.Add(2*time.Minute), old.Add(time.Minute)
			if desiredNewer {
				proposed = old.Add(3 * time.Minute)
			}
			require.NoError(t, db.WriteLastInteractionAt("clock", committed))
			desired := CloneInstanceRow(base)
			desired.Title, desired.ToolData = "renamed", interactionClockData(proposed)
			rows, err := db.MergeInstanceSnapshots([]InstanceSnapshot{{Original: base, Stored: base, Desired: desired}}, nil)
			require.NoError(t, err)
			require.Equal(t, "renamed", rows[0].Title)
			want := committed
			if desiredNewer {
				want = proposed
			}
			require.Equal(t, want.UnixNano(), persistedInteractionClock(t, db, "clock"))
		})
	}
}

func TestMatrixInteractionClockIgnoresUnknownAndFutureEvidence(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.SaveInstance(&InstanceRow{ID: "legacy", LastAccessed: time.Now(), ToolData: json.RawMessage(`{}`)}))
	for _, at := range []time.Time{{}, time.Unix(-1, 0), time.Now().Add(time.Hour)} {
		require.NoError(t, db.WriteLastInteractionAt("legacy", at))
	}
	require.Zero(t, persistedInteractionClock(t, db, "legacy"))
}

func TestMatrixInteractionClockSurvivesTargetedWriteAfterFullSaveRead(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "bulk"}[bulk], func(t *testing.T) {
			db := newTestDB(t)
			old := time.Now().Add(-time.Hour)
			stale := &InstanceRow{ID: "clock-gap", Title: "before", ToolData: interactionClockData(old)}
			require.NoError(t, db.SaveInstance(stale))
			peer, err := Open(db.path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = peer.Close() })
			latest := old.Add(time.Minute)
			called := false
			probe := interactionClockSaveProbe(func(source *StateDB, id string) {
				if source == db && id == stale.ID {
					called = true
					require.NoError(t, peer.WriteLastInteractionAt(id, latest))
				}
			})
			interactionClockFullSaveAfterRead.Store(&probe)
			t.Cleanup(func() { interactionClockFullSaveAfterRead.Store(nil) })
			stale.Title = "after"
			if bulk {
				require.NoError(t, db.SaveInstances([]*InstanceRow{stale}))
			} else {
				require.NoError(t, db.SaveInstance(stale))
			}
			require.True(t, called, "fixture must exercise the exact pre-read/INSERT gap")
			require.Equal(t, latest.UnixNano(), persistedInteractionClock(t, db, stale.ID))
		})
	}
}
