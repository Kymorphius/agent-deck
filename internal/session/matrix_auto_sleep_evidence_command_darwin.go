//go:build darwin

package session

import "golang.org/x/sys/unix"

// matrix/hardened: retrieve native argv without shell ambiguity. The parser
// stops at argc and never decodes, returns, logs or fingerprints environment.
func matrixAutoSleepReadCommand(pid int) (matrixAutoSleepCommand, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return matrixAutoSleepCommand{}, err
	}
	return matrixAutoSleepParseProcArgs(data)
}
