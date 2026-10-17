package session

import (
	"encoding/json"
)

const toolDataAutoSleepHistoryKey = "matrix_auto_sleep_history"

// matrix/hardened: retained terminal artifacts outlive the transient sleep
// marker; waking never strands their only reference or deletes prior output.
func cloneAutoSleepHistory(history []*AutoSleepState) []*AutoSleepState {
	if len(history) == 0 {
		return nil
	}
	out := make([]*AutoSleepState, len(history))
	for n, state := range history {
		out[n] = cloneAutoSleepState(state)
	}
	return out
}
func ReadAutoSleepHistoryFromToolData(td json.RawMessage) []*AutoSleepState {
	var m map[string]json.RawMessage
	if json.Unmarshal(td, &m) != nil {
		return nil
	}
	raw := m[toolDataAutoSleepHistoryKey]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var history []*AutoSleepState
	if json.Unmarshal(raw, &history) != nil {
		return []*AutoSleepState{{Phase: "invalid"}}
	}
	return history
}
func WriteAutoSleepHistoryToToolData(td json.RawMessage, history []*AutoSleepState) json.RawMessage {
	if len(history) == 0 {
		return td
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(td, &m)
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m[toolDataAutoSleepHistoryKey], _ = json.Marshal(history)
	raw, _ := json.Marshal(m)
	return raw
}
func autoSleepHistoryAfterClear(rowData json.RawMessage, state *AutoSleepState, status Status) []*AutoSleepState {
	history := ReadAutoSleepHistoryFromToolData(rowData)
	if state == nil && status != "" {
		if previous := ReadAutoSleepStateFromToolData(rowData); previous != nil {
			history = append(history, cloneAutoSleepState(previous))
		}
	}
	return history
}

// HasAutoSleepHistory reports saved terminal output after a successful wake.
// matrix/hardened: callers must not race on the retained reference slice.
func (i *Instance) HasAutoSleepHistory() bool {
	if i == nil {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.AutoSleepHistory) > 0
}
