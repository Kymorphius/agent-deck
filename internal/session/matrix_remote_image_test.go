package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: Ctrl+V while attached to a remote session pastes a local
// clipboard image as an uploaded path, and otherwise behaves exactly as before.

var matrixTestPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func TestMatrixAttachInput_CtrlVPastesImage(t *testing.T) {
	var calls int
	var out bytes.Buffer
	input := sshAttachInput{writer: &out, pasteImage: func() ([]byte, bool) {
		calls++
		return []byte("\x1b[200~/tmp/x.png\x1b[201~"), true
	}}
	for _, key := range []string{"\x16", "\x1b[118;5u"} {
		out.Reset()
		if detach, err := input.forward([]byte(key)); detach || err != nil {
			t.Fatalf("%q: detach=%v err=%v", key, detach, err)
		}
		if out.String() != "\x1b[200~/tmp/x.png\x1b[201~" {
			t.Fatalf("%q forwarded %q, want the pasted path", key, out.String())
		}
	}
	if calls != 2 {
		t.Fatalf("hook ran %d times", calls)
	}

	// Not a lone key press: forwarded unchanged, hook not consulted.
	out.Reset()
	_, _ = input.forward([]byte("a\x16"))
	if out.String() != "a\x16" || calls != 2 {
		t.Fatalf("mixed read: %q calls=%d", out.String(), calls)
	}
	// Inside a bracketed paste, a 0x16 byte is pasted text, not a key.
	out.Reset()
	_, _ = input.forward([]byte("\x1b[200~abc"))
	_, _ = input.forward([]byte("\x16"))
	if !strings.HasSuffix(out.String(), "\x16") || calls != 2 {
		t.Fatalf("Ctrl+V inside a paste intercepted: %q calls=%d", out.String(), calls)
	}
}

func TestMatrixAttachInput_CtrlVWithoutImageIsForwarded(t *testing.T) {
	var out bytes.Buffer
	input := sshAttachInput{writer: &out, pasteImage: func() ([]byte, bool) { return nil, false }}
	_, _ = input.forward([]byte("\x16"))
	if out.String() != "\x16" {
		t.Fatalf("forwarded %q, want Ctrl+V", out.String())
	}
	out.Reset()
	plain := sshAttachInput{writer: &out}
	_, _ = plain.forward([]byte("\x16"))
	if out.String() != "\x16" {
		t.Fatalf("no hook: forwarded %q", out.String())
	}
}

func TestMatrixClipboardImagePaster(t *testing.T) {
	orig := readLocalClipboardImage
	t.Cleanup(func() { readLocalClipboardImage = orig })
	readLocalClipboardImage = func(context.Context) ([]byte, error) { return matrixTestPNG, nil }

	var gotArgs []string
	var gotStdin []byte
	r := &SSHRunner{name: "lab", runStdinFn: func(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
		gotArgs, gotStdin = args, stdin
		return []byte(`{"success":true,"path":"/remote/uploads/s1/a.png"}` + "\n"), nil
	}}
	pasted, ok := r.clipboardImagePaster("s1")()
	if !ok || string(pasted) != "\x1b[200~/remote/uploads/s1/a.png\x1b[201~" {
		t.Fatalf("pasted %q ok=%v", pasted, ok)
	}
	if strings.Join(gotArgs, " ") != "session image-put s1 --json" || !bytes.Equal(gotStdin, matrixTestPNG) {
		t.Fatalf("ran %v with %d bytes", gotArgs, len(gotStdin))
	}

	// An old remote (unknown verb) or any failure: forward the key instead.
	r.runStdinFn = func(context.Context, []byte, ...string) ([]byte, error) { return nil, errors.New("unknown command") }
	if _, ok := r.clipboardImagePaster("s1")(); ok {
		t.Fatal("failed upload still pasted")
	}
	// No image on the clipboard: nothing is uploaded.
	readLocalClipboardImage = func(context.Context) ([]byte, error) { return nil, nil }
	r.runStdinFn = func(context.Context, []byte, ...string) ([]byte, error) {
		t.Fatal("uploaded without an image")
		return nil, nil
	}
	if _, ok := r.clipboardImagePaster("s1")(); ok {
		t.Fatal("pasted without an image")
	}
}

func TestMatrixStoreSessionImage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := StoreSessionImage("default", "s1", []byte("not an image")); err == nil {
		t.Fatal("text accepted")
	}
	if _, err := StoreSessionImage("default", "../x", matrixTestPNG); err == nil {
		t.Fatal("path-like session id accepted")
	}
	path, err := StoreSessionImage("default", "s1", matrixTestPNG)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || filepath.Base(filepath.Dir(path)) != "s1" || filepath.Ext(path) != ".png" {
		t.Fatalf("stored %s: %v %v", path, info, err)
	}
	// A week-old upload is pruned on the next store.
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreSessionImage("default", "s1", matrixTestPNG); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("old upload not pruned")
	}
}
