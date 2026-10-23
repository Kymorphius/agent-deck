package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// matrix/hardened: all rollout fixtures live in synthetic Codex homes. These
// tests exercise lookup invalidation without scanning the operator's history.
func matrixSeedCachedCodexRollout(t testing.TB, home, day, sid string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", filepath.FromSlash(day))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-fixture-"+sid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMatrixCodexRolloutPathCacheSeparatesHomes(t *testing.T) {
	sid := "019f0000-0000-7000-8000-999999999901"
	firstHome, secondHome := t.TempDir(), t.TempDir()
	firstPath := matrixSeedCachedCodexRollout(t, firstHome, "2026/10/01", sid)
	secondPath := matrixSeedCachedCodexRollout(t, secondHome, "2026/10/02", sid)
	for n := 0; n < 3; n++ {
		if got := codexRolloutPathInHome(sid, firstHome); got != firstPath {
			t.Fatalf("first home path = %q, want %q", got, firstPath)
		}
		if got := codexRolloutPathInHome(sid, secondHome); got != secondPath {
			t.Fatalf("second home path = %q, want %q", got, secondPath)
		}
	}
}

// matrix/hardened: a polling miss may be repeated for up to the refresh
// interval; a fresh (resume/fork) lookup and the next interval both see it.
func TestMatrixCodexRolloutPathCacheRetriesAfterFirstFlush(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999902"
	now := time.Unix(1_800_000_000, 0)
	resolver := codexRolloutPathResolver{clock: func() time.Time { return now }}
	if got := resolver.find(sid, home, false); got != "" {
		t.Fatalf("unflushed path = %q, want empty", got)
	}
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/11", sid)
	if got := resolver.find(sid, home, false); got != "" {
		t.Fatalf("polling miss within the interval = %q, want repeated absence", got)
	}
	if got := resolver.find("  "+sid+"  ", home, true); got != path {
		t.Fatalf("fresh lookup after first flush = %q, want %q", got, path)
	}
	other := "019f0000-0000-7000-8000-999999999912"
	if got := resolver.find(other, home, false); got != "" {
		t.Fatalf("unflushed second path = %q, want empty", got)
	}
	otherPath := matrixSeedCachedCodexRollout(t, home, "2026/10/11", other)
	now = now.Add(codexRolloutIndexRefresh)
	if got := resolver.find(other, home, false); got != otherPath {
		t.Fatalf("polling lookup after the interval = %q, want %q", got, otherPath)
	}
}

func TestMatrixCodexRolloutPathCacheRediscoversRemovedOrMovedRollout(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999903"
	// matrix/hardened: each step is a later polling pass.
	now := time.Unix(1_800_000_000, 0)
	resolver := codexRolloutPathResolver{clock: func() time.Time { now = now.Add(codexRolloutIndexRefresh); return now }}
	codexRolloutPathInHome := func(sid, home string) string { return resolver.find(sid, home, false) }
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/01", sid)
	if got := codexRolloutPathInHome(sid, home); got != path {
		t.Fatalf("initial path = %q, want %q", got, path)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := codexRolloutPathInHome(sid, home); got != "" {
		t.Fatalf("removed path = %q, want empty", got)
	}
	restored := matrixSeedCachedCodexRollout(t, home, "2026/10/02", sid)
	if got := codexRolloutPathInHome(sid, home); got != restored {
		t.Fatalf("restored path = %q, want %q", got, restored)
	}
	movedDir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(movedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(movedDir, filepath.Base(restored))
	if err := os.Rename(restored, moved); err != nil {
		t.Fatal(err)
	}
	if got := codexRolloutPathInHome(sid, home); got != moved {
		t.Fatalf("moved path = %q, want %q", got, moved)
	}
}

func TestMatrixCodexRolloutPathCacheAvoidsStableTreeScans(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999904"
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/11", sid)
	matrixSeedCachedCodexRollout(t, home, "2026/10/12", "fixture-other")
	now := time.Unix(1_800_000_000, 0)
	resolver := codexRolloutPathResolver{clock: func() time.Time { return now }}
	reads := func() int {
		index, ok := resolver.homes.Load(home)
		if !ok {
			return 0
		}
		x := index.(*codexRolloutIndex)
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.reads
	}
	for n := 0; n < 100; n++ {
		if got := resolver.find(sid, home, false); got != path {
			t.Fatalf("lookup %d = %q, want %q", n, got, path)
		}
	}
	if got := reads(); got != 2 {
		t.Fatalf("stable rollout listed %d day directories, want 2", got)
	}
	if got := resolver.find("  ", home, false); got != "" || reads() != 2 {
		t.Fatalf("blank session scanned history: path=%q, reads=%d", got, reads())
	}
	// Repeated misses, across refresh intervals, never re-list unchanged days.
	missing := "019f0000-0000-7000-8000-999999999905"
	for n := 0; n < 50; n++ {
		now = now.Add(codexRolloutIndexRefresh)
		if got := resolver.find(missing, home, n%2 == 0); got != "" {
			t.Fatalf("missing rollout path = %q, want empty", got)
		}
	}
	if got := reads(); got != 2 {
		t.Fatalf("misses re-listed unchanged days: got %d listings, want 2", got)
	}
	// A new rollout changes only its own day directory.
	created := matrixSeedCachedCodexRollout(t, home, "2026/10/12", missing)
	if got := resolver.find(missing, home, true); got != created {
		t.Fatalf("created rollout = %q, want %q", got, created)
	}
	if got := reads(); got != 3 {
		t.Fatalf("new rollout listings = %d, want 3", got)
	}
}

// matrix/hardened: the index must agree with the original Glob, including
// first-match order, empty wildcard, symlinked days and non-directory entries.
func TestMatrixCodexRolloutIndexMatchesGlob(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999913"
	for _, day := range []string{"2026/10/02", "2026/09/30", "2025/12/31"} {
		matrixSeedCachedCodexRollout(t, home, day, sid)
	}
	empty := filepath.Join(home, "sessions", "2026", "10", "02", "rollout--"+sid+"x.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wild := "019f0000-0000-7000-8000-999999999914"
	if err := os.WriteFile(filepath.Join(home, "sessions", "2026", "10", "02", "rollout--"+wild+".jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "2026", "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	linked := t.TempDir()
	linkedSID := "019f0000-0000-7000-8000-999999999915"
	matrixSeedCachedCodexRollout(t, linked, "x/y/z", linkedSID)
	if err := os.Symlink(filepath.Join(linked, "sessions", "x", "y", "z"), filepath.Join(home, "sessions", "2026", "10", "03")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sid, wild, linkedSID, "8000-999999999913", "missing", sid + "x"} {
		var resolver codexRolloutPathResolver
		want := ""
		matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 {
			want = matches[0]
		}
		if got := resolver.find(id, home, false); got != want {
			t.Errorf("index(%q) = %q, Glob = %q", id, got, want)
		}
	}
}

func TestMatrixCodexRolloutPathCacheConcurrentLookups(t *testing.T) {
	home := t.TempDir()
	sid := "019f0000-0000-7000-8000-999999999906"
	path := matrixSeedCachedCodexRollout(t, home, "2026/10/11", sid)
	const workers = 32
	start := make(chan struct{})
	results := make(chan string, workers)
	var group sync.WaitGroup
	for n := 0; n < workers; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for lookup := 0; lookup < 50; lookup++ {
				if got := codexRolloutPathInHome(sid, home); got != path {
					results <- got
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for got := range results {
		t.Errorf("concurrent path = %q, want %q", got, path)
	}
}

// matrix/hardened: compare the old whole-tree Glob with a warm positive cache
// across 120 synthetic daily directories containing 12,001 rollout files.
func BenchmarkMatrixCodexRolloutPathLookup(b *testing.B) {
	home := b.TempDir()
	sid := "019f0000-0000-7000-8000-999999999907"
	for month := 1; month <= 12; month++ {
		for day := 1; day <= 10; day++ {
			date := fmt.Sprintf("2026/%02d/%02d", month, day)
			for file := 0; file < 100; file++ {
				matrixSeedCachedCodexRollout(b, home, date, fmt.Sprintf("fixture-%04d", file))
			}
		}
	}
	path := matrixSeedCachedCodexRollout(b, home, "2026/12/10", sid)
	pattern := filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+sid+".jsonl")
	b.Run("OriginalGlob", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			matches, err := filepath.Glob(pattern)
			if err != nil || len(matches) != 1 || matches[0] != path {
				b.Fatalf("glob matches=%v, err=%v, want %q", matches, err, path)
			}
		}
	})
	b.Run("CachedStat", func(b *testing.B) {
		var resolver codexRolloutPathResolver
		if got := resolver.find(sid, home, false); got != path {
			b.Fatalf("initial path = %q, want %q", got, path)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if got := resolver.find(sid, home, false); got != path {
				b.Fatalf("cached path = %q, want %q", got, path)
			}
		}
	})
	b.Run("IndexedMiss", func(b *testing.B) {
		var resolver codexRolloutPathResolver
		missing := "019f0000-0000-7000-8000-999999999908"
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if got := resolver.find(missing, home, false); got != "" {
				b.Fatalf("missing path = %q", got)
			}
		}
	})
}
