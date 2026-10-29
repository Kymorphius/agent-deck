package session

// matrix/hardened: Codex /fast state for the list badge. The latest
// thread_settings_applied event can sit far behind multi-megabyte tool output
// (a 7.9 MiB line, or 28 MiB of later records, were seen in real rollouts), so
// the bounded turn-interrupt walk cannot be reused. The first read of a file
// walks back to the latest settings event with no byte limit, skipping lines
// too long to be one without reading them; later reads only scan what Codex
// appended since. A replaced or truncated file is read again from the end.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// A settings event is a few KiB; anything longer is tool output.
	codexTierMaxLineBytes = 256 << 10
	codexTierChunkBytes   = 512 << 10
	// Appends larger than this are read by a fresh backward walk instead.
	codexTierMaxAppendBytes = 64 << 20
	codexTierCacheLimit     = 1024
)

var codexTierMarker = []byte(`"thread_settings_applied"`)

type codexTierEntry struct {
	mu        sync.Mutex
	info      os.FileInfo
	scannedTo int64 // offset just past the last complete line read
	tier      string
	valid     bool
}

type codexTierTracker struct {
	mu      sync.Mutex
	entries map[string]*codexTierEntry
}

var matrixCodexTiers = &codexTierTracker{entries: make(map[string]*codexTierEntry)}

// fast reports whether the rollout's latest settings event selects the
// priority tier; ok is false when the file could not be read.
func (c *codexTierTracker) fast(path string) (fast, ok bool) {
	tier, ok := c.tier(path)
	return strings.EqualFold(strings.TrimSpace(tier), "priority"), ok
}

func (c *codexTierTracker) tier(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return "", false
	}
	c.mu.Lock()
	entry := c.entries[abs]
	if entry == nil {
		if len(c.entries) >= codexTierCacheLimit {
			c.entries = make(map[string]*codexTierEntry) // rare: drop and rebuild lazily
		}
		entry = &codexTierEntry{}
		c.entries[abs] = entry
	}
	c.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	f, err := os.Open(abs)
	if err != nil {
		entry.valid = false
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		entry.valid = false
		return "", false
	}
	size := info.Size()
	if entry.valid && os.SameFile(entry.info, info) && size >= entry.scannedTo && size-entry.scannedTo <= codexTierMaxAppendBytes {
		tier, to, err := codexTierScanForward(f, entry.scannedTo, size)
		if err != nil {
			entry.valid = false
			return "", false
		}
		if tier != "" {
			entry.tier = tier
		}
		entry.info, entry.scannedTo = info, to
		return entry.tier, true
	}
	tier, to, err := codexTierScanBackward(f, size)
	if err != nil {
		entry.valid = false
		return "", false
	}
	entry.info, entry.scannedTo, entry.tier, entry.valid = info, to, tier, true
	return tier, true
}

// codexTierFromLine returns the service tier of a settings event, or "".
func codexTierFromLine(line []byte) (string, bool) {
	if !bytes.Contains(line, codexTierMarker) {
		return "", false
	}
	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			Type     string `json:"type"`
			Settings struct {
				ServiceTier string `json:"service_tier"`
			} `json:"thread_settings"`
		} `json:"payload"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil || ev.Type != "event_msg" || ev.Payload.Type != "thread_settings_applied" {
		return "", false
	}
	return ev.Payload.Settings.ServiceTier, true
}

// codexTierScanForward reads complete lines in [from, size) and returns the
// last settings tier among them plus the offset after the last complete line.
func codexTierScanForward(f *os.File, from, size int64) (string, int64, error) {
	if size <= from {
		return "", from, nil
	}
	buf := make([]byte, size-from)
	if n, err := f.ReadAt(buf, from); int64(n) != int64(len(buf)) && err != nil && err != io.EOF {
		return "", from, err
	}
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return "", from, nil // only a partial line so far
	}
	tier := ""
	for _, line := range bytes.Split(buf[:end], []byte{'\n'}) {
		if len(line) > codexTierMaxLineBytes {
			continue
		}
		if t, ok := codexTierFromLine(line); ok {
			tier = t
		}
	}
	return tier, from + int64(end) + 1, nil
}

// codexTierScanBackward walks complete lines from the end of the file to the
// latest settings event, skipping overlong lines unread. It returns the tier
// ("" when there is none) and the offset after the last complete line.
func codexTierScanBackward(f *os.File, size int64) (string, int64, error) {
	if size == 0 {
		return "", 0, nil
	}
	chunk := int64(codexTierChunkBytes)
	buf := make([]byte, min(chunk, size))
	// Find where complete lines end: a trailing partial line is not read yet.
	completeEnd := int64(-1)
	lineEnd := int64(-1)
	for end := size; end > 0; {
		start := max(end-int64(len(buf)), 0)
		n := end - start
		if read, err := f.ReadAt(buf[:n], start); int64(read) != n && err != nil && err != io.EOF {
			return "", 0, err
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			pos := start + i
			if completeEnd < 0 {
				completeEnd, lineEnd = pos+1, pos
				continue
			}
			if tier, found, err := codexTierConsider(f, buf[:n], start, pos+1, lineEnd); err != nil {
				return "", 0, err
			} else if found {
				return tier, completeEnd, nil
			}
			lineEnd = pos
		}
		end = start
	}
	if completeEnd < 0 {
		return "", 0, nil // no complete line yet
	}
	tier, _, err := codexTierConsider(f, nil, 0, 0, lineEnd)
	return tier, completeEnd, err
}

// codexTierConsider checks the line [ls, le). It is taken from the chunk in
// memory when it lies inside it, otherwise read on its own when short enough.
func codexTierConsider(f *os.File, chunk []byte, chunkStart, ls, le int64) (string, bool, error) {
	if le <= ls || le-ls > codexTierMaxLineBytes {
		return "", false, nil
	}
	var line []byte
	if chunk != nil && ls >= chunkStart && le <= chunkStart+int64(len(chunk)) {
		line = chunk[ls-chunkStart : le-chunkStart]
	} else {
		line = make([]byte, le-ls)
		if n, err := f.ReadAt(line, ls); int64(n) != le-ls && err != nil && err != io.EOF {
			return "", false, err
		}
	}
	tier, ok := codexTierFromLine(line)
	return tier, ok, nil
}
