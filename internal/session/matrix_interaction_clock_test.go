package session

// matrix/hardened: only confirmed input advances this independent clock.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMatrixInteractionClockStorageAndLiteRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage := newTestStorage(t)
	inst := NewInstanceWithTool("interaction", "/fixture", "shell")
	inst.LastAccessedAt = time.Now().Add(-time.Hour)
	require.NoError(t, storage.SaveWithGroups([]*Instance{inst}, nil))
	at := time.Now().Add(-time.Minute).UTC()
	require.NoError(t, inst.RecordInteractionAt(at))
	inst.MarkAccessed()
	require.Equal(t, at, inst.LastInteractionAt())
	require.NoError(t, inst.RecordInteractionAt(at.Add(-time.Second)))
	require.NoError(t, storage.SaveWithGroups([]*Instance{inst}, nil))
	loaded, _, err := storage.LoadWithGroups()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, at, loaded[0].LastInteractionAt())
	lite, _, err := storage.LoadLite()
	require.NoError(t, err)
	require.Len(t, lite, 1)
	require.Equal(t, at, lite[0].LastInteractionAt)
	raw, err := json.Marshal(lite[0])
	require.NoError(t, err)
	var wire struct {
		LastInteractionAt time.Time `json:"last_interaction_at"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, at, wire.LastInteractionAt)
}

func TestMatrixInteractionClockDoesNotInferVisitsAndRejectsBadTime(t *testing.T) {
	i := &Instance{LastAccessedAt: time.Now(), lastActivityAt: time.Now()}
	require.True(t, i.LastInteractionAt().IsZero())
	for _, at := range []time.Time{{}, time.Unix(-1, 0), time.Now().Add(time.Hour)} {
		require.NoError(t, i.RecordInteractionAt(at))
	}
	require.True(t, i.LastInteractionAt().IsZero())
	for _, raw := range []string{`{}`, `{"last_interaction_at":0}`, `{"last_interaction_at":"invalid"}`, `null`} {
		require.True(t, ReadLastInteractionAtFromToolData(json.RawMessage(raw)).IsZero(), raw)
	}
}

func TestMatrixInteractionClockOwningProfileAndStaleSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	owner, other := newTestStorage(t), newTestStorage(t)
	first := NewInstanceWithTool("owned", "/fixture", "shell")
	require.NoError(t, owner.SaveWithGroups([]*Instance{first}, nil))
	second := &Instance{ID: first.ID, Title: "unrelated profile", Tool: "shell"}
	require.NoError(t, other.SaveWithGroups([]*Instance{second}, nil))
	stale, _, err := owner.LoadWithGroups()
	require.NoError(t, err)
	at := time.Now().Add(-time.Second).UTC()
	require.NoError(t, first.RecordInteractionAt(at))
	stale[0].Title = "rename only"
	require.NoError(t, owner.SaveWithGroups(stale, nil))
	require.Equal(t, at, stale[0].LastInteractionAt(), "save adopts the concurrently committed maximum")
	owned, _, err := owner.LoadWithGroups()
	require.NoError(t, err)
	require.Equal(t, at, owned[0].LastInteractionAt())
	unrelated, _, err := other.LoadWithGroups()
	require.NoError(t, err)
	require.True(t, unrelated[0].LastInteractionAt().IsZero())
}

func TestMatrixInteractionClockRuntimeReplicaIsMonotonic(t *testing.T) {
	i := &Instance{ID: "replica", Tool: "shell", Command: "shell", Status: StatusIdle}
	at := time.Now().Add(-time.Minute).UTC()
	row := RuntimeSessionSnapshot{ID: i.ID, Profile: DefaultProfile, Epoch: "epoch", Revision: 1,
		Identity: i.RuntimeIdentity(), ObservedAt: time.Now(), Status: StatusIdle, LastInteractionAt: at}
	require.True(t, i.ApplyRuntimeSnapshot(DefaultProfile, row))
	require.Equal(t, at, i.LastInteractionAt())
	// A local confirmed prompt can be newer than the immutable runtime view.
	i.mu.Lock()
	i.lastInteractionAt = at.Add(time.Second)
	i.mu.Unlock()
	row.Revision++
	row.ObservedAt = time.Now()
	require.True(t, i.ApplyRuntimeSnapshot(DefaultProfile, row))
	require.Equal(t, at.Add(time.Second), i.LastInteractionAt())
	copy := &Instance{ID: i.ID, Tool: i.Tool, Command: i.Command, lastInteractionAt: at.Add(2 * time.Second)}
	require.True(t, i.ReconcileRuntimeRegistry(copy))
	require.Equal(t, at.Add(2*time.Second), i.LastInteractionAt())
}
