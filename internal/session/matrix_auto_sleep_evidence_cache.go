package session

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// matrix/hardened: unchanged idle rollouts are inspected once, not re-read in
// every monitor pass and every pre-stop recheck. Share file-derived success
// only; pane/process/delivery evidence is always fresh. Native ctime catches
// same-size rewrites that preserve mtime, and both sides of a hit are checked.
type matrixAutoSleepRolloutEntry struct {
	signature matrixTurnInterruptFileSignature
	evidence  AutoSleepEvidence
	proof     string
	used      uint64
}

type matrixAutoSleepRolloutCache struct {
	mu      sync.Mutex
	entries map[string]matrixAutoSleepRolloutEntry
	clock   uint64
	limit   int
	parse   func(string, string) (AutoSleepEvidence, string, error)
}

var matrixAutoSleepRollouts = matrixAutoSleepRolloutCache{
	entries: map[string]matrixAutoSleepRolloutEntry{}, limit: 64, parse: matrixParseAutoSleepRollout,
}

func matrixAutoSleepRollout(path, threadID string) (AutoSleepEvidence, string, error) {
	return matrixAutoSleepRollouts.load(path, threadID)
}

func (c *matrixAutoSleepRolloutCache) load(path, threadID string) (AutoSleepEvidence, string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return AutoSleepEvidence{}, "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return AutoSleepEvidence{}, "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := path + "\x00" + threadID
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		delete(c.entries, key)
		return AutoSleepEvidence{}, "", errors.New("native rollout cannot be verified")
	}
	signature := matrixTurnInterruptSignature(before)
	c.clock++
	if entry, ok := c.entries[key]; ok && signature.hasChangeTime && entry.signature.equal(signature) {
		if after, err := os.Stat(path); err == nil && signature.equal(matrixTurnInterruptSignature(after)) {
			entry.used = c.clock
			c.entries[key] = entry
			return entry.evidence, entry.proof, nil
		}
	}
	delete(c.entries, key)
	evidence, proof, err := c.parse(path, threadID)
	if err != nil {
		return evidence, proof, err
	}
	after, err := os.Stat(path)
	if err != nil || !signature.equal(matrixTurnInterruptSignature(after)) {
		return AutoSleepEvidence{}, "", errors.New("native rollout changed around parser observation")
	}
	if signature.hasChangeTime {
		if len(c.entries) >= max(c.limit, 1) {
			var oldest string
			var age uint64
			for name, entry := range c.entries {
				if oldest == "" || entry.used < age {
					oldest, age = name, entry.used
				}
			}
			delete(c.entries, oldest)
		}
		c.entries[key] = matrixAutoSleepRolloutEntry{signature: signature, evidence: evidence, proof: proof, used: c.clock}
	}
	return evidence, proof, nil
}
