//go:build linux

package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

// matrix/hardened: /proc cmdline is argv only; never read /proc environ.
func matrixAutoSleepReadCommand(pid int) (matrixAutoSleepCommand, error) {
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return matrixAutoSleepCommand{}, err
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 || len(data) > 4<<20 || data[len(data)-1] != 0 {
		return matrixAutoSleepCommand{}, errors.New("process argv unavailable")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	args := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		args = append(args, string(part))
	}
	return matrixAutoSleepCommand{Executable: executable, Args: args}, nil
}
