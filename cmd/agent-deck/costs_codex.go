package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: share account-aware, exact artifact resolution between
// historical sync and live collection. Unmanaged and SSH rollouts stay out.
func costSyncSessions(instances []*session.Instance) ([]costs.SyncSession, []error) {
	var out []costs.SyncSession
	var errs []error
	for _, inst := range instances {
		if inst.Tool == "claude" && inst.ClaudeSessionID != "" {
			out = append(out, costs.SyncSession{InstanceID: inst.ID, ClaudeSessionID: inst.ClaudeSessionID, ProjectPath: inst.ProjectPath, Tool: inst.Tool})
		} else if session.IsCodexCompatible(inst.Tool) && inst.CodexSessionID != "" && inst.ResolvedCodexHome() != "" {
			path, err := inst.CodexCostRolloutPath()
			if err != nil {
				errs = append(errs, fmt.Errorf("Codex cost source %s: %w", inst.ID, err))
				continue
			}
			out = append(out, costs.SyncSession{InstanceID: inst.ID, Tool: inst.Tool, CodexPath: path, CodexSessionID: inst.CodexSessionID})
		}
	}
	return out, errs
}

// matrix/hardened: tail native usage every 30s in both TUI and headless web.
// Each read consumes only appended complete records after its first pass.
// Prices reload from config, and response IDs dedupe concurrent deck instances.
func startCodexCostCollection(profile string, store *costs.Store) func() {
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		slog.Warn("Codex costs unavailable", "error", err)
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		tails := map[string]*reader.CodexUsageTail{}
		for {
			instances, err := storage.Load()
			if err == nil {
				sources, errs := costSyncSessions(instances)
				for _, err := range errs {
					slog.Warn("Codex cost source", "error", err)
				}
				importer := costs.NewUsageImporter(store, newPricerFromConfig())
				for _, src := range sources {
					if ctx.Err() != nil {
						return
					}
					if src.CodexPath == "" {
						continue
					}
					tail := tails[src.CodexPath]
					if tail == nil {
						tail = &reader.CodexUsageTail{}
						tails[src.CodexPath] = tail
					}
					events, err := tail.Read(ctx, src.CodexPath, src.CodexSessionID)
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						slog.Warn("Codex cost read", "error", err)
						delete(tails, src.CodexPath)
						continue
					}
					_, _, errs := importer.Import(src.InstanceID, events)
					if len(errs) > 0 {
						slog.Warn("Codex cost import", "errors", errs)
						delete(tails, src.CodexPath)
					}
				}
			} else {
				slog.Warn("Codex cost sessions", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done; storage.Close() }
}
