// matrix/hardened: temporarily bypass an IME while the TUI owns navigation keys.
package inputmethod

import (
	"sync"
	"time"
)

// retryFloor keeps a failing attempt from running native calls on every frame.
// It is shorter than the UI's retry tick so scheduled retries always execute.
const retryFloor = 100 * time.Millisecond

// TextSourceAuto resolves the text input method from what the operator uses:
// the remembered one, otherwise the only enabled non-ASCII input method.
const TextSourceAuto = "auto"

type backend interface {
	foreground() bool
	current() string
	ascii() string
	selectSource(string) bool
	// textSources lists enabled, selectable, non-ASCII keyboard input methods.
	textSources() []string
	// owner is the pid of the terminal application this backend is scoped to.
	// Ownership records are only honoured within the same application.
	owner() int
}

// Controller belongs to one foreground terminal, and is used on the UI thread.
// It never guesses which Chinese input method the operator prefers.
//
// matrix/hardened: the controller holds a desired state instead of assuming a
// single attempt succeeded. An attempt that cannot run yet (the terminal app is
// not foreground yet after a window switch, or the native selection is refused)
// leaves the desire pending so the caller keeps retrying. Reconciliation can
// never depend on a keystroke: in navigation mode the IME swallows the first
// key for composition, so that key never reaches the TUI.
type Controller struct {
	mu      sync.Mutex // signal cleanup can race the last UI update
	backend backend
	saved   string
	forced  string
	blurred bool
	// want is the desired navigation state; known records that a desire exists.
	want    bool
	known   bool
	pending bool      // the last attempt did not reach the desired state
	lastTry time.Time // only consulted while pending
	clock   func() time.Time
	// textPolicy is empty (hand back only what we took), TextSourceAuto, or an
	// explicit input source id to select whenever text input gets the focus.
	textPolicy string
}

// New returns nil on unsupported builds, SSH connections, and detached
// processes. textPolicy selects the text-input policy: empty restores only the
// source this controller took away, while TextSourceAuto or an explicit input
// source id also selects one when there is nothing of ours to hand back.
func New(textPolicy string) *Controller {
	b := newBackend()
	if b == nil {
		return nil
	}
	return &Controller{backend: b, textPolicy: textPolicy}
}

func (c *Controller) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// Sync records the desired mode and reconciles the native input source with it:
// it saves the current source before navigation, then restores it for typing.
// A source the user selected manually takes precedence over a pending restore.
// It reports whether the desired mode is in effect; false means the caller
// should call again (the terminal app may still be activating).
func (c *Controller) Sync(navigation, reassert bool) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blurred = false
	changed := !c.known || c.want != navigation
	c.want, c.known = navigation, true
	if !changed && c.pending && c.now().Sub(c.lastTry) < retryFloor {
		trace("sync want=%v throttled", navigation)
		return false
	}
	// Text input is applied when it gains the focus, not continuously: an
	// operator who picks a different source while typing keeps it. Navigation
	// keeps verifying, because those keys belong to agent-deck itself.
	if !navigation && !changed && !c.pending && !reassert {
		return true
	}
	c.lastTry = c.now()
	applied := c.apply()
	c.pending = !applied
	trace("sync want=%v changed=%v applied=%v saved=%q forced=%q",
		navigation, changed, applied, c.saved, c.forced)
	return applied
}

// Restore only changes a source this controller actually selected, and only
// while its own terminal app is foreground. Failed restores remain retryable.
func (c *Controller) Restore() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Exit and signal cleanup hand the operator's IME back for good: drop the
	// navigation desire so a late UI message cannot re-acquire ASCII.
	// Quitting hands back only what agent-deck took: it must not select a text
	// input method on the way out.
	c.want, c.known, c.pending = false, true, !c.restore(false)
	trace("restore saved=%q pending=%v", c.saved, c.pending)
}

// Blur prevents cleanup in a background tab from changing a sibling tab's IME.
// The pending source is retained for the next focused Sync call.
func (c *Controller) Blur() {
	if c != nil {
		c.mu.Lock()
		c.blurred = true
		trace("blur saved=%q forced=%q", c.saved, c.forced)
		c.mu.Unlock()
	}
}

// apply drives the native source toward the recorded desire. It reports whether
// the desire is in effect, so a refused or deferred attempt stays retryable.
func (c *Controller) apply() bool {
	if c.want {
		return c.acquire()
	}
	return c.restore(true)
}

// acquire bypasses the IME for navigation keys. An operator who is already on
// an ASCII keyboard keeps it, and nothing is saved for a later restore.
func (c *Controller) acquire() bool {
	if !c.backend.foreground() {
		trace("acquire deferred: terminal app not foreground")
		return false
	}
	current := c.backend.current()
	if current == "" {
		return false
	}
	if c.forced != "" && current == c.forced {
		return true
	}
	// A changed source can be a manual choice or a different app's preference.
	if c.saved != "" || c.forced != "" {
		c.release()
	}
	ascii := c.backend.ascii()
	if ascii == "" {
		return false
	}
	if current == ascii {
		return true
	}
	if !c.backend.selectSource(ascii) {
		trace("acquire refused: select %q", ascii)
		return false
	}
	c.saved, c.forced = current, ascii
	// The source we just took away is the one the operator uses for text.
	writePreference(current)
	// Publish the pending restore so a sibling tab, or the next run after this
	// process is killed while blurred, can still hand this source back.
	writeOwnership(ownership{
		Forced: ascii, Saved: current,
		App: c.backend.owner(), PID: processID(), At: time.Now(),
	})
	return true
}

// restore hands the operator's own input source back. Having nothing of our own
// to restore makes us look for a record another instance left behind; a blurred
// tab defers to its sibling.
func (c *Controller) restore(policy bool) bool {
	if c.saved == "" {
		return c.restoreInherited(policy)
	}
	if c.blurred {
		trace("restore deferred: blurred tab keeps %q pending", c.saved)
		return false
	}
	if !c.backend.foreground() {
		trace("restore deferred: terminal app not foreground")
		return false
	}
	current := c.backend.current()
	if current == "" {
		return false
	}
	if current != c.forced {
		// The operator chose something else; that choice wins over our pending
		// restore, and ownership is released.
		c.release()
		return true
	}
	if !c.backend.selectSource(c.saved) {
		trace("restore refused: select %q", c.saved)
		return false
	}
	c.release()
	return true
}

// applyTextSource selects the operator's text input method when text input has
// the focus and nothing of ours is pending. It only acts while the desktop sits
// on an ASCII keyboard: an input method that is already active is left alone,
// and so is a source the operator chose while typing.
func (c *Controller) applyTextSource(policy bool) bool {
	if !policy || c.textPolicy == "" {
		return true
	}
	if !c.backend.foreground() {
		trace("text source deferred: terminal app not foreground")
		return false
	}
	current := c.backend.current()
	if current == "" {
		return false
	}
	if current != c.backend.ascii() {
		// An input method is already active; this is what the operator wants.
		writePreference(current)
		return true
	}
	target := c.resolveTextSource()
	if target == "" || target == current {
		return true
	}
	if !c.backend.selectSource(target) {
		trace("text source refused: select %q", target)
		return false
	}
	trace("text source selected: %q", target)
	return true
}

// resolveTextSource never guesses between several input methods: it takes the
// configured one, then the one the operator was last seen using, then the only
// enabled candidate.
func (c *Controller) resolveTextSource() string {
	if c.textPolicy != TextSourceAuto {
		return c.textPolicy
	}
	candidates := c.backend.textSources()
	if learned := readPreference(); learned != "" {
		for _, candidate := range candidates {
			if candidate == learned {
				return learned
			}
		}
		trace("remembered text input method %q is no longer enabled", learned)
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	trace("text source undecided: %d enabled input methods", len(candidates))
	return ""
}

// release drops our ownership, including the shared record when it is ours.
func (c *Controller) release() {
	c.saved, c.forced = "", ""
	clearOwnership(processID())
}

// restoreInherited finishes a restore that another instance of the same
// terminal app started: a sibling tab that is blurred, or an instance that
// exited or was killed while it still held the ASCII source. It only acts while
// the desktop is still on exactly that forced source, so an ASCII keyboard the
// operator selected themselves is never overridden.
func (c *Controller) restoreInherited(policy bool) bool {
	record := readOwnership()
	if record == nil {
		return c.applyTextSource(policy)
	}
	if record.PID == processID() {
		// Our own stale record: we already released ownership above.
		clearOwnership(processID())
		return true
	}
	if record.App != c.backend.owner() {
		// A different terminal application, or a session from before the app
		// restarted. Its record says nothing about this desktop state.
		return c.applyTextSource(policy)
	}
	if c.blurred || !c.backend.foreground() {
		return false
	}
	current := c.backend.current()
	if current == "" {
		return false
	}
	if current != record.Forced {
		// The desktop already moved on; nothing of ours is in effect.
		clearOwnership(record.PID)
		return c.applyTextSource(policy)
	}
	if !c.backend.selectSource(record.Saved) {
		trace("inherited restore refused: select %q", record.Saved)
		return false
	}
	trace("inherited restore: %q from pid=%d", record.Saved, record.PID)
	clearOwnership(record.PID)
	return true
}
