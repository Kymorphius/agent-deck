//go:build !darwin || !cgo

// matrix/hardened: other platforms and CGO-free builds preserve existing input.
package inputmethod

func newBackend() backend {
	trace("backend unavailable: build without cgo, or not macOS")
	return nil
}
