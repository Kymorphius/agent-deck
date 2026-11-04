package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/shellwords"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: missing-terminal recovery must never use the ordinary
// Claude builder's fresh fallback, UUID discovery, or duplicate-pane sweep.
type exactClaudeResumePlan struct {
	sid, home, cwd, executable, artifact, artifactHash, commandHash string
	storage                                                         *Storage // non-nil only for a real locked recovery, not a pure plan
}

// matrix/hardened: both exact providers implement the same final command seam;
// ordinary Start and Restart continue to pass nil and retain their contracts.
type exactNativeResumePlan interface {
	command(*Instance) (string, error)
}

func newExactClaudeResumePlan(inst *Instance) (*exactClaudeResumePlan, error) {
	if inst == nil || inst.Tool != "claude" || inst.SSHHost != "" || inst.IsSandboxed() || inst.UsesTerminalBackend() || inst.SubcommandPassthrough || inst.IsForkAwaitingStart || inst.hasAutoSleepState() || inst.crossHarnessLaunch != nil || inst.hasEffectiveWrapper() || inst.ParentSessionID != "" || inst.IsConductor || inst.WorkerScratchConfigDir != "" {
		return nil, fmt.Errorf("exact recovery requires an ordinary local Claude session without wrapper, child scratch, or alternate backend; use explicit restart for custom launches")
	}
	// matrix/hardened: plugin/channel isolation may require a scratch config
	// even when its previous ephemeral path was not persisted. Never bypass it.
	if inst.NeedsWorkerScratchConfigDir() {
		return nil, fmt.Errorf("exact recovery requires the original ordinary Claude config; use explicit restart for plugin or channel scratch isolation")
	}
	if !IsBareClaudeSessionUUID(inst.ClaudeSessionID) || inst.recordedClaudeSessionID() != inst.ClaudeSessionID {
		return nil, fmt.Errorf("exact original Claude session ID is missing or unverified")
	}
	if strings.TrimSpace(inst.Command) != "" && strings.TrimSpace(inst.Command) != "claude" && !exactClaudeCompletedForkCommand(inst) {
		return nil, fmt.Errorf("exact recovery requires the ordinary Claude command")
	}
	command := strings.TrimSpace(GetClaudeCommandForInstance(inst))
	word, rest, ok := nextShellWord(command)
	if !ok || strings.TrimSpace(rest) != "" || filepath.Base(word) != "claude" {
		return nil, fmt.Errorf("exact recovery requires the ordinary Claude executable")
	}
	if err := exactClaudeExtraArgs(inst.ExtraArgs); err != nil {
		return nil, err
	}
	if len(inst.ToolOptionsJSON) != 0 {
		opts, err := UnmarshalClaudeOptions(inst.ToolOptionsJSON)
		if err != nil {
			return nil, fmt.Errorf("invalid Claude launch options: %w", err)
		}
		if opts.SessionMode != "" && opts.SessionMode != "new" && opts.SessionMode != "resume" {
			return nil, fmt.Errorf("exact recovery refuses an interactive/latest Claude selector")
		}
		if opts.ResumeSessionID != "" && opts.ResumeSessionID != inst.ClaudeSessionID {
			return nil, fmt.Errorf("Claude resume option selects another conversation")
		}
	}
	cwd := inst.EffectiveWorkingDir()
	if !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("original Claude working directory must be absolute")
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("original Claude working directory unavailable: %w", err)
	}
	if st, err := os.Stat(realCwd); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("original Claude working directory unavailable")
	}
	home, err := filepath.EvalSymlinks(GetClaudeConfigDirForInstance(inst))
	if err != nil || !filepath.IsAbs(home) {
		return nil, fmt.Errorf("original Claude config directory unavailable")
	}
	artifact, hash, err := exactClaudeTranscript(home, cwd, realCwd, inst.ClaudeSessionID)
	if err != nil {
		return nil, err
	}
	p := &exactClaudeResumePlan{sid: inst.ClaudeSessionID, home: home, cwd: cwd, executable: word, artifact: artifact, artifactHash: hash}
	// Resolve defaults, model, permission flags and env-source setup now; any
	// change between admission and final assembly invalidates the frozen plan.
	// matrix/hardened: a completed fork's historical one-shot script is never
	// executed, but changing it still invalidates the frozen launch binding.
	sum := sha256.Sum256([]byte(p.assemble(inst) + "\x00" + inst.Command))
	p.commandHash = hex.EncodeToString(sum[:])
	return p, nil
}

// matrix/hardened: Claude forks persist the builder's one-shot script in
// Command even after the transient fork sentinel is consumed. Accept only its
// exact launch-scope prefix and literal argument grammar. Resume never runs
// this script or any of its historical flags: it resumes the verified child.
func exactClaudeCompletedForkCommand(inst *Instance) bool {
	prefix := fmt.Sprintf("cd '%s' && %sexec claude ", inst.ProjectPath, inst.buildBashExportPrefix(false))
	if !strings.HasPrefix(inst.Command, prefix) {
		return false
	}
	args, ok := exactClaudeForkLiteralArgs(strings.TrimPrefix(inst.Command, prefix))
	if !ok || len(args) < 5 || args[0] != "--session-id" || args[1] != inst.ClaudeSessionID || args[2] != "--resume" || !IsBareClaudeSessionUUID(args[3]) || strings.EqualFold(args[3], inst.ClaudeSessionID) || args[4] != "--fork-session" {
		return false
	}
	seen := map[string]bool{}
	for n := 5; n < len(args); n++ {
		flag, value, equal := strings.Cut(args[n], "=")
		if seen[flag] && flag != "--add-dir" {
			return false
		}
		seen[flag] = true
		switch flag {
		case "--model", "--effort", "--permission-mode", "--add-dir", "--name", "--teammate-mode", "--channels", "--append-system-prompt-file":
			if !equal {
				n++
				if n >= len(args) {
					return false
				}
				value = args[n]
			}
			if value == "" || strings.HasPrefix(value, "-") || (flag == "--append-system-prompt-file" && !filepath.IsAbs(value)) {
				return false
			}
		case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--chrome", "--no-chrome", "--verbose":
			if equal {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// matrix/hardened: shellwords only splits; reject expansion and operators
// before splitting so a suffix cannot masquerade as another literal flag.
// Single-quoted metacharacters in old titles/paths remain harmless literals.
func exactClaudeForkLiteralArgs(command string) ([]string, bool) {
	var quote byte
	escaped := false
	for n := 0; n < len(command); n++ {
		c := command[n]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '$' || c == '`' || (quote == 0 && strings.ContainsRune(";|&<>(){}*?[]~\n\r", rune(c))) {
			return nil, false
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
		} else if c == '\'' || c == '"' {
			quote = c
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	return shellwords.Split(command)
}

// matrix/hardened: ExtraArgs may preserve known inert launch settings, but
// may not inject a prompt, another selector, config store, or remote dispatch.
func exactClaudeExtraArgs(args []string) error {
	for n := 0; n < len(args); n++ {
		flag, _, equal := strings.Cut(args[n], "=")
		switch flag {
		case "--model", "--effort", "--permission-mode", "--add-dir", "--name":
			if !equal {
				n++
				if n >= len(args) || strings.HasPrefix(args[n], "-") {
					return fmt.Errorf("incomplete Claude launch argument")
				}
			}
		case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--chrome", "--no-chrome", "--verbose":
			if equal {
				return fmt.Errorf("unsupported Claude launch argument")
			}
		default:
			return fmt.Errorf("exact recovery refuses custom Claude launch arguments; use explicit restart")
		}
	}
	return nil
}

func exactClaudeTranscript(home, cwd, realCwd, sid string) (string, string, error) {
	candidates := map[string]bool{}
	for _, dir := range []string{cwd, realCwd} {
		for _, slug := range []string{SlugifyClaudeProjectPath(dir), ConvertToClaudeDirName(dir)} {
			path := filepath.Join(home, "projects", slug, sid+".jsonl")
			st, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !st.Mode().IsRegular() {
				return "", "", fmt.Errorf("original Claude transcript is unreadable or not a regular file")
			}
			actual, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", "", fmt.Errorf("original Claude transcript path is unreadable")
			}
			candidates[actual] = true
		}
	}
	if len(candidates) != 1 {
		return "", "", fmt.Errorf("exact recovery needs one original Claude transcript in its working-directory scope; found %d", len(candidates))
	}
	var path string
	for candidate := range candidates {
		path = candidate
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", "", fmt.Errorf("original Claude transcript changed before read")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", "", fmt.Errorf("original Claude transcript identity changed before read")
	}
	hash := sha256.New()
	reader := bufio.NewReader(io.TeeReader(f, hash))
	conversation, launchCwd := false, false
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) != 0 {
			var row struct {
				Type      string          `json:"type"`
				SessionID string          `json:"sessionId"`
				Sidechain bool            `json:"isSidechain"`
				Cwd       string          `json:"cwd"`
				Message   json.RawMessage `json:"message"`
			}
			if json.Unmarshal(line, &row) != nil {
				return "", "", fmt.Errorf("original Claude transcript contains an unreadable record")
			}
			if row.Type == "user" || row.Type == "assistant" {
				if row.SessionID != sid || row.Sidechain {
					return "", "", fmt.Errorf("original Claude transcript contains a foreign or subagent conversation")
				}
				if row.Cwd != "" {
					rel, err := filepath.Rel(realCwd, resolveRealPath(row.Cwd))
					if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
						return "", "", fmt.Errorf("original Claude transcript records a foreign working directory")
					}
					if rel == "." {
						launchCwd = true
					}
				}
				var message struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}
				if json.Unmarshal(row.Message, &message) == nil && message.Role == row.Type && len(message.Content) > 0 && string(message.Content) != "null" && string(message.Content) != "\"\"" && string(message.Content) != "[]" {
					conversation = true
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", "", fmt.Errorf("read original Claude transcript: %w", readErr)
		}
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", "", fmt.Errorf("original Claude transcript changed while reading")
	}
	if !conversation || !launchCwd {
		return "", "", fmt.Errorf("original Claude transcript has no main conversation in the launch directory")
	}
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}

func (p *exactClaudeResumePlan) assemble(inst *Instance) string {
	c := inst.detachedClaudeLaunchInstance()
	opts := c.GetClaudeOptions()
	if opts == nil {
		cfg, _ := LoadUserConfig()
		opts = NewClaudeOptions(cfg)
	}
	// The final export runs after configured env files/init scripts and defeats
	// a stale inherited ticket or a sourced CLAUDE_CONFIG_DIR override.
	return c.buildEnvSourceCommand() + c.buildBashExportPrefix(false) + "unset " + claudeLaunchTicketEnv + "; export CLAUDE_CONFIG_DIR=" + shellescape.Quote(p.home) + "; export CLAUDE_SESSION_ID=" + shellescape.Quote(p.sid) + "; cd -- " + shellescape.Quote(p.cwd) + " || exit 78; exec " + shellescape.Quote(p.executable) + " --resume " + shellescape.Quote(p.sid) + c.buildClaudeExtraFlags(opts)
}

func (p *exactClaudeResumePlan) command(inst *Instance) (string, error) {
	current, err := newExactClaudeResumePlan(inst)
	if err != nil {
		return "", err
	}
	current.storage = p.storage
	if *current != *p {
		return "", fmt.Errorf("original Claude identity, transcript or launch configuration changed before resume")
	}
	if p.storage != nil {
		if err := exactClaudeStoredBinding(p.storage, inst); err != nil {
			return "", err
		}
		exists, err := inst.tmuxSession.ProbeExists()
		if err != nil {
			return "", fmt.Errorf("cannot recheck Claude terminal absence: %w", err)
		}
		if exists {
			return "", fmt.Errorf("Claude terminal appeared before command assembly; nothing was restarted")
		}
		if err := guardExactClaudeNativeOwner(inst, p); err != nil {
			return "", err
		}
	}
	return p.assemble(inst), nil
}

// matrix/hardened: strict resumes carry no new-session reservation. They still
// use the detached admission application seam, but cannot invoke a builder
// that mints a UUID, migrates an artifact, or reserves a new/fork budget.
func (p *exactClaudeResumePlan) admission(inst *Instance) (*claudeLaunchAdmission, error) {
	command, err := p.command(inst)
	if err != nil {
		return nil, err
	}
	c := inst.detachedClaudeLaunchInstance()
	c.claudeLaunchKind = "resume"
	return &claudeLaunchAdmission{command: command, planned: c}, nil
}

// matrix/hardened: serialize same-native-ID recoveries as well as the ordinary
// per-instance lock, and refuse an already-open conversation without signals.
func guardExactClaudeNativeOwner(inst *Instance, p *exactClaudeResumePlan) error {
	names, err := tmux.ListAgentDeckSessionsOnSocket(inst.tmuxSession.SocketName)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exactClaudeNoServer(string(exit.Stderr)) {
			return fmt.Errorf("cannot establish other Claude terminal ownership: %w", err)
		}
		names = nil
	}
	for _, name := range names {
		out, err := tmux.OutputBounded(inst.tmuxSession.SocketName, "show-environment", "-t", "="+name)
		if err != nil {
			return fmt.Errorf("cannot read other terminal identity: %w", err)
		}
		for _, value := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if value == "CLAUDE_SESSION_ID="+p.sid || value == "AGENTDECK_INSTANCE_ID="+inst.ID {
				return fmt.Errorf("original Claude conversation is already owned by another terminal; nothing was started or signalled")
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return fmt.Errorf("cannot establish original Claude process absence: %w", err)
	}
	// matrix/hardened: a tmux server can retain its first new-session argv
	// after that pane exits. Only the socket-confirmed server is exempt;
	// real launchers and same-ID Claude processes must continue to block.
	if err := exactClaudeCheckNativeArgvOwners(out, p.sid, func(pid int) (bool, error) {
		return exactClaudeConfirmTmuxServer(pid, func() (int, error) {
			data, err := tmux.OutputBounded(inst.tmuxSession.SocketName, "display-message", "-p", "#{pid}")
			if err != nil {
				return 0, err
			}
			serverPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || serverPID <= 1 {
				return 0, fmt.Errorf("invalid tmux server process identity")
			}
			return serverPID, nil
		}, func(pid int) (string, error) {
			data, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
			return strings.TrimSpace(string(data)), err
		})
	}); err != nil {
		return err
	}
	parents, err := adoptProcessParents()
	if err != nil {
		return err
	}
	// A live Claude may have changed its ID interactively, so argv alone is
	// insufficient. Read the actual validated store's native session registry.
	sessions := filepath.Join(p.home, "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot read Claude native process registry: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessions, entry.Name()))
		if err != nil {
			return fmt.Errorf("cannot read Claude native process record: %w", err)
		}
		var proc claudeProcess
		if json.Unmarshal(data, &proc) != nil {
			return fmt.Errorf("Claude native process record is unreadable")
		}
		if proc.SessionID == p.sid && proc.PID > 1 {
			if _, live := parents[proc.PID]; live {
				return fmt.Errorf("original Claude conversation is still registered to a live process")
			}
		}
	}
	return nil
}

// matrix/hardened: argv text alone is not process ownership. In particular,
// tmux's daemon argv describes the first pane, not a currently running Claude.
// Every matching non-server process remains a conservative duplicate refusal.
func exactClaudeCheckNativeArgvOwners(table []byte, sid string, isServer func(int) (bool, error)) error {
	for _, line := range strings.Split(string(table), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Unrelated ps rows (including kernel/empty-command rows) provide no
		// conversation evidence and need no PID or server-role exception.
		if !strings.Contains(line, sid) || !(strings.Contains(line, "--resume") || strings.Contains(line, "--session-id") || strings.Contains(line, " -r ")) {
			continue
		}
		separator := strings.IndexAny(line, " \t")
		if separator < 1 {
			return fmt.Errorf("cannot establish original Claude process absence: unreadable process table")
		}
		pid, err := strconv.Atoi(line[:separator])
		if err != nil || pid <= 0 {
			return fmt.Errorf("cannot establish original Claude process absence: invalid process ID")
		}
		server, err := isServer(pid)
		if err != nil {
			return fmt.Errorf("cannot establish original Claude process absence: %w", err)
		}
		if !server {
			return fmt.Errorf("original Claude conversation still has a native process; nothing was started or signalled")
		}
	}
	return nil
}

// matrix/hardened: never exempt a tmux-looking launcher by its name. The
// candidate must be the live server behind this exact socket, independently
// identified as tmux, and still the same server after the process-name read.
func exactClaudeConfirmTmuxServer(pid int, serverPID func() (int, error), processName func(int) (string, error)) (bool, error) {
	before, err := serverPID()
	if err != nil {
		return false, err
	}
	if before != pid {
		return false, nil
	}
	comm, err := processName(pid)
	if err != nil {
		return false, err
	}
	switch filepath.Base(strings.TrimSpace(comm)) {
	case "tmux", "tmux: server":
	default:
		return false, fmt.Errorf("tmux server executable identity is unreadable or changed")
	}
	after, err := serverPID()
	if err != nil {
		return false, err
	}
	if after != before {
		return false, fmt.Errorf("tmux server process identity changed while checking native ownership")
	}
	return true, nil
}

// matrix/hardened: only native absent-server diagnostics prove an empty
// server; transport/permission/timeouts must not authorize another process.
func exactClaudeNoServer(message string) bool {
	message = strings.TrimSpace(message)
	return strings.HasPrefix(message, "no server running on ") ||
		(strings.HasPrefix(message, "error connecting to ") && strings.HasSuffix(message, " (No such file or directory)")) || message == "no sessions"
}

func exactClaudeStoredBinding(storage *Storage, inst *Instance) error {
	row, err := autoSleepStoredRow(storage, inst)
	if err != nil {
		return err
	}
	desired, err := instanceToRow(inst)
	if err != nil {
		return err
	}
	if row.GroupPath != desired.GroupPath || row.Title != desired.Title || row.ParentSessionID != desired.ParentSessionID || row.IsConductor != desired.IsConductor {
		return fmt.Errorf("original Claude launch scope changed")
	}
	var a, b map[string]json.RawMessage
	if json.Unmarshal(row.ToolData, &a) != nil || json.Unmarshal(desired.ToolData, &b) != nil {
		return fmt.Errorf("Claude stored launch data is unreadable")
	}
	for _, key := range []string{"claude_session_id", "channels", "plugins", "parent_project_path", "additional_paths", "multi_repo_enabled", "multi_repo_temp_dir", "identity_injection_disabled", "context_level", "subcommand_passthrough", "auto_sleep"} {
		if string(a[key]) != string(b[key]) {
			return fmt.Errorf("original Claude binding changed: %s", key)
		}
	}
	// Older rows omitted the false bit; omission still means recorded identity,
	// while true, malformed, or a change in verification must refuse.
	var storedUnverified, desiredUnverified bool
	for _, pair := range []struct {
		raw json.RawMessage
		dst *bool
	}{{a["claude_session_id_unverified"], &storedUnverified}, {b["claude_session_id_unverified"], &desiredUnverified}} {
		if len(pair.raw) > 0 && json.Unmarshal(pair.raw, pair.dst) != nil {
			return fmt.Errorf("unreadable Claude identity verification")
		}
	}
	if storedUnverified || storedUnverified != desiredUnverified {
		return fmt.Errorf("original Claude identity verification changed")
	}
	if ReadAutoSleepStateFromToolData(row.ToolData) != nil {
		return fmt.Errorf("sleep state changed before Claude recovery")
	}
	return nil
}

// ResumeExactClaudeForOpen only creates a proven-missing ordinary Claude
// terminal. The caller holds the profile mutation lock. No Restart, Stop,
// fresh fallback, disk discovery, user input, or duplicate teardown is used.
func ResumeExactClaudeForOpen(storage *Storage, inst *Instance) error {
	if inst == nil {
		return fmt.Errorf("exact Claude resume: missing session")
	}
	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		return err
	}
	defer release()
	if err := exactClaudeStoredBinding(storage, inst); err != nil {
		return fmt.Errorf("exact Claude stored binding changed: %w", err)
	}
	if inst.tmuxSession == nil {
		return fmt.Errorf("exact Claude resume has no terminal target")
	}
	exists, err := inst.tmuxSession.ProbeExists()
	if err != nil {
		return fmt.Errorf("cannot establish Claude terminal absence: %w", err)
	}
	if exists {
		return fmt.Errorf("Claude terminal appeared before recovery; open again to attach without restarting")
	}
	plan, err := newExactClaudeResumePlan(inst)
	if err != nil {
		return fmt.Errorf("exact Claude resume refused: %w", err)
	}
	plan.storage = storage
	nativeRelease, err := acquireInstanceSpawnLock("claude-native-" + plan.sid)
	if err != nil {
		return err
	}
	defer nativeRelease()
	if err := guardExactClaudeNativeOwner(inst, plan); err != nil {
		return err
	}
	if err := inst.guardOwnedProcessesBeforeSpawn("prepare-open"); err != nil {
		return err
	}
	admission, err := plan.admission(inst)
	if err != nil {
		return err
	}
	inst.InvalidateRuntimeSnapshot()
	return inst.startWithClaudeAdmissionAndExactResume(admission, true, plan)
}
