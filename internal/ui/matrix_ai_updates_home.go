package ui

// matrix/hardened: the settings action manages the local configured Codex home;
// it never follows the selected remote session or starts background polling.
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (h *Home) openAIUpdatesPanel() tea.Cmd {
	if h.aiUpdatesPanel == nil {
		h.aiUpdatesPanel = NewAIUpdatesPanel()
	}
	h.aiUpdatesPanel.SetSize(h.width, h.height)
	// Keep ownership of an in-flight installation even when settings is reopened.
	if h.aiUpdatesPanel.busy != "" {
		return h.aiUpdatesPanel.Open(nil, "")
	}
	cfg, err := session.LoadUserConfig()
	if err != nil {
		return h.aiUpdatesPanel.Open(nil, err.Error())
	}
	binary, home, err := aiUpdatesConfiguration(cfg, h.profile)
	if err != nil {
		return h.aiUpdatesPanel.Open(nil, err.Error())
	}
	scope := binary + "\x00" + home
	if h.aiUpdatesPanel.scope != scope {
		h.aiUpdatesPanel.status = aiupdate.Status{}
		h.aiUpdatesPanel.loaded = false
		h.aiUpdatesPanel.notice, h.aiUpdatesPanel.err = "", ""
		h.aiUpdatesPanel.scroll = 0
		h.aiUpdatesPanel.scope = scope
	}
	h.aiUpdatesPanel.SetComputerUse(homeComputerUse{profile: h.profile})
	return h.aiUpdatesPanel.Open(aiupdate.New(binary, home), "")
}

// homeComputerUse loads its own storage handle, like the lifecycle controller,
// so a refresh never mutates the list's in-memory sessions; Restart records
// each new terminal in the database and the list follows that change.
type homeComputerUse struct{ profile string }

func (c homeComputerUse) load() (*session.Storage, []*session.Instance, error) {
	storage, err := session.NewStorageWithProfile(c.profile)
	if err != nil {
		return nil, nil, err
	}
	instances, err := storage.Load()
	if err != nil {
		_ = storage.Close()
		return nil, nil, err
	}
	return storage, instances, nil
}

func (c homeComputerUse) Inspect() (session.CodexComputerUseReport, error) {
	storage, instances, err := c.load()
	if err != nil {
		return session.CodexComputerUseReport{}, err
	}
	defer storage.Close()
	return session.InspectCodexComputerUse(instances), nil
}

func (c homeComputerUse) Refresh(ctx context.Context) ([]session.CodexComputerUseRefreshResult, error) {
	storage, instances, err := c.load()
	if err != nil {
		return nil, err
	}
	defer storage.Close()
	return session.RefreshCodexComputerUse(ctx, instances, session.InspectCodexComputerUse(instances), session.CodexComputerUseRefreshOptions{}), nil
}

func aiUpdatesConfiguration(cfg *session.UserConfig, profile string) (string, string, error) {
	binary, home := "codex", ""
	if cfg != nil {
		if command := strings.TrimSpace(cfg.Codex.Command); command != "" {
			binary = command
		}
		home = cfg.GetProfileCodexConfigDir(profile)
		if home == "" && cfg.Codex.ConfigDir != "" {
			home = session.ExpandPath(cfg.Codex.ConfigDir)
		}
	}
	if ambient := strings.TrimSpace(os.Getenv("CODEX_HOME")); ambient != "" {
		home = session.ExpandPath(ambient)
	}
	// Accept a literal executable/path only; never reinterpret a shell wrapper,
	// embedded environment assignment, or flags as a different Codex target.
	if strings.ContainsAny(binary, "'\"`$;&|<>()\n\r\t=") || (!filepath.IsAbs(binary) && strings.ContainsFunc(binary, unicode.IsSpace)) {
		return "", home, fmt.Errorf("AI updates require [codex].command to be one executable path or name; shell wrappers, arguments, and environment assignments are not supported")
	}
	binary = session.ExpandPath(binary)
	return binary, home, nil
}

func (h *Home) handleAIUpdatesMsg(msg tea.Msg) (tea.Cmd, bool) {
	result, ok := msg.(aiUpdatesResult)
	if !ok {
		return nil, false
	}
	if h.aiUpdatesPanel != nil {
		h.aiUpdatesPanel.accept(result)
	}
	return nil, true
}
