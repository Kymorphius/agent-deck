package testutil

// matrix/hardened: re-exec'd helper test binaries isolate again in their own
// TestMain and are often killed or end in os.Exit before that cleanup runs.
// Their private dirs now nest in the parent's isolation, and the parent's
// cleanup reaps every server below it.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withTmuxEnv(t *testing.T, tmpdir string, marker bool) {
	t.Helper()
	origDir, hadDir := os.LookupEnv("TMUX_TMPDIR")
	origMarker, hadMarker := os.LookupEnv(TestIsolationMarkerEnv)
	t.Cleanup(func() {
		restore("TMUX_TMPDIR", origDir, hadDir)
		restore(TestIsolationMarkerEnv, origMarker, hadMarker)
	})
	_ = os.Setenv("TMUX_TMPDIR", tmpdir)
	if marker {
		_ = os.Setenv(TestIsolationMarkerEnv, "1")
	} else {
		_ = os.Unsetenv(TestIsolationMarkerEnv)
	}
}

func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Skipf("no short temp dir: %v", err)
	}
	t.Cleanup(func() {
		KillTmuxServersUnder(dir)
		_ = os.RemoveAll(dir)
	})
	return dir
}

func TestIsolateTmuxSocket_NestingRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent func(t *testing.T) string
		marker bool
		nested bool
	}{
		{"inside an isolated test process", func(t *testing.T) string { return shortTempDir(t, "ad-tmux-") }, true, true},
		{"inside a nested helper's dir", func(t *testing.T) string {
			dir, err := os.MkdirTemp(shortTempDir(t, "ad-tmux-"), "t")
			if err != nil {
				t.Fatal(err)
			}
			return dir
		}, true, true},
		{"run-isolated root keeps the top-level layout", func(t *testing.T) string { return shortTempDir(t, "ad-it-") }, true, false},
		{"ad-tmux dir without the marker", func(t *testing.T) string { return shortTempDir(t, "ad-tmux-") }, false, false},
		{"any other directory", func(t *testing.T) string { return t.TempDir() }, true, false},
		{"base too long for sun_path", func(t *testing.T) string {
			return filepath.Join(shortTempDir(t, "ad-tmux-"), strings.Repeat("y", maxNestedTmuxBase))
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := tc.parent(t)
			_ = os.MkdirAll(parent, 0o700)
			withTmuxEnv(t, parent, tc.marker)
			cleanup := IsolateTmuxSocket()
			dir := os.Getenv("TMUX_TMPDIR")
			cleanup()
			if got := filepath.Dir(dir) == filepath.Clean(parent); got != tc.nested {
				t.Fatalf("isolated dir %q nested under %q = %v, want %v", dir, parent, got, tc.nested)
			}
			if wantPrefix := map[bool]string{true: "t", false: "ad-tmux-"}[tc.nested]; !strings.HasPrefix(filepath.Base(dir), wantPrefix) {
				t.Fatalf("isolated dir %q should start with %q", dir, wantPrefix)
			}
			if !tc.nested && filepath.Dir(dir) != "/tmp" {
				t.Fatalf("top-level isolated dir %q left /tmp", dir)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("cleanup left %q behind (%v)", dir, err)
			}
		})
	}
}

const nestedHelperEnv = "AGENT_DECK_TESTUTIL_NESTED_HELPER"

// TestNestedIsolationHelperProcess is the re-exec'd helper: it isolates the
// way a TestMain does, starts a server, reports it, and waits to be killed.
func TestNestedIsolationHelperProcess(t *testing.T) {
	if os.Getenv(nestedHelperEnv) != "1" {
		return
	}
	_ = IsolateTmuxSocket()
	dir := os.Getenv("TMUX_TMPDIR")
	cmd := exec.Command("tmux", "new-session", "-d", "-x", "80", "-y", "24", "-s", "helper", "sleep 600")
	cmd.Env = append(envWithoutTmuxVars(), "TMUX_TMPDIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("ERR %v %s\n", err, out)
		os.Exit(1)
	}
	socket := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	fmt.Printf("SOCKET %s\n", socket)
	time.Sleep(10 * time.Minute)
}

// A helper killed before its own cleanup no longer strands its server: the
// parent's cleanup reaps it.
func TestIsolateTmuxSocket_ParentCleanupReapsKilledHelperServer(t *testing.T) {
	requireTmuxBinary(t)
	parentDir := shortTempDir(t, "ad-tmux-")
	withTmuxEnv(t, parentDir, true)
	cleanup := IsolateTmuxSocket()
	parent := os.Getenv("TMUX_TMPDIR")

	helper := exec.Command(os.Args[0], "-test.run=^TestNestedIsolationHelperProcess$")
	helper.Env = append(os.Environ(), nestedHelperEnv+"=1")
	out, err := helper.StdoutPipe()
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		cleanup()
		t.Fatal(err)
	}
	var socket string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "ERR ") {
			_ = helper.Process.Kill()
			_ = helper.Wait()
			cleanup()
			t.Skipf("helper could not start tmux: %s", line)
		}
		if after, ok := strings.CutPrefix(line, "SOCKET "); ok {
			socket = after
			break
		}
	}
	if socket == "" || !strings.HasPrefix(socket, parent+string(filepath.Separator)) {
		_ = helper.Process.Kill()
		_ = helper.Wait()
		cleanup()
		t.Fatalf("helper socket %q is not nested under the parent's %q", socket, parent)
	}
	pid := tmuxPidOnSocket(t, socket)
	if pid == 0 {
		_ = helper.Process.Kill()
		_ = helper.Wait()
		cleanup()
		t.Fatalf("helper server is not up on %s", socket)
	}
	// SIGKILL: the helper's own cleanup never runs, as with os.Exit.
	_ = helper.Process.Kill()
	_ = helper.Wait()
	if !processAlive(pid) {
		cleanup()
		t.Fatalf("fixture: helper server %d died with its client", pid)
	}

	cleanup()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
		t.Fatalf("killed helper's tmux server %d survived the parent's cleanup", pid)
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("parent cleanup left %q (%v)", parent, err)
	}
}
