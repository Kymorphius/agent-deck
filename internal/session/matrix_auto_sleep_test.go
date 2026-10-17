package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/tmux"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: lifecycle tests use private SQLite/rollouts and per-call fake
// effects, so no real user's pane or process can be stopped by these cases.
func matrixAutoSleepCoreFixture(t *testing.T) (*Storage, *Instance, autoSleepOperations, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	inst, home := newCodexGateInstance(t)
	inst.CodexSessionID = uniqueSID(t)
	inst.Status = StatusWaiting
	seedCodexRolloutWithMeta(t, home, inst.CodexSessionID, "user", "", true)
	storage := newTestStorage(t)
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	alive := &atomic.Bool{}
	alive.Store(true)
	starts := &atomic.Int32{}
	receipt := &procowner.Receipt{Generation: 7, Leader: procowner.Member{PID: 123456, StartID: "original"}, Command: "codex resume " + inst.CodexSessionID}
	ops := defaultAutoSleepOperations()
	ops.gate = func(*Instance, *procowner.Receipt, *AutoSleepState) (func() error, error) {
		return func() error { return nil }, nil
	}
	ops.receipt = func(*Instance) (*procowner.Receipt, error) { return receipt, nil }
	ops.paneIdentity = func(*Instance, *procowner.Receipt) (string, string, error) { return "%123", "$4", nil }
	ops.capture = func(storage *Storage, inst *Instance, state *AutoSleepState) error {
		dir := filepath.Join(filepath.Dir(storage.dbPath), "auto-sleep")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		path := filepath.Join(dir, spawnLockSafeID(inst.ID)+"-fixture.txt")
		data := []byte("retained terminal output\n")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		state.ScrollbackPath = path
		state.ScrollbackSHA256 = hex.EncodeToString(sum[:])
		state.ScrollbackBytes = int64(len(data))
		return nil
	}
	ops.stop = func(*Instance, *procowner.Receipt) error { alive.Store(false); return nil }
	ops.retire = func(*Instance, *procowner.Receipt, *AutoSleepState) error { return nil }
	ops.exists = func(*Instance) (bool, error) { return alive.Load(), nil }
	ops.ownership = func(*Instance) OwnershipStatus {
		verdict := procowner.VerdictClear
		if alive.Load() {
			verdict = procowner.VerdictOwned
		}
		return OwnershipStatus{Receipt: receipt, Report: procowner.Report{Verdict: verdict}, PaneAttached: alive.Load()}
	}
	ops.start = func(inst *Instance) error {
		command := inst.buildCodexCommand(inst.Command)
		if !strings.Contains(command, " resume "+inst.CodexSessionID) || strings.Contains(command, " fork ") {
			return fmt.Errorf("not an exact original resume: %s", command)
		}
		starts.Add(1)
		alive.Store(true)
		return nil
	}
	ops.ready = func(*Instance) error { return nil }
	ops.clearReceipt = func(*procowner.Receipt) error { return nil }
	return storage, inst, ops, alive, starts
}

func TestMatrixAutoSleepRoundTripAndRetainedPreview(t *testing.T) {
	storage, inst, ops, _, starts := matrixAutoSleepCoreFixture(t)
	checks := 0
	if err := autoSleepSessionWithOperations(storage, inst, func() error { checks++; return nil }, ops); err != nil {
		t.Fatal(err)
	}
	if checks != 3 || !inst.IsAutoSleeping() || inst.Status != StatusStopped {
		t.Fatalf("sleep not confirmed: checks=%d state=%+v status=%s", checks, inst.AutoSleep, inst.Status)
	}
	lite, _, err := storage.LoadLite()
	if err != nil || len(lite) != 1 || lite[0].AutoSleep == nil {
		t.Fatalf("lite hydration lost sleep marker: %+v %v", lite, err)
	}
	text, err := ReadAutoSleepScrollback(inst)
	if err != nil || text != "retained terminal output\n" {
		t.Fatalf("preview=%q err=%v", text, err)
	}
	woke, err := wakeAutoSleepingSessionWithOperations(storage, inst, ops)
	if err != nil || !woke || starts.Load() != 1 || inst.IsAutoSleeping() {
		t.Fatalf("wake=%v err=%v starts=%d state=%+v", woke, err, starts.Load(), inst.AutoSleep)
	}
	if !inst.HasAutoSleepHistory() {
		t.Fatal("wake discarded saved history reference")
	}
	saved, readErr := ReadAutoSleepScrollback(inst)
	if readErr != nil || saved != "retained terminal output\n" {
		t.Fatalf("wake lost readable history: %q %v", saved, readErr)
	}
	row, _ := storage.db.LoadInstanceByID(inst.ID)
	if ReadAutoSleepStateFromToolData(row.ToolData) != nil || row.Status != "waiting" {
		t.Fatalf("durable wake missing: %+v", row)
	}
	if time.Since(row.LastAccessed) > 2*time.Second {
		t.Fatal("wake activity time was not durable")
	}
}

func TestMatrixAutoSleepFailedStopRetainsPending(t *testing.T) {
	storage, inst, ops, _, starts := matrixAutoSleepCoreFixture(t)
	ops.stop = func(*Instance, *procowner.Receipt) error { return errors.New("identity no longer verifiable") }
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err == nil {
		t.Fatal("failed stop reported success")
	}
	row, _ := storage.db.LoadInstanceByID(inst.ID)
	state := ReadAutoSleepStateFromToolData(row.ToolData)
	if state == nil || state.Phase != "pending" || inst.IsAutoSleeping() || row.Status != "waiting" {
		t.Fatalf("lost failure state: %+v", row)
	}
	if _, err := ReadAutoSleepScrollback(inst); err != nil {
		t.Fatal(err)
	}
	if _, err := wakeAutoSleepingSessionWithOperations(storage, inst, ops); err == nil || starts.Load() != 0 {
		t.Fatalf("pending live session was respawned: %v", err)
	}
}

func TestMatrixAutoSleepFinalRecheckCancelsBeforeSignal(t *testing.T) {
	storage, inst, ops, _, _ := matrixAutoSleepCoreFixture(t)
	checks, stops, restored := 0, 0, false
	ops.gate = func(*Instance, *procowner.Receipt, *AutoSleepState) (func() error, error) {
		return func() error { restored = true; return nil }, nil
	}
	ops.stop = func(*Instance, *procowner.Receipt) error { stops++; return nil }
	err := autoSleepSessionWithOperations(storage, inst, func() error {
		checks++
		if checks == 3 {
			return errors.New("new user activity")
		}
		return nil
	}, ops)
	if err == nil || stops != 0 || !restored || inst.hasAutoSleepState() {
		t.Fatalf("unsafe final recheck: err=%v stops=%d state=%+v", err, stops, inst.AutoSleep)
	}
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(storage.dbPath), "auto-sleep"))
	if len(entries) != 0 {
		t.Fatalf("cancelled capture left private orphan files: %v", entries)
	}
	row, _ := storage.db.LoadInstanceByID(inst.ID)
	if ReadAutoSleepStateFromToolData(row.ToolData) != nil {
		t.Fatal("pre-signal cancellation left wake intent")
	}
}

func TestMatrixAutoSleepWakeRejectsMissingOrSubagentRollout(t *testing.T) {
	for _, kind := range []string{"missing", "subagent", "custom"} {
		t.Run(kind, func(t *testing.T) {
			storage, inst, ops, _, starts := matrixAutoSleepCoreFixture(t)
			if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
				t.Fatal(err)
			}
			sid := inst.CodexSessionID
			path := CodexRolloutPathForInstance(inst)
			switch kind {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "subagent":
				seedCodexRolloutWithMeta(t, CodexHomeDirForInstance(inst), sid, "subagent", uniqueSID(t), true)
			case "custom":
				inst.Command = "codex exec unsafe"
				if _, err := storage.db.DB().Exec(`UPDATE instances SET command=? WHERE id=?`, inst.Command, inst.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := wakeAutoSleepingSessionWithOperations(storage, inst, ops); err == nil {
				t.Fatal("invalid exact resume was accepted")
			}
			if starts.Load() != 0 || inst.CodexSessionID != sid || !inst.hasAutoSleepState() {
				t.Fatal("invalid resume changed identity or marker")
			}
		})
	}
}

func TestMatrixAutoSleepWakeRetryNeverDoubleSpawns(t *testing.T) {
	storage, inst, ops, _, starts := matrixAutoSleepCoreFixture(t)
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
		t.Fatal(err)
	}
	ops.ready = func(*Instance) error { return errors.New("composer still loading") }
	if _, err := wakeAutoSleepingSessionWithOperations(storage, inst, ops); err == nil {
		t.Fatal("readiness failure accepted")
	}
	if !inst.IsAutoSleeping() || starts.Load() != 1 {
		t.Fatal("failed wake lost marker")
	}
	ops.ready = func(*Instance) error { return nil }
	if woke, err := wakeAutoSleepingSessionWithOperations(storage, inst, ops); err != nil || !woke || starts.Load() != 1 {
		t.Fatalf("retry duplicated wake: woke=%v err=%v starts=%d", woke, err, starts.Load())
	}
}

func TestMatrixAutoSleepConcurrentWakeOneSpawn(t *testing.T) {
	storage, inst, ops, _, starts := matrixAutoSleepCoreFixture(t)
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, model := range []*Instance{inst, loaded[0]} {
		wg.Add(1)
		go func(model *Instance) {
			defer wg.Done()
			_, err := wakeAutoSleepingSessionWithOperations(storage, model, ops)
			errs <- err
		}(model)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("spawned %d copies", starts.Load())
	}
}

func TestMatrixAutoSleepStaleSaveCannotResurrectMarker(t *testing.T) {
	storage, inst, ops, _, _ := matrixAutoSleepCoreFixture(t)
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
		t.Fatal(err)
	}
	row, _ := storage.db.LoadInstanceByID(inst.ID)
	if err := storage.db.CompareAndSwapAutoSleep(row, autoSleepRaw(row.ToolData), nil, "waiting"); err != nil {
		t.Fatal(err)
	}
	inst.Title = "unrelated title edit"
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	after, _ := storage.db.LoadInstanceByID(inst.ID)
	if ReadAutoSleepStateFromToolData(after.ToolData) != nil || after.Status != "waiting" || after.Title != "unrelated title edit" {
		t.Fatalf("stale save resurrected sleep: %+v", after)
	}
}

func TestMatrixAutoSleepManualStopClearsDurableMarker(t *testing.T) {
	storage, inst, ops, _, _ := matrixAutoSleepCoreFixture(t)
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
		t.Fatal(err)
	}
	if err := inst.clearAutoSleepAfterManualStop(); err != nil {
		t.Fatal(err)
	}
	row, _ := storage.db.LoadInstanceByID(inst.ID)
	if inst.hasAutoSleepState() || ReadAutoSleepStateFromToolData(row.ToolData) != nil || row.Status != "stopped" {
		t.Fatal("manual stop left automatic wake intent")
	}
}

func TestMatrixAutoSleepPreviewRejectsTamperingAndSymlink(t *testing.T) {
	storage, inst, ops, _, _ := matrixAutoSleepCoreFixture(t)
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
		t.Fatal(err)
	}
	path := inst.AutoSleep.ScrollbackPath
	if err := os.WriteFile(path, []byte(strings.Repeat("x", int(inst.AutoSleep.ScrollbackBytes))), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAutoSleepScrollback(inst); err == nil {
		t.Fatal("tampered artifact accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "unrelated"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAutoSleepScrollback(inst); err == nil {
		t.Fatal("symlink artifact accepted")
	}
}

func TestMatrixAutoSleepOldModelLaunchGuardReadsDatabase(t *testing.T) {
	storage, inst, ops, _, _ := matrixAutoSleepCoreFixture(t)
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatal(err)
	}
	if err := autoSleepSessionWithOperations(storage, inst, func() error { return nil }, ops); err != nil {
		t.Fatal(err)
	}
	if err := loaded[0].guardAutoSleepOrdinaryLaunch(); err == nil {
		t.Fatal("stale awake model bypassed durable marker")
	}
}

func TestMatrixAutoSleepToolDataUnknownVersionRetained(t *testing.T) {
	raw := json.RawMessage(`{"matrix_auto_sleep":{"version":99,"phase":"future"},"notes":"keep"}`)
	state := ReadAutoSleepStateFromToolData(raw)
	if state == nil || state.Version != 99 {
		t.Fatal("future marker lost")
	}
	out := WriteAutoSleepStateToToolData(raw, nil, false)
	merged := statedb.MergeToolDataExtras(raw, out)
	if ReadAutoSleepStateFromToolData(merged) == nil {
		t.Fatal("unaware writer cleared marker")
	}
	clear := WriteAutoSleepStateToToolData(raw, nil, true)
	if ReadAutoSleepStateFromToolData(statedb.MergeToolDataExtras(raw, clear)) != nil {
		t.Fatal("intentional clear resurrected marker")
	}
}

// matrix/hardened: exercise real private tmux + process ownership, but use an
// inert fake Codex command so no real account/network/session is touched.
func TestMatrixAutoSleepPrivateTmuxResumeAndFirstInput(t *testing.T) {
	skipIfNoTmuxBinary(t)
	inst, home := newCodexGateInstance(t)
	sid := uniqueSID(t)
	inst.CodexSessionID = sid
	seedCodexRolloutWithMeta(t, home, sid, "user", "", true)
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	argsFile := filepath.Join(dir, "argv")
	inputFile := filepath.Join(dir, "input")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellescape.Quote(argsFile) + "\nprintf 'OpenAI Codex\\nretained-history-proof\\n› '\nwhile IFS= read -r line; do\n printf '%s\\n' \"$line\" >> " + shellescape.Quote(inputFile) + "\n printf 'accepted\\n› '\ndone\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	restore := resetUserConfigCache(t, &UserConfig{Codex: CodexSettings{Command: shellescape.Quote(bin)}})
	defer restore()
	inst.IdentityInjectionDisabled = true
	storage := newTestStorage(t)
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.KillAndWait() })
	target := inst.GetTmuxSession()
	if _, err := tmux.OutputBounded(target.SocketName, "set-window-option", "-t", "="+target.Name, "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		_, err := autoSleepOwnedReceipt(inst)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn ownership unavailable: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(6 * time.Second)
	for {
		frame, captureErr := target.CapturePaneFresh()
		if captureErr == nil && strings.Contains(frame, "retained-history-proof") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake Codex composer did not render: %q %v", frame, captureErr)
		}
		time.Sleep(40 * time.Millisecond)
	}
	if err := storage.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	// The final callback runs after tmux input is paused. Even a raw
	// send-keys cannot become a foreground draft during that transition.
	checks := 0
	cancelErr := AutoSleepSession(storage, inst, func() error {
		checks++
		if checks != 3 {
			return nil
		}
		out, err := tmux.OutputBounded(target.SocketName, "display-message", "-p", "-t", target.Name, "#{pane_input_off}")
		if err != nil || strings.TrimSpace(string(out)) != "1" {
			return fmt.Errorf("foreground input was not paused: %q %v", out, err)
		}
		if err = target.SendKeysToPrimaryWindow("input during pause"); err != nil {
			return err
		}
		if err = target.SendEnterToPrimaryWindow(); err != nil {
			return err
		}
		if err = storage.db.WriteLastAccessedForInteraction(inst.ID, time.Now()); err != nil {
			return err
		}
		return errors.New("new interactive attachment")
	})
	if cancelErr == nil || inst.AutoSleepPending() {
		t.Fatalf("new interaction did not cancel sleep: %v", cancelErr)
	}
	if data, _ := os.ReadFile(inputFile); len(data) != 0 {
		t.Fatalf("paused input reached foreground: %q", data)
	}
	inputState, stateErr := tmux.OutputBounded(target.SocketName, "display-message", "-p", "-t", target.Name, "#{pane_input_off}")
	if stateErr != nil || strings.TrimSpace(string(inputState)) != "0" {
		t.Fatalf("cancel did not restore foreground input: %q %v", inputState, stateErr)
	}
	if err := inst.MarkAccessedForInteraction(); err != nil {
		t.Fatal(err)
	}
	if err := AutoSleepSession(storage, inst, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !inst.IsAutoSleeping() {
		t.Fatal("private terminal did not sleep")
	}
	if exists, err := target.ProbeExists(); err != nil || exists {
		t.Fatalf("retained dead pane was not retired: exists=%v err=%v", exists, err)
	}
	preview, err := ReadAutoSleepScrollback(inst)
	if err != nil || !strings.Contains(preview, "retained-history-proof") {
		t.Fatalf("full retained preview=%q err=%v", preview, err)
	}
	if woke, err := WakeAutoSleepingSession(storage, inst); err != nil || !woke {
		t.Fatalf("wake=%v err=%v", woke, err)
	}
	saved, err := ReadAutoSleepScrollback(inst)
	if err != nil || !strings.Contains(saved, "retained-history-proof") {
		t.Fatalf("wake stranded old scrollback: %q %v", saved, err)
	}
	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(argv), "resume\n"+sid+"\n") != 2 {
		t.Fatalf("both launches must resume exact SID: %q", argv)
	}
	if err := target.SendKeysToPrimaryWindow("first input after wake"); err != nil {
		t.Fatal(err)
	}
	if err := target.SendEnterToPrimaryWindow(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(inputFile)
		if string(data) == "first input after wake\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first post-wake input not preserved: %q", data)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestMatrixAutoSleepInteractionRequiresOwningDurableRow(t *testing.T) {
	storage, inst, _, _, _ := matrixAutoSleepCoreFixture(t)
	if err := inst.MarkAccessedForInteraction(); err != nil {
		t.Fatal(err)
	}
	row, err := storage.db.LoadInstanceByID(inst.ID)
	if err != nil || time.Since(row.LastAccessed) > 2*time.Second {
		t.Fatalf("interaction not durable: %+v %v", row, err)
	}
	if err := storage.db.DeleteInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := inst.MarkAccessedForInteraction(); err == nil {
		t.Fatal("missing row was treated as durable activity")
	}
	if err := (&Instance{ID: "unowned"}).MarkAccessedForInteraction(); err == nil {
		t.Fatal("unowned storage was accepted")
	}
}

func TestMatrixAutoSleepInteractionWaitsThenObservesCommittedSleep(t *testing.T) {
	storage, inst, ops, _, _ := matrixAutoSleepCoreFixture(t)
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	continueStop := make(chan struct{})
	sleepDone := make(chan error, 1)
	checks := 0
	go func() {
		sleepDone <- autoSleepSessionWithOperations(storage, inst, func() error {
			checks++
			if checks == 3 {
				close(reached)
				<-continueStop
			}
			return nil
		}, ops)
	}()
	<-reached
	interacted := make(chan error, 1)
	go func() { interacted <- loaded[0].MarkAccessedForInteraction() }()
	select {
	case err := <-interacted:
		t.Fatalf("interaction bypassed sleep spawn lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(continueStop)
	if err := <-sleepDone; err != nil {
		t.Fatal(err)
	}
	if err := <-interacted; err != nil {
		t.Fatal(err)
	}
	if !loaded[0].IsAutoSleeping() || loaded[0].GetStatusThreadSafe() != StatusStopped {
		t.Fatal("late interaction did not observe committed sleep state")
	}
}
