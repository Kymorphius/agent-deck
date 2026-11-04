package session

import (
	"errors"
	"strings"
	"testing"
)

// matrix/hardened: the server's original new-session command remains visible
// after its first pane exits, but it is not a live Claude conversation owner.
func TestMatrixExactClaudeIgnoresConfirmedTmuxServerHistoricalArgv(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	table := []byte(" 37553 /opt/homebrew/bin/tmux -u new-session -d -s old bash -c exec claude --resume " + sid + "\n 99999 /usr/bin/other --resume aaaaaaaa-2222-4333-8444-555555555555\n")
	reads := 0
	err := exactClaudeCheckNativeArgvOwners(table, sid, func(pid int) (bool, error) {
		return exactClaudeConfirmTmuxServer(pid, func() (int, error) {
			reads++
			return 37553, nil
		}, func(got int) (string, error) {
			if got != 37553 {
				t.Fatalf("unexpected process probe: %d", got)
			}
			return "/opt/homebrew/Cellar/tmux/3.8/bin/tmux", nil
		})
	})
	if err != nil || reads != 2 {
		t.Fatalf("historical server argv blocked recovery: reads=%d err=%v", reads, err)
	}
}

// matrix/hardened: only a confirmed server is exempt. Direct providers,
// wrappers, and a tmux client that is starting a provider remain blockers.
func TestMatrixExactClaudeNativeArgvOwnersRemainBlocked(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	for _, command := range []string{
		"/usr/local/bin/claude --resume " + sid,
		"/provider/claude.exe --session-id " + sid,
		"python3 harness.py --resume " + sid,
		"sudo -n -- /provider/claude.exe --resume " + sid,
		"bash -c exec claude -r " + sid,
		"tmux new-session exec claude --resume " + sid,
	} {
		t.Run(command, func(t *testing.T) {
			err := exactClaudeCheckNativeArgvOwners([]byte("42 "+command+"\n"), sid, func(pid int) (bool, error) {
				return exactClaudeConfirmTmuxServer(pid, func() (int, error) { return 37553, nil }, func(int) (string, error) {
					t.Fatal("a non-server must not be exempted by its executable name")
					return "tmux", nil
				})
			})
			if err == nil || !strings.Contains(err.Error(), "still has a native process") {
				t.Fatalf("live provider/launcher owner was ignored: %v", err)
			}
		})
	}
	// Ignoring one daemon must not hide a second, actual owner later in ps.
	err := exactClaudeCheckNativeArgvOwners([]byte("37553 tmux --resume "+sid+"\n42 claude --resume "+sid+"\n"), sid, func(pid int) (bool, error) { return pid == 37553, nil })
	if err == nil {
		t.Fatal("a real owner after the historical server was ignored")
	}
}

func TestMatrixExactClaudeTmuxServerRoleRequiresStableIndependentProof(t *testing.T) {
	for _, kind := range []string{"first-probe-error", "invalid-role", "client-role", "empty-role", "comm-error", "changed-server", "second-probe-error"} {
		t.Run(kind, func(t *testing.T) {
			reads := 0
			server, err := exactClaudeConfirmTmuxServer(37553, func() (int, error) {
				reads++
				if kind == "first-probe-error" || kind == "second-probe-error" && reads == 2 {
					return 0, errors.New("unreadable socket")
				}
				if kind == "changed-server" && reads == 2 {
					return 37554, nil
				}
				return 37553, nil
			}, func(int) (string, error) {
				switch kind {
				case "invalid-role":
					return "/provider/claude.exe", nil
				case "client-role":
					return "tmux: client", nil
				case "empty-role":
					return "", nil
				case "comm-error":
					return "", errors.New("unreadable process")
				}
				return "tmux: server", nil
			})
			if server || err == nil {
				t.Fatalf("unverified server exempted: %v %v", server, err)
			}
		})
	}
	server, err := exactClaudeConfirmTmuxServer(37553, func() (int, error) { return 37553, nil }, func(int) (string, error) { return "tmux: server", nil })
	if !server || err != nil {
		t.Fatalf("Linux server comm was not accepted with socket proof: %v %v", server, err)
	}
}

func TestMatrixExactClaudeProcessTableAndProbeFailuresRefuse(t *testing.T) {
	const sid = "11111111-2222-4333-8444-555555555555"
	for _, table := range []string{"malformed--resume" + sid, "unknown claude --resume " + sid, "0 claude --resume " + sid} {
		if err := exactClaudeCheckNativeArgvOwners([]byte(table), sid, func(int) (bool, error) { return true, nil }); err == nil {
			t.Fatalf("unreadable process table accepted: %q", table)
		}
	}
	if err := exactClaudeCheckNativeArgvOwners([]byte("42 tmux --resume "+sid), sid, func(int) (bool, error) { return false, errors.New("unreadable role") }); err == nil {
		t.Fatal("unknown candidate process role authorized recovery")
	}
	if err := exactClaudeCheckNativeArgvOwners([]byte("0 [kernel]\nunrelated-bad-row\n42\n1 launchd\n42 claude --resume another\n"), sid, func(int) (bool, error) {
		t.Fatal("unrelated processes must not need an exception probe")
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}
