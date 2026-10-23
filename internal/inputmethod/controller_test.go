package inputmethod

// matrix/hardened: exercise ownership/restoration without changing a live IME.
import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type fakeBackend struct {
	active bool
	source string
	deny   bool
	calls  []string
}

// newTestController advances its own clock on every call so the retry floor,
// which only throttles repeated failing attempts, never hides a real attempt.
// It also keeps the shared ownership record inside the test's own directory.
func newTestController(t *testing.T, b backend) *Controller {
	isolateOwnership(t)
	now := time.Unix(0, 0)
	return &Controller{backend: b, clock: func() time.Time {
		now = now.Add(retryFloor)
		return now
	}}
}

// isolateOwnership redirects the cross-instance record to a temporary file, so
// tests never read or write the real data directory.
func isolateOwnership(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "input-method-owner.json")
	previous := ownershipPath
	ownershipPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { ownershipPath = previous })
	return path
}

func (b *fakeBackend) foreground() bool { return b.active }
func (b *fakeBackend) owner() int       { return 4242 }
func (b *fakeBackend) current() string  { return b.source }
func (b *fakeBackend) ascii() string    { return "ABC" }
func (b *fakeBackend) selectSource(id string) bool {
	b.calls = append(b.calls, id)
	if b.deny {
		return false
	}
	b.source = id
	return true
}

func TestInputMethodNavigationAndRestore(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime"}
	c := newTestController(t, b)
	if !c.Sync(true) {
		t.Fatal("foreground navigation sync was not applied")
	}
	if !c.Sync(true) {
		t.Fatal("re-syncing an owned ASCII source must report applied")
	}
	if b.source != "ABC" {
		t.Fatal("navigation must bypass the IME")
	}
	c.Sync(false)
	c.Restore()
	if b.source != "Rime" || !reflect.DeepEqual(b.calls, []string{"ABC", "Rime"}) {
		t.Fatalf("source=%q selections=%v", b.source, b.calls)
	}
}

func TestInputMethodForegroundAndManualChoice(t *testing.T) {
	b := &fakeBackend{source: "Rime"}
	c := newTestController(t, b)
	if c.Sync(true) {
		t.Fatal("a background terminal must report the switch as not applied")
	}
	if len(b.calls) != 0 {
		t.Fatal("background terminal changed the source")
	}
	b.active = true
	c.Sync(true)
	b.active = false
	c.Restore()
	if b.source != "ABC" || len(b.calls) != 1 {
		t.Fatal("background cleanup changed another app")
	}
	b.source, b.active = "OtherIME", true
	c.Sync(false)
	if b.source != "OtherIME" || len(b.calls) != 1 {
		t.Fatal("restore overwrote a manual input-source selection")
	}
	c.Sync(true)
	c.Sync(false)
	if b.source != "OtherIME" {
		t.Fatal("new navigation cycle did not remember the new preference")
	}
}

func TestInputMethodSelectionFailure(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime", deny: true}
	c := newTestController(t, b)
	c.Sync(true)
	if c.saved != "" {
		t.Fatal("failed selection must not acquire ownership")
	}
	b.deny = false
	c.Sync(true)
	b.deny = true
	c.Restore()
	if c.saved != "Rime" {
		t.Fatal("failed restoration must remain retryable")
	}
	b.deny = false
	c.Restore()
	if b.source != "Rime" || c.saved != "" {
		t.Fatal("retry did not restore the source")
	}
}

func TestInputMethodASCIIAndNil(t *testing.T) {
	b := &fakeBackend{active: true, source: "ABC"}
	c := newTestController(t, b)
	c.Sync(true)
	c.Sync(false)
	if len(b.calls) != 0 {
		t.Fatal("an ASCII user should not be forced into a different input method")
	}
	var disabled *Controller
	disabled.Sync(true)
	disabled.Restore()
	disabled.Blur()
}

func TestInputMethodBlurredSiblingTab(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime"}
	c := newTestController(t, b)
	c.Sync(true)
	c.Blur()
	c.Restore()
	if b.source != "ABC" || len(b.calls) != 1 {
		t.Fatal("background-tab cleanup overwrote the sibling tab's source")
	}
	c.Sync(false)
	if b.source != "Rime" {
		t.Fatal("pending source was not restored on focused typing")
	}
}

// matrix/hardened: an attempt that could not run must stay pending. A window
// switch can deliver the terminal's focus report before the app counts as
// foreground, and in navigation mode no keystroke follows to trigger a retry:
// the IME consumes the first key for composition.
func TestInputMethodPendingAttemptIsRetryable(t *testing.T) {
	b := &fakeBackend{source: "Rime"}
	c := newTestController(t, b)
	if c.Sync(true) {
		t.Fatal("navigation was reported as applied while not foreground")
	}
	if c.Sync(true) {
		t.Fatal("repeating the same desire hid that it is still unapplied")
	}
	b.active = true
	if !c.Sync(true) {
		t.Fatal("retry after activation did not acquire the ASCII source")
	}
	if b.source != "ABC" || !reflect.DeepEqual(b.calls, []string{"ABC"}) {
		t.Fatalf("source=%q selections=%v", b.source, b.calls)
	}
	// The same contract holds for a refused restore.
	b.deny = true
	if c.Sync(false) {
		t.Fatal("refused restore was reported as applied")
	}
	b.deny = false
	if !c.Sync(false) {
		t.Fatal("restore retry did not report success")
	}
	if b.source != "Rime" {
		t.Fatalf("source=%q after restore retry", b.source)
	}
}

// The retry floor only throttles a repeated failing attempt, and never a
// changed desire.
func TestInputMethodRetryFloor(t *testing.T) {
	b := &fakeBackend{source: "Rime"}
	isolateOwnership(t)
	frozen := time.Unix(0, 0)
	c := &Controller{backend: b, clock: func() time.Time { return frozen }}
	c.Sync(true)
	b.active = true
	c.Sync(true) // throttled: no native work within the floor
	if len(b.calls) != 0 {
		t.Fatalf("throttled retry ran native selection: %v", b.calls)
	}
	frozen = frozen.Add(retryFloor)
	if !c.Sync(true) {
		t.Fatal("retry after the floor did not run")
	}
	if b.source != "ABC" {
		t.Fatalf("source=%q", b.source)
	}
	// A desire change is immediate even inside the floor.
	b.deny = true
	if c.Sync(false) || len(b.calls) != 2 {
		t.Fatalf("desire change was throttled: calls=%v", b.calls)
	}
}

// A blurred tab keeps its pending restore, and reports it as unapplied so the
// owning instance retries once it is focused again.
func TestInputMethodBlurKeepsRestorePending(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime"}
	c := newTestController(t, b)
	c.Sync(true)
	c.Blur()
	if !c.Sync(false) || b.source != "Rime" {
		t.Fatalf("focused typing after blur did not restore: %q", b.source)
	}
}

// matrix/hardened: ownership of a forced ASCII source has to survive the
// instance that took it. A sibling tab, or the next run after a tab was killed
// while blurred, used to find nothing to restore and left the operator typing
// in ASCII.
func TestInputMethodInheritsSiblingRestore(t *testing.T) {
	path := isolateOwnership(t)
	defer func(previous func() int) { processID = previous }(processID)

	b := &fakeBackend{active: true, source: "Rime"}
	processID = func() int { return 111 }
	owner := &Controller{backend: b}
	if !owner.Sync(true) || b.source != "ABC" {
		t.Fatalf("owner did not take the ASCII source: %q", b.source)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ownership record was not published: %v", err)
	}
	// The owning tab goes to the background and dies there: its restore is
	// deliberately deferred, so the record must outlive it.
	owner.Blur()
	owner.Restore()
	if b.source != "ABC" {
		t.Fatal("a blurred tab restored over a sibling's source")
	}

	processID = func() int { return 222 }
	sibling := &Controller{backend: b}
	if !sibling.Sync(false) {
		t.Fatal("sibling could not finish the inherited restore")
	}
	if b.source != "Rime" {
		t.Fatalf("inherited restore did not hand the IME back: %q", b.source)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a consumed ownership record was left behind")
	}
	// Nothing is left to inherit, and a second pass must stay quiet.
	calls := len(b.calls)
	if !sibling.Sync(false) || len(b.calls) != calls {
		t.Fatalf("a consumed record was replayed: %v", b.calls)
	}
}

// An ASCII keyboard the operator selected themselves is never overridden, and a
// record from another terminal application is ignored.
func TestInputMethodInheritedRestoreLimits(t *testing.T) {
	path := isolateOwnership(t)
	defer func(previous func() int) { processID = previous }(processID)
	processID = func() int { return 333 }

	b := &fakeBackend{active: true, source: "ABC"}
	c := &Controller{backend: b}
	if !c.Sync(false) || len(b.calls) != 0 {
		t.Fatalf("an operator's own ASCII keyboard was changed: %v", b.calls)
	}

	// A record from a different terminal app must not touch this desktop.
	writeOwnership(ownership{Forced: "ABC", Saved: "Rime", App: 9999, PID: 111, At: time.Now()})
	if !c.Sync(false) || len(b.calls) != 0 {
		t.Fatalf("another application's record was replayed: %v", b.calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("another application's record was deleted")
	}

	// Our app, but the desktop already moved to a source we never forced.
	writeOwnership(ownership{Forced: "ABC", Saved: "Rime", App: 4242, PID: 111, At: time.Now()})
	b.source = "OtherIME"
	if !c.Sync(false) || len(b.calls) != 0 {
		t.Fatalf("a stale record changed a source we never forced: %v", b.calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a stale record was kept")
	}

	// An expired record is ignored even when everything else matches.
	writeOwnership(ownership{Forced: "ABC", Saved: "Rime", App: 4242, PID: 111,
		At: time.Now().Add(-2 * ownershipTTL)})
	b.source = "ABC"
	if !c.Sync(false) || len(b.calls) != 0 {
		t.Fatalf("an expired record was replayed: %v", b.calls)
	}
}
