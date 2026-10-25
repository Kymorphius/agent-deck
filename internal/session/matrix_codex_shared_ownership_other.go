//go:build !darwin

package session

// matrix/hardened: do not infer another platform's managed-daemon lifecycle.
import "github.com/asheshgoplani/agent-deck/internal/procowner"

func inspectCodexSharedOwnershipCommand(int) (codexSharedOwnershipCommand, error) {
	return codexSharedOwnershipCommand{}, procowner.ErrUnsupported
}

func readCodexSharedOwnershipFile(string, int) ([]byte, error) {
	return nil, procowner.ErrUnsupported
}
