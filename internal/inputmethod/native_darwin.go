//go:build darwin && cgo

// matrix/hardened: use the native input-source API without hotkey simulation.
package inputmethod

/*
#cgo LDFLAGS: -framework Carbon -framework AppKit
#include <stdlib.h>
#include "native_darwin.h"
*/
import "C"

import (
	"os"
	"unsafe"
)

type nativeBackend struct{ owner C.int }

// matrix/hardened: keep native ancestry lookup available to regression tests.
func nativeParentPID(pid int) int { return int(C.deck_parent_pid(C.int(pid))) }

func newBackend() backend {
	// Never let a remote TUI change the host Mac's desktop input source.
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		trace("backend unavailable: ssh session")
		return nil
	}
	owner := C.deck_terminal_owner()
	if owner == 0 {
		// Nothing in the ancestry lives in an app bundle: a tmux server or a
		// terminal-session server adopted by launchd hides the owning app, so
		// foreground checks could not be scoped to this terminal.
		trace("backend unavailable: no terminal app in the process ancestry (ppid=%d)", os.Getppid())
		return nil
	}
	trace("backend ready: terminal app pid=%d", int(owner))
	return &nativeBackend{owner: owner}
}

func (b *nativeBackend) foreground() bool { return C.deck_is_foreground(b.owner) != 0 }
func (b *nativeBackend) current() string  { return sourceID(false) }
func (b *nativeBackend) ascii() string    { return sourceID(true) }

func sourceID(ascii bool) string {
	var flag C.int
	if ascii {
		flag = 1
	}
	s := C.deck_source_id(flag)
	if s == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(s))
	return C.GoString(s)
}

func (b *nativeBackend) selectSource(id string) bool {
	s := C.CString(id)
	defer C.free(unsafe.Pointer(s))
	return C.deck_select_source(s, b.owner) != 0
}
