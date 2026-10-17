package statedb

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// matrix/hardened: lifecycle writes own only this marker/status, never a stale row.
var ErrAutoSleepConflict = errors.New("auto-sleep session identity or marker changed")

// CompareAndSwapAutoSleep changes only the exact row's sleep marker and optional
// status. The launch identity and previous marker must still match; unrelated
// tool_data fields, titles and grouping are preserved.
func (s *StateDB) CompareAndSwapAutoSleep(expected *InstanceRow, oldMarker, newMarker json.RawMessage, status string) error {
	if expected == nil {
		return fmt.Errorf("auto-sleep: missing expected row")
	}
	if len(oldMarker) == 0 {
		oldMarker = json.RawMessage(`null`)
	}
	if len(newMarker) == 0 {
		newMarker = json.RawMessage(`null`)
	}
	if !json.Valid(oldMarker) || !json.Valid(newMarker) {
		return fmt.Errorf("auto-sleep: invalid marker JSON")
	}
	var sid string
	var td struct {
		SID string `json:"codex_session_id"`
	}
	if err := json.Unmarshal(expected.ToolData, &td); err != nil {
		return err
	}
	sid = td.SID
	return withBusyRetry(func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.Exec(`UPDATE instances SET tool_data=json_set(tool_data,'$.matrix_auto_sleep',json(?),'$.matrix_auto_sleep_history',CASE WHEN json_extract(?,'$') IS NULL AND ?<>'' AND json_extract(tool_data,'$.matrix_auto_sleep') IS NOT NULL THEN json_insert(COALESCE(json_extract(tool_data,'$.matrix_auto_sleep_history'),json('[]')),'$[#]',json_extract(tool_data,'$.matrix_auto_sleep')) ELSE COALESCE(json_extract(tool_data,'$.matrix_auto_sleep_history'),json('[]')) END), status=CASE WHEN ?='' THEN status ELSE ? END, last_accessed=CASE WHEN ? IN ('waiting','starting','running') AND json_extract(?,'$') IS NULL THEN ? ELSE last_accessed END
 WHERE id=? AND tool=? AND command=? AND wrapper=? AND account=? AND project_path=? AND parent_session_id=? AND tmux_session=? AND tmux_socket_name=? AND archived_at=? AND pin=? AND last_accessed=?
 AND COALESCE(json_extract(tool_data,'$.codex_session_id'),'')=?
 AND COALESCE(json_extract(tool_data,'$.ssh_host'),'')=COALESCE(json_extract(?,'$.ssh_host'),'')
 AND COALESCE(json_extract(tool_data,'$.sandbox'),'null')=COALESCE(json_extract(?,'$.sandbox'),'null')
 AND COALESCE(json_extract(tool_data,'$.tool_options'),'null')=COALESCE(json_extract(?,'$.tool_options'),'null')
 AND COALESCE(json_extract(tool_data,'$.extra_args'),'null')=COALESCE(json_extract(?,'$.extra_args'),'null')
 AND COALESCE(json_extract(tool_data,'$.env_file'),'')=COALESCE(json_extract(?,'$.env_file'),'')
 AND COALESCE(json_extract(tool_data,'$.native_backend'),'null')=COALESCE(json_extract(?,'$.native_backend'),'null')
 AND COALESCE(json_extract(tool_data,'$.matrix_auto_sleep'),'null')=COALESCE(json_extract(?,'$'),'null')`,
			string(newMarker), string(newMarker), status, status, status, status, string(newMarker), time.Now().Unix(), expected.ID, expected.Tool, expected.Command, expected.Wrapper, expected.Account, expected.ProjectPath, expected.ParentSessionID, expected.TmuxSession, expected.TmuxSocketName, archivedAtUnix(expected.ArchivedAt), expected.Pin, expected.LastAccessed.Unix(), sid, string(expected.ToolData), string(expected.ToolData), string(expected.ToolData), string(expected.ToolData), string(expected.ToolData), string(expected.ToolData), string(oldMarker))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrAutoSleepConflict
		}
		if _, err = tx.Exec(`INSERT INTO metadata(key,value) VALUES('last_modified',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(time.Now().UnixNano())); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// WriteLastAccessedForInteraction refuses a removed row instead of treating a
// zero-row UPDATE as durable user activity.
// matrix/hardened: attach/send cannot proceed on an unpersisted visit fence.
func (s *StateDB) WriteLastAccessedForInteraction(id string, at time.Time) error {
	return withBusyRetry(func() error {
		result, err := s.db.Exec(`UPDATE instances SET last_accessed=MAX(COALESCE(last_accessed,0),?) WHERE id=?`, at.Unix(), id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("interaction session row no longer exists")
		}
		return nil
	})
}
