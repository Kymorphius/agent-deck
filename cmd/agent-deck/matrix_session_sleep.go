package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: dry-run exposes refusal reasons without stopping sessions.
// Actual sleep is explicit for one ID and uses the same final lifecycle gate.
func handleSessionSleep(profile string, args []string) {
	fs := flag.NewFlagSet("session sleep", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "Check eligibility without stopping any process")
	jsonOut := fs.Bool("json", false, "Print structured eligibility results")
	_ = fs.Parse(args)
	if fs.NArg() > 1 || (!*dry && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "Usage: agent-deck session sleep [--dry-run] [--json] [id]")
		os.Exit(1)
	}
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer storage.Close()
	instances, err := storage.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	after := time.Hour
	if cfg, err := session.LoadUserConfig(); err == nil && cfg != nil {
		after, err = cfg.Lifecycle.SleepDuration()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	targets := instances
	if fs.NArg() == 1 {
		inst, message, _ := ResolveSession(fs.Arg(0), instances)
		if inst == nil {
			fmt.Fprintln(os.Stderr, message)
			os.Exit(1)
		}
		targets = []*session.Instance{inst}
	}
	type result struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Sleeping bool   `json:"sleeping"`
		Eligible bool   `json:"eligible"`
		Slept    bool   `json:"slept"`
		Reason   string `json:"reason,omitempty"`
	}
	results := make([]result, 0, len(targets))
	failed := false
	for _, inst := range targets {
		r := result{ID: inst.ID, Title: inst.Title, Sleeping: inst.IsAutoSleeping()}
		if r.Sleeping {
			r.Reason = "already sleeping"
			results = append(results, r)
			continue
		}
		inst.UpdateStatus()
		proof, checkErr := session.InspectAutoSleepCandidate(inst, instances)
		if checkErr == nil && time.Since(session.AutoSleepIdleSince(inst, proof)) < after {
			checkErr = fmt.Errorf("idle grace period %s has not elapsed", after)
		}
		if checkErr != nil {
			r.Reason = checkErr.Error()
			failed = true
			results = append(results, r)
			continue
		}
		r.Eligible = true
		if !*dry {
			checkErr = session.AutoSleepSession(storage, inst, func() error {
				fresh, err := storage.Load()
				if err != nil {
					return err
				}
				var currentInst *session.Instance
				for _, candidate := range fresh {
					if candidate.ID == inst.ID {
						currentInst = candidate
						break
					}
				}
				if currentInst == nil {
					return fmt.Errorf("session was removed before sleep")
				}
				currentInst.UpdateStatus()
				current, err := session.InspectAutoSleepCandidate(currentInst, fresh)
				if err != nil {
					return err
				}
				if current.Fingerprint != proof.Fingerprint || time.Since(session.AutoSleepIdleSince(currentInst, current)) < after {
					return fmt.Errorf("session changed before sleep")
				}
				return session.CheckAutoSleepStoredIdle(storage, inst, time.Now(), after)
			})
			if checkErr != nil {
				r.Reason = checkErr.Error()
				failed = true
			} else {
				r.Slept = true
			}
		}
		results = append(results, r)
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(results)
	} else {
		for _, r := range results {
			label := "skip"
			if r.Eligible {
				label = "eligible"
			}
			if r.Slept || r.Sleeping {
				label = "sleeping"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", r.ID, label, r.Title, r.Reason)
		}
	}
	if !*dry && failed {
		os.Exit(1)
	}
}
