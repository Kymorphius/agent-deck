package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// matrix/hardened: `session image-put` is what a controller runs over SSH
// when Ctrl+V finds an image on its clipboard (internal/session/matrix_remote_image.go).
func TestMatrixSessionImagePut(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runAgentDeck(t, home, "add", "-t", "pic", "-c", "claude", proj); code != 0 {
		t.Fatalf("add: %s", stderr)
	}
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	stdout, stderr, code := runAgentDeckStdin(t, home, png, "session", "image-put", "pic", "--json")
	var resp struct {
		Path string `json:"path"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &resp) != nil || resp.Path == "" {
		t.Fatalf("image-put: %d %s %s", code, stdout, stderr)
	}
	if data, err := os.ReadFile(resp.Path); err != nil || string(data) != png || filepath.Ext(resp.Path) != ".png" {
		t.Fatalf("stored %s: %v", resp.Path, err)
	}
	if _, _, code := runAgentDeckStdin(t, home, "just text", "session", "image-put", "pic", "--json"); code == 0 {
		t.Fatal("text accepted as an image")
	}
}
