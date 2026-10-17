package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

const autoSleepVersion = 1
const toolDataAutoSleepKey = "matrix_auto_sleep"

// AutoSleepState preserves the exact native conversation and terminal history.
// matrix/hardened: pending is recoverable evidence of an interrupted stop, never
// permission for an automatic second kill or a fresh conversation.
type AutoSleepState struct {
	Version             int       `json:"version"`
	Phase               string    `json:"phase"`
	CodexSessionID      string    `json:"codex_session_id"`
	CodexHome           string    `json:"codex_home"`
	CommandSHA256       string    `json:"command_sha256"`
	SleptAt             time.Time `json:"slept_at"`
	ScrollbackPath      string    `json:"scrollback_path"`
	ScrollbackSHA256    string    `json:"scrollback_sha256"`
	ScrollbackBytes     int64     `json:"scrollback_bytes"`
	OwnershipGeneration uint64    `json:"ownership_generation"`
	PaneID              string    `json:"pane_id"`
	TmuxSessionID       string    `json:"tmux_session_id"`
	TransitionToken     string    `json:"transition_token"`
}

func cloneAutoSleepState(state *AutoSleepState) *AutoSleepState {
	if state == nil {
		return nil
	}
	copy := *state
	return &copy
}

// IsAutoSleeping identifies confirmed sleep, not an ordinary stopped session.
// matrix/hardened: pending stop failures remain separately visible to callers.
func (i *Instance) IsAutoSleeping() bool {
	if i == nil {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.AutoSleep != nil && i.AutoSleep.Version == autoSleepVersion && i.AutoSleep.Phase == "asleep"
}
func (i *Instance) AutoSleepPending() bool {
	if i == nil {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.AutoSleep != nil && i.AutoSleep.Phase == "pending"
}
func (i *Instance) hasAutoSleepState() bool {
	if i == nil {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.AutoSleep != nil
}
func (i *Instance) autoSleepSnapshot() *AutoSleepState {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneAutoSleepState(i.AutoSleep)
}

// WriteAutoSleepStateToToolData uses explicit null only for intentional clears.
// matrix/hardened: an unaware stale writer must preserve a concurrently slept row.
func WriteAutoSleepStateToToolData(td json.RawMessage, state *AutoSleepState, cleared bool) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	if state != nil {
		raw, _ := json.Marshal(state)
		m[toolDataAutoSleepKey] = raw
	} else if cleared {
		m[toolDataAutoSleepKey] = json.RawMessage(`null`)
	} else {
		delete(m, toolDataAutoSleepKey)
	}
	raw, _ := json.Marshal(m)
	return raw
}
func ReadAutoSleepStateFromToolData(td json.RawMessage) *AutoSleepState {
	var m map[string]json.RawMessage
	if json.Unmarshal(td, &m) != nil {
		return nil
	}
	raw := m[toolDataAutoSleepKey]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var state AutoSleepState
	if json.Unmarshal(raw, &state) != nil {
		// matrix/hardened: malformed marker is still a marker; wake refuses it.
		return &AutoSleepState{Phase: "invalid"}
	}
	return &state
}
func autoSleepRaw(td json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(td, &m)
	raw := m[toolDataAutoSleepKey]
	if len(raw) == 0 {
		return json.RawMessage(`null`)
	}
	return raw
}

// matrix/hardened: only the built-in local Codex launch has a provable resume.
func validateAutoSleepScope(inst *Instance) error {
	if inst == nil {
		return fmt.Errorf("auto-sleep: missing session")
	}
	if inst.Tool != "codex" || (strings.TrimSpace(inst.Command) != "codex" && strings.TrimSpace(inst.Command) != "") || inst.Wrapper != "" || inst.SSHHost != "" || inst.IsSandboxed() || inst.UsesTerminalBackend() || inst.SubcommandPassthrough || inst.IsForkAwaitingStart {
		return fmt.Errorf("auto-sleep requires an ordinary local Codex session without custom command or wrapper")
	}
	return nil
}
func validateAutoSleepNativeIdentity(inst *Instance, sid string) error {
	if sid == "" || uuidPatternRE.FindString(sid) != sid || sid != inst.CodexSessionID {
		return fmt.Errorf("auto-sleep: exact original Codex session ID is missing or changed")
	}
	matches, err := exactCodexRolloutMatches(sid, CodexHomeDirForInstance(inst))
	if err != nil {
		return err
	}
	path, err := uniqueRegularArtifact(matches, "auto-sleep original Codex rollout")
	if err != nil {
		return err
	}
	meta := readCodexRolloutThreadMeta(path)
	if !meta.valid || meta.ParentThreadID != "" || meta.ThreadSource == "subagent" {
		return fmt.Errorf("auto-sleep: original rollout is unreadable or a subagent thread")
	}
	// matrix/hardened: filenames alone do not establish the native identity.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var head struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	dec := json.NewDecoder(f)
	if err = dec.Decode(&head); err != nil || head.Type != "session_meta" || head.Payload.ID != sid {
		return fmt.Errorf("auto-sleep: rollout metadata does not match the original Codex ID")
	}
	return nil
}

// matrix/hardened: remember launch configuration identity without persisting
// command strings that can contain operator-supplied environment values.
func autoSleepCommandSHA256(inst *Instance) string {
	sum := sha256.Sum256([]byte(inst.resolveCodexCommand(inst.Command)))
	return hex.EncodeToString(sum[:])
}

func validateAutoSleepResume(inst *Instance) error {
	if err := validateAutoSleepScope(inst); err != nil {
		return err
	}
	state := inst.autoSleepSnapshot()
	if state == nil {
		return nil
	}
	if state.Version != autoSleepVersion || (state.Phase != "asleep" && state.Phase != "pending") || state.CodexHome != CodexHomeDirForInstance(inst) || state.CommandSHA256 != autoSleepCommandSHA256(inst) {
		return fmt.Errorf("auto-sleep: unsupported marker or changed Codex home")
	}
	return validateAutoSleepNativeIdentity(inst, state.CodexSessionID)
}

// matrix/hardened: retain a private, fsynced artifact before any destructive step.
func captureAutoSleepScrollback(storage *Storage, inst *Instance, state *AutoSleepState) error {
	content, err := inst.PreviewPrimaryFull()
	if err != nil {
		return fmt.Errorf("auto-sleep scrollback capture: %w", err)
	}
	dir := filepath.Join(filepath.Dir(storage.dbPath), "auto-sleep")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("auto-sleep artifact directory is not private")
	}
	file, err := os.CreateTemp(dir, spawnLockSafeID(inst.ID)+"-*.txt")
	if err != nil {
		return err
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if _, err = file.WriteString(content); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(content))
	state.ScrollbackPath = path
	state.ScrollbackSHA256 = hex.EncodeToString(sum[:])
	state.ScrollbackBytes = int64(len(content))
	ok = true
	return nil
}

// matrix/hardened: no PID guessed at teardown can become signal authority.
func autoSleepOwnedReceipt(inst *Instance) (*procowner.Receipt, error) {
	status := inst.ownershipStatus()
	if status.LoadErr != nil {
		return nil, status.LoadErr
	}
	if status.Receipt == nil || status.Report.Verdict != procowner.VerdictOwned || !status.PaneAttached || len(status.Survivors) > 0 {
		return nil, fmt.Errorf("auto-sleep: complete live process ownership is not proven: %s", status.Reason())
	}
	root, err := ownershipProber.Inspect(status.Receipt.Leader.PID)
	if err != nil {
		return nil, err
	}
	tree, err := ownershipProber.Descendants(root)
	if err != nil {
		return nil, err
	}
	members := map[string]bool{}
	for _, m := range status.Receipt.All() {
		members[m.Key()] = true
	}
	for _, p := range tree {
		if !p.IsZombie() && !members[(procowner.Member{PID: p.PID, StartID: p.StartID}).Key()] {
			return nil, fmt.Errorf("auto-sleep: live descendant has no spawn ownership receipt")
		}
	}
	return status.Receipt, nil
}

func autoSleepStoredRow(storage *Storage, inst *Instance) (*statedb.InstanceRow, error) {
	if storage == nil || storage.db == nil {
		return nil, fmt.Errorf("auto-sleep storage is unavailable")
	}
	row, err := storage.db.LoadInstanceByID(inst.ID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("auto-sleep session was removed")
	}
	desired, err := instanceToRow(inst)
	if err != nil {
		return nil, err
	}
	if row.Tool != desired.Tool || row.Command != desired.Command || row.Wrapper != desired.Wrapper || row.Account != desired.Account || row.ProjectPath != desired.ProjectPath || row.ParentSessionID != desired.ParentSessionID || row.TmuxSession != desired.TmuxSession || row.TmuxSocketName != desired.TmuxSocketName || !row.ArchivedAt.Equal(desired.ArchivedAt) || row.Pin != desired.Pin || row.LastAccessed.Unix() != desired.LastAccessed.Unix() {
		return nil, statedb.ErrAutoSleepConflict
	}
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(row.ToolData, &a)
	_ = json.Unmarshal(desired.ToolData, &b)
	for _, key := range []string{"codex_session_id", "ssh_host", "ssh_remote_path", "sandbox", "sandbox_container", "tool_options", "extra_args", "env_file", "native_backend"} {
		if string(a[key]) != string(b[key]) {
			return nil, fmt.Errorf("%w: %s", statedb.ErrAutoSleepConflict, key)
		}
	}
	return row, nil
}

// matrix/hardened: an observed lifecycle state is a baseline, not local edit
// intent. Sync only these fields so a later full save cannot resurrect it.
func syncAutoSleepModelFromRow(inst *Instance, row *statedb.InstanceRow) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.AutoSleep = ReadAutoSleepStateFromToolData(row.ToolData)
	inst.AutoSleepHistory = ReadAutoSleepHistoryFromToolData(row.ToolData)
	inst.autoSleepCleared = inst.AutoSleep == nil
	inst.Status = Status(row.Status)
	inst.LastAccessedAt = row.LastAccessed
	if snap := inst.storageSnapshot; snap != nil {
		copy := *snap
		copy.original = statedb.CloneInstanceRow(snap.original)
		copy.stored = statedb.CloneInstanceRow(snap.stored)
		for _, baseline := range []*statedb.InstanceRow{copy.original, copy.stored} {
			if baseline != nil {
				baseline.ToolData = WriteAutoSleepStateToToolData(baseline.ToolData, inst.AutoSleep, true)
				baseline.ToolData = WriteAutoSleepHistoryToToolData(baseline.ToolData, inst.AutoSleepHistory)
				baseline.Status = row.Status
				baseline.LastAccessed = row.LastAccessed
			}
		}
		inst.storageSnapshot = &copy
	}
}

func commitAutoSleepState(storage *Storage, inst *Instance, row *statedb.InstanceRow, state *AutoSleepState, status Status) error {
	marker := json.RawMessage(`null`)
	if state != nil {
		var err error
		marker, err = json.Marshal(state)
		if err != nil {
			return err
		}
	}
	if err := storage.db.CompareAndSwapAutoSleep(row, autoSleepRaw(row.ToolData), marker, string(status)); err != nil {
		return err
	}
	inst.mu.Lock()
	inst.AutoSleep = cloneAutoSleepState(state)
	inst.AutoSleepHistory = autoSleepHistoryAfterClear(row.ToolData, state, status)
	inst.autoSleepCleared = state == nil
	// matrix/hardened: acknowledge only these targeted fields, so an older
	// full-save baseline cannot resurrect a marker another process has cleared.
	if snap := inst.storageSnapshot; snap != nil {
		copy := *snap
		copy.original = statedb.CloneInstanceRow(snap.original)
		copy.stored = statedb.CloneInstanceRow(snap.stored)
		for _, baseline := range []*statedb.InstanceRow{copy.original, copy.stored} {
			if baseline != nil {
				baseline.ToolData = WriteAutoSleepStateToToolData(baseline.ToolData, state, true)
				baseline.ToolData = WriteAutoSleepHistoryToToolData(baseline.ToolData, inst.AutoSleepHistory)
				if status != "" {
					baseline.Status = string(status)
				}
			}
		}
		inst.storageSnapshot = &copy
	}
	if status != "" {
		inst.Status = status
		inst.stopRevision++
	}
	inst.mu.Unlock()
	row.ToolData = WriteAutoSleepStateToToolData(row.ToolData, state, true)
	row.ToolData = WriteAutoSleepHistoryToToolData(row.ToolData, inst.AutoSleepHistory)
	if status != "" {
		row.Status = string(status)
	}
	return nil
}

// ReadAutoSleepScrollback reads the retained terminal history without waking.
// matrix/hardened: only the owning profile's private regular artifact with the
// exact saved size and hash can supply this preview; paths never become arbitrary reads.
func ReadAutoSleepScrollback(inst *Instance) (string, error) {
	if inst == nil {
		return "", fmt.Errorf("auto-sleep preview: missing session")
	}
	inst.mu.RLock()
	states := cloneAutoSleepHistory(inst.AutoSleepHistory)
	state := cloneAutoSleepState(inst.AutoSleep)
	snap := inst.storageSnapshot
	dbPath := ""
	if snap != nil {
		dbPath = snap.dbPath
	}
	inst.mu.RUnlock()
	if state != nil {
		states = append(states, state)
	}
	if len(states) == 0 {
		return "", fmt.Errorf("auto-sleep preview: no retained scrollback")
	}
	if dbPath == "" {
		return "", fmt.Errorf("auto-sleep preview: owning profile is unavailable")
	}
	var parts []string
	seen := map[string]bool{}
	for _, state := range states {
		if state == nil {
			return "", fmt.Errorf("auto-sleep preview: invalid history reference")
		}
		if seen[state.ScrollbackPath] {
			continue
		}
		part, err := readAutoSleepArtifact(inst.ID, dbPath, state)
		if err != nil {
			return "", err
		}
		parts = append(parts, part)
		seen[state.ScrollbackPath] = true
	}
	return strings.Join(parts, "\n\n"), nil
}
func readAutoSleepArtifact(instanceID, dbPath string, state *AutoSleepState) (string, error) {
	if state.Version != autoSleepVersion || (state.Phase != "asleep" && state.Phase != "pending") {
		return "", fmt.Errorf("auto-sleep preview: unsupported retained history reference")
	}
	dir := filepath.Join(filepath.Dir(dbPath), "auto-sleep")
	path := filepath.Clean(state.ScrollbackPath)
	if filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), spawnLockSafeID(instanceID)+"-") {
		return "", fmt.Errorf("auto-sleep preview: artifact is outside the owning profile")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("auto-sleep preview: artifact directory is not private")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != state.ScrollbackBytes || state.ScrollbackBytes < 0 {
		return "", fmt.Errorf("auto-sleep preview: artifact identity, permissions or size changed")
	}
	content, err := io.ReadAll(io.LimitReader(file, state.ScrollbackBytes+1))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	if int64(len(content)) != state.ScrollbackBytes || hex.EncodeToString(sum[:]) != state.ScrollbackSHA256 {
		return "", fmt.Errorf("auto-sleep preview: artifact hash changed")
	}
	return string(content), nil
}

// matrix/hardened: capture server-generated terminal identities before stopping;
// a retained dead pane can then be retired without signalling a replacement.
func autoSleepPaneIdentity(inst *Instance, receipt *procowner.Receipt) (string, string, error) {
	s := inst.GetTmuxSession()
	if s == nil {
		return "", "", fmt.Errorf("auto-sleep terminal unavailable")
	}
	out, err := tmux.OutputBounded(s.SocketName, "list-panes", "-s", "-t", "="+s.Name, "-F", "#{pane_id}\t#{session_id}\t#{pane_pid}\t#{pane_dead}")
	if err != nil {
		return "", "", err
	}
	rows := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(rows) != 1 {
		return "", "", fmt.Errorf("auto-sleep terminal has multiple panes")
	}
	cols := strings.Split(rows[0], "\t")
	if len(cols) != 4 || cols[2] != strconv.Itoa(receipt.Leader.PID) || cols[3] != "0" || !autoSleepTmuxID(cols[0], '%') || !autoSleepTmuxID(cols[1], '$') {
		return "", "", fmt.Errorf("auto-sleep pane identity is not the owned live leader")
	}
	return cols[0], cols[1], nil
}
func autoSleepTmuxID(id string, prefix byte) bool {
	if len(id) < 2 || id[0] != prefix {
		return false
	}
	for _, c := range id[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// matrix/hardened: a cancelled capture is private scratch unless some durable
// marker/history still names it. A database read failure or changed inode is
// uncertainty, so it is retained and reported rather than guessed away.
func cleanupCancelledAutoSleepArtifact(storage *Storage, inst *Instance, state *AutoSleepState, original os.FileInfo) error {
	row, err := storage.db.LoadInstanceByID(inst.ID)
	if err != nil {
		return fmt.Errorf("private auto-sleep artifact retained; reference state unavailable: %w", err)
	}
	if row != nil {
		refs := ReadAutoSleepHistoryFromToolData(row.ToolData)
		if marker := ReadAutoSleepStateFromToolData(row.ToolData); marker != nil {
			refs = append(refs, marker)
		}
		for _, ref := range refs {
			if ref != nil && ref.ScrollbackPath == state.ScrollbackPath {
				return nil
			}
		}
	}
	info, err := os.Lstat(state.ScrollbackPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if original == nil || !os.SameFile(original, info) {
		return fmt.Errorf("private auto-sleep artifact retained; captured file identity changed")
	}
	if _, err = readAutoSleepArtifact(inst.ID, storage.dbPath, state); err != nil {
		return fmt.Errorf("private auto-sleep artifact retained; ownership validation failed: %w", err)
	}
	if err = os.Remove(state.ScrollbackPath); err != nil {
		return fmt.Errorf("remove cancelled private auto-sleep artifact: %w", err)
	}
	return nil
}

// matrix/hardened: server-side input quiescence closes the direct-client draft
// race. Both the live-pane guard and disable action execute in one tmux queue.
func quiesceAutoSleepPane(inst *Instance, receipt *procowner.Receipt, state *AutoSleepState) (func() error, error) {
	s := inst.GetTmuxSession()
	if s == nil {
		return nil, fmt.Errorf("auto-sleep terminal unavailable")
	}
	if !autoSleepTmuxID(state.PaneID, '%') || !autoSleepTmuxID(state.TmuxSessionID, '$') || len(state.TransitionToken) != 32 {
		return nil, fmt.Errorf("auto-sleep transition identity is invalid")
	}
	cond := "#{&&:#{==:#{session_id}," + state.TmuxSessionID + "},#{&&:#{&&:#{==:#{pane_pid}," + strconv.Itoa(receipt.Leader.PID) + "},#{==:#{pane_dead},0}},#{&&:#{&&:#{==:#{pane_input_off},0},#{==:#{session_windows},1}},#{==:#{window_panes},1}}}}"
	apply := "set-option -p -t " + state.PaneID + " @agent_deck_auto_sleep " + state.TransitionToken + " ; select-pane -d -t " + state.PaneID
	restore := func() error {
		condition := "#{&&:#{==:#{session_id}," + state.TmuxSessionID + "},#{&&:#{==:#{@agent_deck_auto_sleep}," + state.TransitionToken + "},#{&&:#{==:#{pane_pid}," + strconv.Itoa(receipt.Leader.PID) + "},#{==:#{pane_dead},0}}}}"
		action := "select-pane -e -t " + state.PaneID + " ; set-option -pu -t " + state.PaneID + " @agent_deck_auto_sleep ; display-message -t " + state.PaneID + " 'Automatic sleep cancelled; input enabled, please retype'"
		out, err := tmux.OutputBounded(s.SocketName, "if-shell", "-F", "-t", state.PaneID, condition, action, "display-message -p unchanged")
		if err != nil {
			return err
		}
		if strings.Contains(string(out), "unchanged") {
			return fmt.Errorf("original live input gate no longer matches")
		}
		return nil
	}
	out, err := tmux.OutputBounded(s.SocketName, "if-shell", "-F", "-t", state.PaneID, cond, apply, "display-message -p refused")
	if err != nil {
		return restore, err
	}
	if strings.Contains(string(out), "refused") {
		return nil, fmt.Errorf("auto-sleep live pane changed before input pause")
	}
	return restore, nil
}

// matrix/hardened: cancellation owns only its nonce marker. Reload its row so
// a new attachment or pin can cancel sleep without being overwritten or leaving
// the live pane permanently labelled pending.
func clearCancelledAutoSleep(storage *Storage, inst *Instance, original *statedb.InstanceRow) error {
	row, err := storage.db.LoadInstanceByID(inst.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	if string(autoSleepRaw(row.ToolData)) != string(autoSleepRaw(original.ToolData)) {
		return statedb.ErrAutoSleepConflict
	}
	return commitAutoSleepState(storage, inst, row, nil, "")
}

func retireAutoSleepDeadPane(inst *Instance, receipt *procowner.Receipt, state *AutoSleepState) error {
	s := inst.GetTmuxSession()
	if s == nil {
		return fmt.Errorf("auto-sleep terminal unavailable")
	}
	exists, err := s.ProbeExists()
	if err != nil || !exists {
		return err
	}
	if !autoSleepTmuxID(state.PaneID, '%') || !autoSleepTmuxID(state.TmuxSessionID, '$') {
		return fmt.Errorf("original terminal identity is unavailable")
	}
	// The server evaluates the old pane id, transition token, old session id, dead flag,
	// and one-pane layout in the same command as kill-pane. No process scanning
	// or client-side check/kill window can broaden this to a live replacement.
	cond := "#{&&:#{==:#{session_id}," + state.TmuxSessionID + "},#{&&:#{&&:#{==:#{@agent_deck_auto_sleep}," + state.TransitionToken + "},#{==:#{pane_dead},1}},#{&&:#{==:#{session_windows},1},#{==:#{window_panes},1}}}}"
	out, err := tmux.OutputBounded(s.SocketName, "if-shell", "-F", "-t", state.PaneID, cond, "kill-pane -t "+state.PaneID, "display-message -p refused")
	if err != nil {
		if alive, probeErr := s.ProbeExists(); probeErr == nil && !alive {
			return nil
		}
		return err
	}
	if strings.Contains(string(out), "refused") {
		details, _ := tmux.OutputBounded(s.SocketName, "list-panes", "-s", "-t", "="+s.Name, "-F", "#{pane_id}/#{session_id}/#{pane_pid}/#{pane_dead}/#{session_windows}/#{window_panes}")
		return fmt.Errorf("dead pane was replaced or its layout changed (expected %s/%s/%d/dead/1/1; observed %s)", state.PaneID, state.TmuxSessionID, receipt.Leader.PID, strings.TrimSpace(string(details)))
	}
	return nil
}

// matrix/hardened: focused tests replace lifecycle effects per call; production
// always supplies the identity-verifying implementation, never process globals.
type autoSleepOperations struct {
	gate         func(*Instance, *procowner.Receipt, *AutoSleepState) (func() error, error)
	paneIdentity func(*Instance, *procowner.Receipt) (string, string, error)
	retire       func(*Instance, *procowner.Receipt, *AutoSleepState) error
	capture      func(*Storage, *Instance, *AutoSleepState) error
	receipt      func(*Instance) (*procowner.Receipt, error)
	stop         func(*Instance, *procowner.Receipt) error
	exists       func(*Instance) (bool, error)
	ownership    func(*Instance) OwnershipStatus
	start        func(*Instance) error
	ready        func(*Instance) error
	clearReceipt func(*procowner.Receipt) error
}

func defaultAutoSleepOperations() autoSleepOperations {
	return autoSleepOperations{
		capture: captureAutoSleepScrollback, receipt: autoSleepOwnedReceipt,
		paneIdentity: autoSleepPaneIdentity, retire: retireAutoSleepDeadPane, gate: quiesceAutoSleepPane,
		stop: func(inst *Instance, current *procowner.Receipt) error {
			inst.InvalidateRuntimeSnapshot()
			inst.bumpSpawnGenAndBarrier()
			report := procowner.Reap(ownershipProber, ownershipSignaler, current, teardownReapOptions(true))
			if report.Verdict != procowner.VerdictClear {
				markOwnershipRecoveryRequired(ownershipStore(), current, report.Reason)
				return fmt.Errorf("auto-sleep stop incomplete; pending marker retained: %s", report.Describe())
			}
			return nil
		},
		exists: func(inst *Instance) (bool, error) {
			if inst.tmuxSession == nil {
				return false, fmt.Errorf("auto-sleep terminal is not initialized")
			}
			return inst.tmuxSession.ProbeExists()
		},
		ownership: func(inst *Instance) OwnershipStatus { return inst.ownershipStatus() },
		start:     func(inst *Instance) error { return inst.startWithClaudeAdmission(nil, true) },
		ready: func(inst *Instance) error {
			return send.WaitForAgentReady(inst.tmuxSession, inst.Tool, 30*time.Second, send.PromptGates{CodexPrompt: true})
		},
		clearReceipt: ownershipStore().Clear,
	}
}

// AutoSleepSession snapshots a completed Codex session, then stops only the
// process identities recorded at spawn. The caller supplies fresh completion,
// draft, approval, child-task, attachment and activity checks.
// matrix/hardened: lock order is spawn then native acceptance on every path.
func AutoSleepSession(storage *Storage, inst *Instance, recheck func() error) error {
	return autoSleepSessionWithOperations(storage, inst, recheck, defaultAutoSleepOperations())
}

func autoSleepSessionWithOperations(storage *Storage, inst *Instance, recheck func() error, ops autoSleepOperations) (returnErr error) {
	if err := validateAutoSleepScope(inst); err != nil {
		return err
	}
	if recheck == nil {
		return fmt.Errorf("auto-sleep requires a fresh safety recheck")
	}
	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		return err
	}
	defer release()
	row, err := autoSleepStoredRow(storage, inst)
	if err != nil {
		return err
	}
	if ReadAutoSleepStateFromToolData(row.ToolData) != nil {
		return fmt.Errorf("auto-sleep marker already exists; inspect or wake before retrying")
	}
	if err = validateAutoSleepNativeIdentity(inst, inst.CodexSessionID); err != nil {
		return err
	}
	acceptance, err := AcquireCodexAcceptanceLock(inst.CodexSessionID, 3*time.Second)
	if err != nil {
		return err
	}
	defer acceptance.Release()
	if err = recheck(); err != nil {
		return err
	}
	receipt, err := ops.receipt(inst)
	if err != nil {
		return err
	}
	state := &AutoSleepState{Version: autoSleepVersion, Phase: "pending", CodexSessionID: inst.CodexSessionID, CodexHome: CodexHomeDirForInstance(inst), SleptAt: time.Now().UTC(), OwnershipGeneration: receipt.Generation, CommandSHA256: autoSleepCommandSHA256(inst)}
	state.TransitionToken, err = newCodexSubmissionAttemptID()
	if err != nil {
		return err
	}
	state.PaneID, state.TmuxSessionID, err = ops.paneIdentity(inst, receipt)
	if err != nil {
		return err
	}
	if err = ops.capture(storage, inst, state); err != nil {
		return err
	}
	// matrix/hardened: only this capture's exact inode can be removed after
	// a cancellation. Durable or uncertain references always keep the artifact.
	capturedInfo, captureStatErr := os.Lstat(state.ScrollbackPath)
	if captureStatErr != nil {
		return captureStatErr
	}
	defer func() {
		if cleanupErr := cleanupCancelledAutoSleepArtifact(storage, inst, state, capturedInfo); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err = recheck(); err != nil {
		return err
	}
	if err = commitAutoSleepState(storage, inst, row, state, ""); err != nil {
		return err
	}
	// matrix/hardened: pause foreground input in the tmux server before the
	// final fresh inspection; a newly attached client cannot create a draft in
	// the inspection-to-signal window. Cooperating attaches also hold spawn lock.
	var restoreInput func() error
	signalsStarted := false
	defer func() {
		if signalsStarted {
			return
		}
		if restoreInput != nil {
			if restoreErr := restoreInput(); restoreErr != nil {
				returnErr = fmt.Errorf("%v; auto-sleep input recovery is unconfirmed; pending marker retained: %w", returnErr, restoreErr)
				return
			}
		}
		if clearErr := clearCancelledAutoSleep(storage, inst, row); clearErr != nil {
			returnErr = fmt.Errorf("%v; auto-sleep cancellation marker clear failed: %w", returnErr, clearErr)
		}
	}()
	restoreInput, err = ops.gate(inst, receipt, state)
	if err != nil {
		return err
	}
	if err = recheck(); err != nil {
		return fmt.Errorf("auto-sleep cancelled before stop: %w", err)
	}
	current, err := ops.receipt(inst)
	if err != nil {
		return err
	}
	if current.Generation != receipt.Generation || current.Leader.Key() != receipt.Leader.Key() {
		return fmt.Errorf("auto-sleep ownership changed before stop")
	}
	signalsStarted = true
	if err = ops.stop(inst, current); err != nil {
		return err
	}
	if err = ops.retire(inst, current, state); err != nil {
		return fmt.Errorf("auto-sleep owned processes exited but dead terminal retirement is unconfirmed; pending marker retained: %w", err)
	}
	// Reap proves identity exit; only exact tmux absence additionally proves the
	// retained terminal no longer names any live process. Never kill unknown panes.
	exists, err := ops.exists(inst)
	if err != nil || exists {
		return fmt.Errorf("auto-sleep owned processes exited but terminal removal is unconfirmed; pending marker retained")
	}
	state.Phase = "asleep"
	state.SleptAt = time.Now().UTC()
	if err = commitAutoSleepState(storage, inst, row, state, StatusStopped); err != nil {
		return fmt.Errorf("auto-sleep processes exited; pending recovery marker retained: %w", err)
	}
	_ = ops.clearReceipt(current)
	_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{InstanceID: inst.ID, Tool: inst.Tool, Action: "auto_sleep", Source: "idle_lifecycle", Candidate: state.CodexSessionID, Reason: "completed idle conversation preserved"})
	return nil
}

// WakeAutoSleepingSession resumes the exact original user thread. Failed
// identity/readiness/storage checks retain the marker and never start fresh.
// matrix/hardened: reload under the existing spawn lock prevents double-spawn.
func WakeAutoSleepingSession(storage *Storage, inst *Instance) (bool, error) {
	return wakeAutoSleepingSessionWithOperations(storage, inst, defaultAutoSleepOperations())
}

func wakeAutoSleepingSessionWithOperations(storage *Storage, inst *Instance, ops autoSleepOperations) (bool, error) {
	if inst == nil {
		return false, fmt.Errorf("wake: missing session")
	}
	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		return false, err
	}
	defer release()
	if storage == nil || storage.db == nil {
		return false, fmt.Errorf("wake storage is unavailable")
	}
	row, err := storage.db.LoadInstanceByID(inst.ID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, fmt.Errorf("wake session was removed")
	}
	state := ReadAutoSleepStateFromToolData(row.ToolData)
	if state == nil {
		syncAutoSleepModelFromRow(inst, row)
		return false, nil
	}
	// matrix/hardened: ordinary active sessions need no lifecycle shape CAS.
	row, err = autoSleepStoredRow(storage, inst)
	if err != nil {
		return false, err
	}
	inst.mu.Lock()
	inst.AutoSleep = cloneAutoSleepState(state)
	inst.mu.Unlock()
	if err = validateAutoSleepResume(inst); err != nil {
		return false, err
	}
	acceptance, err := AcquireCodexAcceptanceLock(state.CodexSessionID, 3*time.Second)
	if err != nil {
		return false, err
	}
	defer acceptance.Release()
	exists, err := ops.exists(inst)
	if err != nil {
		return false, err
	}
	own := ops.ownership(inst)
	if state.Phase == "pending" {
		if own.LoadErr != nil || own.Receipt == nil || own.Receipt.Generation != state.OwnershipGeneration || own.Report.Verdict != procowner.VerdictClear {
			return false, fmt.Errorf("auto-sleep stop is pending; original processes are not proven exited, inspect session ownership before waking")
		}
		if exists {
			if err = ops.retire(inst, own.Receipt, state); err != nil {
				return false, fmt.Errorf("pending stop: original dead terminal cannot be safely retired: %w", err)
			}
			exists, err = ops.exists(inst)
			if err != nil || exists {
				return false, fmt.Errorf("pending stop: original terminal removal is unconfirmed")
			}
		}
	}
	if exists {
		// A previous wake may have spawned before readiness/storage failed. It may
		// be observed, never duplicated; the recorded command must name this resume.
		if own.Receipt == nil || own.LoadErr != nil || !own.PaneAttached || own.Report.Verdict != procowner.VerdictOwned || !strings.Contains(own.Receipt.Command, " resume "+state.CodexSessionID) {
			return false, fmt.Errorf("wake: an existing terminal is not a proven resume of the original conversation")
		}
	} else {
		if err = inst.guardOwnedProcessesBeforeSpawn("wake"); err != nil {
			return false, err
		}
		if err = ops.start(inst); err != nil {
			return false, err
		}
	}
	if err = ops.ready(inst); err != nil {
		return false, fmt.Errorf("wake started the original conversation but readiness is unconfirmed; marker retained, retry wake: %w", err)
	}
	if inst.CodexSessionID != state.CodexSessionID {
		return false, fmt.Errorf("wake native identity changed; original marker retained")
	}
	if err = commitAutoSleepState(storage, inst, row, nil, StatusWaiting); err != nil {
		return false, fmt.Errorf("wake started the original conversation but marker persistence failed: %w", err)
	}
	inst.mu.Lock()
	inst.LastAccessedAt = time.Now()
	inst.mu.Unlock()
	_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{InstanceID: inst.ID, Tool: inst.Tool, Action: "auto_wake", Source: "idle_lifecycle", Candidate: state.CodexSessionID, Reason: "resumed original conversation"})
	return true, nil
}

// matrix/hardened: explicit manual stop cancels automatic wake intent.
func (i *Instance) clearAutoSleepAfterManualStop() error {
	state := i.autoSleepSnapshot()
	db := i.restartDB.Load()
	if db == nil {
		if state == nil {
			return nil
		}
		return fmt.Errorf("manual stop succeeded but auto-sleep marker could not be cleared: storage unavailable")
	}
	row, err := db.LoadInstanceByID(i.ID)
	if err != nil {
		return err
	}
	if row == nil {
		if state == nil {
			return nil
		}
		return fmt.Errorf("manual stop succeeded but session row is missing")
	}
	if ReadAutoSleepStateFromToolData(row.ToolData) == nil {
		i.mu.Lock()
		i.AutoSleep = nil
		i.autoSleepCleared = true
		i.mu.Unlock()
		return nil
	}
	if err = db.CompareAndSwapAutoSleep(row, autoSleepRaw(row.ToolData), json.RawMessage(`null`), string(StatusStopped)); err != nil {
		return err
	}
	i.mu.Lock()
	i.AutoSleep = nil
	i.AutoSleepHistory = autoSleepHistoryAfterClear(row.ToolData, nil, StatusStopped)
	i.autoSleepCleared = true
	if snap := i.storageSnapshot; snap != nil {
		copy := *snap
		copy.original = statedb.CloneInstanceRow(snap.original)
		copy.stored = statedb.CloneInstanceRow(snap.stored)
		for _, baseline := range []*statedb.InstanceRow{copy.original, copy.stored} {
			if baseline != nil {
				baseline.ToolData = WriteAutoSleepStateToToolData(baseline.ToolData, nil, true)
				baseline.ToolData = WriteAutoSleepHistoryToToolData(baseline.ToolData, i.AutoSleepHistory)
				baseline.Status = string(StatusStopped)
			}
		}
		i.storageSnapshot = &copy
	}
	i.mu.Unlock()
	return nil
}

// matrix/hardened: callers can hold stale models across an idle sweep. Read the
// marker again only after they own the existing cross-process spawn lock.
func (i *Instance) guardAutoSleepOrdinaryLaunch() error {
	if i.hasAutoSleepState() {
		return fmt.Errorf("session is auto-sleeping; wake the original conversation before starting or restarting")
	}
	if db := i.restartDB.Load(); db != nil {
		row, err := db.LoadInstanceByID(i.ID)
		if err != nil {
			return err
		}
		if row != nil {
			if state := ReadAutoSleepStateFromToolData(row.ToolData); state != nil {
				i.mu.Lock()
				i.AutoSleep = state
				i.mu.Unlock()
				return fmt.Errorf("session is auto-sleeping; wake the original conversation before starting or restarting")
			}
		}
	}
	return nil
}

// matrix/hardened: allow callers to distinguish a concurrent lifecycle change.
func IsAutoSleepConflict(err error) bool { return errors.Is(err, statedb.ErrAutoSleepConflict) }
