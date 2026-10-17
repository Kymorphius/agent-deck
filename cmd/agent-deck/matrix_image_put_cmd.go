package main

// matrix/hardened: `agent-deck session image-put <id>` stores an image read
// from stdin for a session and prints its path. A controller attached to this
// host's session runs it over SSH when Ctrl+V finds an image on the
// controller's clipboard, then pastes the path into the pane
// (internal/session/matrix_remote_image.go).

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func handleSessionImagePut(profile string, args []string) {
	fs := flag.NewFlagSet("session image-put", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session image-put <id|title> [--json] < image")
		fmt.Println()
		fmt.Println("Store a PNG, JPEG, GIF or WebP image from stdin for the session (kept a week)")
		fmt.Println("and print its path, which pastes into Claude Code as an [Image #n] attachment.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	out := NewCLIOutput(*jsonOutput, false)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	_, instances, _, err := loadSessionData(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	inst, errMsg, errCode := ResolveSession(fs.Arg(0), instances)
	if inst == nil {
		out.Error(errMsg, errCode)
		os.Exit(2)
		return
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, session.MatrixImageMaxBytes+1))
	if err != nil {
		out.Error(fmt.Sprintf("read image: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	path, err := session.StoreSessionImage(profile, inst.ID, data)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	out.Print(path+"\n", map[string]any{"success": true, "path": path})
}
