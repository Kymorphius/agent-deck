package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// matrix/hardened: use real conductor files and count the full base builder,
// proving both disk-artifact reads and synthesis are shared per publication.
func matrixWriteCCArtifacts(t *testing.T, dir, headline string) {
	t.Helper()
	dir = filepath.Join(dir, "agent-deck")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{"headline": headline})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "OPEN-ITEMS.md"), []byte("## D. DECISIONS\n- #120 "+headline+"\n## E. DONE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// matrix/hardened: sixteen live views and REST reuse one artifact build; an
// unchanged menu in the next generation must still pick up artifact changes.
func TestMatrixCommandCenterPublicationFanoutAndArtifactRefresh(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
	srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
	artifactDir := t.TempDir()
	matrixWriteCCArtifacts(t, artifactDir, "before")
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		return buildCommandCenterSnapshot(menu, srv.cfg.Profile, artifactDir, nil)
	}
	trackers := make([]*ccPublicationTracker, 16)
	streams := make([]<-chan menuPublication, len(trackers))
	for n := range trackers {
		trackers[n] = &ccPublicationTracker{}
		var release func()
		streams[n], release = srv.menuPublisher.Subscribe(time.Hour, trackers[n].observe)
		t.Cleanup(release)
	}
	publications := make([]menuPublication, len(streams))
	for n := range streams {
		publications[n] = matrixPublisherPublication(t, streams[n])
	}
	type result struct {
		cc  commandCenterPublication
		err error
	}
	results := make(chan result, len(streams))
	var workers sync.WaitGroup
	for n := range streams {
		workers.Add(1)
		go func(n int) {
			defer workers.Done()
			_, cc, err := srv.commandCenterForPublication(publications[n], trackers[n])
			results <- result{cc, err}
		}(n)
	}
	workers.Wait()
	close(results)
	var first commandCenterPublication
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.snapshot == nil {
			first = got.cc
		}
		if got.cc.snapshot != first.snapshot || got.cc.fingerprint != first.fingerprint {
			t.Fatal("a connection independently synthesized an equivalent base")
		}
	}
	if _, err := srv.loadCommandCenterSnapshot(nil); err != nil {
		t.Fatal(err)
	}
	if srv.commandCenterBuilds.Load() != 1 || source.calls.Load() != 1 || first.snapshot.Conductors[0].CurrentlyWorkingOn != "before" {
		t.Fatalf("first fanout builds=%d source=%d snapshot=%+v", srv.commandCenterBuilds.Load(), source.calls.Load(), first.snapshot)
	}
	matrixWriteCCArtifacts(t, artifactDir, "after")
	srv.notifyMenuChanged()
	publication := matrixPublisherPublication(t, streams[0])
	if publication.Fingerprint != publications[0].Fingerprint {
		t.Fatal("fixture changed menu contents instead of artifact-only state")
	}
	_, next, err := srv.commandCenterForPublication(publication, trackers[0])
	if err != nil {
		t.Fatal(err)
	}
	if next.snapshot.Conductors[0].CurrentlyWorkingOn != "after" || len(next.snapshot.DecisionsWaiting) != 1 || next.snapshot.DecisionsWaiting[0].Question != "#120 after" {
		t.Fatalf("unchanged menu hid new artifact state: %+v", next.snapshot)
	}
	if next.fingerprint == first.fingerprint || srv.commandCenterBuilds.Load() != 2 || source.calls.Load() != 2 || first.snapshot.Conductors[0].CurrentlyWorkingOn != "before" {
		t.Fatalf("derived generation was reused or mutated: builds=%d source=%d", srv.commandCenterBuilds.Load(), source.calls.Load())
	}
	if _, err := srv.loadCommandCenterSnapshot(nil); err != nil {
		t.Fatal(err)
	}
	if srv.commandCenterBuilds.Load() != 2 {
		t.Fatal("REST repeated a live view's artifact build")
	}
}

// matrix/hardened: an old generation's slow artifact read cannot hold the
// publisher mutex or delay a new authoritative menu collection.
func TestMatrixCommandCenterPublicationSlowBuildDoesNotBlockCollector(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
	srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
	entered, gate := make(chan struct{}), make(chan struct{})
	var first sync.Once
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		first.Do(func() { close(entered); <-gate })
		return buildCommandCenterSnapshot(menu, srv.cfg.Profile, "", nil)
	}
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	oldDone := make(chan error, 1)
	go func() { _, err := srv.loadCommandCenterSnapshot(nil); oldDone <- err }()
	<-entered
	srv.notifyMenuChanged()
	newDone := make(chan error, 1)
	go func() { _, err := srv.menuPublisher.LoadMenuSnapshot(); newDone <- err }()
	select {
	case err := <-newDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("artifact build held up the menu collector")
	}
	close(gate)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if source.calls.Load() != 2 {
		t.Fatalf("source generations=%d, want 2", source.calls.Load())
	}
}

// matrix/hardened: identical menu IDs/content on separate server instances do
// not share profile/artifact-derived data or counters across publisher owners.
func TestMatrixCommandCenterPublicationServerIsolation(t *testing.T) {
	for n := 0; n < 2; n++ {
		source := &matrixPublisherIntegrationSource{snapshot: ccTestMenu()}
		srv, _, _ := matrixPublisherIntegrationServer(t, source, "")
		artifactDir := t.TempDir()
		headline := fmt.Sprintf("server-%d", n)
		matrixWriteCCArtifacts(t, artifactDir, headline)
		srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
			return buildCommandCenterSnapshot(menu, headline, artifactDir, nil)
		}
		for read := 0; read < 2; read++ {
			cc, err := srv.loadCommandCenterSnapshot(nil)
			if err != nil || cc.Profile != headline || cc.Conductors[0].CurrentlyWorkingOn != headline {
				t.Fatalf("server %d inherited another owner: %+v, %v", n, cc, err)
			}
		}
		if srv.commandCenterBuilds.Load() != 1 {
			t.Fatalf("server %d builds=%d, want 1", n, srv.commandCenterBuilds.Load())
		}
	}
}
