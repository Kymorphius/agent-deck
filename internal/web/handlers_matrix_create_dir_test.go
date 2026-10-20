package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: a missing project directory answers 409
// DIRECTORY_NOT_FOUND with the path; createDirectory makes it, then creates.
func TestCreateSessionMissingDirectoryAsksThenCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "project")
	var gotPath string
	m := &fakeMutator{}
	m.createSessionFn = func(title, tool, projectPath, groupPath, modelID, reasoningEffort string) (string, error) {
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			return "", &session.MissingProjectDirError{Path: projectPath}
		}
		gotPath = projectPath
		return "sess-new", nil
	}
	srv := NewServer(Config{ListenAddr: "127.0.0.1:0", WebMutations: true})
	srv.mutator = m
	post := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/sessions", bytes.NewReader(b))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}

	w := post(map[string]any{"title": "t", "tool": "shell", "projectPath": dir})
	var resp struct {
		Error struct{ Code string } `json:"error"`
		Path  string                `json:"path"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusConflict || resp.Error.Code != ErrCodeDirectoryNotFound || resp.Path != dir {
		t.Fatalf("missing dir: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("directory created without confirmation")
	}

	w = post(map[string]any{"title": "t", "tool": "shell", "projectPath": dir, "createDirectory": true})
	if w.Code != http.StatusCreated {
		t.Fatalf("confirmed create: %d %s", w.Code, w.Body.String())
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || gotPath != dir {
		t.Fatalf("directory not created or wrong path passed: %v %q", err, gotPath)
	}
}
