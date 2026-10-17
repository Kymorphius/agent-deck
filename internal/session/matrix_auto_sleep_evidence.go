package session

// matrix/hardened: automatic sleep is a destructive lifecycle boundary. Every
// observation below fails closed; a waiting status alone never authorizes stop.
import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// AutoSleepEvidence binds a proven normal completion to one live user thread.
// The caller must apply its idle deadline and repeat this inspection while it
// holds the spawn and Codex acceptance locks, immediately before stopping.
type AutoSleepEvidence struct {
	ThreadID    string
	TurnID      string
	CompletedAt time.Time
	Fingerprint string
}

type matrixAutoSleepPane struct {
	PID   int
	ID    string
	Frame string
	Proof string
}

// Test seams remain read-only and are never replaced by production callers.
var matrixAutoSleepPaneProbe = matrixInspectAutoSleepPane
var matrixAutoSleepProcessProbe = matrixInspectAutoSleepProcesses
var matrixAutoSleepLiveID = (*Instance).LiveCodexThreadID
var matrixAutoSleepResume = LocateCodexResume
var matrixAutoSleepOwnership = (*Instance).OwnershipStatus
var matrixAutoSleepCommandProbe = matrixAutoSleepReadCommand
var matrixAutoSleepCodexExecutable = func() (string, error) {
	word, rest, ok := nextShellWord(GetCodexCommand())
	if !ok || strings.TrimSpace(rest) != "" || (filepath.Base(word) != "codex" && filepath.Base(word) != "codex.exe") {
		return "", errors.New("configured Codex command is a custom wrapper")
	}
	command, err := matrixAutoSleepResolveCommand(word, nil)
	return command.Executable, err
}

// InspectAutoSleepCandidate performs fresh, read-only qualification. It takes
// no lifecycle locks itself, so it is safe inside the stop owner's recheck.
func InspectAutoSleepCandidate(inst *Instance, instances []*Instance) (AutoSleepEvidence, error) {
	fail := func(reason string) (AutoSleepEvidence, error) {
		return AutoSleepEvidence{}, fmt.Errorf("auto sleep: %s", reason)
	}
	if inst == nil {
		return fail("missing session")
	}
	inst.mu.RLock()
	id, tool, command, wrapper := inst.ID, inst.Tool, strings.TrimSpace(inst.Command), inst.Wrapper
	sid, account, status := inst.CodexSessionID, inst.Account, inst.Status
	var dbPath string
	if inst.storageSnapshot != nil {
		dbPath = inst.storageSnapshot.dbPath
	}
	excluded := !inst.ArchivedAt.IsZero() || inst.Pin != PinNone || inst.IsSSH() || inst.IsSandboxed() || inst.SandboxContainer != "" || inst.IsConductor || inst.SubcommandPassthrough
	inst.mu.RUnlock()
	if id == "" || tool != "codex" || (command != "" && command != "codex") || wrapper != "" || excluded || inst.UsesTerminalBackend() {
		return fail("only unpinned ordinary local Codex tmux sessions are supported")
	}
	if status != StatusIdle && status != StatusWaiting {
		return fail("session is running or its status is not proven idle")
	}
	if !filepath.IsAbs(dbPath) {
		return fail("session's persisted profile identity is unknown")
	}
	if !IsBareCodexThreadID(sid) || matrixAutoSleepLiveID(inst) != sid {
		return fail("stored thread does not have one exact live process owner")
	}
	for _, other := range instances {
		if other == nil || other.ID == id {
			continue
		}
		other.mu.RLock()
		parent, otherSID, otherStatus, archived := other.ParentSessionID, other.CodexSessionID, other.Status, !other.ArchivedAt.IsZero()
		other.mu.RUnlock()
		if archived {
			continue
		}
		if otherSID == sid || parent == id {
			// Registry statuses are snapshots, including "stopped". A child
			// can have resumed through another TUI since that snapshot.
			s := other.GetTmuxSession()
			if other.UsesTerminalBackend() || s == nil {
				return fail("related session process liveness is unverified")
			}
			alive, err := s.ProbeExists()
			if err != nil || alive || otherStatus != StatusStopped {
				return fail("related session is still active or unverified")
			}
			ownership := other.OwnershipStatus()
			if ownership.LoadErr != nil || (ownership.Receipt != nil && ownership.Report.Verdict != procowner.VerdictClear) {
				return fail("related session may retain owned background processes")
			}
		}
	}
	loc, err := matrixAutoSleepResume(account, sid)
	if err != nil {
		return fail("exact resume unavailable: " + err.Error())
	}
	head, ok := readCodexRolloutHead(loc.Rollout)
	if !ok || !head.UserThread || head.ID != sid || canonicalDir(loc.Home) != canonicalDir(inst.getCodexHomeDir()) {
		return fail("resume artifact is not this account's exact user thread")
	}
	evidence, rolloutProof, err := matrixAutoSleepRollout(loc.Rollout, sid)
	if err != nil {
		return fail(err.Error())
	}
	pane, err := matrixAutoSleepPaneProbe(inst)
	if err != nil {
		return fail(err.Error())
	}
	processProof, err := matrixAutoSleepProcessProbe(inst, pane.PID)
	if err != nil {
		return fail(err.Error())
	}
	queueProof, err := matrixAutoSleepPendingProof(id, sid, filepath.Dir(dbPath))
	if err != nil {
		return fail(err.Error())
	}
	// Bracket slower process/queue probes. A native turn or human attachment
	// that begins during them must invalidate this inspection itself, not
	// merely be left for a future monitor sweep to notice.
	_, confirmedRolloutProof, err := matrixAutoSleepRollout(loc.Rollout, sid)
	if err != nil || confirmedRolloutProof != rolloutProof {
		return fail("native rollout changed during eligibility inspection")
	}
	confirmedPane, err := matrixAutoSleepPaneProbe(inst)
	if err != nil || confirmedPane.PID != pane.PID || confirmedPane.ID != pane.ID || confirmedPane.Frame != pane.Frame || confirmedPane.Proof != pane.Proof {
		return fail("composer, pane or attached clients changed during inspection")
	}
	if matrixAutoSleepLiveID(inst) != sid {
		return fail("live thread changed during inspection")
	}
	h := sha256.Sum256([]byte(strings.Join([]string{sid, rolloutProof, pane.Proof, pane.Frame, processProof, queueProof}, "\x00")))
	evidence.Fingerprint = hex.EncodeToString(h[:])
	return evidence, nil
}

// matrix/hardened: read every record, with explicit bounds, because an old
// still-running native agent must not disappear behind a tail-only scan.
func matrixParseAutoSleepRollout(path, threadID string) (AutoSleepEvidence, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return AutoSleepEvidence{}, "", fmt.Errorf("rollout unreadable: %w", err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > 64<<20 {
		return AutoSleepEvidence{}, "", errors.New("rollout is nonregular, unreadable or exceeds inspection bound")
	}
	r := bufio.NewReaderSize(f, 64<<10)
	hash := sha256.New()
	var evidence AutoSleepEvidence
	evidence.ThreadID = threadID
	completed := false
	agents := matrixAutoSleepAgents{calls: map[string]matrixAutoSleepAgentCall{}, active: map[string]bool{}}
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil || len(line) > 16<<20 || line[len(line)-1] != '\n' || len(bytes.TrimSpace(line)) == 0 {
			return AutoSleepEvidence{}, "", errors.New("rollout has a partial, oversized or unreadable record")
		}
		_, _ = hash.Write(line)
		var record struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil || record.Type == "" || len(record.Payload) == 0 || !json.Valid(record.Payload) {
			return AutoSleepEvidence{}, "", errors.New("rollout contains malformed JSON")
		}
		switch record.Type {
		case "session_meta", "event_msg", "response_item", "turn_context", "compacted":
		default:
			return AutoSleepEvidence{}, "", errors.New("rollout contains an unknown native record type")
		}
		if record.Type == "event_msg" {
			var ev struct {
				Type   string          `json:"type"`
				TurnID string          `json:"turn_id"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(record.Payload, &ev) != nil || ev.Type == "" {
				return AutoSleepEvidence{}, "", errors.New("rollout has an invalid native event")
			}
			switch ev.Type {
			case "task_complete", "turn_complete":
				at, parseErr := time.Parse(time.RFC3339Nano, record.Timestamp)
				completed = parseErr == nil && !at.IsZero() && at.Before(time.Now().Add(time.Minute)) && ev.TurnID != "" && (len(ev.Error) == 0 || string(ev.Error) == "null")
				evidence.TurnID, evidence.CompletedAt = ev.TurnID, at
			case "token_count", "context_compacted":
				// Non-action metadata may follow a durable normal completion.
			default:
				// New input, a started/aborted turn, an approval, a tool event,
				// or an unknown event cannot preserve a prior idle verdict.
				completed = false
			}
		}
		if record.Type == "turn_context" || record.Type == "compacted" {
			completed = false
		}
		if record.Type == "response_item" {
			if err := agents.observe(record.Payload); err != nil {
				return AutoSleepEvidence{}, "", err
			}
			// Any actual input/tool operation after completion invalidates it.
			// Ordinary assistant output is followed by native task_complete.
			completed = false
		}
	}
	after, err := f.Stat()
	current, pathErr := os.Stat(path)
	if err != nil || pathErr != nil || !os.SameFile(before, current) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return AutoSleepEvidence{}, "", errors.New("rollout changed during inspection")
	}
	if !completed {
		return AutoSleepEvidence{}, "", errors.New("latest native turn is not a complete normal completion")
	}
	if len(agents.active) > 0 || len(agents.calls) > 0 {
		return AutoSleepEvidence{}, "", errors.New("native child agent has no proven terminal result")
	}
	return evidence, fmt.Sprintf("%d/%d/%x", before.Size(), before.ModTime().UnixNano(), hash.Sum(nil)), nil
}

type matrixAutoSleepAgentCall struct{ kind, id string }
type matrixAutoSleepAgents struct {
	calls  map[string]matrixAutoSleepAgentCall
	active map[string]bool
}

// matrix/hardened: only structured agent lifecycle results retire native
// children; a parent's final prose and task_complete do not prove child exit.
func (a *matrixAutoSleepAgents) observe(payload []byte) error {
	var item struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(payload, &item) != nil || item.Type == "" {
		return errors.New("rollout has an invalid response item")
	}
	if item.Type == "function_call" && (item.Name == "spawn_agent" || item.Name == "close_agent" || item.Name == "wait_agent") {
		if item.CallID == "" {
			return errors.New("native agent call has no identity")
		}
		var args struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(item.Arguments), &args) != nil {
			return errors.New("native agent call arguments are malformed")
		}
		a.calls[item.CallID] = matrixAutoSleepAgentCall{item.Name, args.ID}
	}
	if item.Type != "function_call_output" {
		return nil
	}
	call, relevant := a.calls[item.CallID]
	if !relevant {
		return nil
	}
	var text string
	data := []byte(item.Output)
	if json.Unmarshal(item.Output, &text) == nil {
		data = []byte(text)
	}
	var out struct {
		AgentID string                     `json:"agent_id"`
		Error   json.RawMessage            `json:"error"`
		Status  map[string]json.RawMessage `json:"status"`
	}
	if json.Unmarshal(data, &out) != nil || (len(out.Error) > 0 && string(out.Error) != "null") {
		return errors.New("native agent result is not verifiable")
	}
	switch call.kind {
	case "spawn_agent":
		if out.AgentID == "" {
			return errors.New("spawned native agent has no durable identity")
		}
		a.active[out.AgentID] = true
	case "close_agent":
		if call.id == "" || !a.active[call.id] {
			return errors.New("closed native agent identity is unknown")
		}
		// close_agent returns the previous status, but a successful native
		// tool result acknowledges the close operation itself.
		delete(a.active, call.id)
	case "wait_agent":
		for id, value := range out.Status {
			var status map[string]json.RawMessage
			if json.Unmarshal(value, &status) == nil {
				if _, ok := status["completed"]; ok {
					delete(a.active, id)
				}
				if _, ok := status["errored"]; ok {
					delete(a.active, id)
				}
			}
		}
	}
	delete(a.calls, item.CallID)
	return nil
}

func matrixInspectAutoSleepPane(inst *Instance) (matrixAutoSleepPane, error) {
	s := inst.GetTmuxSession()
	if s == nil {
		return matrixAutoSleepPane{}, errors.New("tmux session unavailable")
	}
	out, err := tmux.OutputBounded(inst.TmuxSocketName, "list-panes", "-s", "-t", "="+s.Name, "-F", "#{pane_id}\t#{pane_pid}\t#{pane_dead}")
	if err != nil {
		return matrixAutoSleepPane{}, fmt.Errorf("pane inventory unavailable: %w", err)
	}
	rows := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(rows) != 1 {
		return matrixAutoSleepPane{}, errors.New("session has multiple or unknown panes")
	}
	cols := strings.Split(rows[0], "\t")
	if len(cols) != 3 || !strings.HasPrefix(cols[0], "%") || cols[2] != "0" {
		return matrixAutoSleepPane{}, errors.New("pane identity or liveness unavailable")
	}
	pid, err := strconv.Atoi(cols[1])
	if err != nil || pid <= 1 {
		return matrixAutoSleepPane{}, errors.New("pane process identity unavailable")
	}
	clients, err := tmux.OutputBounded(inst.TmuxSocketName, "list-clients", "-t", "="+s.Name, "-F", "#{client_control_mode}\t#{client_flags}")
	if err != nil {
		return matrixAutoSleepPane{}, fmt.Errorf("attached client inventory unavailable: %w", err)
	}
	if err := matrixAutoSleepClients(string(clients)); err != nil {
		return matrixAutoSleepPane{}, err
	}
	frame, err := s.CapturePaneFresh()
	if err != nil {
		return matrixAutoSleepPane{}, fmt.Errorf("fresh composer capture unavailable: %w", err)
	}
	if err := matrixAutoSleepComposer(frame); err != nil {
		return matrixAutoSleepPane{}, err
	}
	return matrixAutoSleepPane{PID: pid, ID: cols[0], Frame: frame, Proof: string(out) + "\x00" + string(clients)}, nil
}

func matrixAutoSleepClients(output string) error {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 2 || cols[0] != "1" {
			return errors.New("an interactive or unverified client is attached")
		}
		flags := "," + cols[1] + ","
		if !strings.Contains(flags, ",read-only,") || !strings.Contains(flags, ",ignore-size,") {
			return errors.New("a writable control client is attached")
		}
	}
	return nil
}

func matrixAutoSleepComposer(frame string) error {
	if !matrixCapacityComposerReady(frame) {
		return errors.New("composer is busy, has a draft, approval or menu")
	}
	plain := strings.ToLower(tmux.StripANSI(frame))
	for _, marker := range []string{"queued follow-up", "queued input", "background terminal", "background task", "[image", "[attachment", "attached image", "attached file"} {
		if strings.Contains(plain, marker) {
			return errors.New("pane shows queued input, an attachment or background work")
		}
	}
	return nil
}

// matrix/hardened: do not call readers that skip corrupt inbox/queue records or
// expire a held send as a side effect. Unknown durable delivery always blocks.
func matrixAutoSleepPendingProof(id, sid, profileDir string) (string, error) {
	var proof strings.Builder
	// Resolve the canonical data roots explicitly. Convenience readers fall
	// back to /tmp on a path-resolution error, which is not absence proof.
	if _, err := dataPath("inboxes", "inboxes"); err != nil {
		return "", fmt.Errorf("inbox location unavailable: %w", err)
	}
	if _, err := runtimeDataPath("inbox-inflight"); err != nil {
		return "", fmt.Errorf("inflight location unavailable: %w", err)
	}
	for _, path := range []string{InboxPathFor(id), inboxInflightPathFor(id)} {
		data, err := matrixAutoSleepReadFile(path, 16<<20)
		if err != nil {
			return "", fmt.Errorf("pending delivery record unreadable: %w", err)
		}
		if len(bytes.TrimSpace(data)) != 0 {
			return "", errors.New("pending inbox, inflight or held send exists")
		}
		proof.WriteString(path + "\x00")
	}
	for _, name := range []string{"held-send", "held-send-ledger"} {
		dir, err := runtimeDataPath(name)
		if err != nil {
			return "", fmt.Errorf("held send location unavailable: %w", err)
		}
		path := filepath.Join(dir, sanitizeInboxName(id)+".json")
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("a held send exists or its absence cannot be verified")
		}
		proof.WriteString(path + "\x00")
	}
	locks, err := resolveLocksDirForSpawnLock()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(sid))
	marker := filepath.Join(locks, "codex-submissions", hex.EncodeToString(digest[:])+".json")
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("a Codex submission is unresolved or unreadable")
	}
	if !filepath.IsAbs(profileDir) {
		return "", errors.New("persisted send queue profile unavailable")
	}
	queueDir := sendqueue.Dir(profileDir)
	entries, err := os.ReadDir(queueDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("durable send queue unreadable: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := matrixAutoSleepReadFile(filepath.Join(queueDir, entry.Name()), 16<<20)
		if err != nil {
			return "", fmt.Errorf("durable send queue record unreadable: %w", err)
		}
		var record sendqueue.Record
		if json.Unmarshal(data, &record) != nil || record.SessionID == "" || record.SendID == "" {
			return "", errors.New("durable send queue contains malformed records")
		}
		switch record.State {
		case sendqueue.StateQueued, sendqueue.StateTyping, sendqueue.StateTyped, sendqueue.StateSubmitted, sendqueue.StateLanded, sendqueue.StateFailed:
		default:
			return "", errors.New("durable send queue contains an unknown delivery state")
		}
		if record.SessionID == id {
			// matrix/hardened: Final also means the queue stopped observing an
			// uncertain typed/submitted send (Settled). That is not proof its
			// input landed or was rejected, so it never authorizes sleep.
			if record.State != sendqueue.StateLanded && record.State != sendqueue.StateFailed {
				return "", errors.New("durable send queue has unfinished delivery")
			}
			proof.Write(data)
		}
	}
	return proof.String(), nil
}

func matrixAutoSleepReadFile(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("record is nonregular or exceeds inspection bound")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, errors.New("record changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("record unreadable or oversized")
	}
	after, err := f.Stat()
	if err != nil || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("record changed while reading")
	}
	return data, nil
}

// matrixAutoSleepMCPCommands intentionally decodes only public command/argv
// metadata. It never materializes MCP env, headers or credential fields.
func matrixAutoSleepMCPCommands(home string) ([]matrixAutoSleepCommand, error) {
	data, err := matrixAutoSleepReadFile(filepath.Join(home, "config.toml"), 4<<20)
	if err != nil {
		return nil, fmt.Errorf("MCP command metadata unreadable: %w", err)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, errors.New("MCP command metadata is malformed")
	}
	var commands []matrixAutoSleepCommand
	for _, server := range cfg.Servers {
		if server.Command == "" {
			continue
		}
		cmd, err := matrixAutoSleepResolveCommand(server.Command, server.Args)
		if err != nil {
			// An unrelated unsupported config never blesses a process. A
			// descendant that needs it will be rejected as unknown below.
			continue
		}
		commands = append(commands, cmd)
	}
	return commands, nil
}

func matrixInspectAutoSleepProcesses(inst *Instance, panePID int) (string, error) {
	status := matrixAutoSleepOwnership(inst)
	if status.LoadErr != nil || status.Receipt == nil || status.Receipt.InstanceID != inst.ID || status.Receipt.Generation == 0 || status.Receipt.State != procowner.StateLive || status.Report.Verdict != procowner.VerdictOwned || !status.PaneAttached || len(status.Survivors) != 0 || status.Receipt.Leader.PID != panePID {
		return "", errors.New("running process tree has no complete verified spawn receipt")
	}
	prober := ownershipProber
	root, err := prober.Inspect(panePID)
	if err != nil {
		return "", errors.New("pane process start identity unavailable")
	}
	descendants, err := prober.Descendants(root)
	if err != nil {
		return "", errors.New("pane process descendants are unverified")
	}
	processes := append([]procowner.ProcInfo{root}, descendants...)
	owned := map[string]bool{}
	for _, member := range status.Report.Owned() {
		owned[member.Key()] = true
	}
	commands, err := matrixAutoSleepMCPCommands(inst.getCodexHomeDir())
	if err != nil {
		return "", err
	}
	codexExecutable, err := matrixAutoSleepCodexExecutable()
	if err != nil {
		return "", errors.New("ordinary configured Codex executable cannot be verified")
	}
	var proof []string
	byPID := map[int]procowner.ProcInfo{}
	commandsByPID := map[int]matrixAutoSleepCommand{}
	codexPID := 0
	for _, process := range processes {
		byPID[process.PID] = process
		key := fmt.Sprintf("%d/%s", process.PID, process.StartID)
		if !owned[key] {
			return "", fmt.Errorf("process %d was not recorded in this spawn receipt", process.PID)
		}
		command, err := matrixAutoSleepCommandProbe(process.PID)
		if err != nil {
			return "", fmt.Errorf("process %d executable or exact argv unavailable", process.PID)
		}
		commandsByPID[process.PID] = command
		if command.Executable == codexExecutable {
			if codexPID != 0 {
				return "", errors.New("pane has multiple live Codex executables")
			}
			codexPID = process.PID
		}
	}
	if codexPID == 0 {
		return "", errors.New("ordinary Codex executable is absent from the owned tree")
	}
	for _, process := range processes {
		key := fmt.Sprintf("%d/%s", process.PID, process.StartID)
		command := commandsByPID[process.PID]
		switch {
		case process.PID == codexPID:
			// Exact live thread ownership above establishes the Codex process;
			// no sibling backend or app-server process is allowed in this tree.
			if command.Executable != codexExecutable {
				return "", errors.New("live Codex executable is a custom wrapper")
			}
			if codexPID != panePID && process.PPID != panePID {
				return "", errors.New("Codex is behind an unverified process wrapper")
			}
		case process.PID == panePID:
			// Permit only the one pane shell above Codex, never a subshell.
			if !isShellBinary(filepath.Base(command.Executable)) || byPID[codexPID].PPID != panePID || len(processes) < 2 {
				return "", errors.New("pane leader is not the direct Codex parent")
			}
		default:
			if process.PPID != codexPID || !matrixAutoSleepConfiguredMCP(command, commands) {
				return "", fmt.Errorf("process %d is background work or an unrecognized MCP descendant", process.PID)
			}
		}
		// Identity must bracket the argv read; no pid-only allowlist survives
		// exec/recycle or a changed parent while the inspection is running.
		current, err := prober.Inspect(process.PID)
		if err != nil || current.StartID != process.StartID || current.UID != process.UID || current.PPID != process.PPID {
			return "", fmt.Errorf("process %d changed identity while inspected", process.PID)
		}
		confirmed, err := matrixAutoSleepCommandProbe(process.PID)
		if err != nil || !matrixAutoSleepConfiguredMCP(command, []matrixAutoSleepCommand{confirmed}) {
			return "", fmt.Errorf("process %d changed executable while inspected", process.PID)
		}
		encoded, _ := json.Marshal(command)
		proof = append(proof, key+"/"+strconv.Itoa(process.PPID)+"/"+string(encoded))
	}
	sort.Strings(proof)
	var receiptProof []string
	for _, member := range status.Receipt.All() {
		receiptProof = append(receiptProof, member.Key())
	}
	sort.Strings(receiptProof)
	return fmt.Sprintf("%d/%s/%s/%s", status.Receipt.Generation, status.Receipt.Leader.Key(), strings.Join(receiptProof, "\x00"), strings.Join(proof, "\x00")), nil
}
