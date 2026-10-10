package session

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// matrix/hardened: a Claude turn that ended on an API error (the "abnormal
// interruption" class).
//
// When the API fails mid-turn ("API Error: Connection lost mid-response",
// "Connection dropped (ECONNRESET)", "Can't reach the API server") Claude Code
// ends the turn without firing Stop and redraws an empty composer. Every signal
// agent-deck reads then says the turn finished normally: the light goes to
// waiting, the substate to idle-at-empty-prompt. Nothing tells the user the work
// stopped halfway and needs a "continue".
//
// The pane banner is no use here: it renders as an ordinary "⏺" reply line
// without the " · " separator the #1400 banner guard requires, and its wording
// is shared with prose. The transcript records the failure as structured data
// instead, the same source usage-limit (#1802) reads:
//
//	{"type":"assistant","isApiErrorMessage":true,"error":"server_error",
//	 "message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: ..."}]}}
//
// The verdict: the latest main-conversation user or assistant record is such an
// API-error reply. Any later user record (a typed prompt, "继续", the automatic
// "usage limit has reset" continuation) means the session moved past it. Checked
// against every transcript on the maintainer's machine (2026-10-09): 83 API-error
// records, each followed by a user record before any other assistant record.
//
// Known limitation: a local slash command typed after the failure (/cost, /model)
// also writes a user record, so it clears the verdict although the task itself
// was not resumed.
//
// The coarse status is never changed. The verdict is cached on the Instance by
// the status poll (updateStatus) so CachedSubstate, which the TUI calls on its
// render path, stays filesystem-free.

// SubstateAPIInterrupted marks a Claude session whose last turn ended on an API
// error rather than completing. Pairs with status "waiting"/"idle". The error
// kind and message are in SubstateDetail ("server_error: Connection lost
// mid-response").
const SubstateAPIInterrupted Substate = "api-interrupted"

const (
	// matrixTurnInterruptScanInterval throttles the transcript read per session,
	// like usageLimitScanInterval.
	matrixTurnInterruptScanInterval = 5 * time.Second
	// matrixTurnInterruptChunkBytes is the backward read window.
	matrixTurnInterruptChunkBytes = 256 << 10
	// matrixTurnInterruptMaxScanBytes bounds how far back the scan walks. The
	// decisive record is the last user/assistant one, normally within the last
	// few KiB; if none is found within this many bytes there is no verdict.
	matrixTurnInterruptMaxScanBytes = 8 << 20
	// matrixTurnInterruptMaxLineBytes: a longer line is not read. Its type is
	// then unknown, so the scan stops without a verdict rather than look past
	// what may be a newer user record (an API-error record is a few KiB).
	matrixTurnInterruptMaxLineBytes = 1 << 20
	// matrixTurnInterruptMessageMax caps the message kept for display.
	matrixTurnInterruptMessageMax = 160
)

// matrixTurnInterrupt is one API-error turn end.
type matrixTurnInterrupt struct {
	Kind    string // the record's "error": server_error, rate_limit, overloaded, ...
	Message string // the rendered text, "API Error: " prefix removed
}

// Detail is the SubstateDetail text.
func (t matrixTurnInterrupt) Detail() string {
	kind := t.Kind
	if kind == "" {
		kind = "unknown"
	}
	if t.Message == "" {
		return kind
	}
	return kind + ": " + t.Message
}

// matrixTurnInterruptMemo is the per-Instance cache, guarded by Instance.mu.
// sessionID is the Claude session the verdict was formed for; a rebind
// discards it. gen versions in-flight scans so an older one cannot publish
// over a newer result.
type matrixTurnInterruptMemo struct {
	sessionID string
	scannedAt time.Time
	gen       uint64
	found     bool
	verdict   matrixTurnInterrupt
}

// matrixInterruptRecord is the subset of a transcript line the scan reads.
type matrixInterruptRecord struct {
	Type              string `json:"type"`
	IsSidechain       bool   `json:"isSidechain"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Message           struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// matrixInterruptMessage extracts the first text block, trimmed for display.
func matrixInterruptMessage(content json.RawMessage) string {
	var text string
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = b.Text
				break
			}
		}
	} else {
		_ = json.Unmarshal(content, &text)
	}
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimPrefix(text, "API Error: ")
	// "Connection lost mid-response. The response above may be incomplete."
	// keeps only its first sentence; the rest is the same for every failure.
	text = strings.TrimSuffix(text, " The response above may be incomplete.")
	if r := []rune(text); len(r) > matrixTurnInterruptMessageMax {
		text = string(r[:matrixTurnInterruptMessageMax-1]) + "…"
	}
	return text
}

// matrixLatestTurnInterrupt reports whether the latest main-conversation
// user/assistant record in the transcript at path is an API-error reply.
// found is false for "no", and also for "cannot tell" (unreadable file, no
// such record within the scan bound, an oversized line in the way): the
// caller then shows the session as before, never as interrupted.
func matrixLatestTurnInterrupt(path string) (matrixTurnInterrupt, bool) {
	f, err := os.Open(path)
	if err != nil {
		return matrixTurnInterrupt{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return matrixTurnInterrupt{}, false
	}

	// decide reads the line [start,end) and reports (verdict, found, stop).
	decide := func(start, end int64) (matrixTurnInterrupt, bool, bool) {
		if end <= start {
			return matrixTurnInterrupt{}, false, false
		}
		if end-start > matrixTurnInterruptMaxLineBytes {
			return matrixTurnInterrupt{}, false, true
		}
		line := make([]byte, end-start)
		if _, err := f.ReadAt(line, start); err != nil && err != io.EOF {
			return matrixTurnInterrupt{}, false, true
		}
		var rec matrixInterruptRecord
		if json.Unmarshal(line, &rec) != nil {
			return matrixTurnInterrupt{}, false, false // blank or partial line
		}
		if rec.IsSidechain {
			return matrixTurnInterrupt{}, false, false
		}
		switch rec.Type {
		case "user":
			return matrixTurnInterrupt{}, false, true
		case "assistant":
			if !rec.IsAPIErrorMessage {
				return matrixTurnInterrupt{}, false, true
			}
			return matrixTurnInterrupt{
				Kind:    strings.TrimSpace(rec.Error),
				Message: matrixInterruptMessage(rec.Message.Content),
			}, true, true
		}
		return matrixTurnInterrupt{}, false, false // system, attachment, snapshot, ...
	}

	buf := make([]byte, min(int64(matrixTurnInterruptChunkBytes), info.Size()))
	lineEnd := info.Size()
	floor := max(info.Size()-matrixTurnInterruptMaxScanBytes, 0)
	for end := info.Size(); end > floor; {
		start := max(end-int64(len(buf)), floor)
		n := end - start
		if _, err := f.ReadAt(buf[:n], start); err != nil && err != io.EOF {
			return matrixTurnInterrupt{}, false
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			if v, found, stop := decide(start+i+1, lineEnd); stop {
				return v, found
			}
			lineEnd = start + i
		}
		end = start
	}
	if floor == 0 {
		// The first line has no newline before it.
		if v, found, stop := decide(0, lineEnd); stop {
			return v, found
		}
	}
	return matrixTurnInterrupt{}, false
}

// matrixRefreshTurnInterrupt rescans the transcript, throttled, and caches the
// verdict. Called by updateStatus after it releases i.mu; takes i.mu itself.
func (i *Instance) matrixRefreshTurnInterrupt() {
	i.mu.Lock()
	sessionID := strings.TrimSpace(i.ClaudeSessionID)
	m := &i.matrixInterrupt
	// The transcript is a local Claude file: other tools and SSH-backed
	// sessions (conversation on the remote host) have none to read.
	if sessionID == "" || !IsClaudeCompatible(i.Tool) || i.IsSSH() {
		*m = matrixTurnInterruptMemo{}
		i.mu.Unlock()
		return
	}
	if m.sessionID != sessionID {
		*m = matrixTurnInterruptMemo{sessionID: sessionID}
	}
	if !m.scannedAt.IsZero() && time.Since(m.scannedAt) < matrixTurnInterruptScanInterval {
		i.mu.Unlock()
		return
	}
	m.scannedAt = time.Now()
	m.gen++
	gen := m.gen
	i.mu.Unlock()

	path := locateHandoffTranscript(i)
	if !transcriptBelongsToSession(path, sessionID) {
		return
	}
	verdict, found := matrixLatestTurnInterrupt(path)

	i.mu.Lock()
	defer i.mu.Unlock()
	if strings.TrimSpace(i.ClaudeSessionID) == sessionID && m.sessionID == sessionID && m.gen == gen {
		m.found, m.verdict = found, verdict
	}
}

// matrixTurnInterruptCached returns the cached verdict for the session the
// instance is bound to now. Never touches the filesystem.
func (i *Instance) matrixTurnInterruptCached() (matrixTurnInterrupt, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	m := i.matrixInterrupt
	if !m.found || m.sessionID == "" || m.sessionID != strings.TrimSpace(i.ClaudeSessionID) {
		return matrixTurnInterrupt{}, false
	}
	return m.verdict, true
}

// matrixOverlayTurnInterrupt replaces a pane verdict that says nothing more
// specific (none, idle at prompt, hook lag) with api-interrupted when the
// cached transcript verdict says the last turn ended on an API error. A pane
// that shows live work, an open menu, background work or its own error banner
// keeps its verdict.
func (i *Instance) matrixOverlayTurnInterrupt(sub Substate) Substate {
	switch sub {
	case SubstateNone, SubstateIdleAtEmptyPrompt, SubstateHookLag:
	default:
		return sub
	}
	if _, ok := i.matrixTurnInterruptCached(); ok {
		return SubstateAPIInterrupted
	}
	return sub
}
