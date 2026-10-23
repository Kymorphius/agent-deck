// matrix/hardened: ownership of a forced ASCII source outlives the process.
//
// Ownership used to live only in one Controller. A sibling tab, a tab that was
// killed while blurred, and a reloaded TUI therefore all left the operator's
// own input method behind: the desktop stayed on the ASCII source we forced,
// and the instance the operator typed in next had nothing of its own to
// restore. This record lets any instance of the same terminal app finish a
// restore that another one started.
package inputmethod

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/agentpaths"
)

// ownershipTTL bounds how long a record is trusted. The owning app's pid is the
// real guard (it changes when the terminal app restarts, and a record from an
// earlier login must never be replayed); this only sweeps forgotten files.
const ownershipTTL = 12 * time.Hour

type ownership struct {
	// Forced is the ASCII source agent-deck selected; Saved is the operator's
	// own source that a restore has to put back.
	Forced string    `json:"forced"`
	Saved  string    `json:"saved"`
	App    int       `json:"app"` // owning terminal application pid
	PID    int       `json:"pid"` // the TUI process that forced it
	At     time.Time `json:"at"`
}

// processID is overridden in tests to stand in for a sibling instance.
var processID = os.Getpid

// ownershipPath is overridden in tests so they never touch the real data dir.
var ownershipPath = func() (string, error) {
	return agentpaths.EffectiveDataPath(filepath.Join("runtime", "input-method-owner.json"), "runtime")
}

func readOwnership() *ownership {
	path, err := ownershipPath()
	if err != nil {
		return nil
	}
	// #nosec G304 -- path comes from our own data directory resolver.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var record ownership
	if err := json.Unmarshal(data, &record); err != nil {
		trace("ownership record unreadable: %v", err)
		return nil
	}
	if record.Forced == "" || record.Saved == "" || record.App <= 0 {
		return nil
	}
	if time.Since(record.At) > ownershipTTL {
		trace("ownership record expired: %s", record.At.Format(time.RFC3339))
		return nil
	}
	return &record
}

func writeOwnership(record ownership) {
	path, err := ownershipPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	// Replace atomically: a half-written record must never be read back.
	temp, err := os.CreateTemp(filepath.Dir(path), ".input-method-owner-*")
	if err != nil {
		return
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(name)
		return
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
	}
}

// clearOwnership removes the record only when it still describes this process,
// so releasing our own ownership cannot discard a sibling's pending restore.
func clearOwnership(pid int) {
	record := readOwnership()
	if record == nil || record.PID != pid {
		return
	}
	if path, err := ownershipPath(); err == nil {
		_ = os.Remove(path)
	}
}
