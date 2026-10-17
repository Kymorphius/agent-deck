package web

// matrix/hardened: optional capability on the existing authenticated mutator;
// installing a read-only menu provider never creates a new wake endpoint.
// Successful wakes have already waited for the exact resumed composer.
type sessionSleepingWaker interface {
	WakeSleepingSession(sessionID string) (tmuxName, socketName string, woke bool, err error)
}
