package session

import (
	"strings"
	"testing"
)

// matrix/hardened: `launch --env-file` is sourced after every configured
// source (so it wins), tolerates a file its owner already removed, and is
// absent when unset.
func TestBuildEnvSourceCommand_EnvFileSourcedLast(t *testing.T) {
	userConfigCacheMu.Lock()
	orig := userConfigCache
	userConfigCache = &UserConfig{MCPs: make(map[string]MCPDef), Tools: map[string]ToolDef{"claude": {Env: map[string]string{"A": "1"}}}}
	userConfigCacheMu.Unlock()
	defer func() { userConfigCacheMu.Lock(); userConfigCache = orig; userConfigCacheMu.Unlock() }()

	inst := &Instance{Tool: "claude", ProjectPath: "/tmp", EnvFile: "/tmp/task/agent.env"}
	got := inst.buildEnvSourceCommand()
	src := `[ -f "/tmp/task/agent.env" ] && source "/tmp/task/agent.env"`
	if !strings.Contains(got, src) {
		t.Fatalf("env file not sourced: %q", got)
	}
	if inline := strings.Index(got, "export A="); inline >= 0 && inline > strings.Index(got, src) {
		t.Fatalf("env file must come after tool env: %q", got)
	}
	inst.EnvFile = ""
	if strings.Contains(inst.buildEnvSourceCommand(), "agent.env") {
		t.Fatal("no env file, no source")
	}
}
