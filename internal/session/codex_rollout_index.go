package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// matrix/hardened: status polling asked for the rollouts of every Codex row on
// every pass. Missing rollouts (threads whose JSONL never flushed or was
// removed) were never cached, so each miss globbed the whole dated tree: with
// ~4500 rollouts in ~120 day directories this was ~90% of the monitor's CPU.
//
// The index lists codexHome/sessions/*/*/* in filepath.Glob order and keeps
// each day directory's names, keyed by that directory's mtime and size.
// Creating, renaming or removing a rollout changes its directory, so a refresh
// re-reads only changed days. Appending to a rollout changes nothing here.
// Lookups are answered from a per-generation memo (including absence).
const codexRolloutIndexRefresh = time.Second

type codexRolloutDay struct {
	mod   time.Time
	size  int64
	names []string // sorted, as Glob matches them
}

type codexRolloutIndex struct {
	home string

	mu      sync.Mutex
	scanned time.Time
	order   []string                   // day directories in Glob order
	days    map[string]codexRolloutDay // day directory -> listing
	results map[string]string          // session id -> path ("" = absent)
	reads   int                        // day directory listings, for tests
}

func (x *codexRolloutIndex) lookup(sessionID string, now time.Time, fresh bool) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if fresh || x.days == nil || now.Sub(x.scanned) >= codexRolloutIndexRefresh || now.Before(x.scanned) {
		x.refresh()
		x.scanned = now
	}
	if path, ok := x.results[sessionID]; ok {
		return path
	}
	// Same match as Glob "rollout-*-<id>.jsonl"; first match in Glob order.
	path := ""
	suffix := "-" + sessionID + ".jsonl"
search:
	for _, day := range x.order {
		for _, name := range x.days[day].names {
			if len(name) >= len("rollout-")+len(suffix) && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, suffix) {
				path = filepath.Join(day, name)
				break search
			}
		}
	}
	x.results[sessionID] = path
	return path
}

// refresh mirrors filepath.Glob's walk: every entry at each level, directories
// (symlinks followed) descended, names sorted. Listing errors read as empty.
func (x *codexRolloutIndex) refresh() {
	var order []string
	for _, year := range codexRolloutIndexChildren(filepath.Join(x.home, "sessions")) {
		for _, month := range codexRolloutIndexChildren(year) {
			order = append(order, codexRolloutIndexChildren(month)...)
		}
	}
	days := make(map[string]codexRolloutDay, len(order))
	changed := x.days == nil || len(order) != len(x.order)
	kept := order[:0]
	for i, day := range order {
		info, err := os.Stat(day)
		if err != nil || !info.IsDir() {
			changed = true
			continue
		}
		if !changed && x.order[i] != day {
			changed = true
		}
		if old, ok := x.days[day]; ok && old.mod.Equal(info.ModTime()) && old.size == info.Size() {
			days[day] = old
		} else {
			changed = true
			x.reads++
			days[day] = codexRolloutDay{mod: info.ModTime(), size: info.Size(), names: codexRolloutIndexNames(day)}
		}
		kept = append(kept, day)
	}
	x.order, x.days = kept, days
	if changed || x.results == nil {
		x.results = make(map[string]string)
	}
}

func codexRolloutIndexChildren(dir string) []string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	names := codexRolloutIndexNames(dir)
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(dir, name)
	}
	return paths
}

func codexRolloutIndexNames(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := f.Readdirnames(-1)
	_ = f.Close()
	sort.Strings(names)
	return names
}
