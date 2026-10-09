package ui

// matrix/hardened: vim-style count prefix for the session list ("3j", "12k").
//
// Digits are a count only, as in vim: "3j" moves three rows down, "12k"
// twelve up (also with up/down/ctrl+n/ctrl+p). "0" extends a pending count
// ("10j"); with no count pending it still clears the status filter. The old
// plain-digit "jump to root group N" moved to "'" followed by the digit
// ("'3"). Any other key drops a pending count or "'". ":" opens the line
// prompt in matrix_line_numbers.go.

const matrixNavCountMax = 999

// matrixGroupJumpKey is the prefix for jumping to root group N ("'3").
const matrixGroupJumpKey = "'"

// matrixCmdKey opens the ":" prompt (":12" jumps to line 12).
const matrixCmdKey = ":"

type matrixNavCount struct {
	n         int
	groupJump bool   // "'" pressed; the next digit picks the root group
	cmd       bool   // ":" prompt open (matrix_line_numbers.go)
	cmdBuf    string // text typed after ":"
	noNumbers bool   // ":set nonu"; survives resets
}

// matrixResetNav drops any pending count, "'" or ":" prompt.
func (h *Home) matrixResetNav() {
	h.navCount = matrixNavCount{noNumbers: h.navCount.noNumbers}
}

// matrixNavCountKey reports whether key reads or extends a pending count.
func matrixNavCountKey(key string) bool {
	switch key {
	case "up", "k", "ctrl+p", "down", "j", "ctrl+n",
		"0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return true
	}
	return false
}

// matrixHandleNavPrefix handles "'", the digit after it, and count digits.
// It returns true when the key was consumed.
func (h *Home) matrixHandleNavPrefix(key string) bool {
	if h.navCount.groupJump {
		h.matrixResetNav()
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			h.jumpToRootGroup(int(key[0] - '0'))
			return true
		}
		// Not a group digit: the "'" is dropped and the key acts as usual.
		return false
	}
	if key == matrixGroupJumpKey {
		h.matrixResetNav()
		h.navCount.groupJump = true
		return true
	}
	if key == matrixCmdKey {
		h.matrixResetNav()
		h.navCount.cmd = true
		return true
	}
	if !matrixNavCountKey(key) {
		h.matrixResetNav()
		return false
	}
	if len(key) != 1 || key[0] < '0' || key[0] > '9' {
		return false // j/k: matrixTakeNavCount reads the count
	}
	if key == "0" && h.navCount.n == 0 {
		return false // plain "0" keeps clearing the status filter
	}
	if n := h.navCount.n*10 + int(key[0]-'0'); n <= matrixNavCountMax {
		h.navCount.n = n
	}
	return true
}

// matrixTakeNavCount returns the step count for a j/k press (1 without a
// count) and clears the pending count.
func (h *Home) matrixTakeNavCount() int {
	n := h.navCount.n
	h.matrixResetNav()
	if n < 1 {
		return 1
	}
	return n
}
