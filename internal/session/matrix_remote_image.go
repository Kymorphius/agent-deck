package session

// matrix/hardened: Ctrl+V pastes a local clipboard image into a remote
// session. Claude Code reads images from the clipboard of the host it runs on,
// which a remote attach cannot reach, but it also turns a pasted image path
// into an [Image #n] attachment (the web terminal's image button relies on
// the same). So while attached to a remote session over SSH or mosh, a lone
// Ctrl+V is intercepted: if this machine's clipboard holds an image, it is
// uploaded with `agent-deck session image-put` on the remote and the stored
// path is pasted into the pane. Anything else (no image, an old remote, a
// failed upload) forwards the Ctrl+V unchanged.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// MatrixImageMaxBytes caps one pasted or uploaded image.
	MatrixImageMaxBytes = 20 << 20
	matrixImageKeep     = 7 * 24 * time.Hour
	matrixPasteTimeout  = 20 * time.Second
)

// Claude reads these four; the type is sniffed from the bytes.
var matrixImageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// StoreSessionImage saves an image for session sessionID under
// <profile dir>/uploads/<id>/ (the directory the web terminal's image upload
// uses), drops that session's uploads older than a week, and returns the
// absolute path.
func StoreSessionImage(profile, sessionID string, data []byte) (string, error) {
	if sessionID == "" || sessionID != filepath.Base(sessionID) || sessionID == "." || sessionID == ".." {
		return "", errors.New("invalid session id")
	}
	if len(data) > MatrixImageMaxBytes {
		return "", errors.New("image is larger than 20 MB")
	}
	ext, ok := matrixImageExt[http.DetectContentType(data)]
	if len(data) == 0 || !ok {
		return "", errors.New("only PNG, JPEG, GIF or WebP images")
	}
	profileDir, err := GetProfileDir(profile)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(profileDir, "uploads", sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	pruneSessionImages(dir, time.Now().Add(-matrixImageKeep))
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	path := filepath.Join(dir, time.Now().Format("20060102-150405-")+hex.EncodeToString(suffix[:])+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func pruneSessionImages(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// matrixIsCtrlV reports whether one read is exactly a Ctrl+V key press: the
// legacy control byte, or its CSI-u form when the remote app enabled the
// kitty/fixterms keyboard protocol.
func matrixIsCtrlV(data []byte) bool {
	switch string(data) {
	case "\x16", "\x1b[118;5u", "\x1b[118;5:1u":
		return true
	}
	return false
}

// readLocalClipboardImage returns the image on this machine's clipboard, or
// nil when it holds none.
var readLocalClipboardImage = func(ctx context.Context) ([]byte, error) {
	switch runtime.GOOS {
	case "darwin":
		f, err := os.CreateTemp("", "agent-deck-paste-*.png")
		if err != nil {
			return nil, err
		}
		path := f.Name()
		_ = f.Close()
		defer os.Remove(path)
		// «class PNGf» covers screenshots and images copied from apps; it
		// fails (no image) for text, which is the common case.
		script := []string{
			"-e", "set png to (the clipboard as «class PNGf»)",
			"-e", fmt.Sprintf("set f to open for access POSIX file %q with write permission", path),
			"-e", "set eof f to 0",
			"-e", "write png to f",
			"-e", "close access f",
		}
		if err := exec.CommandContext(ctx, "osascript", script...).Run(); err != nil {
			return nil, nil
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			return nil, nil
		}
		return data, nil
	default:
		for _, argv := range [][]string{
			{"wl-paste", "--no-newline", "--type", "image/png"},
			{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
		} {
			if _, err := exec.LookPath(argv[0]); err != nil {
				continue
			}
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
			if err == nil && len(out) > 0 {
				return out, nil
			}
		}
		return nil, nil
	}
}

// clipboardImagePaster returns the attach hook for a remote session: the
// bracketed-paste bytes of the uploaded image's remote path, or ok=false to
// forward the key unchanged.
func (r *SSHRunner) clipboardImagePaster(sessionID string) func() ([]byte, bool) {
	return func() ([]byte, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), matrixPasteTimeout)
		defer cancel()
		img, err := readLocalClipboardImage(ctx)
		if err != nil || len(img) == 0 || len(img) > MatrixImageMaxBytes {
			return nil, false
		}
		out, err := r.runWithStdin(ctx, img, "session", "image-put", sessionID, "--json")
		var resp struct {
			Path string `json:"path"`
		}
		if err != nil || json.Unmarshal(bytes.TrimSpace(out), &resp) != nil || resp.Path == "" {
			sessionLog.Warn("remote_image_paste_failed",
				slog.String("remote", r.name),
				slog.String("error", fmt.Sprintf("%v", err)))
			return nil, false
		}
		return []byte("\x1b[200~" + resp.Path + "\x1b[201~"), true
	}
}
