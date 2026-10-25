package session

// matrix/hardened: prove shared runtimes are protected without weakening the
// receipt contract for genuinely escaped processes. All processes are fakes.
import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexSharedTestProber struct {
	*sessionFakeProber
	children map[int][]procowner.ProcInfo
	treeErr  error
}

func (p *codexSharedTestProber) Name() string { return procowner.ProviderDarwinPS }
func (p *codexSharedTestProber) Descendants(root procowner.ProcInfo) ([]procowner.ProcInfo, error) {
	return p.children[root.PID], p.treeErr
}
func (p *codexSharedTestProber) CompareStart(a, b string) (int, error) {
	return strings.Compare(a, b), nil
}

type codexSharedFixture struct {
	home    string
	filter  codexSharedOwnershipFilter
	prober  *codexSharedTestProber
	receipt *procowner.Receipt
	command map[int]codexSharedOwnershipCommand
}

func newCodexSharedFixture(t *testing.T) *codexSharedFixture {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("managed-daemon receipt exception currently audits Darwin only")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	f := &codexSharedFixture{home: home, command: map[int]codexSharedOwnershipCommand{}}
	f.prober = &codexSharedTestProber{sessionFakeProber: newSessionFakeProber(), children: map[int][]procowner.ProcInfo{}}
	f.receipt = &procowner.Receipt{
		Version: procowner.ReceiptVersion, InstanceID: "shared-runtime-test", Generation: 1,
		State: procowner.StateLive, Provider: procowner.ProviderDarwinPS, BootID: "boot-1",
		Leader: procowner.Member{PID: 4100, StartID: "1000", UID: os.Getuid(), PGID: 4100, Role: procowner.RoleLeader},
	}
	executable := filepath.Join(home, "packages", "app-server-daemon", "releases", "0.164.0-test-target", "bin", "codex")
	require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0700))
	require.NoError(t, os.WriteFile(executable, []byte("test executable identity"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "app-server-daemon"), 0700))
	for idx, name := range []string{"daemon.pid", "daemon-updater.pid"} {
		pid := 4200 + idx
		start := "2000"
		f.prober.alive(pid, start, os.Getuid())
		f.receipt.Members = append(f.receipt.Members, procowner.Member{PID: pid, StartID: start, UID: os.Getuid(), PGID: pid, Role: procowner.RoleDescendant})
		data, marshalErr := json.Marshal(codexSharedOwnershipRecord{PID: pid, Start: start})
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(home, "app-server-daemon", name), data, 0600))
		args := executable + " app-server --listen unix:// --analytics-default-enabled --managed-daemon"
		if idx == 1 {
			args = executable + " app-server daemon pid-update-loop"
		}
		f.command[pid] = codexSharedOwnershipCommand{Executable: executable, Arguments: args}
	}
	f.prober.alive(4300, "3000", os.Getuid())
	child := f.prober.procs[4300]
	child.PPID, child.PGID = 4200, 4200
	f.prober.procs[4300] = child
	f.prober.children[4200] = []procowner.ProcInfo{child}
	f.receipt.Members = append(f.receipt.Members, procowner.Member{PID: child.PID, StartID: child.StartID, UID: child.UID, PGID: child.PGID, Role: procowner.RoleDescendant})
	f.filter = codexSharedOwnershipFilter{home: home, read: readCodexSharedOwnershipRecord, inspect: func(pid int) (codexSharedOwnershipCommand, error) {
		return f.command[pid], nil
	}}
	return f
}

func TestMatrixCodexSharedOwnershipOldReceiptAndReap(t *testing.T) {
	for _, teardown := range []bool{false, true} {
		t.Run(map[bool]string{false: "reconcile", true: "teardown"}[teardown], func(t *testing.T) {
			f := newCodexSharedFixture(t)
			t.Setenv("CODEX_HOME", f.home)
			previous := codexSharedOwnershipInspectCommand
			codexSharedOwnershipInspectCommand = f.filter.inspect
			t.Cleanup(func() { codexSharedOwnershipInspectCommand = previous })
			signaler := &countingSignaler{}
			restore := swapOwnershipProbe(t, f.prober, signaler)
			defer restore()
			inst := NewInstance("shared-runtime-test", t.TempDir())
			inst.Tool, inst.ID = "codex", f.receipt.InstanceID
			store := ownershipStore()
			_, err := store.Commit(inst.ID, func(*procowner.Receipt) (*procowner.Receipt, error) { return f.receipt, nil })
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.ForceClear(inst.ID) })

			status := inst.OwnershipStatus()
			require.NoError(t, status.LoadErr)
			assert.True(t, status.Admissible(), "shared daemon is not an escaped session tree")
			assert.Equal(t, procowner.VerdictClear, status.Report.Verdict)
			assert.Len(t, status.Receipt.Members, 3, "status must preserve the original CAS receipt")
			if teardown {
				inst.clearOwnershipAfterTeardown(true)
			} else {
				report, reconcileErr := inst.ReconcileOwnership()
				require.NoError(t, reconcileErr)
				assert.Equal(t, procowner.VerdictClear, report.Verdict)
			}
			assert.Empty(t, signaler.calls, "neither shared root nor its children may receive signals")
			after, err := store.Load(inst.ID)
			require.NoError(t, err)
			assert.Nil(t, after)
		})
	}
}

type codexSharedRemovingSignaler struct {
	prober *codexSharedTestProber
	pids   []int
}

func (s *codexSharedRemovingSignaler) Signal(pid int, _ syscall.Signal) error {
	s.pids = append(s.pids, pid)
	delete(s.prober.procs, pid)
	return nil
}

func TestMatrixCodexSharedOwnershipDoesNotExemptEscapedProcess(t *testing.T) {
	f := newCodexSharedFixture(t)
	f.prober.alive(4400, "4000", os.Getuid())
	f.receipt.Members = append(f.receipt.Members, procowner.Member{PID: 4400, StartID: "4000", UID: os.Getuid(), PGID: 4400})
	view, err := f.filter.receipt(f.receipt, f.prober)
	require.NoError(t, err)
	report := procowner.Verify(f.prober, view)
	require.Equal(t, procowner.VerdictOwned, report.Verdict)
	require.Len(t, report.Owned(), 1)
	assert.Equal(t, 4400, report.Owned()[0].PID)
	signaler := &codexSharedRemovingSignaler{prober: f.prober}
	reaped := reapCodexSessionOwnership(f.filter, f.prober, signaler, f.receipt, procowner.ReapOptions{})
	assert.Equal(t, procowner.VerdictClear, reaped.Verdict)
	assert.Equal(t, []int{4400}, signaler.pids)
	assert.Contains(t, f.prober.procs, 4200)
	assert.Contains(t, f.prober.procs, 4201)
	assert.Contains(t, f.prober.procs, 4300)
}

func TestMatrixCodexSharedOwnershipAttributionNarrowing(t *testing.T) {
	f := newCodexSharedFixture(t)
	f.prober.alive(f.receipt.Leader.PID, f.receipt.Leader.StartID, os.Getuid())
	f.prober.children[f.receipt.Leader.PID] = []procowner.ProcInfo{f.prober.procs[4200], f.prober.procs[4201], f.prober.procs[4300]}
	r := *f.receipt
	r.Members = nil
	added, err := procowner.Attribute(f.prober, &r, nil)
	require.NoError(t, err)
	require.Len(t, added, 3)
	view, err := f.filter.receipt(&r, f.prober)
	require.NoError(t, err)
	assert.Empty(t, view.Members)
	assert.Equal(t, r.Leader, view.Leader)
	assert.Len(t, r.Members, 3, "narrowing is a view rather than an in-place mutation")
}

func TestMatrixCodexSharedOwnershipIdentityFailuresNeverSignal(t *testing.T) {
	cases := map[string]func(*testing.T, *codexSharedFixture){
		"pgid differs": func(_ *testing.T, f *codexSharedFixture) {
			p := f.prober.procs[4200]
			p.PGID++
			f.prober.procs[4200] = p
		},
		"argv role differs": func(_ *testing.T, f *codexSharedFixture) {
			c := f.command[4200]
			c.Arguments += " --extra"
			f.command[4200] = c
		},
		"other home executable": func(_ *testing.T, f *codexSharedFixture) {
			c := f.command[4200]
			c.Executable = "/other/codex"
			f.command[4200] = c
		},
		"descendant scan failed": func(_ *testing.T, f *codexSharedFixture) { f.prober.treeErr = errors.New("process table unavailable") },
		"process command unreadable": func(_ *testing.T, f *codexSharedFixture) {
			f.filter.inspect = func(int) (codexSharedOwnershipCommand, error) {
				return codexSharedOwnershipCommand{}, errors.New("unreadable")
			}
		},
		"corrupt metadata": func(t *testing.T, f *codexSharedFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.home, "app-server-daemon", "daemon.pid"), []byte("{"), 0600))
		},
		"oversize metadata": func(t *testing.T, f *codexSharedFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.home, "app-server-daemon", "daemon.pid"), []byte(strings.Repeat("x", codexSharedOwnershipRecordLimit+1)), 0600))
		},
		"symlink metadata": func(t *testing.T, f *codexSharedFixture) {
			path := filepath.Join(f.home, "app-server-daemon", "daemon.pid")
			require.NoError(t, os.Rename(path, path+".saved"))
			require.NoError(t, os.Symlink(path+".saved", path))
		},
		"missing metadata": func(t *testing.T, f *codexSharedFixture) {
			require.NoError(t, os.Remove(filepath.Join(f.home, "app-server-daemon", "daemon.pid")))
		},
		"stale metadata": func(t *testing.T, f *codexSharedFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.home, "app-server-daemon", "daemon.pid"), []byte(`{"pid":4200,"processStartTime":"older"}`), 0600))
		},
		"symlink executable": func(t *testing.T, f *codexSharedFixture) {
			path := f.command[4200].Executable
			require.NoError(t, os.Rename(path, path+".saved"))
			require.NoError(t, os.Symlink(path+".saved", path))
		},
		"metadata changes": func(_ *testing.T, f *codexSharedFixture) {
			read := f.filter.read
			calls := map[string]int{}
			f.filter.read = func(path string) ([]byte, error) {
				data, err := read(path)
				calls[path]++
				if calls[path] == 3 {
					data = append(data, ' ')
				}
				return data, err
			}
		},
		"identity changes": func(_ *testing.T, f *codexSharedFixture) {
			inspect := f.filter.inspect
			calls := 0
			f.filter.inspect = func(pid int) (codexSharedOwnershipCommand, error) {
				calls++
				if calls == 2 {
					p := f.prober.procs[pid]
					p.StartID = "9999"
					f.prober.procs[pid] = p
				}
				return inspect(pid)
			}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCodexSharedFixture(t)
			arrange(t, f)
			view, err := f.filter.receipt(f.receipt, f.prober)
			require.Error(t, err)
			assert.Nil(t, view)
			f = newCodexSharedFixture(t)
			arrange(t, f)
			signaler := &countingSignaler{}
			report := reapCodexSessionOwnership(f.filter, f.prober, signaler, f.receipt, procowner.ReapOptions{})
			assert.Equal(t, procowner.VerdictUnknown, report.Verdict)
			assert.Empty(t, signaler.calls)
		})
	}
}

func TestMatrixCodexSharedOwnershipNeverExemptsLeaderOrIdentityMismatch(t *testing.T) {
	for _, kind := range []string{"leader", "start", "uid", "other home"} {
		t.Run(kind, func(t *testing.T) {
			f := newCodexSharedFixture(t)
			switch kind {
			case "leader":
				f.receipt.Leader = f.receipt.Members[0]
				f.receipt.Leader.Role = procowner.RoleLeader
			case "start":
				p := f.prober.procs[4200]
				p.StartID = "9999"
				f.prober.procs[4200] = p
			case "uid":
				p := f.prober.procs[4200]
				p.UID++
				f.prober.procs[4200] = p
			case "other home":
				f.filter.home = t.TempDir()
			}
			view, err := f.filter.receipt(f.receipt, f.prober)
			if kind == "start" || kind == "uid" {
				require.Error(t, err)
				assert.Nil(t, view)
				return
			}
			require.NoError(t, err)
			assert.True(t, view.HasMember(f.receipt.Members[0]), "the candidate must remain subject to ordinary verification")
			assert.Equal(t, f.receipt.Leader, view.Leader)
		})
	}
}

func TestMatrixCodexSharedOwnershipProtectsChildrenWithoutRecordedRoot(t *testing.T) {
	f := newCodexSharedFixture(t)
	f.receipt.Members = f.receipt.Members[1:]
	view, err := f.filter.receipt(f.receipt, f.prober)
	require.NoError(t, err)
	assert.Empty(t, view.Members)
	signaler := &countingSignaler{}
	report := reapCodexSessionOwnership(f.filter, f.prober, signaler, f.receipt, procowner.ReapOptions{})
	assert.Equal(t, procowner.VerdictClear, report.Verdict)
	assert.Empty(t, signaler.calls)
}

func TestMatrixCodexSharedOwnershipUnrelatedBrokenMetadataDoesNotBlock(t *testing.T) {
	f := newCodexSharedFixture(t)
	f.receipt.Members = nil
	f.prober.alive(4400, "4000", os.Getuid())
	f.receipt.Members = []procowner.Member{{PID: 4400, StartID: "4000", UID: os.Getuid(), PGID: 4400}}
	require.NoError(t, os.WriteFile(filepath.Join(f.home, "app-server-daemon", "daemon.pid"), []byte("{"), 0600))
	view, err := f.filter.receipt(f.receipt, f.prober)
	require.NoError(t, err)
	assert.Equal(t, f.receipt, view)
	signaler := &codexSharedRemovingSignaler{prober: f.prober}
	report := reapCodexSessionOwnership(f.filter, f.prober, signaler, f.receipt, procowner.ReapOptions{})
	assert.Equal(t, procowner.VerdictClear, report.Verdict)
	assert.Equal(t, []int{4400}, signaler.pids)
}

func TestMatrixCodexSharedOwnershipAttributionRetriesAfterTemporaryFailure(t *testing.T) {
	f := newCodexSharedFixture(t)
	t.Setenv("CODEX_HOME", f.home)
	f.prober.alive(f.receipt.Leader.PID, f.receipt.Leader.StartID, os.Getuid())
	f.prober.children[f.receipt.Leader.PID] = []procowner.ProcInfo{f.prober.procs[4200], f.prober.procs[4201], f.prober.procs[4300]}
	inst := NewInstance("shared-runtime-retry-test", t.TempDir())
	inst.ID, inst.Tool = f.receipt.InstanceID, "codex"
	inst.spawnGen.Store(1)
	store := ownershipStoreAt(t.TempDir())
	_, err := store.Commit(inst.ID, func(*procowner.Receipt) (*procowner.Receipt, error) { return f.receipt, nil })
	require.NoError(t, err)
	previous := codexSharedOwnershipInspectCommand
	calls := 0
	codexSharedOwnershipInspectCommand = func(pid int) (codexSharedOwnershipCommand, error) {
		calls++
		if calls == 1 {
			return codexSharedOwnershipCommand{}, errors.New("native startup not ready")
		}
		if calls == 2 {
			f.prober.alive(4400, "4000", os.Getuid())
			f.prober.children[f.receipt.Leader.PID] = append(f.prober.children[f.receipt.Leader.PID], f.prober.procs[4400])
		}
		return f.filter.inspect(pid)
	}
	defer func() { codexSharedOwnershipInspectCommand = previous }()
	wake, done := make(chan struct{}), make(chan struct{})
	sharedFilter := inst.codexSharedOwnershipFilter()
	go func() {
		defer close(done)
		inst.attributeOwnedTree(f.receipt, store, f.prober, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, wake, sharedFilter)
	}()
	defer func() { close(wake); <-done }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, loadErr := store.Load(inst.ID)
		require.NoError(t, loadErr)
		if r.HasMember(procowner.Member{PID: 4400, StartID: "4000"}) {
			assert.False(t, r.HasMember(procowner.Member{PID: 4200, StartID: "2000"}))
			assert.False(t, r.HasMember(procowner.Member{PID: 4201, StartID: "2000"}))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("temporary shared-runtime failure must not stop later session attribution")
}
