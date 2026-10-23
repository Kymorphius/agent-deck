// matrix/hardened: opt-in decision trace. The input-source dance depends on
// terminal focus reports and app activation order, neither of which is visible
// after the fact, so a misbehaving switch is otherwise impossible to diagnose.
package inputmethod

import (
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	debugMu   sync.Mutex
	debugSink io.Writer
)

// SetDebugWriter enables tracing to w. A nil writer disables it. Tracing is off
// unless the CLI explicitly turns it on, and it never touches the input source.
func SetDebugWriter(w io.Writer) {
	debugMu.Lock()
	debugSink = w
	debugMu.Unlock()
}

// Trace records one line for callers outside this package (the UI layer logs
// which message triggered a sync).
func Trace(format string, args ...any) { trace(format, args...) }

func trace(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugSink == nil {
		return
	}
	fmt.Fprintf(debugSink, "%s %s\n",
		time.Now().Format("2006-01-02T15:04:05.000"), fmt.Sprintf(format, args...))
}
