package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// matrix/hardened: a remote open is permission to resume this exact user
// conversation, never permission to replace its binding with a fresh/fork ID.
type exactCodexResumePlan struct {
	sid, home, commandHash, cwd, executable string
}

func newExactCodexResumePlan(inst *Instance) (*exactCodexResumePlan, error) {
	if err := validateAutoSleepScope(inst); err != nil {
		return nil, err
	}
	if inst.hasAutoSleepState() {
		return nil, fmt.Errorf("sleeping session must use its exact wake lifecycle")
	}
	if err := validateAutoSleepNativeIdentity(inst, inst.CodexSessionID); err != nil {
		return nil, err
	}
	cwd := inst.EffectiveWorkingDir()
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("original Codex working directory is unavailable: %s", cwd)
	}
	// An arbitrary configured shell command could already contain resume/fork
	// or execute another program. This narrow recovery supports the executable
	// only; explicit R remains available for custom launch plans.
	command := strings.TrimSpace(inst.resolveCodexCommand(inst.Command))
	word, rest, ok := nextShellWord(command)
	if !ok || strings.TrimSpace(rest) != "" || filepath.Base(word) != "codex" {
		return nil, fmt.Errorf("exact remote recovery requires the ordinary Codex executable; use explicit restart for custom launch commands")
	}
	return &exactCodexResumePlan{sid: inst.CodexSessionID, home: CodexHomeDirForInstance(inst), commandHash: autoSleepCommandSHA256(inst), cwd: cwd, executable: word}, nil
}

func (p *exactCodexResumePlan) command(inst *Instance) (string, error) {
	current, err := newExactCodexResumePlan(inst)
	if err != nil {
		return "", err
	}
	if *current != *p {
		return "", fmt.Errorf("original Codex binding or launch configuration changed before resume")
	}
	// Export the exact home that was validated, including the default home;
	// .env sourcing cannot silently send this resume to a different store.
	prefix := inst.buildEnvSourceCommand() + fmt.Sprintf("AGENTDECK_INSTANCE_ID=%s AGENTDECK_TITLE=%s AGENTDECK_TOOL=codex AGENTDECK_PROFILE=%s CODEX_HOME=%s ",
		shellescape.Quote(inst.ID), shellescape.Quote(inst.Title), shellescape.Quote(sessionProfileEnvValue()), shellescape.Quote(p.home))
	command := shellescape.Quote(p.executable)
	if inst.codexSharedThread() {
		return prefix + command + " resume " + p.sid, nil
	}
	return prefix + command + inst.resolveCodexYoloFlag() + inst.resolveCodexModelFlag() + inst.resolveCodexReasoningEffortFlag() + inst.codexIdentityFlag(p.home, p.cwd) + " resume " + p.sid, nil
}

// ResumeExactCodexForOpen only creates a missing terminal. Its caller holds
// the profile mutation lock; the spawn lock closes races with ordinary starts.
// No stop, teardown, user input, automatic-sleep marker or fresh fallback exists.
// matrix/hardened: storage shape and native rollout are checked again inside
// this lock, and the command builder checks the frozen binding at spawn time.
func ResumeExactCodexForOpen(storage *Storage, inst *Instance) error {
	if inst == nil {
		return fmt.Errorf("exact resume: missing session")
	}
	release, err := acquireInstanceSpawnLock(inst.ID)
	if err != nil {
		return err
	}
	defer release()
	row, err := autoSleepStoredRow(storage, inst)
	if err != nil {
		return fmt.Errorf("exact resume binding changed: %w", err)
	}
	if ReadAutoSleepStateFromToolData(row.ToolData) != nil {
		return fmt.Errorf("automatic sleep state changed before exact resume; open again through its wake lifecycle")
	}
	if inst.tmuxSession == nil {
		return fmt.Errorf("exact resume has no terminal target")
	}
	exists, err := inst.tmuxSession.ProbeExists()
	if err != nil {
		return fmt.Errorf("exact resume cannot establish terminal absence: %w", err)
	}
	if exists {
		return fmt.Errorf("terminal appeared before exact resume; open again to attach without restarting")
	}
	plan, err := newExactCodexResumePlan(inst)
	if err != nil {
		return fmt.Errorf("exact resume refused: %w", err)
	}
	acceptance, err := AcquireCodexAcceptanceLock(plan.sid, 3*time.Second)
	if err != nil {
		return err
	}
	defer acceptance.Release()
	if err := inst.guardOwnedProcessesBeforeSpawn("prepare-open"); err != nil {
		return err
	}
	inst.InvalidateRuntimeSnapshot()
	return inst.startWithClaudeAdmissionAndExactResume(nil, true, plan)
}
