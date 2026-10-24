package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/creack/pty"
)

// matrix/hardened: queued interactive visits must start the saved launch plan
// exactly once, using real CLI/PTY/tmux plumbing and a local fake harness only.
type matrixQueuedAttachFixture struct {
	t       *testing.T
	bin     string
	home    string
	socket  string
	env     []string
	storage *session.Storage
	target  *session.Instance
	spawns  string
	harness string
}

// matrix/hardened: keep every profile, process and socket inside test-owned
// state; agentDeckTestEnv strips TMUX_TMPDIR, so explicitly restore the runner's.
func matrixNewQueuedAttachFixture(t *testing.T, cap int) *matrixQueuedAttachFixture {
	t.Helper()
	for _, name := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("requires %s", name)
		}
	}
	if os.Getenv("TMUX_TMPDIR") == "" {
		t.Fatal("run this integration test through scripts/run-isolated.py")
	}
	f := &matrixQueuedAttachFixture{t: t, bin: channelsCLIBinary(t), home: t.TempDir()}
	f.socket = fmt.Sprintf("queued-attach-%d", time.Now().UnixNano())
	for key, value := range map[string]string{
		"HOME": f.home, "XDG_CONFIG_HOME": filepath.Join(f.home, ".config"),
		"XDG_DATA_HOME":  filepath.Join(f.home, ".local", "share"),
		"XDG_CACHE_HOME": filepath.Join(f.home, ".cache"), "AGENTDECK_PROFILE": "queued-attach-test",
	} {
		t.Setenv(key, value)
	}
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	previousDB := statedb.GetGlobal()
	statedb.SetGlobal(nil)
	t.Cleanup(func() { statedb.SetGlobal(previousDB) })
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.home, ".config", "agent-deck", "config.toml"), "[tmux]\nsocket_name = '"+f.socket+"'\n")
	write(filepath.Join(f.home, ".zshrc"), "# matrix/hardened: isolated attach fixture\n")
	f.spawns = filepath.Join(f.home, "spawns")
	f.harness = filepath.Join(f.home, "harness.py")
	write(f.harness, `import os, sys, tty
with open(`+fmt.Sprintf("%q", f.spawns)+`, "a") as f:
    f.write(str(os.getpid()) + " " + " ".join(sys.argv[1:]) + "\n")
if "--fail" in sys.argv:
    print("QUEUED-ATTACH-INITIALIZATION-FAILED", flush=True)
    sys.exit(7)
tty.setraw(0)
sys.stdout.write("\033[2J\033[HQUEUED-ATTACH-READY PID=" + str(os.getpid()) + "\r\n")
sys.stdout.flush()
while True:
    data = os.read(0, 8192)
    if not data: break
    sys.stdout.write("\r\nRX:" + data.hex())
    sys.stdout.flush()
`)
	f.env = agentDeckTestEnv(f.home, []string{
		"AGENTDECK_PROFILE=queued-attach-test", "TERM=xterm-256color",
		"TMUX_TMPDIR=" + os.Getenv("TMUX_TMPDIR"),
		"XDG_CACHE_HOME=" + filepath.Join(f.home, ".cache"),
	})
	var err error
	f.storage, err = session.NewStorageWithProfile("queued-attach-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.storage.Close() })
	f.target = session.NewInstanceWithTool("queued target", f.home, "shell")
	f.target.GroupPath = "queued-attach-group"
	f.target.Command = "/bin/sh"
	f.target.Wrapper = "python3 -u '" + f.harness + "' --kept-wrapper"
	f.target.Status = session.StatusQueued
	tree := session.NewGroupTreeWithGroups([]*session.Instance{f.target}, []*session.GroupData{{
		Name: "queued attach group", Path: f.target.GroupPath, MaxConcurrent: cap,
	}})
	if err := f.storage.SaveWithGroups([]*session.Instance{f.target}, tree); err != nil {
		t.Fatal(err)
	}
	return f
}

// matrix/hardened: disk reads, rather than a stale fixture pointer, establish
// what another CLI or remote list reader will observe after attach.
func (f *matrixQueuedAttachFixture) stored(id string) *session.Instance {
	f.t.Helper()
	instances, _, err := f.storage.LoadWithGroups()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.ID == id {
			return inst
		}
	}
	f.t.Fatalf("stored instance %s is missing", id)
	return nil
}

// matrix/hardened: protect output readback from the PTY drainer under -race.
type matrixQueuedAttachClient struct {
	t        *testing.T
	cmd      *exec.Cmd
	terminal *os.File
	mu       sync.Mutex
	output   bytes.Buffer
	done     chan struct{}
	readDone chan struct{}
	err      error
}

func (f *matrixQueuedAttachFixture) attach() *matrixQueuedAttachClient {
	f.t.Helper()
	c := &matrixQueuedAttachClient{t: f.t, done: make(chan struct{}), readDone: make(chan struct{})}
	c.cmd = exec.Command(f.bin, "session", "attach", f.target.ID)
	c.cmd.Env, c.cmd.Dir = f.env, f.home
	var err error
	c.terminal, err = pty.StartWithSize(c.cmd, &pty.Winsize{Cols: 127, Rows: 40})
	if err != nil {
		f.t.Fatal(err)
	}
	go func() { c.err = c.cmd.Wait(); close(c.done) }()
	f.t.Cleanup(func() {
		_ = c.cmd.Process.Kill()
		_ = c.terminal.Close()
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
			f.t.Error("queued attach client cleanup timed out")
		}
	})
	go func() {
		defer close(c.readDone)
		buf := make([]byte, 8192)
		var pending []byte
		for {
			n, err := c.terminal.Read(buf)
			if n > 0 {
				c.mu.Lock()
				_, _ = c.output.Write(buf[:n])
				c.mu.Unlock()
				pending = append(pending, buf[:n]...)
				// matrix/hardened: emulate terminal query replies so attach cannot
				// mistake terminal setup traffic for user input in the harness.
				for _, query := range []struct{ request, response string }{
					{"\x1b]11;?\x1b\\", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
					{"\x1b]11;?\a", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
					{"\x1b[6n", "\x1b[1;1R"},
				} {
					for bytes.Contains(pending, []byte(query.request)) {
						_, _ = c.terminal.WriteString(query.response)
						pending = bytes.Replace(pending, []byte(query.request), nil, 1)
					}
				}
				if len(pending) > 64 {
					pending = pending[len(pending)-64:]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *matrixQueuedAttachClient) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
}

func (c *matrixQueuedAttachClient) waitReady() {
	c.t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.text(), "QUEUED-ATTACH-READY") {
			return
		}
		select {
		case <-c.done:
			c.t.Fatalf("attach exited before harness frame: %v; output: %q", c.waitExit(), c.text())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("attach did not reach harness frame: %q", c.text())
}

func (c *matrixQueuedAttachClient) waitExit() error {
	c.t.Helper()
	select {
	case <-c.done:
		select {
		case <-c.readDone:
		case <-time.After(time.Second):
		}
		return c.err
	case <-time.After(12 * time.Second):
		c.t.Fatalf("attach did not exit: %q", c.text())
		return nil
	}
}

// matrix/hardened: the append-only fake-harness log records actual launches,
// including their PID and the wrapper/command arguments preserved by attach.
func (f *matrixQueuedAttachFixture) spawnLines() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.spawns)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' })
}

// matrix/hardened: session start --no-wait can return before the fake process
// writes its first record, so fixture setup must wait for actual harness proof.
func (f *matrixQueuedAttachFixture) waitSpawn() []string {
	f.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if lines := f.spawnLines(); len(lines) != 0 {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "-L", f.socket, "capture-pane", "-p")
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	f.t.Fatalf("fake harness did not start; pane=%q error=%v", out, err)
	return nil
}

// matrix/hardened: detach through the actual binding rather than terminating
// the client, preserving the started process for independent persisted checks.
func (c *matrixQueuedAttachClient) detach() {
	c.t.Helper()
	if _, err := c.terminal.WriteString("\x11"); err != nil {
		c.t.Fatal(err)
	}
	if err := c.waitExit(); err != nil {
		c.t.Fatalf("Ctrl+Q detach failed: %v; output=%q", err, c.text())
	}
}

// matrix/hardened: this is the absent-pane queued first-open regression; both
// a visible live frame and durable state must agree that startup succeeded.
func TestMatrixQueuedAttachFirstInteractiveVisit(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	c := f.attach()
	c.waitReady()
	stored := f.stored(f.target.ID)
	if stored.Status == session.StatusQueued || stored.Status == session.StatusError || !stored.Exists() {
		t.Fatalf("first attach did not durably start queued session: status=%s exists=%v", stored.Status, stored.Exists())
	}
	if stored.Command != f.target.Command || stored.Wrapper != f.target.Wrapper {
		t.Fatalf("queued attach changed saved launch plan: command=%q wrapper=%q", stored.Command, stored.Wrapper)
	}
	if spawns := f.spawnLines(); len(spawns) != 1 || !strings.Contains(spawns[0], "--kept-wrapper") {
		t.Fatalf("saved wrapper was not executed exactly once: %q", spawns)
	}
	c.detach()
}

// matrix/hardened: piping attach is not an intentional visit and must neither
// consume group capacity nor create a queued process behind the caller's back.
func TestMatrixQueuedAttachNoTTYDoesNotStart(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	out, err := f.run("session", "attach", f.target.ID)
	if err == nil || (!strings.Contains(strings.ToLower(out), "terminal") && !strings.Contains(strings.ToLower(out), "tty")) {
		t.Fatalf("non-TTY attach must refuse explicitly: err=%v output=%q", err, out)
	}
	if got := f.stored(f.target.ID).Status; got != session.StatusQueued {
		t.Errorf("non-TTY attach changed status to %s", got)
	}
	if spawns := f.spawnLines(); len(spawns) != 0 {
		t.Errorf("non-TTY attach spawned harness: %q", spawns)
	}
}

// matrix/hardened: persisted queued state can be stale while a pane survives;
// an interactive visit must reuse that exact process instead of restarting it.
func TestMatrixQueuedAttachLiveStaleQueueDoesNotRestart(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	if out, err := f.run("session", "start", f.target.ID, "--no-wait", "--json"); err != nil {
		t.Fatalf("fixture start: %v %s", err, out)
	}
	stored := f.stored(f.target.ID)
	stored.Status = session.StatusQueued
	if err := f.storage.Save([]*session.Instance{stored}); err != nil {
		t.Fatal(err)
	}
	before := f.waitSpawn()
	if len(before) != 1 {
		t.Fatalf("fixture did not launch exactly one harness: %q", before)
	}
	c := f.attach()
	c.waitReady()
	if after := f.spawnLines(); len(after) != 1 || after[0] != before[0] {
		t.Fatalf("stale-queued attach restarted a live process: before=%q after=%q", before, after)
	}
	c.detach()
}

// matrix/hardened: manual stop/crash records are not permission to implicitly
// restart; only the queued first-launch state participates in this transition.
func TestMatrixQueuedAttachStoppedAndErrorStayStopped(t *testing.T) {
	for _, status := range []session.Status{session.StatusStopped, session.StatusError} {
		t.Run(string(status), func(t *testing.T) {
			f := matrixNewQueuedAttachFixture(t, 0)
			f.target.Status = status
			if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
				t.Fatal(err)
			}
			c := f.attach()
			if err := c.waitExit(); err == nil {
				t.Fatalf("%s absent session attach claimed success: %q", status, c.text())
			}
			if got := f.stored(f.target.ID).Status; got != status {
				t.Errorf("attach changed %s status to %s", status, got)
			}
			if spawns := f.spawnLines(); len(spawns) != 0 {
				t.Errorf("attach auto-restarted %s session: %q", status, spawns)
			}
		})
	}
}

// matrix/hardened: startup accepted by tmux can still die before its first
// frame; verification must report failure rather than persist a running lie.
func TestMatrixQueuedAttachInitializationFailureIsNotSuccess(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	// matrix/hardened: a custom harness is the pane's initial process; a shell
	// tool deliberately returns to its still-live prompt after a command exits.
	f.target.Tool = "matrix-queued-attach-failure"
	f.target.Wrapper = "python3 -u '" + f.harness + "' --fail"
	if err := f.storage.Save([]*session.Instance{f.target}); err != nil {
		t.Fatal(err)
	}
	c := f.attach()
	if err := c.waitExit(); err == nil {
		t.Fatalf("failed initialization claimed success: %q", c.text())
	}
	if strings.Contains(c.text(), "QUEUED-ATTACH-READY") || !strings.Contains(strings.ToLower(c.text()), "error") {
		t.Errorf("failed initialization must report an error without attaching a ready frame: %q", c.text())
	}
	if got := f.stored(f.target.ID).Status; got != session.StatusError {
		t.Errorf("failed initialization persisted status=%s, want error", got)
	}
	if spawns := f.spawnLines(); len(spawns) != 1 {
		t.Errorf("expected exactly one failed initialization attempt: %q", spawns)
	}
	second := f.attach()
	if err := second.waitExit(); err == nil {
		t.Errorf("second visit to failed session claimed success: %q", second.text())
	}
	if spawns := f.spawnLines(); len(spawns) != 1 {
		t.Errorf("second visit automatically retried failed initialization: %q", spawns)
	}
}

// matrix/hardened: two independent remote/CLI opens can race before either
// caller stores its result; the shared profile transition must admit one spawn.
func TestMatrixQueuedAttachConcurrentOpenStartsOnce(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	first, second := f.attach(), f.attach()
	first.waitReady()
	second.waitReady()
	if spawns := f.spawnLines(); len(spawns) != 1 {
		t.Fatalf("concurrent attach must start one process: %q", spawns)
	}
	stored := f.stored(f.target.ID)
	if stored.Status == session.StatusQueued || !stored.Exists() {
		t.Fatalf("concurrent attach left no durable live process: status=%s exists=%v", stored.Status, stored.Exists())
	}
	first.detach()
	second.detach()
}

// matrix/hardened: a capped interactive visit must keep the queue and explain
// the actual group/cap restriction instead of claiming the session is broken.
func TestMatrixQueuedAttachAtGroupCap(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 1)
	blocker := session.NewInstanceWithTool("running sibling", f.home, "shell")
	blocker.GroupPath, blocker.Status = f.target.GroupPath, session.StatusRunning
	if err := f.storage.Save([]*session.Instance{blocker}); err != nil {
		t.Fatal(err)
	}
	c := f.attach()
	if err := c.waitExit(); err == nil {
		t.Fatalf("at-cap attach claimed success: %q", c.text())
	}
	if out := strings.ToLower(c.text()); !strings.Contains(out, f.target.GroupPath) || !strings.Contains(out, "cap") || !strings.Contains(out, "1") {
		t.Errorf("at-cap attach must name group and cap 1: %q", c.text())
	}
	if got := f.stored(f.target.ID).Status; got != session.StatusQueued {
		t.Errorf("at-cap status=%s, want queued", got)
	}
	if data, err := os.ReadFile(f.spawns); err == nil && len(data) > 0 {
		t.Errorf("at-cap attach spawned harness: %s", data)
	}
}

// matrix/hardened: a database rejection after a verified spawn is an uncertain
// persistence result, not permission to kill the process or claim full success.
func TestMatrixQueuedAttachSaveFailureKeepsStartedPane(t *testing.T) {
	f := matrixNewQueuedAttachFixture(t, 0)
	query := fmt.Sprintf(`CREATE TRIGGER matrix_queued_attach_reject_save
BEFORE UPDATE OF status ON instances
WHEN OLD.id = '%s' AND OLD.status = 'queued' AND NEW.status IN ('starting', 'running', 'idle', 'waiting')
BEGIN SELECT RAISE(FAIL, 'matrix queued attach save rejected'); END`, f.target.ID)
	if _, err := f.storage.GetDB().DB().Exec(query); err != nil {
		t.Fatal(err)
	}
	c := f.attach()
	if err := c.waitExit(); err == nil {
		t.Fatalf("save failure claimed success: %q", c.text())
	}
	out := strings.ToLower(c.text())
	if !strings.Contains(out, "was started") || !strings.Contains(out, "could not be saved") {
		t.Errorf("save failure must explain surviving started process and failed save: %q", c.text())
	}
	if spawns := f.waitSpawn(); len(spawns) != 1 {
		t.Errorf("save failure must leave exactly one started process: %q", spawns)
	}
	stored := f.stored(f.target.ID)
	if stored.Status != session.StatusQueued {
		t.Errorf("rejected save unexpectedly changed durable status to %s", stored.Status)
	}
	if !stored.Exists() {
		t.Error("save failure killed the verified live pane")
	}
}

// matrix/hardened: retain a timeout on non-interactive CLI invocations, which
// must fail before touching any queued process.
func (f *matrixQueuedAttachFixture) run(args ...string) (string, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin, args...)
	cmd.Env, cmd.Dir = f.env, f.home
	out, err := cmd.CombinedOutput()
	return string(out), err
}
