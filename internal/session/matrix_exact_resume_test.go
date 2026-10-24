package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: prove fail-closed command planning without launching Codex.
func TestMatrixExactResumePlanPreservesNativeID(t *testing.T) {
	inst, home := newCodexGateInstance(t)
	inst.CodexSessionID = uniqueSID(t)
	seedCodexRolloutWithMeta(t, home, inst.CodexSessionID, "user", "", true)
	plan, err := newExactCodexResumePlan(inst)
	if err != nil {
		t.Fatal(err)
	}
	command, err := plan.command(inst)
	if err != nil || !strings.Contains(command, " resume "+inst.CodexSessionID) || strings.Contains(command, " fork ") || !strings.Contains(command, "CODEX_HOME=") {
		t.Fatalf("command=%q err=%v", command, err)
	}
	inst.CodexSessionID = uniqueSID(t)
	if _, err := plan.command(inst); err == nil {
		t.Fatal("changed native binding accepted")
	}
}

func TestMatrixExactResumePlanRejectsUnsafeIdentity(t *testing.T) {
	for _, kind := range []string{"missing", "subagent", "mismatched-meta", "symlink", "ambiguous", "cwd", "wrapper", "custom-command", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			inst, home := newCodexGateInstance(t)
			inst.CodexSessionID = uniqueSID(t)
			path := seedCodexRolloutWithMeta(t, home, inst.CodexSessionID, "user", "", true)
			switch kind {
			case "missing":
				_ = os.Remove(path)
			case "subagent":
				seedCodexRolloutWithMeta(t, home, inst.CodexSessionID, "subagent", uniqueSID(t), true)
			case "mismatched-meta":
				data, _ := os.ReadFile(path)
				data = []byte(strings.Replace(string(data), inst.CodexSessionID, uniqueSID(t), 1))
				_ = os.WriteFile(path, data, 0600)
			case "symlink":
				moved := filepath.Join(t.TempDir(), "source.jsonl")
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, path); err != nil {
					t.Fatal(err)
				}
			case "ambiguous":
				data, _ := os.ReadFile(path)
				other := filepath.Join(filepath.Dir(path), "rollout-duplicate-"+inst.CodexSessionID+".jsonl")
				if err := os.WriteFile(other, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "cwd":
				inst.ProjectPath += "/absent"
			case "wrapper":
				inst.Wrapper = "wrapper"
			case "custom-command":
				inst.Command = "codex resume --last"
			case "sleep":
				inst.AutoSleep = &AutoSleepState{Phase: "invalid"}
			}
			if _, err := newExactCodexResumePlan(inst); err == nil {
				t.Fatal("unsafe plan accepted")
			}
			if inst.CodexSessionID == "" {
				t.Fatal("refusal discarded native binding")
			}
		})
	}
}

func TestMatrixExactResumeRechecksDurableSleepMarker(t *testing.T) {
	storage, inst, _, _, _ := matrixAutoSleepCoreFixture(t)
	// Keep the caller stale while another writer persists a sleep marker.
	fresh, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	fresh[0].AutoSleep = &AutoSleepState{Phase: "invalid"}
	if err := storage.Save(fresh); err != nil {
		t.Fatal(err)
	}
	if err := ResumeExactCodexForOpen(storage, inst); err == nil || !strings.Contains(err.Error(), "sleep state changed") {
		t.Fatalf("stale recovery bypassed durable marker: %v", err)
	}
	row, _ := storage.db.LoadInstanceByID(inst.ID)
	if ReadAutoSleepStateFromToolData(row.ToolData) == nil {
		t.Fatal("refusal cleared durable marker")
	}
}

// matrix/hardened: a validated plan cannot degrade after a rollout disappears.
func TestMatrixExactResumePlanRevalidatesArtifactBeforeCommand(t *testing.T) {
	for _, kind := range []string{"missing", "subagent"} {
		t.Run(kind, func(t *testing.T) {
			inst, home := newCodexGateInstance(t)
			inst.CodexSessionID = uniqueSID(t)
			path := seedCodexRolloutWithMeta(t, home, inst.CodexSessionID, "user", "", true)
			plan, err := newExactCodexResumePlan(inst)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				_ = os.Remove(path)
			} else {
				seedCodexRolloutWithMeta(t, home, inst.CodexSessionID, "subagent", uniqueSID(t), true)
			}
			if command, err := plan.command(inst); err == nil || command != "" {
				t.Fatalf("changed artifact became a launch: %q %v", command, err)
			}
			if inst.CodexSessionID != plan.sid {
				t.Fatal("refusal discarded original ID")
			}
		})
	}
}
