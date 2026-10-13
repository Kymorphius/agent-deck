// matrix/hardened: a rendered viewer never forwards queries to a second console.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func TestPaintSnapshotOmitsRawDeviceQueries(t *testing.T) {
	r := terminalhost.Response{Render: "native screen", Output: []byte("\x1b[c\x1b[6n"), CursorX: 2, CursorY: 3}
	var output bytes.Buffer
	if err := paintSnapshot(&output, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "native screen") || !strings.Contains(output.String(), "\x1b[4;3H") {
		t.Fatalf("bad snapshot: %q", output.String())
	}
	if strings.Contains(output.String(), "\x1b[c") || strings.Contains(output.String(), "\x1b[6n") {
		t.Fatal("raw device query reached client console")
	}
}
