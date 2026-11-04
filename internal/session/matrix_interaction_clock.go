package session

// matrix/hardened: recent interaction means submitted input, independently of
// attach/MRU/lifecycle access and background agent output.

import (
	"encoding/json"
	"time"
)

const toolDataLastInteractionAtKey = "last_interaction_at"

func validInteractionAt(at time.Time) bool {
	return !at.IsZero() && at.UnixNano() > 0 && !at.After(time.Now().Add(time.Minute))
}

func WriteLastInteractionAtToToolData(td json.RawMessage, at time.Time) json.RawMessage {
	values := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &values)
	}
	if values == nil {
		values = map[string]json.RawMessage{}
	}
	if validInteractionAt(at) {
		values[toolDataLastInteractionAtKey], _ = json.Marshal(at.UnixNano())
	} else {
		delete(values, toolDataLastInteractionAtKey)
	}
	out, _ := json.Marshal(values)
	return out
}

func ReadLastInteractionAtFromToolData(td json.RawMessage) time.Time {
	var values struct {
		At int64 `json:"last_interaction_at"`
	}
	if json.Unmarshal(td, &values) != nil || values.At <= 0 {
		return time.Time{}
	}
	at := time.Unix(0, values.At).UTC()
	if !validInteractionAt(at) {
		return time.Time{}
	}
	return at
}

func (i *Instance) LastInteractionAt() time.Time {
	if i == nil {
		return time.Time{}
	}
	i.mu.RLock()
	at := i.lastInteractionAt
	i.mu.RUnlock()
	if row, _ := i.runtimeView(); row != nil && validInteractionAt(row.LastInteractionAt) && row.LastInteractionAt.After(at) {
		at = row.LastInteractionAt
	}
	return at
}

// RecordInteractionAt accepts confirmed submission evidence only. Callers must
// not use attach, focus, draft keystrokes or assistant output as that evidence.
// Database IO never holds i.mu; the targeted SQL and stale-save merges both
// keep the maximum stamp across independently loaded processes.
func (i *Instance) RecordInteractionAt(at time.Time) error {
	if i == nil || !validInteractionAt(at) {
		return nil
	}
	i.mu.Lock()
	if at.After(i.lastInteractionAt) {
		i.lastInteractionAt = at.UTC()
	}
	to := i.lastInteractionAt
	skip := !to.After(i.lastInteractionPersisted)
	i.mu.Unlock()
	if skip {
		return nil
	}
	db := i.stateDB()
	if db == nil {
		return nil
	}
	if err := db.WriteLastInteractionAt(i.ID, to, i.producerGuardArgs()...); err != nil {
		return err
	}
	i.mu.Lock()
	if to.After(i.lastInteractionPersisted) {
		i.lastInteractionPersisted = to
	}
	i.mu.Unlock()
	return nil
}
