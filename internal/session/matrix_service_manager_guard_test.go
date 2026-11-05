package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// matrix/hardened: under test isolation launchctl/systemctl verbs that change
// the real service manager never start; read-only queries still do. Outside
// it nothing is blocked.
func TestServiceManagerCommandBlocksMutationsUnderTestIsolation(t *testing.T) {
	t.Setenv(agentDeckTestIsolationEnv, "1")
	for _, args := range [][]string{
		{"launchctl", "load", "/tmp/x.plist"},
		{"launchctl", "unload", "/tmp/x.plist"},
		{"launchctl", "bootstrap", "gui/501", "/tmp/x.plist"},
		{"launchctl", "kickstart", "-k", "gui/501/x"},
		{"systemctl", "--user", "enable", "--now", "x.service"},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "stop", "x.service"},
	} {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue // not installed here: nothing could run anyway
		}
		err := serviceManagerCommand(args[0], args[1:]...).Run()
		if !errors.Is(err, errServiceManagerBlocked) {
			t.Errorf("%v ran under test isolation (err=%v)", args, err)
		}
	}
	for _, args := range [][]string{
		{"launchctl", "list", "com.example"},
		{"launchctl", "print", "gui/501/x"},
		{"launchctl", "managername"},
		{"systemctl", "--user", "is-active", "x.service"},
	} {
		if cmd := serviceManagerCommand(args[0], args[1:]...); errors.Is(cmd.Err, errServiceManagerBlocked) {
			t.Errorf("read-only %v blocked", args)
		}
	}
	// A stand-in a test put on PATH from a temp dir is not the real manager.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := serviceManagerCommand("launchctl", "load", "/tmp/x.plist").Run(); err != nil {
		t.Errorf("stand-in launchctl did not run: %v", err)
	}
	t.Setenv("PATH", "/bin:/usr/bin")
	t.Setenv(agentDeckTestIsolationEnv, "")
	if cmd := serviceManagerCommand("launchctl", "load", "/tmp/x.plist"); errors.Is(cmd.Err, errServiceManagerBlocked) {
		t.Errorf("production load blocked")
	}
}
