package ui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the web create path must refuse a missing project
// directory (the TUI asks first) and report the resolved path, before it
// touches storage or tmux.
func TestWebMutatorCreateSessionRefusesMissingDirectory(t *testing.T) {
	m := &WebMutator{}
	missing := filepath.Join(t.TempDir(), "not", "there")
	_, err := m.CreateSession("t", "shell", missing, "", "", "")
	var want *session.MissingProjectDirError
	if !errors.As(err, &want) || want.Path != missing {
		t.Fatalf("err = %v, want MissingProjectDirError{%s}", err, missing)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSession("t", "shell", file, "", "", ""); err == nil || errors.As(err, &want) {
		t.Fatalf("a regular file must be rejected as not a directory, got %v", err)
	}
}
