package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: private native artifacts, without a real Claude invocation.
func matrixExactClaudeFixture(t *testing.T) (*Instance, string, string) {
	t.Helper()
	home := isolatedHomeDir(t)
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
		t.Setenv("HOME", home)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	inst := newGuardInstance(t, home)
	inst.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(home, ".claude", "projects", SlugifyClaudeProjectPath(inst.ProjectPath), inst.ClaudeSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := ""
	for _, role := range []string{"user", "assistant"} {
		row := map[string]any{"type": role, "sessionId": inst.ClaudeSessionID, "cwd": inst.ProjectPath, "isSidechain": false, "message": map[string]any{"role": role, "content": "hello"}}
		line, _ := json.Marshal(row)
		data += string(line) + "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return inst, home, path
}

func TestMatrixExactClaudePlanPreservesOriginalConversation(t *testing.T) {
	inst, home, _ := matrixExactClaudeFixture(t)
	inst.SetClaudeOptions(&ClaudeOptions{SessionMode: "new", SkipPermissions: true, Model: "sonnet"})
	inst.EnvFile = filepath.Join(home, "old.env")
	os.WriteFile(inst.EnvFile, []byte("export CLAUDE_CONFIG_DIR=/foreign\nexport CLAUDE_LAUNCH_GUARD_TICKET=old\n"), 0600)
	plan, err := newExactClaudeResumePlan(inst)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := plan.command(inst)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--resume " + inst.ClaudeSessionID, "--dangerously-skip-permissions", "--model sonnet", "export CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"), "unset " + claudeLaunchTicketEnv} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("missing %q in %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--session-id") || strings.Contains(cmd, "--fork-session") {
		t.Fatal(cmd)
	}
	admission, err := plan.admission(inst)
	if err != nil {
		t.Fatal(err)
	}
	if admission.ticketPath != "" || admission.planned.ClaudeSessionID != inst.ClaudeSessionID || admission.planned.claudeLaunchKind != "resume" {
		t.Fatal("resume became new-session admission")
	}
}

func TestMatrixExactClaudePlanRefusesUnsafeRecovery(t *testing.T) {
	for _, kind := range []string{"missing", "subagent", "foreign-id", "foreign-cwd", "symlink", "metadata-only", "empty-content", "missing-launch-cwd", "malformed", "tainted", "prefix", "wrapper", "custom-command", "extra-selector", "extra-prompt", "options-selector", "options-latest", "child", "scratch", "scratch-required-plugins", "scratch-required-topology", "fork", "ssh", "changed-config", "changed-native", "changed-options", "changed-artifact"} {
		t.Run(kind, func(t *testing.T) {
			inst, _, path := matrixExactClaudeFixture(t)
			plan, err := newExactClaudeResumePlan(inst)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			switch kind {
			case "missing":
				os.Remove(path)
			case "subagent":
				os.WriteFile(path, []byte(strings.ReplaceAll(string(data), `"isSidechain":false`, `"isSidechain":true`)), 0600)
			case "foreign-id":
				os.WriteFile(path, []byte(strings.ReplaceAll(string(data), inst.ClaudeSessionID, "aaaaaaaa-2222-4333-8444-555555555555")), 0600)
			case "foreign-cwd":
				os.WriteFile(path, []byte(strings.ReplaceAll(string(data), inst.ProjectPath, "/foreign")), 0600)
			case "symlink":
				target := filepath.Join(t.TempDir(), "original.jsonl")
				os.Rename(path, target)
				os.Symlink(target, path)
			case "metadata-only":
				os.WriteFile(path, []byte(`{"type":"summary","sessionId":"`+inst.ClaudeSessionID+`"}`+"\n"), 0600)
			case "empty-content":
				os.WriteFile(path, []byte(strings.ReplaceAll(string(data), `"content":"hello"`, `"content":""`)), 0600)
			case "missing-launch-cwd":
				os.WriteFile(path, []byte(strings.ReplaceAll(string(data), inst.ProjectPath, inst.ProjectPath+"/subdir")), 0600)
			case "malformed":
				os.WriteFile(path, []byte("{broken\n"), 0600)
			case "tainted":
				inst.markClaudeSessionIDFromDiskScan(inst.ClaudeSessionID)
			case "prefix":
				inst.ClaudeSessionID = inst.ClaudeSessionID[:18]
			case "wrapper":
				inst.Wrapper = "wrapper {command}"
			case "custom-command":
				inst.Command = "claude --continue"
			case "extra-selector":
				inst.ExtraArgs = []string{"--session-id", "foreign"}
			case "extra-prompt":
				inst.ExtraArgs = []string{"continue working"}
			case "options-selector":
				inst.SetClaudeOptions(&ClaudeOptions{SessionMode: "resume", ResumeSessionID: "foreign"})
			case "options-latest":
				inst.SetClaudeOptions(&ClaudeOptions{SessionMode: "continue"})
			case "child":
				inst.ParentSessionID = "parent"
			case "scratch":
				inst.WorkerScratchConfigDir = t.TempDir()
			case "scratch-required-plugins":
				inst.Plugins = []string{"example@local"}
			case "scratch-required-topology":
				previous := hostHasTelegramConductor
				hostHasTelegramConductor = func() bool { return true }
				t.Cleanup(func() { hostHasTelegramConductor = previous })
			case "fork":
				inst.IsForkAwaitingStart = true
			case "ssh":
				inst.SSHHost = "remote"
			case "changed-config":
				t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			case "changed-native":
				inst.ClaudeSessionID = "aaaaaaaa-2222-4333-8444-555555555555"
			case "changed-options":
				inst.SetClaudeOptions(&ClaudeOptions{Model: "changed"})
			case "changed-artifact":
				os.WriteFile(path, append(data, []byte("{\"type\":\"summary\"}\n")...), 0600)
			}
			if cmd, err := plan.command(inst); err == nil || cmd != "" {
				t.Fatalf("unsafe %s accepted: %s %v", kind, cmd, err)
			}
		})
	}
}

func TestMatrixExactClaudePlanAllowsRecordedSubdirectoryCWD(t *testing.T) {
	inst, _, path := matrixExactClaudeFixture(t)
	data, _ := os.ReadFile(path)
	// A real Claude conversation follows Bash cd, but retains its launch record.
	line, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": inst.ClaudeSessionID, "cwd": filepath.Join(inst.ProjectPath, "app", "static"), "message": map[string]any{"role": "assistant", "content": "nested"}})
	os.WriteFile(path, append(data, append(line, '\n')...), 0600)
	if _, err := newExactClaudeResumePlan(inst); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixExactClaudeStoredBindingChangesFailClosed(t *testing.T) {
	for _, key := range []string{"claude_session_id", "claude_session_id_unverified", "tool_options", "multi_repo_temp_dir", "is_conductor"} {
		t.Run(key, func(t *testing.T) {
			inst, _, _ := matrixExactClaudeFixture(t)
			storage := newTestStorage(t)
			if err := storage.Save([]*Instance{inst}); err != nil {
				t.Fatal(err)
			}
			if err := exactClaudeStoredBinding(storage, inst); err != nil {
				t.Fatal(err)
			}
			row, _ := storage.db.LoadInstanceByID(inst.ID)
			var fields map[string]any
			json.Unmarshal(row.ToolData, &fields)
			switch key {
			case "claude_session_id":
				fields[key] = "aaaaaaaa-2222-4333-8444-555555555555"
			case "claude_session_id_unverified":
				fields[key] = true
			case "tool_options":
				fields[key] = map[string]any{"changed": true}
			case "multi_repo_temp_dir":
				fields[key] = filepath.Join(t.TempDir(), "changed-working-directory")
			case "is_conductor":
				row.IsConductor = true
			}
			row.ToolData, _ = json.Marshal(fields)
			if err := storage.db.SaveInstance(row); err != nil {
				t.Fatal(err)
			}
			if err := exactClaudeStoredBinding(storage, inst); err == nil {
				t.Fatal("changed persisted binding accepted")
			}
		})
	}
}

// matrix/hardened: pre-taint-field registries still carry a recorded identity;
// missing false must not make old ordinary sessions permanently unrecoverable.
func TestMatrixExactClaudeLegacyRecordedIdentity(t *testing.T) {
	inst, _, _ := matrixExactClaudeFixture(t)
	storage := newTestStorage(t)
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	row, err := storage.db.LoadInstanceByID(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(row.ToolData, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "claude_session_id_unverified")
	row.ToolData, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.db.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	if err := exactClaudeStoredBinding(storage, inst); err != nil {
		t.Fatalf("legacy false verification rejected: %v", err)
	}
}
