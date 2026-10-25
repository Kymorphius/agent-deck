//go:build darwin

package session

// matrix/hardened: read-only probes for the audited Darwin managed-daemon layout.
import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func inspectCodexSharedOwnershipCommand(pid int) (codexSharedOwnershipCommand, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Separate columns avoid guessing where an executable path with spaces
	// ends. Neither command is evaluated by a shell.
	executable, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return codexSharedOwnershipCommand{}, err
	}
	arguments, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return codexSharedOwnershipCommand{}, err
	}
	return codexSharedOwnershipCommand{Executable: strings.TrimSpace(string(executable)), Arguments: strings.TrimSpace(string(arguments))}, nil
}

func readCodexSharedOwnershipFile(path string, limit int) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, fmt.Errorf("shared runtime record is not a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}
