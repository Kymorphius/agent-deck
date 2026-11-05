package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// matrix/hardened: a process started by the test harness (testutil sets
// AGENT_DECK_TEST_ISOLATED=1 and CLI subprocesses inherit it) must never load,
// unload, enable or start units in the real per-user launchd or systemd. HOME
// isolation cannot reach the service manager: `conductor setup` run by
// TestConductorSetupCLI_* registered com.agentdeck.transition-notifier, a
// KeepAlive job pointing at a test binary in a temp dir, for the developer on
// every full run. Its preceding `launchctl unload <plist>` would also evict a
// real notifier carrying the same label. Read-only queries still run, and so
// do stand-in launchctl/systemctl scripts a test put on PATH from a temp dir.
const agentDeckTestIsolationEnv = "AGENT_DECK_TEST_ISOLATED"

var errServiceManagerBlocked = errors.New("test isolation: the real launchd/systemd is not modified")

// serviceManagerCommand is exec.Command for launchctl and systemctl. Under test
// isolation a mutating invocation fails at Start with errServiceManagerBlocked
// and never runs.
func serviceManagerCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	if cmd.Err == nil && os.Getenv(agentDeckTestIsolationEnv) == "1" &&
		serviceManagerMutates(args) && !testStandIn(cmd.Path) {
		cmd.Err = errServiceManagerBlocked
	}
	return cmd
}

// testStandIn reports whether a resolved executable lives in a temp dir, where
// tests place fake launchctl/systemctl scripts; the real ones never do.
func testStandIn(path string) bool {
	if path == "" {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
		root = filepath.Clean(root)
		if root != "" && root != "/" && strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// serviceManagerMutates reports whether the first non-flag argument is a verb
// that can change the service manager's state; unknown verbs count as mutating.
func serviceManagerMutates(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "list", "print", "managername", "is-active", "is-enabled", "is-failed",
			"status", "show", "cat", "list-units", "list-unit-files":
			return false
		}
		return true
	}
	return false
}
