package main

import (
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: keep unknown visits absent for consumers of the visit clock.
func matrixListAccessedAt(inst *session.Instance) string {
	if inst.LastAccessedAt.IsZero() {
		return ""
	}
	return inst.LastAccessedAt.Format(time.RFC3339Nano)
}

// matrix/hardened: Recent interactions use an independent clock; visits,
// creation and background output cannot fill an unknown submission time.
func matrixListInteractionAt(inst *session.Instance) string {
	if inst == nil {
		return ""
	}
	at := inst.LastInteractionAt()
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}
