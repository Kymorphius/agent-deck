package tmux

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: send-keys must name a non-existent target client on tmux
// with -c, so a read-only observer cannot make tmux 3.7+ refuse it.
func TestMatrixSendKeysClientFlagByVersion(t *testing.T) {
	for ver, want := range map[string]bool{
		"3.2a": false, "3.3a": false, "3.4": true, "3.7c": true, "3.8": true, "4.0": true, "": false, "next": false,
	} {
		if got := tmuxSupportsSendKeysClientFlag(ver); got != want {
			t.Errorf("tmuxSupportsSendKeysClientFlag(%q) = %v, want %v", ver, got, want)
		}
	}
}

func TestMatrixWithSendKeysClient(t *testing.T) {
	old := hostTmuxVersionString()
	t.Cleanup(func() { hostTmuxVersion = old })

	hostTmuxVersion = "3.8"
	cases := []struct{ in, want []string }{
		{[]string{"send-keys", "-t", "%1", "Enter"}, []string{"send-keys", "-c", sendKeysNoTargetClient, "-t", "%1", "Enter"}},
		{[]string{"send-keys", "-l", "-t", "s", "--", "-c"}, []string{"send-keys", "-c", sendKeysNoTargetClient, "-l", "-t", "s", "--", "-c"}},
		{[]string{"send-keys", "-c", "/dev/ttys1", "-t", "s", "x"}, []string{"send-keys", "-c", "/dev/ttys1", "-t", "s", "x"}},
		{[]string{"paste-buffer", "-t", "s"}, []string{"paste-buffer", "-t", "s"}},
	}
	for _, c := range cases {
		if got := withSendKeysClient(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("withSendKeysClient(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := SendKeysClientFlag(); got != "-c "+sendKeysNoTargetClient+" " {
		t.Errorf("SendKeysClientFlag() = %q", got)
	}

	hostTmuxVersion = "3.2a"
	in := []string{"send-keys", "-t", "s", "Enter"}
	if got := withSendKeysClient(in); !reflect.DeepEqual(got, in) {
		t.Errorf("tmux 3.2a must keep the old argv, got %q", got)
	}
	if got := SendKeysClientFlag(); got != "" {
		t.Errorf("SendKeysClientFlag() on 3.2a = %q", got)
	}
}

// matrix/hardened: with a read-only control client attached (as agent-deck's
// own observers are), tmux 3.7+ picks it as send-keys' target client and
// refused the Enter. The send must still type and submit.
func TestMatrixSendKeysSurvivesReadOnlyObserver(t *testing.T) {
	requireTmux(t)
	if !tmuxSupportsSendKeysClientFlag(hostTmuxVersionString()) {
		t.Skip("host tmux has no send-keys -c")
	}
	socket, target := makeIsolatedServer(t)

	observer := exec.Command("tmux", "-L", socket, "-C", "attach-session", "-f", "read-only,ignore-size", "-t", target)
	stdin, err := observer.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Start(); err != nil {
		t.Fatalf("attach read-only observer: %v", err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = observer.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("tmux", "-L", socket, "list-clients", "-F", "#{client_flags}").Output()
		if strings.Contains(string(out), "read-only") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("read-only observer never attached")
		}
		time.Sleep(50 * time.Millisecond)
	}

	s := &Session{Name: target, SocketName: socket}
	if err := s.SendKeysAndEnter("echo matrix-$((40+2))"); err != nil {
		t.Fatalf("SendKeysAndEnter with a read-only observer attached: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", target).Output()
		if strings.Contains(string(out), "matrix-42") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("command was not submitted; pane:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
