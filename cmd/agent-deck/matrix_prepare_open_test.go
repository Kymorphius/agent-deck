package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// matrix/hardened: use the real fork builder, then model a completed first
// launch whose durable Command still contains the one-shot fork script. Each
// primary transcript carries its own native ID; no parent history is selected.
func matrixPrepareCompletedClaudeForkFixture(t *testing.T, liveParent bool) (*matrixQueuedAttachFixture, *session.Instance, string, string) {
	t.Helper()
	f, parentID, parentPath := matrixPrepareClaudeFixture(t)
	parent := f.target
	if liveParent {
		if out, raw, err := matrixRunPrepare(f); err != nil || out.Disposition != "resumed" {
			t.Fatalf("parent setup: %+v %s %v", out, raw, err)
		}
		f.waitSpawn()
		parent = f.stored(parent.ID)
	}
	parent.ClaudeDetectedAt = time.Now()
	forked, command, err := parent.CreateForkedInstanceWithOptions("original fork title", parent.GroupPath, &session.ClaudeOptions{SkipPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	childID := forked.ClaudeSessionID
	if childID == "" || childID == parentID || !strings.Contains(command, "--fork-session") || !strings.Contains(command, "--resume "+parentID) {
		t.Fatalf("fixture is not a native fork: child=%s parent=%s command=%s", childID, parentID, command)
	}
	data, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(filepath.Dir(parentPath), childID+".jsonl")
	if err := os.WriteFile(childPath, []byte(strings.ReplaceAll(string(data), parentID, childID)), 0600); err != nil {
		t.Fatal(err)
	}
	// Renaming after the initial fork must not replay its old --name or the
	// parent's identity injection when this child is opened again.
	forked.Title = "renamed completed fork"
	forked.IsForkAwaitingStart = false
	forked.Status = session.StatusStopped
	if err := f.storage.Save([]*session.Instance{forked}); err != nil {
		t.Fatal(err)
	}
	f.target = forked
	return f, parent, childID, childPath
}

// matrix/hardened: later remote opens resume the existing child conversation,
// retain its legacy launch evidence, and leave the live parent untouched.
func TestMatrixPrepareOpenCompletedClaudeForkResumesChildAndPreservesParent(t *testing.T) {
	f, parent, childID, _ := matrixPrepareCompletedClaudeForkFixture(t, true)
	parentPID, err := parent.GetTmuxSession().PanePID()
	if err != nil {
		t.Fatal(err)
	}
	before := f.stored(f.target.ID)
	out, raw, err := matrixRunPrepare(f)
	if err != nil || out.Disposition != "resumed" {
		t.Fatalf("completed fork recovery: %+v %s %v", out, raw, err)
	}
	lines := matrixPrepareWaitSpawnCount(t, f, 2)
	childLaunch := lines[1]
	if !strings.Contains(childLaunch, "--resume "+childID) || strings.Contains(childLaunch, parent.ClaudeSessionID) || strings.Contains(childLaunch, "--session-id") || strings.Contains(childLaunch, "--fork-session") || strings.Contains(childLaunch, "original fork title") || strings.Contains(childLaunch, filepath.Join("identity", parent.ID, "identity.md")) {
		t.Fatalf("replayed parent/first-fork launch instead of child resume: %v", lines)
	}
	child := f.stored(f.target.ID)
	childPID, err := child.GetTmuxSession().PanePID()
	if err != nil {
		t.Fatal(err)
	}
	if child.ClaudeSessionID != childID || child.Command != before.Command {
		t.Fatalf("completed fork recovery changed original binding/evidence: %s %s", child.ClaudeSessionID, child.Command)
	}
	out, raw, err = matrixRunPrepare(f)
	if err != nil || out.Disposition != "existing" || len(f.spawnLines()) != 2 {
		t.Fatalf("completed fork was launched twice: %+v %s %v spawns=%v", out, raw, err, f.spawnLines())
	}
	if afterPID, err := f.stored(f.target.ID).GetTmuxSession().PanePID(); err != nil || afterPID != childPID {
		t.Fatalf("existing open changed child pane: %d -> %d (%v)", childPID, afterPID, err)
	}
	client := f.attach()
	client.waitReady()
	client.detach()
	if afterPID, err := parent.GetTmuxSession().PanePID(); err != nil || afterPID != parentPID {
		t.Fatalf("child open changed live parent pane: %d -> %d (%v)", parentPID, afterPID, err)
	}
	if after := f.stored(f.target.ID); after.ClaudeSessionID != childID || after.Command != before.Command || len(f.spawnLines()) != 2 {
		t.Fatalf("attach changed completed fork binding or launch count: %s %s %v", after.ClaudeSessionID, after.Command, f.spawnLines())
	}
}

// matrix/hardened: concurrent visits to a completed fork must share one child
// resume, even though the stored command still describes its first fork.
func TestMatrixPrepareOpenCompletedClaudeForkConcurrentRecoverySpawnsOnce(t *testing.T) {
	f, parent, childID, _ := matrixPrepareCompletedClaudeForkFixture(t, false)
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for n := 0; n < 2; n++ {
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
	lines := f.waitSpawn()
	if !strings.Contains(dispositions, "resumed|") || !strings.Contains(dispositions, "existing|") || len(lines) != 1 || !strings.Contains(lines[0], "--resume "+childID) || strings.Contains(lines[0], parent.ClaudeSessionID) || strings.Contains(lines[0], "--fork-session") {
		t.Fatalf("duplicate or foreign completed fork recovery: %s %v", dispositions, lines)
	}
}

// matrix/hardened: a fork-shaped custom command or uncompleted fork cannot
// authorize a new conversation, parent resume, or execution of its old script.
func TestMatrixPrepareOpenCompletedClaudeForkInvalidEvidenceNeverReplays(t *testing.T) {
	for _, kind := range []string{"wrong-child", "missing-history", "extra-shell", "duplicate-selector"} {
		t.Run(kind, func(t *testing.T) {
			f, _, childID, path := matrixPrepareCompletedClaudeForkFixture(t, false)
			marker := filepath.Join(f.home, "old-fork-script-was-executed")
			switch kind {
			case "wrong-child":
				f.target.Command = strings.Replace(f.target.Command, childID, uuid.NewString(), 1)
			case "missing-history":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "extra-shell":
				f.target.Command += "; printf unsafe > '" + marker + "'"
			case "duplicate-selector":
				f.target.Command += " --session-id " + childID
			}
			if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
				t.Fatal(err)
			}
			before := f.stored(f.target.ID)
			_, raw, err := matrixRunPrepare(f)
			if err == nil || len(f.spawnLines()) != 0 {
				t.Fatalf("unsafe completed fork launch: %s %v spawns=%v", raw, err, f.spawnLines())
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("old fork script was executed: %v", err)
			}
			after := f.stored(f.target.ID)
			if after.ClaudeSessionID != childID || after.Command != before.Command || after.Status != before.Status || after.Exists() {
				t.Fatalf("refusal altered completed fork identity/lifecycle: %s/%s %s", after.ClaudeSessionID, after.Status, after.Command)
			}
		})
	}
}

// matrix/hardened: when a live parent already wrote a launch line, the ordinary
// fixture's first-line wait cannot establish that the child actually started.
func matrixPrepareWaitSpawnCount(t *testing.T, f *matrixQueuedAttachFixture, want int) []string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if lines := f.spawnLines(); len(lines) >= want {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing completed fork launch: want %d lines, got %v", want, f.spawnLines())
	return nil
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
// matrix/hardened: on Darwin the tmux server keeps the argv of its first
// new-session, including a long-dead Claude's --resume payload. That server
// must stay alive for its peer terminal without blocking this conversation.
func TestMatrixPrepareOpenClaudeIgnoresTmuxServerHistoricalResume(t *testing.T) {
	f, sid, _ := matrixPrepareClaudeFixture(t)
	f.target.Status = session.StatusStopped
	if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("tmux", append([]string{"-u", "-L", f.socket}, args...)...)
		cmd.Env = f.env
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %s %v", args, raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	seed := f.target.GetTmuxSession().Name
	// The first server command must contain the native selector. Starting an
	// unrelated server first would make this test miss the production failure.
	run("new-session", "-d", "-s", seed, "exec python3 -u '"+f.harness+"' --resume "+sid)
	f.waitSpawn()
	serverPID := run("display-message", "-p", "#{pid}")
	oldPID := run("display-message", "-p", "-t", "="+seed+":0.0", "#{pane_pid}")
	peer := "agentdeck_prepare-open-history-peer"
	run("new-session", "-d", "-s", peer, "sleep 600")
	peerPID := run("display-message", "-p", "-t", "="+peer+":0.0", "#{pane_pid}")
	// Only this test-owned primary disappears; the peer keeps its server alive.
	run("kill-session", "-t", "="+seed)
	deadline := time.Now().Add(3 * time.Second)
	for {
		cmd := exec.Command("ps", "-p", oldPID, "-o", "command=")
		cmd.Env = f.env
		raw, err := cmd.Output()
		if err != nil || !strings.Contains(string(raw), sid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture's old native process still lives: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	serverCommand := exec.Command("ps", "-p", serverPID, "-o", "command=")
	serverCommand.Env = f.env
	serverArgv, err := serverCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("cannot inspect retained tmux server: %s %v", serverArgv, err)
	}
	if !strings.Contains(string(serverArgv), "--resume "+sid) {
		if runtime.GOOS != "darwin" {
			t.Skipf("%s tmux rewrites server argv; no historical native selector to exercise: %s", runtime.GOOS, serverArgv)
		}
		t.Fatalf("Darwin fixture did not retain its first new-session argv: %s", serverArgv)
	}
	if got := run("display-message", "-p", "#{pid}"); got != serverPID {
		t.Fatalf("fixture replaced its tmux server: %s -> %s", serverPID, got)
	}
	out, raw, err := matrixRunPrepare(f)
	if err != nil || out.Disposition != "resumed" {
		t.Fatalf("tmux history blocked missing-primary recovery: %+v %s %v; server=%s", out, raw, err, serverArgv)
	}
	lines := matrixPrepareWaitSpawnCount(t, f, 2)
	if !strings.Contains(lines[1], "--resume "+sid) || strings.Contains(lines[1], "--session-id") || strings.Contains(lines[1], "--fork-session") {
		t.Fatalf("recovery did not preserve original native identity: %v", lines)
	}
	child := f.stored(f.target.ID)
	childPID, err := child.GetTmuxSession().PanePID()
	if err != nil {
		t.Fatal(err)
	}
	out, raw, err = matrixRunPrepare(f)
	if err != nil || out.Disposition != "existing" || len(f.spawnLines()) != 2 {
		t.Fatalf("second open replaced recovered process: %+v %s %v spawns=%v", out, raw, err, f.spawnLines())
	}
	client := f.attach()
	client.waitReady()
	client.detach()
	if got := run("display-message", "-p", "-t", "="+peer+":0.0", "#{pane_pid}"); got != peerPID {
		t.Fatalf("recovery killed or replaced peer pane: %s -> %s", peerPID, got)
	}
	if got := run("display-message", "-p", "#{pid}"); got != serverPID {
		t.Fatalf("recovery replaced tmux server: %s -> %s", serverPID, got)
	}
	if afterPID, err := f.stored(f.target.ID).GetTmuxSession().PanePID(); err != nil || afterPID != childPID {
		t.Fatalf("reopen/attach replaced original child pane: %d -> %d (%v)", childPID, afterPID, err)
	}
	if f.stored(f.target.ID).ClaudeSessionID != sid {
		t.Fatal("recovery changed retained native conversation")
	}
}

// matrix/hardened: excluding a verified tmux server must not exclude an
// actual live native launch whose selector is visible only in process argv.
func TestMatrixPrepareOpenClaudeLiveProcessOwnerWithoutTmuxMarkerIsPreserved(t *testing.T) {
	f, sid, _ := matrixPrepareClaudeFixture(t)
	peer := "agentdeck_prepare-open-process-owner-peer"
	startPeer := exec.Command("tmux", "-L", f.socket, "new-session", "-d", "-s", peer, "sleep 600")
	startPeer.Env = f.env
	if raw, err := startPeer.CombinedOutput(); err != nil {
		t.Fatalf("cannot start process-owner peer: %s %v", raw, err)
	}
	readPeerPID := func() string {
		t.Helper()
		cmd := exec.Command("tmux", "-L", f.socket, "display-message", "-p", "-t", "="+peer+":0.0", "#{pane_pid}")
		cmd.Env = f.env
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("process-owner peer disappeared: %s %v", raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	peerPID := readPeerPID()
	owner := exec.Command("python3", "-u", "-c", "import signal; signal.pause()", "--resume", sid)
	owner.Env = f.env
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = owner.Process.Kill() // Only the provider fixture created by this test.
		_ = owner.Wait()
	})
	readOwner := func() string {
		t.Helper()
		cmd := exec.Command("ps", "-p", fmt.Sprint(owner.Process.Pid), "-o", "command=")
		cmd.Env = f.env
		raw, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(raw), "--resume "+sid) {
			t.Fatalf("native owner fixture disappeared: %s %v", raw, err)
		}
		return string(raw)
	}
	readOwner()
	_, raw, err := matrixRunPrepare(f)
	if err == nil || len(f.spawnLines()) != 0 {
		t.Fatalf("live native process allowed a duplicate resume: %s %v spawns=%v", raw, err, f.spawnLines())
	}
	// macOS's python launcher may exec its app bundle between these reads;
	// the same PID retaining the selector establishes liveness without pinning
	// the launcher's transient executable spelling.
	readOwner()
	if afterPID := readPeerPID(); afterPID != peerPID {
		t.Fatalf("live owner refusal replaced unrelated peer: %s -> %s", peerPID, afterPID)
	}
	if f.stored(f.target.ID).ClaudeSessionID != sid {
		t.Fatal("live process refusal changed original native identity")
	}
}

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
