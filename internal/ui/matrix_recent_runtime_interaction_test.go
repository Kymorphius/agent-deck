package ui

// matrix/hardened: prompt-only observations must refresh Recent even when a
// complete fast turn leaves the same status at both monitor sample times.

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestMatrixRecentRuntimeInteractionOnlyChangeIsPublished(t *testing.T) {
	inst := session.NewInstanceWithGroupAndTool("conversation", "/fixture", "work", "codex")
	inst.Status = session.StatusIdle
	source := &matrixRuntimeUISource{}
	home := matrixRuntimeUIHome(t, source, inst)
	defer home.cancel()
	observation := matrixRuntimeUIObservation(inst, 1, session.StatusIdle)
	home.applyRuntimeObservation(observation, false)
	observation = matrixRuntimeUIObservation(inst, 2, session.StatusIdle)
	if home.applyRuntimeObservation(observation, false) {
		t.Fatal("an unchanged status with no interaction fabricated a menu update")
	}
	observation = matrixRuntimeUIObservation(inst, 3, session.StatusIdle)
	interaction := time.Now().Add(-time.Second)
	observation.Sessions[0].LastInteractionAt = interaction
	if !home.applyRuntimeObservation(observation, false) {
		t.Fatal("a new actual interaction was hidden by unchanged status")
	}
	if !inst.LastInteractionAt().Equal(interaction) {
		t.Fatal("runtime interaction was not applied to the selected session")
	}
	if home.applyRuntimeObservation(observation, false) {
		t.Fatal("replaying the same interaction produced another menu change")
	}
}
