package tmux

import "strings"

// matrix/hardened: tmux 3.7+ (upstream commit 610bf013, "A few additional
// readonly checks") refuses `send-keys` with "client is read-only" when the
// command's TARGET client is read-only. Without -c, tmux picks the target
// client itself: the most recently active attached client on the server.
// agent-deck's own observers (control pipes, status monitors) attach with
// `-f read-only,ignore-size`, so they are routinely that client, and every
// send-keys from agent-deck then fails with exit status 1 while paste-buffer
// still works. A multi-line `session send` therefore pasted its body and lost
// the Enter, leaving "[Pasted text #N]" in the composer; every later send saw
// an occupied composer and refused. Naming a client that cannot exist leaves
// the target client empty (send-keys is CMD_CLIENT_CANFAIL), which skips the
// read-only check while the keys still go to the -t pane.
const sendKeysNoTargetClient = "agent-deck-no-target-client"

// sendKeysClientFlagMinMajor/Minor is the first tmux with `send-keys -c`
// (3.4). Older tmux rejects the flag, and also has no read-only check to
// avoid, so nothing is added there.
const (
	sendKeysClientFlagMinMajor = 3
	sendKeysClientFlagMinMinor = 4
)

// tmuxSupportsSendKeysClientFlag reports whether ver accepts `send-keys -c`.
// An unknown version keeps the historical argv.
func tmuxSupportsSendKeysClientFlag(ver string) bool {
	major, minor, _, ok := splitTmuxVersion(ver)
	if !ok {
		return false
	}
	return major > sendKeysClientFlagMinMajor ||
		(major == sendKeysClientFlagMinMajor && minor >= sendKeysClientFlagMinMinor)
}

// SendKeysClientFlag returns the `-c <client>` text to put right after
// `send-keys` in a tmux command line built as a string (control-mode
// commands, shell snippets), with a trailing space; "" on tmux without -c.
func SendKeysClientFlag() string {
	if !tmuxSupportsSendKeysClientFlag(hostTmuxVersionString()) {
		return ""
	}
	return "-c " + sendKeysNoTargetClient + " "
}

// withSendKeysClient inserts `-c <client>` after a leading send-keys when the
// caller did not name a client and the host tmux supports the flag.
func withSendKeysClient(args []string) []string {
	if len(args) == 0 || (args[0] != "send-keys" && args[0] != "send") {
		return args
	}
	if sendKeysNamesClient(args[1:]) || !tmuxSupportsSendKeysClientFlag(hostTmuxVersionString()) {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[0], "-c", sendKeysNoTargetClient)
	return append(out, args[1:]...)
}

// sendKeysNamesClient reports whether the send-keys options already carry -c.
// Only the option prefix is scanned, so literal key text is never mistaken
// for a flag.
func sendKeysNamesClient(opts []string) bool {
	for i := 0; i < len(opts); i++ {
		a := opts[i]
		if a == "--" || len(a) < 2 || !strings.HasPrefix(a, "-") {
			return false
		}
		switch a {
		case "-c":
			return true
		case "-N", "-t":
			i++
		}
	}
	return false
}
