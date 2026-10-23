package main

// matrix/hardened: AGENTDECK_IME_DEBUG turns on the input-method decision
// trace. Input-source switching depends on terminal focus reports and app
// activation order; without a trace a misbehaving switch leaves no evidence.
import (
	"os"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/agentpaths"
	"github.com/asheshgoplani/agent-deck/internal/inputmethod"
)

// startInputMethodTrace opens the file named by AGENTDECK_IME_DEBUG ("1" picks
// the default log path) and returns a stop function. Tracing stays off when the
// variable is unset, and failing to open the file is never fatal: the switching
// feature has to keep working without its log.
func startInputMethodTrace() func() {
	value := strings.TrimSpace(os.Getenv("AGENTDECK_IME_DEBUG"))
	switch strings.ToLower(value) {
	case "", "0", "off", "false":
		return func() {}
	}
	path := value
	switch strings.ToLower(value) {
	case "1", "on", "true":
		resolved, err := agentpaths.EffectiveDataPath(filepath.Join("logs", "input-method.log"), "logs")
		if err != nil {
			return func() {}
		}
		path = resolved
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return func() {}
	}
	// #nosec G304 -- the operator opts in and names this trace file explicitly.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}
	}
	inputmethod.SetDebugWriter(file)
	inputmethod.Trace("trace opened pid=%d version=%s", os.Getpid(), Version)
	return func() {
		inputmethod.Trace("trace closed pid=%d", os.Getpid())
		inputmethod.SetDebugWriter(nil)
		_ = file.Close()
	}
}
