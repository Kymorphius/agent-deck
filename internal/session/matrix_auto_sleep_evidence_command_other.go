//go:build !darwin && !linux

package session

import "errors"

// matrix/hardened: unsupported argv providers never qualify an automatic stop.
func matrixAutoSleepReadCommand(pid int) (matrixAutoSleepCommand, error) {
	return matrixAutoSleepCommand{}, errors.New("exact process argv unsupported")
}
