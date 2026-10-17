package session

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
)

// matrix/hardened: these fixtures never start, stop, signal or send input to a
// real agent. Native activity and durable delivery remain separate evidence.
func matrixAutoSleepFixture(t *testing.T, suffix ...string) (*Instance, string, time.Time) {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	inst := &Instance{ID: "sleep-evidence-fixture", Tool: "codex", Command: "codex", Status: StatusWaiting, CodexSessionID: matrixCodexThread, ProjectPath: cwd}
	inst.storageSnapshot = &instanceStorageSnapshot{dbPath: filepath.Join(t.TempDir(), "state.db")}
	at := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Millisecond)
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"thread_source":"user","source":"cli","cwd":%q}}`, matrixCodexThread, cwd)
	done := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-sleep","error":null}}`, at.Format(time.RFC3339Nano))
	dir := filepath.Join(home, "sessions", "2026", "10", "17")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-17T10-00-00-"+matrixCodexThread+".jsonl")
	lines := append([]string{meta, done}, suffix...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return inst, path, at
}

func matrixAutoSleepStubCandidate(t *testing.T) {
	t.Helper()
	priorLive, priorPane, priorProcesses := matrixAutoSleepLiveID, matrixAutoSleepPaneProbe, matrixAutoSleepProcessProbe
	matrixAutoSleepLiveID = func(i *Instance) string { return i.CodexSessionID }
	matrixAutoSleepPaneProbe = func(*Instance) (matrixAutoSleepPane, error) {
		return matrixAutoSleepPane{PID: 40, ID: "%1", Frame: "› \n\n ? for shortcuts", Proof: "pane-40"}, nil
	}
	matrixAutoSleepProcessProbe = func(*Instance, int) (string, error) { return "owned-idle-tree", nil }
	t.Cleanup(func() {
		matrixAutoSleepLiveID, matrixAutoSleepPaneProbe, matrixAutoSleepProcessProbe = priorLive, priorPane, priorProcesses
	})
}

func TestMatrixAutoSleepCandidateExactNormalCompletion(t *testing.T) {
	inst, path, at := matrixAutoSleepFixture(t)
	matrixAutoSleepStubCandidate(t)
	evidence, err := InspectAutoSleepCandidate(inst, []*Instance{inst})
	if err != nil || evidence.ThreadID != matrixCodexThread || evidence.TurnID != "turn-sleep" || !evidence.CompletedAt.Equal(at) || len(evidence.Fingerprint) != 64 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	again, err := InspectAutoSleepCandidate(inst, []*Instance{inst})
	if err != nil || again.Fingerprint != evidence.Fingerprint {
		t.Fatalf("unchanged evidence was unstable: %+v %v", again, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"event_msg","payload":{"type":"user_message","message":"continue"}}` + "\n")
	_ = f.Close()
	if _, err := InspectAutoSleepCandidate(inst, []*Instance{inst}); err == nil {
		t.Fatal("new native user input retained sleep eligibility")
	}
}

func TestMatrixAutoSleepLatestNativeEventRefusals(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"running", `{"type":"event_msg","payload":{"type":"task_started","turn_id":"new"}}`},
		{"approval", `{"type":"event_msg","payload":{"type":"exec_approval_request"}}`},
		{"aborted", `{"type":"event_msg","payload":{"type":"turn_aborted"}}`},
		{"unknown event", `{"type":"event_msg","payload":{"type":"new_unknown_event"}}`},
		{"malformed", `{"type":"event_msg",`},
		{"unknown record", `{"type":"new_unknown_record","payload":{}}`},
		{"tool after completion", `{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"later"}}`},
		{"failed completion", `{"timestamp":"2026-10-16T00:00:00Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"error-turn","error":{"message":"offline"}}}`},
		{"completion missing turn id", `{"timestamp":"2026-10-16T00:00:00Z","type":"event_msg","payload":{"type":"turn_complete"}}`},
		{"completion missing time", `{"type":"event_msg","payload":{"type":"turn_complete","turn_id":"new"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path, _ := matrixAutoSleepFixture(t, tc.line)
			if _, _, err := matrixAutoSleepRollout(path, matrixCodexThread); err == nil {
				t.Fatal("unsafe latest native event accepted")
			}
		})
	}
	t.Run("complete JSON without newline", func(t *testing.T) {
		_, path, _ := matrixAutoSleepFixture(t)
		data, _ := os.ReadFile(path)
		_ = os.WriteFile(path, data[:len(data)-1], 0600)
		if _, _, err := matrixAutoSleepRollout(path, matrixCodexThread); err == nil {
			t.Fatal("unflushed final record accepted")
		}
	})
}

func TestMatrixAutoSleepInstanceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Instance)
	}{
		{"running", func(i *Instance) { i.Status = StatusRunning }},
		{"unknown", func(i *Instance) { i.Status = StatusUnknown }},
		{"pinned", func(i *Instance) { i.Pin = PinTop }},
		{"archived", func(i *Instance) { i.ArchivedAt = time.Now() }},
		{"ssh", func(i *Instance) { i.SSHHost = "fixture" }},
		{"sandbox", func(i *Instance) { i.Sandbox = &SandboxConfig{Enabled: true} }},
		{"custom", func(i *Instance) { i.Command = "codex custom" }},
		{"wrapper", func(i *Instance) { i.Wrapper = "wrapper" }},
		{"wrong id", func(i *Instance) { i.CodexSessionID = "wrong" }},
		{"native", func(i *Instance) { i.terminalBackendExpected = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, _, _ := matrixAutoSleepFixture(t)
			matrixAutoSleepStubCandidate(t)
			tc.edit(inst)
			if _, err := InspectAutoSleepCandidate(inst, nil); err == nil {
				t.Fatal("unsupported session accepted")
			}
		})
	}
	t.Run("live identity ambiguous", func(t *testing.T) {
		inst, _, _ := matrixAutoSleepFixture(t)
		matrixAutoSleepStubCandidate(t)
		matrixAutoSleepLiveID = func(*Instance) string { return "" }
		if _, err := InspectAutoSleepCandidate(inst, nil); err == nil {
			t.Fatal("ambiguous live thread accepted")
		}
	})
	t.Run("child stopped snapshot is not proof", func(t *testing.T) {
		inst, _, _ := matrixAutoSleepFixture(t)
		matrixAutoSleepStubCandidate(t)
		child := &Instance{ID: "child", ParentSessionID: inst.ID, Status: StatusStopped}
		if _, err := InspectAutoSleepCandidate(inst, []*Instance{inst, child}); err == nil {
			t.Fatal("unprobed child accepted on stale stopped status")
		}
	})
}

func TestMatrixAutoSleepComposerAndClients(t *testing.T) {
	empty := "› \n\n  ? for shortcuts 100% context left"
	if err := matrixAutoSleepComposer(empty); err != nil {
		t.Fatalf("empty composer: %v", err)
	}
	for _, frame := range []string{
		"› unsent user draft\n\n  ? for shortcuts 100% context left",
		"Would you like to run this command?\n› 1. Yes, proceed\n  2. No",
		"• Queued follow-up inputs\n  ↳ next task\n" + empty,
		"[Image #1]\n" + empty,
		"1 background terminal running\n" + empty,
	} {
		if err := matrixAutoSleepComposer(frame); err == nil {
			t.Fatalf("unsafe composer accepted: %q", frame)
		}
	}
	for _, clients := range []string{"", "1\tattached,control-mode,ignore-size,no-output,read-only\n"} {
		if err := matrixAutoSleepClients(clients); err != nil {
			t.Fatalf("read-only observer blocked: %v", err)
		}
	}
	for _, clients := range []string{"0\tattached\n", "1\tattached,control-mode\n", "unparseable", "1\tread-only\n"} {
		if err := matrixAutoSleepClients(clients); err == nil {
			t.Fatalf("interactive/unknown client accepted: %q", clients)
		}
	}
}

func TestMatrixAutoSleepDurableDeliveryRefusals(t *testing.T) {
	inst, _, _ := matrixAutoSleepFixture(t)
	for _, path := range []string{InboxPathFor(inst.ID), inboxInflightPathFor(inst.ID), heldSendPath(inst.ID), ledgerOwedSenderPath(inst.ID)} {
		t.Run(filepath.Base(filepath.Dir(path))+filepath.Base(path), func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			for _, contents := range []string{`{"valid":"pending"}`, `{broken`} {
				_ = os.WriteFile(path, []byte(contents), 0600)
				if _, err := matrixAutoSleepPendingProof(inst.ID, inst.CodexSessionID, filepath.Dir(inst.storageSnapshot.dbPath)); err == nil {
					t.Fatal("pending or corrupt delivery accepted")
				}
			}
			_ = os.Remove(path)
		})
	}
	profileDir := filepath.Dir(inst.storageSnapshot.dbPath)
	dir := sendqueue.Dir(profileDir)
	_ = os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "sleep-fixture.json")
	t.Cleanup(func() { _ = os.Remove(path) })
	for _, state := range []string{sendqueue.StateQueued, sendqueue.StateTyping, sendqueue.StateTyped, sendqueue.StateSubmitted, "unknown"} {
		data, _ := json.Marshal(sendqueue.Record{SendID: "sleep-fixture", SessionID: inst.ID, State: state})
		_ = os.WriteFile(path, data, 0600)
		if _, err := matrixAutoSleepPendingProof(inst.ID, inst.CodexSessionID, profileDir); err == nil {
			t.Fatalf("unfinished delivery state accepted: %s", state)
		}
	}
	_ = os.WriteFile(path, []byte("{broken"), 0600)
	if _, err := matrixAutoSleepPendingProof(inst.ID, inst.CodexSessionID, profileDir); err == nil {
		t.Fatal("bad queue JSON accepted")
	}
	for _, state := range []string{sendqueue.StateLanded, sendqueue.StateFailed} {
		data, _ := json.Marshal(sendqueue.Record{SendID: "sleep-fixture", SessionID: inst.ID, State: state})
		_ = os.WriteFile(path, data, 0600)
		if _, err := matrixAutoSleepPendingProof(inst.ID, inst.CodexSessionID, profileDir); err != nil {
			t.Fatalf("terminal delivery blocked: %v", err)
		}
	}
}

// matrix/hardened: queue Final is a worker lifecycle verdict, not evidence of
// successful input acceptance. A settled send may remain typed/submitted even
// when the composer is empty and the only durable turn is an old completion.
func TestMatrixAutoSleepRefusesSettledUnconfirmedQueuedInput(t *testing.T) {
	for _, state := range []string{sendqueue.StateTyped, sendqueue.StateSubmitted} {
		for _, settled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-settled-%t", state, settled), func(t *testing.T) {
				inst, rollout, _ := matrixAutoSleepFixture(t)
				matrixAutoSleepStubCandidate(t)
				dir := sendqueue.Dir(filepath.Dir(inst.storageSnapshot.dbPath))
				now := time.Now().UTC()
				id, err := sendqueue.NextID(dir, now)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(rollout)
				if err != nil {
					t.Fatal(err)
				}
				record := &sendqueue.Record{
					SendID: id, SessionID: inst.ID, SessionTitle: "sleep fixture", Tool: "codex",
					Message: "follow up after the prior completion", State: state, Settled: settled,
					Verdict: "unknown", Reason: "outcome unknown; not retyped, transcript watch ended",
					CreatedAt:      now.Add(-90 * time.Minute).Format(time.RFC3339Nano),
					UpdatedAt:      now.Add(-60 * time.Minute).Format(time.RFC3339Nano),
					Deadline:       now.Add(-60 * time.Minute).Format(time.RFC3339Nano),
					TranscriptPath: rollout, TranscriptFrom: info.Size(), Sender: "cli",
				}
				if err := sendqueue.Save(dir, record); err != nil {
					t.Fatal(err)
				}
				loaded, err := sendqueue.Load(dir, id)
				if err != nil || loaded.State != state || loaded.Settled != settled || loaded.Final() != settled {
					t.Fatalf("invalid queue fixture: %+v %v", loaded, err)
				}
				if _, err := InspectAutoSleepCandidate(inst, []*Instance{inst}); err == nil || !strings.Contains(err.Error(), "unfinished delivery") {
					t.Fatalf("unconfirmed input allowed sleep: state=%s settled=%t err=%v", state, settled, err)
				}
			})
		}
	}
}

func TestMatrixAutoSleepNativeChildMustFinish(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			_, path, at := matrixAutoSleepFixture(t)
			data, _ := os.ReadFile(path)
			meta := strings.Split(string(data), "\n")[0]
			lines := []string{meta,
				`{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"spawn","arguments":"{}"}}`,
				`{"type":"response_item","payload":{"type":"function_call_output","call_id":"spawn","output":"{\"agent_id\":\"child-1\"}"}}`,
			}
			if terminal {
				lines = append(lines,
					`{"type":"response_item","payload":{"type":"function_call","name":"wait_agent","call_id":"wait","arguments":"{\"ids\":[\"child-1\"]}"}}`,
					`{"type":"response_item","payload":{"type":"function_call_output","call_id":"wait","output":"{\"status\":{\"child-1\":{\"completed\":\"done\"}}}"}}`)
			}
			lines = append(lines, fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete","turn_id":"done"}}`, at.Format(time.RFC3339Nano)))
			_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
			_, _, err := matrixAutoSleepRollout(path, matrixCodexThread)
			if (err == nil) != terminal {
				t.Fatalf("terminal=%v err=%v", terminal, err)
			}
		})
	}
}

// matrix/hardened: compatibility fixtures contain only synthetic metadata.
// They establish both what may preserve completion and what must invalidate it.
func matrixAutoSleepMetadataLine(t *testing.T, kind string, payload any, at time.Time) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"timestamp": at.Format(time.RFC3339Nano), "type": kind, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func matrixAutoSleepTokenFixture() map[string]any {
	usage := func() map[string]any {
		return map[string]any{"input_tokens": 10, "cached_input_tokens": 2, "cache_write_input_tokens": 0, "output_tokens": 3, "reasoning_output_tokens": 1, "total_tokens": 13}
	}
	return map[string]any{
		"thread_id": matrixCodexThread, "session_id": matrixCodexThread, "turn_id": "turn-sleep", "root_turn_id": "turn-sleep", "response_id": "response-fixture",
		"usage": usage(), "turn_token_usage": usage(), "thread_token_usage": usage(),
	}
}

func matrixAutoSleepSettingsFixture(cwd string) map[string]any {
	return map[string]any{
		"type": "thread_settings_applied", "thread_id": matrixCodexThread,
		"thread_settings": map[string]any{
			"model": "model-fixture", "model_provider_id": "provider-fixture", "cwd": cwd,
			"approval_policy": "never", "approvals_reviewer": "user", "permission_profile": map[string]any{"type": "disabled"},
			"collaboration_mode":      map[string]any{"mode": "default", "settings": map[string]any{"model": "model-fixture", "reasoning_effort": "medium", "developer_instructions": nil}},
			"runtime_workspace_roots": []string{cwd}, "disabled_plugin_ids": []string{}, "service_tier": "standard",
		},
	}
}

func TestMatrixAutoSleepKnownMetadataPreservesOrInvalidatesCompletion(t *testing.T) {
	for _, kind := range []string{"world_state", "inter_agent_communication_metadata", "token_usage_record", "thread_settings_applied"} {
		for _, finishAgain := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-new-completion-%t", kind, finishAgain), func(t *testing.T) {
				inst, path, at := matrixAutoSleepFixture(t)
				payload := any(nil)
				recordType := kind
				switch kind {
				case "world_state":
					payload = map[string]any{"full": true, "state": map[string]any{"fixture_context": "snapshot"}}
				case "inter_agent_communication_metadata":
					payload = map[string]any{"trigger_turn": true}
				case "token_usage_record":
					payload = matrixAutoSleepTokenFixture()
				case "thread_settings_applied":
					recordType, payload = "event_msg", matrixAutoSleepSettingsFixture(inst.ProjectPath)
				}
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, recordType, payload, at.Add(time.Millisecond)) + "\n")
				if finishAgain {
					_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn-sleep"}, at.Add(2*time.Millisecond)) + "\n")
				}
				_ = f.Close()
				proof, _, err := matrixAutoSleepRollout(path, matrixCodexThread)
				want := finishAgain || kind == "token_usage_record" || kind == "thread_settings_applied"
				if (err == nil) != want || want && proof.TurnID != "turn-sleep" {
					t.Fatalf("kind=%s new-completion=%t proof=%+v err=%v", kind, finishAgain, proof, err)
				}
			})
		}
	}
}

func TestMatrixAutoSleepMetadataSchemaAndIdentityRefusals(t *testing.T) {
	tests := []struct {
		name, kind string
		payload    func(string) map[string]any
	}{
		{"world missing full", "world_state", func(string) map[string]any { return map[string]any{"state": map[string]any{}} }},
		{"world wrong state", "world_state", func(string) map[string]any { return map[string]any{"full": true, "state": "unknown"} }},
		{"communication wrong flag", "inter_agent_communication_metadata", func(string) map[string]any { return map[string]any{"trigger_turn": "true"} }},
		{"communication new field", "inter_agent_communication_metadata", func(string) map[string]any { return map[string]any{"trigger_turn": false, "new_action": true} }},
	}
	for _, field := range []string{"thread_id", "session_id", "turn_id", "root_turn_id"} {
		field := field
		tests = append(tests, struct {
			name, kind string
			payload    func(string) map[string]any
		}{"accounting wrong " + field, "token_usage_record", func(string) map[string]any {
			p := matrixAutoSleepTokenFixture()
			p[field] = "different-identity"
			return p
		}})
	}
	for _, mutation := range []string{"missing usage", "negative count", "noninteger count", "unknown count", "unknown record field"} {
		mutation := mutation
		tests = append(tests, struct {
			name, kind string
			payload    func(string) map[string]any
		}{mutation, "token_usage_record", func(string) map[string]any {
			p := matrixAutoSleepTokenFixture()
			switch mutation {
			case "missing usage":
				delete(p, "usage")
			case "negative count":
				p["usage"].(map[string]any)["input_tokens"] = -1
			case "noninteger count":
				p["usage"].(map[string]any)["input_tokens"] = 1.5
			case "unknown count":
				p["usage"].(map[string]any)["new_action"] = true
			case "unknown record field":
				p["new_action"] = true
			}
			return p
		}})
	}
	for _, mutation := range []string{"wrong thread", "missing snapshot", "missing model", "relative cwd", "unknown permission", "unknown setting", "wrong roots", "missing collaboration"} {
		mutation := mutation
		tests = append(tests, struct {
			name, kind string
			payload    func(string) map[string]any
		}{mutation, "event_msg", func(cwd string) map[string]any {
			p := matrixAutoSleepSettingsFixture(cwd)
			s := p["thread_settings"].(map[string]any)
			switch mutation {
			case "wrong thread":
				p["thread_id"] = "different-identity"
			case "missing snapshot":
				delete(p, "thread_settings")
			case "missing model":
				delete(s, "model")
			case "relative cwd":
				s["cwd"] = "relative"
			case "unknown permission":
				s["permission_profile"] = map[string]any{"type": "managed"}
			case "unknown setting":
				s["new_action"] = true
			case "wrong roots":
				s["runtime_workspace_roots"] = []int{1}
			case "missing collaboration":
				delete(s, "collaboration_mode")
			}
			return p
		}})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inst, path, at := matrixAutoSleepFixture(t)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, test.kind, test.payload(inst.ProjectPath), at) + "\n")
			// A later normal parent completion cannot bless an unknown schema.
			_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn-sleep"}, at) + "\n")
			_ = f.Close()
			if _, _, err := matrixAutoSleepRollout(path, matrixCodexThread); err == nil {
				t.Fatal("unsupported metadata became eligible")
			}
		})
	}
}

func TestMatrixAutoSleepTaskPathCollaborationStaysUnsupported(t *testing.T) {
	for _, test := range []struct {
		name, namespace, tool, args, output string
	}{
		{"bare task spawn", "", "spawn_agent", `{"task_name":"worker_fixture","message":"fixture"}`, `{"task_name":"/root/worker_fixture"}`},
		{"namespaced task spawn", "collaboration", "spawn_agent", `{"task_name":"worker_fixture","message":"fixture"}`, `{"task_name":"/root/worker_fixture"}`},
		{"bare followup without prior spawn", "", "followup_task", `{"target":"/root/worker_fixture","message":"fixture"}`, `null`},
		{"namespaced followup without prior spawn", "collaboration", "followup_task", `{"target":"/root/worker_fixture","message":"fixture"}`, `null`},
		{"prefixed followup", "", "collaboration.followup_task", `{"target":"/root/worker_fixture","message":"fixture"}`, `null`},
		{"task-path wait", "", "wait_agent", `{"timeout_ms":1000}`, `{"message":"fixture","timed_out":false}`},
		{"new collaboration operation", "collaboration", "future_operation", `{}`, `null`},
		{"versioned collaboration namespace", "collaboration.v2", "future_operation", `{}`, `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, path, at := matrixAutoSleepFixture(t)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, "response_item", map[string]any{"type": "function_call", "name": test.tool, "namespace": test.namespace, "arguments": test.args, "call_id": "fixture-call"}, at) + "\n")
			_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "fixture-call", "output": test.output}, at) + "\n")
			_, _ = f.WriteString(matrixAutoSleepMetadataLine(t, "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn-sleep"}, at) + "\n")
			_ = f.Close()
			if _, _, err := matrixAutoSleepRollout(path, matrixCodexThread); err == nil {
				t.Fatal("parent completion washed away unsupported collaboration")
			}
		})
	}
}

type matrixAutoSleepFakeProber struct {
	processes map[int]procowner.ProcInfo
	changed   int
	reads     map[int]int
}

func (*matrixAutoSleepFakeProber) Name() string            { return "fixture" }
func (*matrixAutoSleepFakeProber) BootID() (string, error) { return "fixture-boot", nil }
func (p *matrixAutoSleepFakeProber) Inspect(pid int) (procowner.ProcInfo, error) {
	info, ok := p.processes[pid]
	if !ok {
		return procowner.ProcInfo{}, procowner.ErrNoProcess
	}
	p.reads[pid]++
	if p.changed == pid && p.reads[pid] > 1 {
		info.StartID = "recycled"
	}
	return info, nil
}
func (p *matrixAutoSleepFakeProber) Descendants(root procowner.ProcInfo) ([]procowner.ProcInfo, error) {
	var result []procowner.ProcInfo
	for _, process := range p.processes {
		if process.PID != root.PID {
			result = append(result, process)
		}
	}
	return result, nil
}

func TestMatrixAutoSleepOnlyExactConfiguredMCPMayRemain(t *testing.T) {
	for _, kind := range []string{"codex-only", "configured-idle-mcp", "background-command", "unknown-node", "nested-helper", "missing-receipt-member", "recycled-pid"} {
		t.Run(kind, func(t *testing.T) {
			inst, _, _ := matrixAutoSleepFixture(t)
			node := filepath.Join(t.TempDir(), "node")
			_ = os.WriteFile(node, []byte("#!/bin/sh\nexit 0\n"), 0700)
			node, _ = filepath.EvalSymlinks(node)
			config := fmt.Sprintf("[mcp_servers.fixture]\ncommand = %q\nargs = [\"/fixture/mcp.mjs\"]\nenabled = false\n", node)
			_ = os.WriteFile(filepath.Join(inst.getCodexHomeDir(), "config.toml"), []byte(config), 0600)
			p := &matrixAutoSleepFakeProber{processes: map[int]procowner.ProcInfo{40: {PID: 40, PPID: 1, StartID: "root", UID: os.Getuid()}}, reads: map[int]int{}}
			commands := map[int]matrixAutoSleepCommand{40: {Executable: "/fixture/codex", Args: []string{"resume", matrixCodexThread}}}
			if kind != "codex-only" {
				p.processes[41] = procowner.ProcInfo{PID: 41, PPID: 40, StartID: "mcp", UID: os.Getuid()}
				commands[41] = matrixAutoSleepCommand{Executable: node, Args: []string{"/fixture/mcp.mjs"}}
			}
			switch kind {
			case "background-command":
				commands[41] = matrixAutoSleepCommand{Executable: "/bin/sh", Args: []string{"-c", "sleep 100"}}
			case "unknown-node":
				commands[41] = matrixAutoSleepCommand{Executable: node, Args: []string{"/fixture/background.mjs"}}
			case "nested-helper":
				p.processes[42] = procowner.ProcInfo{PID: 42, PPID: 41, StartID: "helper", UID: os.Getuid()}
				commands[42] = commands[41]
			case "recycled-pid":
				p.changed = 40
			}
			receipt := &procowner.Receipt{InstanceID: inst.ID, Generation: 1, State: procowner.StateLive, Leader: procowner.Member{PID: 40, StartID: "root"}}
			report := procowner.Report{Verdict: procowner.VerdictOwned}
			for pid, info := range p.processes {
				if kind == "missing-receipt-member" && pid == 41 {
					continue
				}
				report.Members = append(report.Members, procowner.MemberStatus{Member: procowner.Member{PID: pid, StartID: info.StartID}, State: procowner.StateOwned})
			}
			oldProber, oldOwnership, oldPIDs, oldCommand, oldCodexExecutable := ownershipProber, matrixAutoSleepOwnership, codexPaneProcessPIDs, matrixAutoSleepCommandProbe, matrixAutoSleepCodexExecutable
			ownershipProber = p
			matrixAutoSleepOwnership = func(*Instance) OwnershipStatus {
				return OwnershipStatus{Receipt: receipt, Report: report, PaneAttached: true}
			}
			codexPaneProcessPIDs = func(*Instance) ([]int, error) { return []int{40}, nil }
			matrixAutoSleepCodexExecutable = func() (string, error) { return "/fixture/codex", nil }
			matrixAutoSleepCommandProbe = func(pid int) (matrixAutoSleepCommand, error) {
				cmd, ok := commands[pid]
				if !ok {
					return cmd, errors.New("unknown process")
				}
				return cmd, nil
			}
			t.Cleanup(func() {
				ownershipProber, matrixAutoSleepOwnership, codexPaneProcessPIDs, matrixAutoSleepCommandProbe, matrixAutoSleepCodexExecutable = oldProber, oldOwnership, oldPIDs, oldCommand, oldCodexExecutable
			})
			_, err := matrixInspectAutoSleepProcesses(inst, 40)
			want := kind == "codex-only" || kind == "configured-idle-mcp"
			if (err == nil) != want {
				t.Fatalf("kind=%s err=%v", kind, err)
			}
		})
	}
}

func TestMatrixAutoSleepProcArgsKeepsBoundariesAndSkipsEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node")
	_ = os.WriteFile(path, []byte("fixture"), 0700)
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 4)
	data = append(data, []byte(path+"\x00\x00"+path+"\x00arg with spaces\x00\x00last\x00CREDENTIAL_MUST_NOT_BE_PARSED=secret\x00")...)
	cmd, err := matrixAutoSleepParseProcArgs(data)
	if err != nil || len(cmd.Args) != 3 || cmd.Args[0] != "arg with spaces" || cmd.Args[1] != "" || cmd.Args[2] != "last" {
		t.Fatalf("argv boundaries lost: %#v %v", cmd, err)
	}
	for _, malformed := range [][]byte{nil, {2, 0, 0, 0, 'x'}, data[:8]} {
		if _, err := matrixAutoSleepParseProcArgs(malformed); err == nil {
			t.Fatal("malformed OS argv accepted")
		}
	}
}

func TestMatrixAutoSleepNativeArgvProvider(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("exact argv unsupported on this platform")
	}
	command, err := matrixAutoSleepReadCommand(os.Getpid())
	if err != nil || !filepath.IsAbs(command.Executable) {
		t.Fatalf("native argv unavailable: %v", err)
	}
	// Only compare the kernel's argv. The provider never returns environment.
	if strings.Join(command.Args, "\x00") != strings.Join(os.Args[1:], "\x00") {
		t.Fatal("native argument vector differs from current process argv")
	}
}

func TestMatrixAutoSleepRolloutCacheUsesNativeSignatureAndBound(t *testing.T) {
	_, path, _ := matrixAutoSleepFixture(t)
	parses := 0
	cache := matrixAutoSleepRolloutCache{entries: map[string]matrixAutoSleepRolloutEntry{}, limit: 2}
	cache.parse = func(path, id string) (AutoSleepEvidence, string, error) {
		parses++
		return matrixParseAutoSleepRollout(path, id)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 4; n++ {
		if _, _, err := cache.load(path, matrixCodexThread); err != nil {
			t.Fatal(err)
		}
	}
	if matrixTurnInterruptSignature(before).hasChangeTime && parses != 1 {
		t.Fatalf("unchanged file parsed %d times", parses)
	}
	data, _ := os.ReadFile(path)
	// Rewrite at the same inode, size and mtime. ctime must invalidate it.
	data = []byte(strings.Replace(string(data), "turn-sleep", "turn-other", 1))
	_ = os.WriteFile(path, data, 0600)
	_ = os.Chtimes(path, before.ModTime(), before.ModTime())
	updated, _, err := cache.load(path, matrixCodexThread)
	if err != nil || updated.TurnID != "turn-other" {
		t.Fatalf("same-size native rewrite returned old evidence: %+v %v", updated, err)
	}
	for n := 0; n < 4; n++ {
		copyPath := filepath.Join(t.TempDir(), "rollout.jsonl")
		_ = os.WriteFile(copyPath, data, 0600)
		if _, _, err := cache.load(copyPath, matrixCodexThread); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) > 2 {
		t.Fatal("rollout cache exceeded its bound")
	}
}

func TestMatrixAutoSleepRechecksAfterSlowerProbes(t *testing.T) {
	for _, change := range []string{"native input", "composer draft", "attach query failed"} {
		t.Run(change, func(t *testing.T) {
			inst, path, _ := matrixAutoSleepFixture(t)
			matrixAutoSleepStubCandidate(t)
			if change == "native input" {
				matrixAutoSleepProcessProbe = func(*Instance, int) (string, error) {
					f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					_, _ = f.WriteString(`{"type":"event_msg","payload":{"type":"user_message","message":"continue"}}` + "\n")
					_ = f.Close()
					return "tree", nil
				}
			} else {
				calls := 0
				matrixAutoSleepPaneProbe = func(*Instance) (matrixAutoSleepPane, error) {
					calls++
					if calls > 1 {
						if change == "attach query failed" {
							return matrixAutoSleepPane{}, errors.New("query failed")
						}
						return matrixAutoSleepPane{PID: 40, ID: "%1", Frame: "› user draft", Proof: "pane-40"}, nil
					}
					return matrixAutoSleepPane{PID: 40, ID: "%1", Frame: "› ", Proof: "pane-40"}, nil
				}
			}
			if _, err := InspectAutoSleepCandidate(inst, nil); err == nil {
				t.Fatal("racing input/client change accepted")
			}
		})
	}
}
