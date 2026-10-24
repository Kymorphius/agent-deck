package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/asheshgoplani/agent-deck/internal/core"
)

// matrix/hardened: flags/help are parsed before opening storage or acquiring a
// mutation lock, so capability probes are always read-only and need no TTY.
func cliSessionPrepareOpen(profile string, args []string) {
	fs := flag.NewFlagSet("session prepare-open", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session prepare-open [--json] <full-id>")
		fmt.Println("Prepare an explicit remote visit without attaching or sending input.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(1)
	}
	mode := &jsonModeFlag{}
	if *jsonOutput {
		mode.mode = jsonLegacy
	}
	res := runCore(profile, mode, core.IDSessionPrepareOpen, core.SessionPrepareOpenIn{Profile: profile, Session: fs.Arg(0)}, nil)
	defer res.Finish()
	if res.Err != nil {
		exitCoreError(NewCLIOutput(*jsonOutput, false), mode, res, nil)
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(res.Out); err != nil {
			fmt.Fprintln(os.Stderr, "Error: cannot format prepare-open result:", err)
			os.Exit(1)
		}
		return
	}
	out := res.Out.(core.SessionPrepareOpenOut)
	fmt.Printf("%s: %s (%s)\n", out.ID, out.Disposition, out.Status)
}
