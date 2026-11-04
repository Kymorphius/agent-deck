package tmux

import (
	"strings"
	"time"
)

// matrix/hardened: a session that a completed probe proved absent is not
// re-probed with a forked tmux client on every status pass. The monitor polls
// every registered session, and long-dead error rows (often 100+) otherwise
// cost has-session/list-panes/batch forks many times per second.
//
// The memo only ever repeats an absence verdict. Any positive in-process
// evidence (the per-pass session cache, the per-socket cache or a live control
// pipe — exactly ExistsCached) clears it immediately, so a session revived by
// another process reappears on the next pass. Start() and a name/socket change
// also clear it. Backoff grows from 15s to 5m per consecutive absent verdict.
const (
	matrixAbsentBackoffMin = 15 * time.Second
	matrixAbsentBackoffMax = 5 * time.Minute
)

type matrixAbsentMemo struct {
	name, socket string
	until        time.Time
	backoff      time.Duration
}

func (s *Session) matrixNoteAbsent() {
	s.absentMu.Lock()
	defer s.absentMu.Unlock()
	m := &s.absent
	socket := strings.TrimSpace(s.SocketName)
	if m.name != s.Name || m.socket != socket {
		*m = matrixAbsentMemo{name: s.Name, socket: socket}
	}
	if m.backoff == 0 {
		m.backoff = matrixAbsentBackoffMin
	} else if m.backoff < matrixAbsentBackoffMax {
		m.backoff = min(2*m.backoff, matrixAbsentBackoffMax)
	}
	m.until = time.Now().Add(m.backoff)
}

// NoteAbsentFromListing records that a complete `list-sessions` on this
// session's own socket did not contain it. matrix/hardened: that listing is the
// same completed absence verdict a has-session probe would fork for, so a
// freshly loaded row (a `list --json` process, a remote-agent probe) does not
// pay one has-session per long-dead session. Callers must only pass a
// successful listing; a failed or timed-out one proves nothing.
func (s *Session) NoteAbsentFromListing() {
	if s != nil {
		s.matrixNoteAbsent()
	}
}

// NotePresentFromListing is the positive counterpart: a complete listing of
// the session's own socket contains it, so any earlier absence verdict is
// void. matrix/hardened: long-lived listers reuse rows across listings, and a
// pane revived outside the registry must not wait out the absence backoff.
func (s *Session) NotePresentFromListing() {
	if s != nil {
		s.matrixNotePresent()
	}
}

func (s *Session) matrixNotePresent() {
	s.absentMu.Lock()
	s.absent = matrixAbsentMemo{}
	s.absentMu.Unlock()
}

// KnownAbsent reports whether a recent completed probe proved this session
// absent and no in-process positive evidence has appeared since. It never
// spawns a subprocess. A false result says nothing about liveness.
func (s *Session) KnownAbsent() bool {
	if s == nil {
		return false
	}
	s.absentMu.Lock()
	m := s.absent
	s.absentMu.Unlock()
	if m.until.IsZero() || m.name != s.Name || m.socket != strings.TrimSpace(s.SocketName) || !time.Now().Before(m.until) {
		return false
	}
	if s.ExistsCached() {
		s.matrixNotePresent()
		return false
	}
	return true
}
