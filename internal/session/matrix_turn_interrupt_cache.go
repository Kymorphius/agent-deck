package session

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

// matrix/hardened: Web snapshots reconstruct Instances, so an Instance-local
// timer cannot prevent repeated parses of unchanged transcript files. Share
// only file-derived results; keep status, refresh cadence and rebind guards on
// each Instance. The bound limits memory even across many one-off CLI reads.
const matrixTurnInterruptFileCacheLimit = 1024

type matrixTurnInterruptFileKey struct {
	tool string
	path string
}

type matrixTurnInterruptFileResult struct {
	meta      codexThreadMeta
	verdict   matrixTurnInterrupt
	found     bool
	cacheable bool
	// matrix/hardened: latest thread_settings_applied asked for the priority
	// ("fast") service tier.
	codexFast bool
	// matrix/hardened: exact native user-record evidence, never access/output.
	interactionAt time.Time
	interactionID string
}

type matrixTurnInterruptFileSignature struct {
	info          os.FileInfo
	changeSec     int64
	changeNsec    int64
	hasChangeTime bool
}

type matrixTurnInterruptFileEntry struct {
	mu        sync.Mutex
	lastUsed  uint64 // guarded by the cache's mu
	users     int    // guarded by the cache's mu; active entries cannot be evicted
	signature matrixTurnInterruptFileSignature
	result    matrixTurnInterruptFileResult
	valid     bool
}

type matrixTurnInterruptFileCache struct {
	mu      sync.Mutex
	entries map[matrixTurnInterruptFileKey]*matrixTurnInterruptFileEntry
	limit   int
	clock   uint64
	space   *sync.Cond
	// matrix/hardened: per-cache seam counts parses without replacing shared
	// process globals while concurrent refreshes are running.
	parse func(tool, path string) matrixTurnInterruptFileResult
}

var matrixInterruptFiles = newMatrixTurnInterruptFileCache(matrixTurnInterruptFileCacheLimit)

func newMatrixTurnInterruptFileCache(limit int) *matrixTurnInterruptFileCache {
	cache := &matrixTurnInterruptFileCache{
		entries: make(map[matrixTurnInterruptFileKey]*matrixTurnInterruptFileEntry),
		limit:   max(limit, 1),
		parse:   matrixParseTurnInterruptFile,
	}
	cache.space = sync.NewCond(&cache.mu)
	return cache
}

// matrix/hardened: a path, not a session ID, separates account homes. Resolve
// aliases before selecting the entry, and never remember a missing path.
func (c *matrixTurnInterruptFileCache) load(tool, path string) matrixTurnInterruptFileResult {
	path, err := filepath.Abs(path)
	if err != nil {
		return matrixTurnInterruptFileResult{}
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return matrixTurnInterruptFileResult{}
	}
	key := matrixTurnInterruptFileKey{tool: tool, path: path}
	entry := c.entry(key)
	defer c.release(entry)
	// Merge simultaneous reads of this file without serializing other files.
	entry.mu.Lock()
	defer entry.mu.Unlock()

	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		entry.valid = false
		return matrixTurnInterruptFileResult{}
	}
	signature := matrixTurnInterruptSignature(before)
	if entry.valid && entry.signature.equal(signature) {
		return entry.result
	}
	entry.valid = false
	result := c.parse(tool, path)
	// matrix/hardened: metadata and tail are separate reads. A replacement or
	// append during either read cannot prove a new interaction's exact owner.
	// Keep legacy interrupt display behavior, but never publish that clock.
	after, err := os.Stat(path)
	stable := err == nil && signature.equal(matrixTurnInterruptSignature(after))
	if !stable {
		result.interactionAt, result.interactionID = time.Time{}, ""
	} else if result.cacheable {
		entry.signature, entry.result, entry.valid = signature, result, true
	}
	return result
}

func (c *matrixTurnInterruptFileCache) entry(key matrixTurnInterruptFileKey) *matrixTurnInterruptFileEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		c.clock++
		if entry := c.entries[key]; entry != nil {
			entry.lastUsed = c.clock
			entry.users++
			return entry
		}
		if len(c.entries) < c.limit {
			break
		}
		var oldestKey matrixTurnInterruptFileKey
		var oldest *matrixTurnInterruptFileEntry
		for k, entry := range c.entries {
			if entry.users == 0 && (oldest == nil || entry.lastUsed < oldest.lastUsed) {
				oldestKey, oldest = k, entry
			}
		}
		if oldest != nil {
			delete(c.entries, oldestKey)
			break
		}
		// matrix/hardened: never evict an in-flight parse or its waiting
		// callers; otherwise a later caller could start a duplicate parse.
		c.space.Wait()
	}
	entry := &matrixTurnInterruptFileEntry{lastUsed: c.clock, users: 1}
	c.entries[key] = entry
	return entry
}

func (c *matrixTurnInterruptFileCache) release(entry *matrixTurnInterruptFileEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.users--
	if entry.users == 0 {
		c.space.Broadcast()
	}
}

func matrixTurnInterruptSignature(info os.FileInfo) matrixTurnInterruptFileSignature {
	sec, nsec, ok := matrixTurnInterruptChangeTime(info)
	return matrixTurnInterruptFileSignature{info: info, changeSec: sec, changeNsec: nsec, hasChangeTime: ok}
}

func (s matrixTurnInterruptFileSignature) equal(other matrixTurnInterruptFileSignature) bool {
	return os.SameFile(s.info, other.info) && s.info.Size() == other.info.Size() &&
		s.info.ModTime().Equal(other.info.ModTime()) && s.info.Mode() == other.info.Mode() &&
		s.hasChangeTime == other.hasChangeTime && s.changeSec == other.changeSec && s.changeNsec == other.changeNsec
}

// matrix/hardened: Darwin/BSD expose Ctimespec and Linux exposes Ctim. Read the
// optional platform fields without build-specific syscall types, so Windows
// still compiles and uses identity/size/mtime/mode when no change time exists.
// Change time catches same-inode, same-size rewrites that preserve mtime.
func matrixTurnInterruptChangeTime(info os.FileInfo) (int64, int64, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, 0, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		stamp := v.FieldByName(name)
		if stamp.Kind() != reflect.Struct {
			continue
		}
		sec, nsec := stamp.FieldByName("Sec"), stamp.FieldByName("Nsec")
		if sec.CanInt() && nsec.CanInt() {
			return sec.Int(), nsec.Int(), true
		}
	}
	sec, nsec := v.FieldByName("Ctime"), v.FieldByName("Ctimensec")
	if sec.CanInt() && nsec.CanInt() {
		return sec.Int(), nsec.Int(), true
	}
	return 0, 0, false
}

// matrix/hardened: cache the immutable metadata observation together with the
// latest-turn verdict. Invalid heads, partial JSON and I/O failures remain
// retryable; a successful "not interrupted" scan is worth sharing too.
func matrixParseTurnInterruptFile(tool, path string) matrixTurnInterruptFileResult {
	var result matrixTurnInterruptFileResult
	switch tool {
	case "claude":
		result.verdict, result.found, result.cacheable, result.interactionAt, result.interactionID = matrixScanLatestClaudeTurnFacts(path)
	case "codex":
		result.meta = readCodexRolloutThreadMeta(path)
		if result.meta.ThreadSource == "subagent" || result.meta.ParentThreadID != "" {
			result.cacheable = result.meta.valid
			return result
		}
		result.verdict, result.found, result.cacheable, result.interactionAt = matrixScanLatestCodexTurnFacts(path)
		if !result.meta.valid || result.meta.ID == "" || result.meta.ThreadSource != "user" {
			// matrix/hardened: legacy interrupt detection remains fail-open,
			// but a new interaction requires positive native/main-thread proof.
			result.interactionAt = time.Time{}
		}
		// matrix/hardened: incremental and unbounded; see matrix_codex_tier_tracker.go.
		fast, fastOK := matrixCodexTiers.fast(path)
		result.codexFast = fast
		result.cacheable = result.cacheable && fastOK && result.meta.valid
	}
	return result
}
