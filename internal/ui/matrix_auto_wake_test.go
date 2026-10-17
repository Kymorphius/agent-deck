package ui

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: opening a sleeping row must go through exact resume rather
// than the normal restart fallback, even when the terminal appears absent.
func TestMatrixSleepingOpenUsesWakeCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, phase := range []string{"asleep", "pending"} {
		for _, embedded := range []bool{false, true} {
			inst := session.NewInstanceWithTool("sleeping", t.TempDir(), "codex")
			inst.AutoSleep = &session.AutoSleepState{Version: 1, Phase: phase}
			inst.LastAccessedAt = time.Now().Add(-2 * time.Hour)
			storage, err := session.NewStorageWithProfile(fmt.Sprintf("wake-%s-%t", phase, embedded))
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			if err := storage.Save([]*session.Instance{inst}); err != nil {
				t.Fatal(err)
			}
			loaded, err := storage.Load()
			if err != nil || len(loaded) != 1 {
				t.Fatalf("load: %v", err)
			}
			inst = loaded[0]
			items := []session.Item{{Type: session.ItemTypeSession, Session: inst, Level: 1}}
			h := newTestHomeWithItems(100, 30, items)
			h.storage = storage
			h.interactionAccess = nil // Real sleep tests use the production durable gate.
			h.embeddedLayout = embedded
			h.instances = []*session.Instance{inst}
			h.instanceByID = map[string]*session.Instance{inst.ID: inst}
			_, cmd := h.activateSelectedItem()
			if cmd == nil {
				t.Fatalf("embedded=%v: no wake command", embedded)
			}
			row, err := storage.GetDB().LoadInstanceByID(inst.ID)
			if err != nil || row == nil || time.Since(row.LastAccessed) > time.Minute {
				t.Fatalf("open did not publish access before wake: %v %+v", err, row)
			}
			// Native identity is intentionally incomplete: exact wake must fail
			// closed before launching anything, including pending-cleanup recovery.
			msg, ok := cmd().(matrixAutoWokeMsg)
			if !ok || msg.id != inst.ID || msg.err == nil {
				t.Fatalf("embedded=%v: wake result=%+v", embedded, msg)
			}
			h.matrixAutoWakeFinished(msg)
			if h.err == nil || !strings.Contains(h.err.Error(), "wake") || !(inst.IsAutoSleeping() || inst.AutoSleepPending()) {
				t.Fatalf("failed wake hidden or marker lost: %v", h.err)
			}
		}
	}
}

func TestMatrixSleepingOpenRefusesWhenAccessCannotBePersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage, err := session.NewStorageWithProfile("access-closed")
	if err != nil {
		t.Fatal(err)
	}
	inst := session.NewInstanceWithTool("pending", t.TempDir(), "codex")
	inst.AutoSleep = &session.AutoSleepState{Version: 1, Phase: "pending"}
	if err := storage.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	loaded, err := storage.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v", err)
	}
	inst = loaded[0]
	h := newTestHomeWithItems(100, 30, []session.Item{{Type: session.ItemTypeSession, Session: inst}})
	h.storage = storage
	h.interactionAccess = nil
	_ = storage.Close()
	_, cmd := h.activateSelectedItem()
	if cmd != nil || h.err == nil || !strings.Contains(h.err.Error(), "recording access") || !inst.AutoSleepPending() || h.isAttaching.Load() {
		t.Fatalf("access persistence failure did not refuse open: cmd=%v err=%v", cmd != nil, h.err)
	}
}

func TestMatrixSleepStatusLabelDistinguishesManualStop(t *testing.T) {
	if got := matrixSessionStatusLabel(session.StatusStopped, true, false); got != "sleeping" {
		t.Fatalf("sleep label=%q", got)
	}
	if got := matrixSessionStatusLabel(session.StatusStopped, false, false); got != "stopped" {
		t.Fatalf("manual stop label=%q", got)
	}
	if got := matrixSessionStatusLabel(session.StatusStopped, false, true); got != "sleep pending" {
		t.Fatalf("pending label=%q", got)
	}
}

func TestMatrixSleepingWakeCompletionDoesNotRestartConcurrentManualStop(t *testing.T) {
	inst := session.NewInstanceWithTool("stopped during wake", t.TempDir(), "codex")
	inst.Status = session.StatusStopped
	h := newTestHomeWithItems(100, 30, []session.Item{{Type: session.ItemTypeSession, Session: inst}})
	_, cmd := h.matrixAutoWakeFinished(matrixAutoWokeMsg{id: inst.ID})
	if cmd != nil || h.err == nil || !strings.Contains(h.err.Error(), "no longer running") {
		t.Fatalf("wake completion started a stopped process: cmd=%v err=%v", cmd != nil, h.err)
	}
}

// matrix/hardened: passive preview reads the retained artifact even though no
// terminal exists, and reports tampering rather than waking to recreate output.
func TestMatrixSleepingPreviewReadsSavedScrollbackWithoutWake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage, err := session.NewStorageWithProfile("sleep-preview")
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	inst := session.NewInstanceWithTool("saved preview", t.TempDir(), "codex")
	dir := filepath.Join(filepath.Dir(storage.Path()), "auto-sleep")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, inst.ID+"-fixture.log")
	content := "saved terminal output\nSAVED_SCROLLBACK_VISIBLE\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	inst.AutoSleep = &session.AutoSleepState{Version: 1, Phase: "asleep", ScrollbackPath: path, ScrollbackBytes: int64(len(content)), ScrollbackSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
	inst.Status = session.StatusStopped
	if err := storage.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	loaded, err := storage.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v", err)
	}
	inst = loaded[0]
	h := newTestHomeWithItems(100, 30, []session.Item{{Type: session.ItemTypeSession, Session: inst}})
	msg := h.fetchPreview(inst, inst.ID, -1)().(previewFetchedMsg)
	if msg.err != nil || msg.content != content || !inst.IsAutoSleeping() {
		t.Fatalf("saved preview=%+v sleeping=%v", msg, inst.IsAutoSleeping())
	}
	h.previewCache[inst.ID] = msg.content
	view := h.renderPreviewPane(100, 25)
	if !strings.Contains(view, "SAVED_SCROLLBACK_VISIBLE") || !strings.Contains(view, "sleeping") {
		t.Fatalf("saved history not visible: %q", view)
	}
	inst.AutoSleep.Phase = "pending"
	msg = h.fetchPreview(inst, inst.ID, -1)().(previewFetchedMsg)
	if msg.err != nil || msg.content != content || !inst.AutoSleepPending() {
		t.Fatalf("pending preview did not retain artifact: %+v", msg)
	}
	if view := h.renderPreviewPane(100, 25); !strings.Contains(view, "sleep pending") {
		t.Fatalf("pending cleanup was hidden: %q", view)
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	msg = h.fetchPreview(inst, inst.ID, -1)().(previewFetchedMsg)
	if !strings.Contains(msg.content, "Saved terminal history unavailable") || !inst.AutoSleepPending() {
		t.Fatalf("artifact failure hidden or woke: %+v", msg)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	// Model the successful wake's independent history reference, as persisted
	// and tested by the core. The pager must retain it beside the new pane.
	prior := *inst.AutoSleep
	inst.AutoSleepHistory = []*session.AutoSleepState{&prior}
	inst.AutoSleep = nil
	combined, err := matrixScrollbackWithSavedHistory(inst, func() (string, error) { return "LIVE_AFTER_WAKE", nil })
	if err != nil || !strings.Contains(combined, "SAVED_SCROLLBACK_VISIBLE") || !strings.Contains(combined, "LIVE_AFTER_WAKE") || inst.IsAutoSleeping() {
		t.Fatalf("wake history absent from pager: %v %q", err, combined)
	}
}
