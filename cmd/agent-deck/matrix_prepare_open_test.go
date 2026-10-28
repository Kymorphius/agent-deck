package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/core"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/google/uuid"
)

// matrix/hardened: use the isolated queued-attach tmux/CLI fixture with a
// fake executable named codex; no real provider process or user terminal runs.
func matrixPrepareCodexFixture(t *testing.T) (*matrixQueuedAttachFixture, string, string) {
	f := matrixNewQueuedAttachFixture(t, 0)
	home := filepath.Join(f.home, ".codex")
	sid := "019f0000-0000-7000-8000-000000008881"
	dir := filepath.Join(home, "sessions", "2026", "10", "24")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-test-"+sid+".jsonl")
	if err := os.WriteFile(rollout, []byte(`{"type":"session_meta","payload":{"id":"`+sid+`","cwd":"`+f.home+`","thread_source":"user"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(f.home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec python3 -u '" + f.harness + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	f.env = append(f.env, "CODEX_HOME="+home)
	f.target.Tool, f.target.Command, f.target.Wrapper = "codex", "codex", ""
	f.target.CodexSessionID, f.target.Status = sid, session.StatusError
	if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
		t.Fatal(err)
	}
	return f, sid, rollout
}

// matrix/hardened: a retained Claude binding and real primary conversation
// must survive missing-terminal recovery without invoking a provider service.
func matrixPrepareClaudeFixture(t *testing.T) (*matrixQueuedAttachFixture, string, string) {
	t.Helper()
	f := matrixNewQueuedAttachFixture(t, 0)
	// matrix/hardened: live fake providers from earlier cases are intentionally
	// retained until runner cleanup, so every case needs a distinct native ID.
	sid := uuid.NewString()
	// matrix/hardened: macOS temp roots can have a /var -> /private/var alias;
	// the fixture uses one working-directory spelling for native identity.
	project, err := filepath.EvalSymlinks(f.home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.home, ".claude", "projects", session.SlugifyClaudeProjectPath(project))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	var transcript strings.Builder
	for _, role := range []string{"user", "assistant"} {
		data, err := json.Marshal(map[string]any{
			"type": role, "sessionId": sid, "cwd": project, "isSidechain": false,
			"message": map[string]any{"role": role, "content": "retained primary conversation"},
		})
		if err != nil {
			t.Fatal(err)
		}
		transcript.Write(data)
		transcript.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(transcript.String()), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(f.home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec python3 -u '" + f.harness + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.target.Tool, f.target.Command, f.target.Wrapper = "claude", "claude", ""
	f.target.ProjectPath = project
	f.target.ClaudeSessionID, f.target.Status = sid, session.StatusError
	if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
		t.Fatal(err)
	}
	return f, sid, path
}

func matrixRunPrepare(f *matrixQueuedAttachFixture) (core.SessionPrepareOpenOut, string, error) {
	cmd := exec.Command(f.bin, "session", "prepare-open", "--json", f.target.ID)
	cmd.Env, cmd.Dir = f.env, f.home
	data, err := cmd.CombinedOutput()
	var out core.SessionPrepareOpenOut
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	return out, string(data), err
}

// matrix/hardened: remote opens recover the exact saved Claude thread once,
// and a subsequent open preserves a live pane despite a stale error status.
func TestMatrixPrepareOpenClaudeExactRecoveryAndLiveReuse(t *testing.T) {
	f, sid, _ := matrixPrepareClaudeFixture(t)
	out, raw, err := matrixRunPrepare(f)
	if err != nil || out.Disposition != "resumed" {
		t.Fatalf("Claude recovery: %+v %s %v", out, raw, err)
	}
	lines := f.waitSpawn()
	if len(lines) != 1 || !strings.Contains(lines[0], "--resume "+sid) || strings.Contains(lines[0], "--session-id") || strings.Contains(lines[0], "--fork-session") {
		t.Fatalf("Claude recovery was not exact: %v", lines)
	}
	saved := f.stored(f.target.ID)
	if saved.ClaudeSessionID != sid || saved.Command != "claude" || saved.Wrapper != "" || saved.AutoSleep != nil {
		t.Fatalf("Claude binding or launch plan changed: %+v", saved)
	}
	saved.Status = session.StatusError
	if err := f.storage.Save([]*session.Instance{saved}); err != nil {
		t.Fatal(err)
	}
	out, raw, err = matrixRunPrepare(f)
	if err != nil || out.Disposition != "existing" || len(f.spawnLines()) != 1 || f.spawnLines()[0] != lines[0] {
		t.Fatalf("live Claude was replaced: %+v %s %v; spawns=%v", out, raw, err, f.spawnLines())
	}
	client := f.attach()
	client.waitReady()
	client.detach()
	if got := f.stored(f.target.ID).ClaudeSessionID; got != sid {
		t.Fatalf("attach changed original Claude ID: %s", got)
	}
}

// matrix/hardened: concurrent remote visits share one profile transition and
// preserve a single original-thread process rather than duplicate resumes.
func TestMatrixPrepareOpenClaudeConcurrentRecoverySpawnsOnce(t *testing.T) {
	f, _, _ := matrixPrepareClaudeFixture(t)
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, raw, err := matrixRunPrepare(f)
			results <- fmt.Sprintf("%s|%v|%s", out.Disposition, err, raw)
		}()
	}
	wg.Wait()
	close(results)
	var dispositions string
	for result := range results {
		if !strings.Contains(result, "|<nil>|") {
			t.Fatal(result)
		}
		dispositions += result + "\n"
	}
	if !strings.Contains(dispositions, "resumed|") || !strings.Contains(dispositions, "existing|") || len(f.waitSpawn()) != 1 {
		t.Fatalf("duplicate Claude recovery: %s %v", dispositions, f.spawnLines())
	}
}

// matrix/hardened: absent or untrusted evidence must remain an error rather
// than selecting another transcript or silently opening a new conversation.
func TestMatrixPrepareOpenClaudeInvalidEvidenceNeverStartsFresh(t *testing.T) {
	for _, kind := range []string{"missing", "mismatch", "sidechain", "tainted", "no-binding"} {
		t.Run(kind, func(t *testing.T) {
			f, sid, path := matrixPrepareClaudeFixture(t)
			switch kind {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "mismatch", "sidechain":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				text := string(data)
				if kind == "mismatch" {
					text = strings.ReplaceAll(text, sid, "019f0000-0000-7000-8000-000000008892")
				} else {
					text = strings.ReplaceAll(text, `"isSidechain":false`, `"isSidechain":true`)
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			case "tainted", "no-binding":
				var data []byte
				if err := f.storage.GetDB().DB().QueryRow("SELECT tool_data FROM instances WHERE id = ?", f.target.ID).Scan(&data); err != nil {
					t.Fatal(err)
				}
				var toolData map[string]any
				if err := json.Unmarshal(data, &toolData); err != nil {
					t.Fatal(err)
				}
				if kind == "tainted" {
					toolData["claude_session_id_unverified"] = true
				} else {
					// matrix/hardened: normal storage protects a known ID from
					// accidental clearing; model genuinely missing durable evidence.
					delete(toolData, "claude_session_id")
				}
				data, err := json.Marshal(toolData)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.storage.GetDB().DB().Exec("UPDATE instances SET tool_data = ? WHERE id = ?", string(data), f.target.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := f.stored(f.target.ID)
			if kind == "no-binding" && before.ClaudeSessionID != "" {
				t.Fatal("fixture still contains a durable native binding")
			}
			_, raw, err := matrixRunPrepare(f)
			if err == nil || len(f.spawnLines()) != 0 {
				t.Fatalf("invalid Claude evidence launched: %s %v; spawns=%v", raw, err, f.spawnLines())
			}
			after := f.stored(f.target.ID)
			if after.ClaudeSessionID != before.ClaudeSessionID || after.Status != before.Status || after.Exists() {
				t.Fatalf("refusal changed Claude identity/lifecycle: before=%s/%s after=%s/%s", before.ClaudeSessionID, before.Status, after.ClaudeSessionID, after.Status)
			}
		})
	}
}

// matrix/hardened: capacity must not convert a stopped/error Claude recovery
// into a queued fresh-start plan or discard its original native binding.
func TestMatrixPrepareOpenClaudeCapPreservesRecoveryIntent(t *testing.T) {
	for _, state := range []session.Status{session.StatusStopped, session.StatusError} {
		t.Run(string(state), func(t *testing.T) {
			f, sid, _ := matrixPrepareClaudeFixture(t)
			f.target.Status = state
			busy := session.NewInstanceWithTool("busy", f.home, "shell")
			busy.GroupPath, busy.Status = f.target.GroupPath, session.StatusRunning
			_, groups, err := f.storage.LoadWithGroups()
			if err != nil {
				t.Fatal(err)
			}
			tree := session.NewGroupTreeWithGroups([]*session.Instance{f.target, busy}, groups)
			tree.Groups[f.target.GroupPath].MaxConcurrent = 1
			if err := f.storage.SaveWithGroups([]*session.Instance{f.target, busy}, tree); err != nil {
				t.Fatal(err)
			}
			out, raw, err := matrixRunPrepare(f)
			if err != nil || out.Disposition != "queued" || out.Status != state || out.MaxConcurrent != 1 || len(f.spawnLines()) != 0 {
				t.Fatalf("Claude cap bypass: %+v %s %v", out, raw, err)
			}
			if saved := f.stored(f.target.ID); saved.Status != state || saved.ClaudeSessionID != sid {
				t.Fatalf("blocked recovery lost original Claude intent: %s %s", saved.Status, saved.ClaudeSessionID)
			}
		})
	}
}

// matrix/hardened: a second live owner is a refusal, never an excuse to run
// Restart's duplicate sweep and kill another terminal with the same native ID.
func TestMatrixPrepareOpenClaudeLiveNativeOwnerIsPreserved(t *testing.T) {
	f, sid, _ := matrixPrepareClaudeFixture(t)
	foreign := "agentdeck_prepare-open-claude-owner"
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("tmux", append([]string{"-L", f.socket}, args...)...)
		cmd.Env = f.env
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %s %v", args, raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	run("new-session", "-d", "-s", foreign, "sleep 600")
	run("set-environment", "-t", "="+foreign, "CLAUDE_SESSION_ID", sid)
	before := run("display-message", "-p", "-t", "="+foreign+":0.0", "#{pane_pid}")
	_, raw, err := matrixRunPrepare(f)
	if err == nil || len(f.spawnLines()) != 0 {
		t.Fatalf("live native owner allowed a second Claude: %s %v", raw, err)
	}
	if after := run("display-message", "-p", "-t", "="+foreign+":0.0", "#{pane_pid}"); after != before {
		t.Fatalf("refusal killed or replaced native owner: %s -> %s", before, after)
	}
	if f.stored(f.target.ID).ClaudeSessionID != sid {
		t.Fatal("native-owner refusal changed original conversation binding")
	}
}

func TestMatrixPrepareOpenExactRecoveryWithoutTTYAndReusesLiveError(t *testing.T) {
	f, sid, _ := matrixPrepareCodexFixture(t)
	out, raw, err := matrixRunPrepare(f)
	if err != nil || out.Disposition != "resumed" || out.CodexSessionID != sid {
		t.Fatalf("%+v %s %v", out, raw, err)
	}
	lines := f.waitSpawn()
	if len(lines) != 1 || !strings.Contains(lines[0], "resume "+sid) || strings.Contains(lines[0], "fork") {
		t.Fatalf("unsafe launch: %v", lines)
	}
	saved := f.stored(f.target.ID)
	if saved.CodexSessionID != sid || saved.Command != "codex" || saved.Wrapper != "" || saved.AutoSleep != nil {
		t.Fatalf("binding or launch plan changed: %+v", saved)
	}
	saved.Status = session.StatusError
	if err := f.storage.Save([]*session.Instance{saved}); err != nil {
		t.Fatal(err)
	}
	// matrix/hardened: state.db stores this timestamp in whole seconds. Seed
	// an older durable visit so a fast second open proves a real update.
	if _, err := f.storage.GetDB().DB().Exec("UPDATE instances SET last_accessed = ? WHERE id = ?", time.Now().Add(-time.Minute).Unix(), f.target.ID); err != nil {
		t.Fatal(err)
	}
	beforeVisit := f.stored(f.target.ID).LastAccessedAt
	out, raw, err = matrixRunPrepare(f)
	if err != nil || out.Disposition != "existing" || len(f.spawnLines()) != 1 {
		t.Fatalf("live cached error restarted: %+v %s %v", out, raw, err)
	}
	if !f.stored(f.target.ID).LastAccessedAt.After(beforeVisit) {
		t.Fatal("existing remote visit did not persist its activity fence")
	}
}

func TestMatrixPrepareOpenConcurrentRecoverySpawnsOnce(t *testing.T) {
	f, _, _ := matrixPrepareCodexFixture(t)
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, raw, err := matrixRunPrepare(f)
			results <- fmt.Sprintf("%s|%v|%s", out.Disposition, err, raw)
		}()
	}
	wg.Wait()
	close(results)
	var dispositions string
	for result := range results {
		if !strings.Contains(result, "|<nil>|") {
			t.Fatal(result)
		}
		dispositions += result + "\n"
	}
	if !strings.Contains(dispositions, "resumed|") || !strings.Contains(dispositions, "existing|") || len(f.waitSpawn()) != 1 {
		t.Fatalf("duplicate recovery: %s %v", dispositions, f.spawnLines())
	}
}

func TestMatrixPrepareOpenInvalidNativeRefusesWithoutFreshOrFork(t *testing.T) {
	for _, kind := range []string{"missing", "subagent", "mismatch", "provider"} {
		t.Run(kind, func(t *testing.T) {
			f, sid, path := matrixPrepareCodexFixture(t)
			switch kind {
			case "missing":
				_ = os.Remove(path)
			case "subagent":
				_ = os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+sid+`","thread_source":"subagent"}}`+"\n"), 0600)
			case "mismatch":
				_ = os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"019f0000-0000-7000-8000-000000008882","thread_source":"user"}}`+"\n"), 0600)
			case "provider":
				f.target.Tool = "claude"
				if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
					t.Fatal(err)
				}
			}
			_, raw, err := matrixRunPrepare(f)
			if err == nil || len(f.spawnLines()) != 0 {
				t.Fatalf("invalid recovery spawned: %s %v", raw, err)
			}
			if f.stored(f.target.ID).CodexSessionID != sid {
				t.Fatal("refusal changed native ID")
			}
		})
	}
}

func TestMatrixPrepareOpenCapPreservesRecoveryIntent(t *testing.T) {
	for _, kind := range []string{"queued", "stopped", "error", "asleep", "pending"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _ := matrixPrepareCodexFixture(t)
			state := session.Status(kind)
			if kind == "asleep" || kind == "pending" {
				state = session.StatusStopped
				f.target.AutoSleep = &session.AutoSleepState{Version: 1, Phase: kind, CodexSessionID: f.target.CodexSessionID}
			}
			f.target.Status = state
			busy := session.NewInstanceWithTool("busy", f.home, "shell")
			busy.GroupPath, busy.Status = f.target.GroupPath, session.StatusRunning
			_, groups, err := f.storage.LoadWithGroups()
			if err != nil {
				t.Fatal(err)
			}
			tree := session.NewGroupTreeWithGroups([]*session.Instance{f.target, busy}, groups)
			tree.Groups[f.target.GroupPath].MaxConcurrent = 1
			if err := f.storage.SaveWithGroups([]*session.Instance{f.target, busy}, tree); err != nil {
				t.Fatal(err)
			}
			out, raw, err := matrixRunPrepare(f)
			if err != nil || out.Disposition != "queued" || out.Status != state || out.MaxConcurrent != 1 || len(f.spawnLines()) != 0 {
				t.Fatalf("cap bypass: %+v %s %v", out, raw, err)
			}
			saved := f.stored(f.target.ID)
			if saved.Status != state || ((kind == "asleep" || kind == "pending") && saved.AutoSleep == nil) {
				t.Fatal("blocked recovery lost strict intent")
			}
		})
	}
}

func TestMatrixPrepareOpenQueuedFirstStartWithoutTTY(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	out, raw, err := matrixRunPrepare(f)
	if err != nil || out.Disposition != "started" || len(f.waitSpawn()) != 1 {
		t.Fatalf("queued first open: %+v %s %v", out, raw, err)
	}
}

func TestMatrixPrepareOpenHelpIsReadOnly(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	before := f.stored(f.target.ID)
	cmd := exec.Command(f.bin, "session", "prepare-open", "--help")
	cmd.Env, cmd.Dir = f.env, f.home
	raw, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(raw), "prepare-open") || len(f.spawnLines()) != 0 {
		t.Fatalf("help mutated or failed: %s %v", raw, err)
	}
	after := f.stored(f.target.ID)
	if before.Status != after.Status || !before.LastAccessedAt.Equal(after.LastAccessedAt) {
		t.Fatal("help changed session state")
	}
}

// matrix/hardened: an auxiliary dead pane is not the AI's primary terminal.
func TestMatrixPrepareOpenLivePrimaryIgnoresDeadAuxiliary(t *testing.T) {
	f, _, _ := matrixPrepareCodexFixture(t)
	if _, raw, err := matrixRunPrepare(f); err != nil {
		t.Fatalf("%s %v", raw, err)
	}
	f.waitSpawn()
	tm := f.stored(f.target.ID).GetTmuxSession()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("tmux", append([]string{"-L", f.socket}, args...)...)
		cmd.Env = f.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	pane := run("split-window", "-d", "-t", "="+tm.Name+":0.0", "-P", "-F", "#{pane_id}", "sleep 1")
	run("set-option", "-p", "-t", pane, "remain-on-exit", "on")
	deadline := time.Now().Add(4 * time.Second)
	for run("display-message", "-p", "-t", pane, "#{pane_dead}") != "1" {
		if time.Now().After(deadline) {
			t.Fatal("test auxiliary pane did not exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, raw, err := matrixRunPrepare(f)
	if err != nil || out.Disposition != "existing" || len(f.spawnLines()) != 1 {
		t.Fatalf("live primary blocked/restarted: %+v %s %v", out, raw, err)
	}
}

func TestMatrixPrepareOpenRetainedDeadPrimaryRefusesWithoutCleanup(t *testing.T) {
	// matrix/hardened: neither provider's exact recovery may erase a retained
	// failed primary pane or silently turn it into another spawn attempt.
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			fixture := matrixPrepareCodexFixture
			if provider == "claude" {
				fixture = matrixPrepareClaudeFixture
			}
			f, _, _ := fixture(t)
			if _, raw, err := matrixRunPrepare(f); err != nil {
				t.Fatalf("%s %v", raw, err)
			}
			f.waitSpawn()
			tm := f.stored(f.target.ID).GetTmuxSession()
			for _, args := range [][]string{
				{"set-option", "-p", "-t", "=" + tm.Name + ":0.0", "remain-on-exit", "on"},
				{"respawn-pane", "-k", "-t", "=" + tm.Name + ":0.0", "exit 7"},
			} {
				cmd := exec.Command("tmux", append([]string{"-L", f.socket}, args...)...)
				cmd.Env = f.env
				if raw, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s %v", raw, err)
				}
			}
			time.Sleep(100 * time.Millisecond)
			_, raw, err := matrixRunPrepare(f)
			if err == nil || !strings.Contains(raw, "retained dead terminal") || len(f.spawnLines()) != 1 {
				t.Fatalf("dead pane restarted: %s %v", raw, err)
			}
			if exists, err := tm.ProbeExists(); err != nil || !exists {
				t.Fatalf("refusal destroyed retained terminal: %v %v", exists, err)
			}
		})
	}
}

// matrix/hardened: historical tool bindings cannot authorize unrelated AI
// teardown when the current session is being recovered as ordinary Codex.
func TestMatrixPrepareOpenCodexRecoveryPreservesForeignClaudePane(t *testing.T) {
	f, _, _ := matrixPrepareCodexFixture(t)
	f.target.ClaudeSessionID = "019f0000-0000-7000-8000-000000008889"
	if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
		t.Fatal(err)
	}
	foreign := "agentdeck_prepare-open-foreign"
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("tmux", append([]string{"-L", f.socket}, args...)...)
		cmd.Env = f.env
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %s %v", args, raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	run("new-session", "-d", "-s", foreign, "sleep 600")
	run("set-environment", "-t", "="+foreign, "CLAUDE_SESSION_ID", f.target.ClaudeSessionID)
	before := run("display-message", "-p", "-t", "="+foreign+":0.0", "#{pane_pid}")
	if _, raw, err := matrixRunPrepare(f); err != nil {
		t.Fatalf("%s %v", raw, err)
	}
	if after := run("display-message", "-p", "-t", "="+foreign+":0.0", "#{pane_pid}"); after != before {
		t.Fatalf("exact Codex recovery replaced a foreign pane: %s -> %s", before, after)
	}
	saved := f.stored(f.target.ID)
	if saved.ClaudeSessionID != f.target.ClaudeSessionID {
		t.Fatal("exact Codex recovery erased historical provider metadata")
	}
	if env := run("show-environment", "-t", "="+saved.GetTmuxSession().Name); strings.Contains(env, "CLAUDE_SESSION_ID=") {
		t.Fatalf("exact Codex recovery propagated a foreign provider binding: %s", env)
	}
}
