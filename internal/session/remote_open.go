package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// matrix/hardened: opening is a server-authoritative operation. Cached row
// status never authorizes restarting a remote process.
var ErrRemotePrepareOpenUnsupported = errors.New("remote prepare-open is unsupported; update Agent Deck on the remote (or use R for an explicit restart)")

type RemoteOpenResult struct {
	ID             string `json:"id"`
	Disposition    string `json:"disposition"`
	Status         string `json:"status"`
	Group          string `json:"group,omitempty"`
	MaxConcurrent  int    `json:"max_concurrent,omitempty"`
	CodexSessionID string `json:"codex_session_id,omitempty"`
}

func (r *SSHRunner) PrepareOpen(ctx context.Context, id string) (RemoteOpenResult, error) {
	var result RemoteOpenResult
	if strings.TrimSpace(id) == "" {
		return result, fmt.Errorf("remote open requires a session ID")
	}
	// matrix/hardened: help is a read-only capability probe, performed for
	// each explicit open. Authentication and transport errors fail closed.
	out, err := r.Run(ctx, "session", "prepare-open", "--help")
	if err != nil {
		if remotePrepareOpenUnknown(out, err) {
			return result, ErrRemotePrepareOpenUnsupported
		}
		return result, fmt.Errorf("probe remote open support: %w", err)
	}
	// This verb is deliberately mutating in remoteVerbReadOnly. A reply lost
	// after dispatch must be reconciled by reading, never replayed over SSH.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	out, err = r.Run(ctx, "session", "prepare-open", "--json", id)
	if err != nil {
		return result, fmt.Errorf("prepare remote open: %w", err)
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return result, fmt.Errorf("decode remote open result (open may have completed): %w", err)
	}
	if result.ID != id {
		return result, fmt.Errorf("remote open returned a different session ID (open may have completed)")
	}
	switch Status(result.Status) {
	case StatusRunning, StatusWaiting, StatusIdle, StatusError, StatusStarting, StatusStopped, StatusQueued, StatusUnknown:
	default:
		return result, fmt.Errorf("remote open returned invalid status %q (open may have completed)", result.Status)
	}
	if result.Disposition == "queued" && result.MaxConcurrent <= 0 {
		return result, fmt.Errorf("remote open returned an invalid concurrency limit (open may have completed)")
	}
	switch result.Disposition {
	case "existing", "started", "resumed", "queued":
		return result, nil
	default:
		return result, fmt.Errorf("remote open returned unknown disposition %q (open may have completed)", result.Disposition)
	}
}

func remotePrepareOpenUnknown(out []byte, err error) bool {
	// matrix/hardened: only the exact old CLI diagnostic permits fallback.
	// A wrapper's permission denied, timeout, or generic SSH exit is not a
	// statement about the installed remote's capabilities.
	for _, text := range []string{string(out), err.Error()} {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "ssh command failed: exit status 1: ")
			line = strings.TrimPrefix(line, "ssh command failed: ")
			if line == "Error: unknown session command: prepare-open" || line == "unknown session command: prepare-open" {
				return true
			}
		}
	}
	return false
}
