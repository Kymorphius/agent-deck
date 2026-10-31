package aiupdate

// matrix/hardened: the desktop app materializes its bundled Computer Use plugin
// into CODEX_HOME. A Codex process that was already running (the shared daemon
// or an in-process TUI) keeps the MCP servers it loaded at start and never
// picks up a plugin installed after it. This file only reads local files so
// callers can tell such a process apart; it never contacts the network.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	bundledMarketplace = "openai-bundled"
	computerUsePlugin  = "unified-computer-use"
	// ComputerUseServer is the MCP server the Computer Use plugin declares.
	ComputerUseServer = "cua_repl"
)

// ComputerUsePluginPathMarker appears in the arguments of every process the
// plugin launches from its materialized cache, followed by the plugin version.
const ComputerUsePluginPathMarker = "/" + bundledMarketplace + "/" + computerUsePlugin + "/"

var safePluginVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// ComputerUseBundle is what the desktop app last materialized into CODEX_HOME.
type ComputerUseBundle struct {
	// Present: the desktop bundle lists the plugin and its MCP server exists.
	Present bool `json:"present"`
	// Enabled: config.toml enables the plugin and its server is not disabled.
	Enabled        bool   `json:"enabled"`
	AppVersion     string `json:"app_version,omitempty"`
	PluginVersion  string `json:"plugin_version,omitempty"`
	MaterializedAt int64  `json:"materialized_at_unix,omitempty"`
	// Command and Args are the exact cua_repl launch from the plugin manifest.
	Command string   `json:"-"`
	Args    []string `json:"-"`
}

// Active reports whether Codex processes in this home are expected to run Computer Use.
func (b ComputerUseBundle) Active() bool { return b.Present && b.Enabled && b.PluginVersion != "" }

// Key identifies one materialization; it changes when the desktop app installs again.
func (b ComputerUseBundle) Key() string {
	return fmt.Sprintf("%t/%t/%s/%s/%d", b.Present, b.Enabled, b.AppVersion, b.PluginVersion, b.MaterializedAt)
}

// ReadComputerUseBundle reads the desktop bundle in home. A home without a
// desktop bundle returns the zero value and no error.
func ReadComputerUseBundle(home string) (ComputerUseBundle, error) {
	var bundle ComputerUseBundle
	keyPath := filepath.Join(home, ".tmp", "bundled-marketplaces", bundledMarketplace, ".materialization-key")
	data, err := readRegularFile(keyPath, 1<<20)
	if os.IsNotExist(err) {
		return bundle, nil
	}
	if err != nil {
		return bundle, err
	}
	var key struct {
		AppVersion      string `json:"appVersion"`
		MarketplaceName string `json:"marketplaceName"`
		Plugins         []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		return bundle, fmt.Errorf("desktop plugin bundle record is malformed: %w", err)
	}
	if key.MarketplaceName != "" && key.MarketplaceName != bundledMarketplace {
		return bundle, nil
	}
	info, err := os.Lstat(keyPath)
	if err != nil {
		return bundle, err
	}
	bundle.AppVersion = key.AppVersion
	bundle.MaterializedAt = info.ModTime().Unix()
	for _, plugin := range key.Plugins {
		if plugin.Name == computerUsePlugin {
			bundle.PluginVersion = plugin.Version
		}
	}
	if bundle.PluginVersion == "" {
		return bundle, nil
	}
	// The version becomes a path component; never let it escape the cache.
	if !safePluginVersion.MatchString(bundle.PluginVersion) {
		return ComputerUseBundle{}, errors.New("desktop Computer Use plugin version is not a plain version string")
	}
	manifest := filepath.Join(home, "plugins", "cache", bundledMarketplace, computerUsePlugin, bundle.PluginVersion, ".mcp.json")
	data, err = readRegularFile(manifest, 1<<20)
	if os.IsNotExist(err) {
		return bundle, nil
	}
	if err != nil {
		return bundle, err
	}
	var servers struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Enabled *bool    `json:"enabled"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &servers); err != nil {
		return bundle, fmt.Errorf("Computer Use plugin manifest is malformed: %w", err)
	}
	server, ok := servers.MCPServers[ComputerUseServer]
	if !ok || server.Command == "" {
		return bundle, nil
	}
	bundle.Present = true
	bundle.Command, bundle.Args = server.Command, slices.Clone(server.Args)
	enabled, err := computerUseConfigEnabled(home)
	if err != nil {
		return bundle, err
	}
	bundle.Enabled = enabled && (server.Enabled == nil || *server.Enabled)
	return bundle, nil
}

// computerUseConfigEnabled mirrors Codex: a bundled plugin runs only once the
// user enabled it in config.toml.
func computerUseConfigEnabled(home string) (bool, error) {
	data, err := readRegularFile(filepath.Join(home, "config.toml"), 8<<20)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Plugins map[string]struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"plugins"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return false, fmt.Errorf("Codex config.toml is unreadable: %w", err)
	}
	entry, ok := cfg.Plugins[computerUsePlugin+"@"+bundledMarketplace]
	return ok && entry.Enabled != nil && *entry.Enabled, nil
}

// ComputerUsePluginVersion returns the plugin version a process was launched
// from, judged by its argument vector. ok is false for unrelated processes.
func (b ComputerUseBundle) ComputerUsePluginVersion(argv []string) (string, bool) {
	for _, arg := range argv {
		_, rest, found := strings.Cut(arg, ComputerUsePluginPathMarker)
		if !found {
			continue
		}
		version, _, _ := strings.Cut(rest, "/")
		if safePluginVersion.MatchString(version) {
			return version, true
		}
	}
	// A future manifest may stop naming its cache path; the exact launch still identifies it.
	if b.Command != "" && len(argv) == len(b.Args)+1 && argv[0] == b.Command && slices.Equal(argv[1:], b.Args) {
		return b.PluginVersion, true
	}
	return "", false
}
