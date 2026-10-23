package inputmethod

// matrix/hardened: the trace must record why the feature stays off, because a
// refused backend otherwise leaves no evidence at all.
import (
	"bytes"
	"strings"
	"testing"
)

func TestInputMethodTraceSink(t *testing.T) {
	var buf bytes.Buffer
	SetDebugWriter(&buf)
	t.Cleanup(func() { SetDebugWriter(nil) })
	Trace("hello %s", "world")
	if got := buf.String(); !strings.Contains(got, "hello world") {
		t.Fatalf("trace line missing: %q", got)
	}
	SetDebugWriter(nil)
	before := buf.Len()
	Trace("silence")
	if buf.Len() != before {
		t.Fatal("tracing continued after it was disabled")
	}
}

func TestInputMethodTraceRecordsRefusedBackend(t *testing.T) {
	var buf bytes.Buffer
	SetDebugWriter(&buf)
	t.Cleanup(func() { SetDebugWriter(nil) })
	t.Setenv("SSH_CONNECTION", "10.0.0.1 1 10.0.0.2 22")
	if c := New(TextSourceAuto); c != nil {
		t.Fatal("an SSH session must not take over the desktop input source")
	}
	if got := buf.String(); !strings.Contains(got, "ssh session") {
		t.Fatalf("refusal was not traced: %q", got)
	}
}
