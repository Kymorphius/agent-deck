package session

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// matrix/hardened: argv boundaries come from the OS, never strings.Fields(ps).
// Only the exact configured executable and argument vector can identify a
// resident MCP. Generic node/python/ssh names confer no permission to stop.
type matrixAutoSleepCommand struct {
	Executable string
	Args       []string
}

func matrixAutoSleepResolveCommand(command string, args []string) (matrixAutoSleepCommand, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return matrixAutoSleepCommand{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return matrixAutoSleepCommand{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return matrixAutoSleepCommand{}, err
	}
	return matrixAutoSleepCommand{Executable: path, Args: slices.Clone(args)}, nil
}

func matrixAutoSleepConfiguredMCP(process matrixAutoSleepCommand, configured []matrixAutoSleepCommand) bool {
	for _, command := range configured {
		if process.Executable == command.Executable && slices.Equal(process.Args, command.Args) {
			return true
		}
	}
	return false
}

// matrix/hardened: Darwin puts argc, executable path, padding, argv, then env
// in KERN_PROCARGS2. Parse exactly argc entries and never inspect the remainder.
func matrixAutoSleepParseProcArgs(data []byte) (matrixAutoSleepCommand, error) {
	if len(data) < 5 {
		return matrixAutoSleepCommand{}, errors.New("short process argument record")
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	if argc <= 0 || argc > 65536 {
		return matrixAutoSleepCommand{}, errors.New("invalid process argc")
	}
	data = data[4:]
	end := bytes.IndexByte(data, 0)
	if end <= 0 {
		return matrixAutoSleepCommand{}, errors.New("missing process executable")
	}
	executable := string(data[:end])
	data = data[end+1:]
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}
	argv := make([]string, 0, argc)
	for n := 0; n < argc; n++ {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			return matrixAutoSleepCommand{}, errors.New("incomplete process argv")
		}
		argv = append(argv, string(data[:end]))
		data = data[end+1:]
	}
	if !filepath.IsAbs(executable) || argv[0] == "" || strings.ContainsRune(executable, '\n') {
		return matrixAutoSleepCommand{}, errors.New("invalid process executable path")
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return matrixAutoSleepCommand{}, err
	}
	return matrixAutoSleepCommand{Executable: canonical, Args: argv[1:]}, nil
}
