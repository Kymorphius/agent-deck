package aiupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// matrix/hardened: fixtures mirror the files the Codex app writes into CODEX_HOME.
func writeComputerUseHome(t *testing.T, version, config string, serverEnabled bool) string {
	t.Helper()
	home := t.TempDir()
	keyDir := filepath.Join(home, ".tmp", "bundled-marketplaces", "openai-bundled")
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := `{"version":1,"appVersion":"26.1027.1","marketplaceName":"openai-bundled","plugins":[{"name":"browser","version":"26.1027.1"},{"name":"unified-computer-use","version":"` + version + `"}]}`
	if err := os.WriteFile(filepath.Join(keyDir, ".materialization-key"), []byte(key), 0o644); err != nil {
		t.Fatal(err)
	}
	if safePluginVersion.MatchString(version) {
		pluginDir := filepath.Join(home, "plugins", "cache", "openai-bundled", "unified-computer-use", version)
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		enabled := "true"
		if !serverEnabled {
			enabled = "false"
		}
		manifest := `{"mcpServers":{"cua_repl":{"command":"/opt/cua/node","args":["--import","file://` + pluginDir + `/require-browser-access.mjs","/opt/cua/cua-repl.mjs"],"enabled":` + enabled + `}}}`
		if err := os.WriteFile(filepath.Join(pluginDir, ".mcp.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

const computerUseEnabledConfig = "model = \"x\"\n\n[plugins.\"unified-computer-use@openai-bundled\"]\nenabled = true\n"

func TestReadComputerUseBundleEnabled(t *testing.T) {
	home := writeComputerUseHome(t, "26.1027.1", computerUseEnabledConfig, true)
	bundle, err := ReadComputerUseBundle(home)
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Active() || bundle.PluginVersion != "26.1027.1" || bundle.AppVersion != "26.1027.1" || bundle.MaterializedAt == 0 {
		t.Fatalf("bundle = %+v", bundle)
	}
	if bundle.Command != "/opt/cua/node" || len(bundle.Args) != 3 {
		t.Fatalf("launch = %q %q", bundle.Command, bundle.Args)
	}
}

func TestReadComputerUseBundleInactive(t *testing.T) {
	for name, home := range map[string]string{
		"no desktop bundle":   t.TempDir(),
		"not enabled":         writeComputerUseHome(t, "26.1027.1", "model = \"x\"\n", true),
		"no config":           writeComputerUseHome(t, "26.1027.1", "", true),
		"server disabled":     writeComputerUseHome(t, "26.1027.1", computerUseEnabledConfig, false),
		"plugin not in cache": writeComputerUseHome(t, "26.1027.1", computerUseEnabledConfig, true),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "plugin not in cache" {
				if err := os.RemoveAll(filepath.Join(home, "plugins")); err != nil {
					t.Fatal(err)
				}
			}
			bundle, err := ReadComputerUseBundle(home)
			if err != nil {
				t.Fatal(err)
			}
			if bundle.Active() {
				t.Fatalf("bundle unexpectedly active: %+v", bundle)
			}
		})
	}
}

func TestReadComputerUseBundleRejectsPathVersion(t *testing.T) {
	home := writeComputerUseHome(t, "../../escape", computerUseEnabledConfig, true)
	if _, err := ReadComputerUseBundle(home); err == nil {
		t.Fatal("a version that is not a plain path component must be refused")
	}
}

func TestComputerUsePluginVersionFromArgv(t *testing.T) {
	bundle := ComputerUseBundle{PluginVersion: "26.1027.1", Command: "/opt/cua/node", Args: []string{"/opt/cua/cua-repl.mjs"}}
	cases := []struct {
		argv    []string
		version string
		ok      bool
	}{
		{[]string{"/opt/cua/node", "--import", "file:///h/.codex/plugins/cache/openai-bundled/unified-computer-use/26.0901.7/require-browser-access.mjs", "x.mjs"}, "26.0901.7", true},
		{[]string{"/opt/cua/node", "/opt/cua/cua-repl.mjs"}, "26.1027.1", true},
		{[]string{"/opt/cua/node", "./scripts/start-mcp.mjs"}, "", false},
		{[]string{"node", "/h/.codex/plugins/cache/openai-bundled/unified-computer-use/../x"}, "", false},
	}
	for _, tc := range cases {
		version, ok := bundle.ComputerUsePluginVersion(tc.argv)
		if version != tc.version || ok != tc.ok {
			t.Errorf("%q: got %q %v, want %q %v", tc.argv, version, ok, tc.version, tc.ok)
		}
	}
}

func TestParseCLIVersionAcceptsPrerelease(t *testing.T) {
	for output, want := range map[string]string{"codex-cli 0.166.0\n": "0.166.0", "codex-cli 0.166.0-alpha.5.1\n": "0.166.0-alpha.5.1"} {
		got, err := parseCLIVersion([]byte(output))
		if err != nil || got != want {
			t.Errorf("parseCLIVersion(%q) = %q, %v", output, got, err)
		}
	}
	for _, output := range []string{"codex 0.166.0", "codex-cli 0.166", "codex-cli 0.166.0 extra"} {
		if _, err := parseCLIVersion([]byte(output)); err == nil {
			t.Errorf("parseCLIVersion(%q) accepted", output)
		}
	}
	// Package and daemon comparison stay on stable versions only.
	if _, err := parseVersion([]byte("codex-cli 0.166.0-alpha.5.1")); err == nil {
		t.Error("parseVersion must keep rejecting prerelease versions")
	}
}
