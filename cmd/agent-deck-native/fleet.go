// matrix/hardened: native registry/runtime integration stays separate from tmux.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativefleet"
	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/nativeui"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

func runFleet(config options, stdout, stderr io.Writer) error {
	if config.Command == "serve" {
		return runFleetServer(config, stdout)
	}
	if config.Command == "ui" {
		return nativeui.Run(&nativeService{pipe: config.Pipe}, config.Cwd)
	}
	request := terminalhost.Request{SessionID: config.SessionID, RunID: config.RunID, Name: config.Name, Tool: config.Tool}
	switch config.Command {
	case "list":
		request.Op = "fleet-list"
	case "create":
		launch, err := launchSpec(config.Tool, config.Cwd, config.Executable, config.Args, config.Env, config.Cols, config.Rows)
		if err != nil {
			return err
		}
		request.Op, request.Launch, request.AutoStart = "fleet-create", &launch, !config.NoStart
	case "start", "rename", "remove":
		request.Op = "fleet-" + config.Command
		if request.RunID == "" {
			info, err := lookupSession(context.Background(), config.Pipe, config.SessionID)
			if err != nil {
				return err
			}
			request.RunID = info.RunID
		}
	case "shutdown":
		request.Op = "fleet-shutdown"
	default:
		return errors.New("unsupported managed command")
	}
	response, err := callHost(context.Background(), config.Pipe, request)
	if err != nil {
		// matrix/hardened: a saved descriptor survives failed auto-start; expose
		// its identity so a retry can start it instead of creating a duplicate.
		if config.Command == "create" && len(response.Sessions) == 1 {
			if config.JSON {
				if writeErr := json.NewEncoder(stdout).Encode(response); writeErr != nil {
					return errors.Join(err, writeErr)
				}
			} else {
				info := response.Sessions[0]
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", info.ID, info.Name, info.Tool, info.State, info.Dir)
			}
			return fmt.Errorf("会话已创建，启动失败；可用 start --session %s 重试：%w", response.Sessions[0].ID, err)
		}
		return err
	}
	if config.JSON {
		return json.NewEncoder(stdout).Encode(response)
	}
	if len(response.Sessions) > 0 {
		for _, info := range response.Sessions {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", info.ID, info.Name, info.Tool, info.State, info.Dir)
		}
		return nil
	}
	_, err = fmt.Fprintln(stdout, config.Command+": completed")
	return err
}

func runFleetServer(config options, stdout io.Writer) (resultErr error) {
	listener, err := winipc.Listen(config.Pipe)
	if err != nil {
		return err
	}
	defer listener.Close()
	store, err := nativeregistry.Open(config.StateDir)
	if err != nil {
		return err
	}
	server, err := nativefleet.New(store, func(launch terminalhost.LaunchSpec) (terminalhost.Process, error) {
		process, err := conpty.Start(conpty.Options{Executable: launch.Executable, Args: launch.Args, Env: mergedEnv(os.Environ(), launch.Env), Dir: launch.Dir, Cols: launch.Cols, Rows: launch.Rows})
		if err != nil {
			return nil, err
		}
		return process, nil
	})
	if err != nil {
		_ = store.Close()
		return err
	}
	// matrix/hardened: only the manager may release its consumed store;
	// uncertain process cleanup keeps registry ownership fenced until exit.
	defer func() { resultErr = errors.Join(resultErr, server.Close()) }()
	if err := json.NewEncoder(stdout).Encode(struct {
		Ready   bool   `json:"ready"`
		Pipe    string `json:"pipe"`
		HostPID int    `json:"host_pid"`
	}{true, config.Pipe, os.Getpid()}); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	return server.Serve(listener)
}

func launchSpec(tool, cwd, executable string, args, env []string, cols, rows int) (terminalhost.LaunchSpec, error) {
	if executable == "" {
		switch tool {
		case "shell":
			if runtime.GOOS == "windows" {
				executable = os.Getenv("COMSPEC")
				if executable == "" {
					executable = "cmd.exe"
				}
				args = []string{"/d", "/q"}
			} else {
				executable = "/bin/sh"
			}
		case "codex":
			if path, err := exec.LookPath("codex.exe"); err == nil {
				executable = path
			} else if runtime.GOOS == "windows" {
				candidates := []string{filepath.Join(os.Getenv("APPDATA"), "npm", "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")}
				for _, candidate := range candidates {
					if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
						executable = candidate
						break
					}
				}
			} else {
				executable = "codex"
			}
			if executable == "" {
				return terminalhost.LaunchSpec{}, errors.New("找不到 Codex 原生程序；请安装 Codex 或通过 create 指定程序路径")
			}
		default:
			return terminalhost.LaunchSpec{}, errors.New("custom tool requires an explicit executable")
		}
	}
	launch := terminalhost.LaunchSpec{Executable: executable, Args: args, Env: env, Dir: cwd, Cols: cols, Rows: rows}
	return launch, terminalhost.ValidateLaunchSpec(launch)
}

func lookupSession(ctx context.Context, pipe, id string) (terminalhost.SessionInfo, error) {
	response, err := callHost(ctx, pipe, terminalhost.Request{Op: "fleet-list"})
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	for _, info := range response.Sessions {
		if info.ID == id {
			return info, nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("找不到所选会话")
}

type nativeService struct{ pipe string }

func (s *nativeService) List(ctx context.Context) ([]terminalhost.SessionInfo, error) {
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-list"})
	return r.Sessions, err
}
func (s *nativeService) Create(ctx context.Context, name, tool, cwd string) (terminalhost.SessionInfo, error) {
	launch, err := launchSpec(tool, cwd, "", nil, nil, 120, 30)
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-create", Name: name, Tool: tool, Launch: &launch, AutoStart: true})
	if err != nil {
		if len(r.Sessions) == 1 {
			return r.Sessions[0], err
		}
		return terminalhost.SessionInfo{}, err
	}
	return oneSession(r)
}
func (s *nativeService) Start(ctx context.Context, id string) (terminalhost.SessionInfo, error) {
	info, err := lookupSession(ctx, s.pipe, id)
	if err != nil {
		return info, err
	}
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-start", SessionID: id, RunID: info.RunID})
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	return oneSession(r)
}
func (s *nativeService) Stop(ctx context.Context, id, runID string) error {
	_, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-stop", SessionID: id, RunID: runID})
	return err
}
func (s *nativeService) Rename(ctx context.Context, id, name string) error {
	info, err := lookupSession(ctx, s.pipe, id)
	if err != nil {
		return err
	}
	_, err = callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-rename", SessionID: id, RunID: info.RunID, Name: name})
	return err
}
func (s *nativeService) Remove(ctx context.Context, id, runID string) error {
	_, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-remove", SessionID: id, RunID: runID})
	return err
}
func (s *nativeService) AttachCommand(info terminalhost.SessionInfo) (*exec.Cmd, error) {
	if info.ID == "" || info.RunID == "" {
		return nil, errors.New("会话尚未启动")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, "attach", "--pipe", s.pipe, "--session", info.ID, "--run-id", info.RunID), nil
}
func oneSession(response terminalhost.Response) (terminalhost.SessionInfo, error) {
	if len(response.Sessions) != 1 {
		return terminalhost.SessionInfo{}, errors.New("管理服务返回了不完整的会话身份")
	}
	return response.Sessions[0], nil
}
