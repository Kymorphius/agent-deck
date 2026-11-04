package statedb

// matrix/hardened: actual submitted input has a separate, monotonic clock.
// It lives in the extras zone so older binaries preserve it without a schema
// migration and a stale full-row save cannot turn a visit into an interaction.

import (
	"encoding/json"
	"sync/atomic"
	"time"
)

const interactionClockKey = "last_interaction_at"

// matrix/hardened: the legacy full-save path pre-reads extras before taking
// the SQLite writer. Re-read this one clock inside its INSERT statement so a
// targeted write in that gap cannot be overwritten. Both statements bind the
// instance ID at parameter 1 and tool_data at parameter 22; explicit numbered
// references reuse those arguments without changing the surrounding row shape.
const interactionClockInsertValueSQL = `(SELECT CASE WHEN stamp > 0
	THEN json_set(?22,'$.last_interaction_at',stamp) ELSE ?22 END FROM
	(SELECT MAX(
		CASE WHEN json_valid(?22) THEN CASE WHEN json_type(?22,'$.last_interaction_at')='integer'
		AND json_extract(?22,'$.last_interaction_at') BETWEEN 1 AND (unixepoch('now')+61)*1000000000
		THEN json_extract(?22,'$.last_interaction_at') ELSE 0 END ELSE 0 END,
		COALESCE((SELECT CASE WHEN json_valid(tool_data) THEN CASE
		WHEN json_type(tool_data,'$.last_interaction_at')='integer'
		AND json_extract(tool_data,'$.last_interaction_at') BETWEEN 1 AND (unixepoch('now')+61)*1000000000
		THEN json_extract(tool_data,'$.last_interaction_at') ELSE 0 END ELSE 0 END
		FROM instances WHERE id=?1),0)) AS stamp))`

// matrix/hardened: deterministic tests inject a targeted write in the legacy
// pre-read/INSERT gap. Production never installs a probe.
type interactionClockSaveProbe func(*StateDB, string)

var interactionClockFullSaveAfterRead atomic.Pointer[interactionClockSaveProbe]

func (s *StateDB) probeInteractionClockFullSave(id string) {
	if probe := interactionClockFullSaveAfterRead.Load(); probe != nil {
		(*probe)(s, id)
	}
}

func interactionClockStamp(raw json.RawMessage) int64 {
	var stamp int64
	if json.Unmarshal(raw, &stamp) != nil || stamp <= 0 || stamp > time.Now().Add(time.Minute).UnixNano() {
		return 0
	}
	return stamp
}

// WriteLastInteractionAt atomically keeps the later confirmed interaction.
// Nanoseconds retain ordering for multiple submitted prompts within a second.
func (s *StateDB) WriteLastInteractionAt(id string, at time.Time, expectedTouch ...int64) error {
	if at.IsZero() || at.UnixNano() <= 0 || at.After(time.Now().Add(time.Minute)) {
		return nil
	}
	return s.guardedRuntimeUpdate(id, expectedTouch,
		`UPDATE instances SET tool_data=json_set(COALESCE(tool_data,'{}'),'$.last_interaction_at',
		MAX(CASE WHEN json_type(tool_data,'$.last_interaction_at')='integer'
		AND json_extract(tool_data,'$.last_interaction_at') BETWEEN 1 AND ?
		THEN json_extract(tool_data,'$.last_interaction_at') ELSE 0 END, ?)) WHERE id=?`,
		time.Now().Add(time.Minute).UnixNano(), at.UnixNano(), id)
}
