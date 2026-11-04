package main

import (
	"bytes"
	"encoding/json"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the remote-agent probe runs whenever the state DB moves,
// which a TUI heartbeat alone makes every two seconds. Hydrating every row
// again each time (tool data, per-row config lookups, pattern compilation)
// was most of an idle agent's remaining CPU. Like the headless Web catalogue
// (MATRIX-HARDENED §43), the lister keeps one hydration per registry
// generation: it is reused while SQLite's data_version, connection epoch,
// edit watermark and user config are unchanged, and also when only other
// tables moved and the raw instances/groups rows read back identical. Any
// difference, or any failure to tell, hydrates afresh.

// remoteAgentRegistryKey identifies what one hydration was taken from.
type remoteAgentRegistryKey struct {
	version int64
	epoch   uint64
	touch   time.Time
	config  os.FileInfo
}

// sameGeneration ignores data_version: commits to unrelated tables move it.
func (k remoteAgentRegistryKey) sameGeneration(o remoteAgentRegistryKey) bool {
	return k.epoch == o.epoch && k.touch.Equal(o.touch) && remoteAgentSameConfig(k.config, o.config)
}

// remoteAgentRegistry is one hydration and the per-row state the lister keeps
// for it. A new generation starts with fresh instances and empty maps.
type remoteAgentRegistry struct {
	key       remoteAgentRegistryKey
	raw       []byte
	instances []*session.Instance
	groups    []*session.GroupData
	// observed holds rows carrying an applied monitor verdict.
	observed map[*session.Instance]bool
	// hooksLoaded holds absent rows whose hook evidence was read; a gone pane
	// writes no new hook events, so it is read once per generation.
	hooksLoaded map[*session.Instance]bool
}

func newRemoteAgentRegistry(instances []*session.Instance, groups []*session.GroupData) *remoteAgentRegistry {
	return &remoteAgentRegistry{instances: instances, groups: groups, observed: map[*session.Instance]bool{}, hooksLoaded: map[*session.Instance]bool{}}
}

// registry returns the current hydration. Caller holds l.mu.
func (l *remoteAgentLister) registry() (*remoteAgentRegistry, error) {
	key, keyed := l.registryKey()
	if keyed && l.cache != nil && l.cache.key.version == key.version && l.cache.key.sameGeneration(key) {
		l.registryHits++
		return l.cache, nil
	}
	var raw []byte
	if keyed {
		if snapshot, before, after, err := l.observer.Snapshot(); err == nil && before == after && before == key.version {
			if encoded, err := json.Marshal(snapshot); err == nil {
				raw = encoded
			}
		}
	}
	if raw != nil && l.cache != nil && l.cache.key.sameGeneration(key) && bytes.Equal(raw, l.cache.raw) {
		l.cache.key.version = key.version
		l.registryHits++
		return l.cache, nil
	}
	instances, groups, err := l.loadRegistry()
	if err != nil {
		l.cache = nil
		return nil, err
	}
	l.registryLoads++
	reg := newRemoteAgentRegistry(instances, groups)
	// The raw rows were read before this hydration, so a write in between
	// only makes the next comparison miss; it can never pin stale rows.
	l.cache = nil
	if raw != nil {
		reg.key, reg.raw = key, raw
		l.cache = reg
	}
	return reg, nil
}

// registryKey reads the cheap identity of the registry; ok is false when any
// part cannot be read, and the caller then hydrates without caching.
func (l *remoteAgentLister) registryKey() (remoteAgentRegistryKey, bool) {
	if l.observer == nil {
		return remoteAgentRegistryKey{}, false
	}
	version, epoch, err := l.observer.Probe()
	if err != nil {
		return remoteAgentRegistryKey{}, false
	}
	touch, err := l.storage.GetUpdatedAt()
	if err != nil {
		return remoteAgentRegistryKey{}, false
	}
	config, err := remoteAgentConfigInfo()
	if err != nil {
		return remoteAgentRegistryKey{}, false
	}
	return remoteAgentRegistryKey{version: version, epoch: epoch, touch: touch, config: config}, true
}

// remoteAgentConfigInfo stats the user config hydration reads (tool patterns,
// tool definitions); nil when there is none. Mirrors the Web catalogue.
func remoteAgentConfigInfo() (os.FileInfo, error) {
	path, err := session.GetUserConfigPath()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return info, err
}

func remoteAgentSameConfig(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}
