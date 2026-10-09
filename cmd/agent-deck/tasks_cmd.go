package main

// matrix/hardened: `agent-deck tasks` — the World Store task board from the
// CLI. World Store (personal-panel) owns the tasks; this only reads them and
// sends the user's explicit edits through the local task bridge. See
// internal/worldstore.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

func handleTasks(profile string, args []string) {
	if len(args) == 0 {
		handleTasksList(args)
		return
	}
	switch args[0] {
	case "list", "ls":
		handleTasksList(args[1:])
	case "show":
		handleTasksShow(args[1:])
	case "move":
		handleTasksMove(args[1:])
	case "done":
		handleTasksDone(args[1:])
	case "link", "unlink":
		handleTasksLink(profile, args[0] == "link", args[1:])
	case "launch":
		handleTasksLaunch(profile, args[1:])
	case "init-links":
		handleTasksInitLinks(args[1:])
	case "help", "--help", "-h":
		printTasksHelp()
	default:
		if strings.HasPrefix(args[0], "-") {
			handleTasksList(args)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: unknown tasks command: %s\n", args[0])
		printTasksHelp()
		os.Exit(1)
	}
}

func printTasksHelp() {
	fmt.Println("Usage: agent-deck tasks <command> [options]")
	fmt.Println()
	fmt.Println("World Store task board. Tasks live only in World Store (personal-panel);")
	fmt.Println("agent-deck reads them through the local task bridge and writes only what")
	fmt.Println("you ask for. Configure with [worldstore] bridge = \"/abs/path/to/")
	fmt.Println("personal-panel/scripts/multica-task-bridge.cjs\" in config.toml.")
	fmt.Println("In the TUI the board opens with alt+t.")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  list [--all] [--filter s]   Board by column (default command)")
	fmt.Println("  show <task>                 One task with plan, result and linked sessions")
	fmt.Println("  move <task> <status>        Set status: 待办, 进行中, 暂停, 已取消")
	fmt.Println("  done <task> [--result s]    Mark complete (records completion time)")
	fmt.Println("  link <task> [session]       Link a session (default: current session)")
	fmt.Println("  unlink <task> [session]     Remove a session link")
	fmt.Println("  launch <task> [path]        Start a session on the task and link it")
	fmt.Println("  init-links --yes            Create the agent-deck link field in World Store")
	fmt.Println()
	fmt.Println("<task> is a task ID, a unique ID prefix (6+ chars), or an exact task name.")
}

func worldStoreClient() (*worldstore.Client, session.WorldStoreSettings, error) {
	cfg, err := session.LoadUserConfig()
	if err != nil {
		return nil, session.WorldStoreSettings{}, err
	}
	ws := cfg.WorldStore
	client, err := worldstore.New(worldstore.Config{Bridge: ws.Bridge, Node: ws.Node})
	return client, ws, err
}

// tasksSetup parses fs, builds the client and reads the board.
func tasksSetup(fs *flag.FlagSet, args []string, jsonOut *bool) (*CLIOutput, *worldstore.Client, session.WorldStoreSettings, *worldstore.Board) {
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	out := NewCLIOutput(*jsonOut, false)
	client, ws, err := worldStoreClient()
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	board, err := client.Board(context.Background())
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	return out, client, ws, board
}

func tasksErrorCode(err error) string {
	if code := worldstore.CodeOf(err); code != "" {
		return code
	}
	return ErrCodeInvalidOperation
}

func tasksErrorText(err error) string {
	switch worldstore.CodeOf(err) {
	case worldstore.CodeWriteUncertain:
		return err.Error() + "\nThe write may have been applied. Run `agent-deck tasks show <task>` to check; do not repeat it blindly."
	case worldstore.CodeConflict:
		return err.Error() + "\nThe task changed since it was read; review it and try again."
	case worldstore.CodeUnavailable:
		return err.Error() + "\nIs World Store running? (one window open, socket at ~/Library/Application Support/Personal Panel/maintenance/api.sock)"
	}
	return err.Error()
}

func resolveTask(board *worldstore.Board, ref string) (*worldstore.Task, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("task is required")
	}
	if t := board.Find(ref); t != nil {
		return t, nil
	}
	var matches []*worldstore.Task
	for i := range board.Tasks {
		t := &board.Tasks[i]
		if t.Name == ref || (len(ref) >= 6 && strings.HasPrefix(t.ID, ref)) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no task matches %q", ref)
	case 1:
		return matches[0], nil
	}
	var ids []string
	for _, m := range matches {
		ids = append(ids, m.ID+"  "+m.Name)
	}
	return nil, fmt.Errorf("%q matches %d tasks; use the ID:\n  %s", ref, len(matches), strings.Join(ids, "\n  "))
}

func formatPlanDate(ms *int64) string {
	if ms == nil {
		return ""
	}
	return time.UnixMilli(*ms).Local().Format("2006-01-02")
}

func handleTasksList(args []string) {
	fs := flag.NewFlagSet("tasks list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	all := fs.Bool("all", false, "Include the done column")
	filter := fs.String("filter", "", "Only tasks whose name, category or status contains this text")
	out, _, _, board := tasksSetup(fs, args, jsonOut)

	cols := worldstore.Columns(board.Tasks, *filter)
	if *jsonOut {
		out.Success("", map[string]any{"success": true, "owner": board.Owner, "updatedAt": board.UpdatedAt, "columns": cols})
		return
	}
	var sb strings.Builder
	for _, c := range cols {
		if c.Lane == "done" && !*all {
			fmt.Fprintf(&sb, "%s (%d, --all to show)\n\n", c.Title, len(c.Tasks)+c.Hidden)
			continue
		}
		fmt.Fprintf(&sb, "%s (%d)\n", c.Title, len(c.Tasks)+c.Hidden)
		for _, t := range c.Tasks {
			line := "  " + t.ID[:min(16, len(t.ID))] + "  " + t.Name
			var tags []string
			if c.Lane == "review" {
				tags = append(tags, t.Status)
			}
			if t.Category != "" {
				tags = append(tags, t.Category)
			}
			if d := formatPlanDate(t.PlanDate); d != "" {
				tags = append(tags, "计划 "+d)
			}
			if n := len(t.AgentDeckLinks); n > 0 {
				tags = append(tags, fmt.Sprintf("%d session(s)", n))
			}
			if len(tags) > 0 {
				line += "  [" + strings.Join(tags, " · ") + "]"
			}
			sb.WriteString(line + "\n")
		}
		if c.Hidden > 0 {
			fmt.Fprintf(&sb, "  … %d older\n", c.Hidden)
		}
		sb.WriteString("\n")
	}
	fmt.Print(sb.String())
}

func handleTasksShow(args []string) {
	fs := flag.NewFlagSet("tasks show", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	out, client, _, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	full, err := client.Get(context.Background(), board.Owner, t.ID)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	// task.get omits links; the list read already has them.
	full.AgentDeckLinks, full.LinksUnknown = t.AgentDeckLinks, t.LinksUnknown
	if *jsonOut {
		out.Success("", map[string]any{"success": true, "owner": board.Owner, "task": full})
		return
	}
	fmt.Printf("%s\n  id: %s\n  status: %s\n", full.Name, full.ID, full.Status)
	if full.Category != "" {
		fmt.Printf("  category: %s\n", full.Category)
	}
	if d := formatPlanDate(full.PlanDate); d != "" {
		fmt.Printf("  plan date: %s\n", d)
	}
	if full.ReadOnly {
		fmt.Println("  read-only (imported record)")
	}
	if full.PlanText != "" {
		fmt.Printf("  plan:\n    %s\n", strings.ReplaceAll(full.PlanText, "\n", "\n    "))
	}
	if full.ResultText != "" {
		fmt.Printf("  result:\n    %s\n", strings.ReplaceAll(full.ResultText, "\n", "\n    "))
	}
	switch {
	case full.LinksUnknown:
		fmt.Println("  sessions: stored links are in an unknown format; check the task in World Store")
	case len(full.AgentDeckLinks) > 0:
		fmt.Println("  sessions:")
		for _, l := range full.AgentDeckLinks {
			fmt.Printf("    %s  (%s, linked %s)\n", l.SessionID, l.DeviceID, time.Unix(l.LinkedAt, 0).Local().Format("2006-01-02 15:04"))
		}
	}
}

func handleTasksMove(args []string) {
	fs := flag.NewFlagSet("tasks move", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	out, client, _, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	status := strings.TrimSpace(fs.Arg(1))
	if status == worldstore.StatusDone {
		out.Error("use `agent-deck tasks done` to complete a task", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	saved, err := client.SetStatus(context.Background(), board.Owner, *t, status)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	out.Success(fmt.Sprintf("%s → %s", saved.Name, saved.Status), map[string]any{"success": true, "task": saved})
}

func handleTasksDone(args []string) {
	fs := flag.NewFlagSet("tasks done", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	result := fs.String("result", "", "Completion note (optional; empty keeps the stored result)")
	out, client, _, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	saved, err := client.Complete(context.Background(), board.Owner, *t, *result)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	out.Success(fmt.Sprintf("%s → %s", saved.Name, saved.Status), map[string]any{"success": true, "task": saved})
}

func handleTasksLink(profile string, link bool, args []string) {
	name := "tasks unlink"
	if link {
		name = "tasks link"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	out, client, ws, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	_, instances, _, err := loadSessionData(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	device := worldstore.DeviceID(ws.DeviceID)
	sessionID := fs.Arg(1)
	if inst, msg, code := ResolveSessionOrCurrent(sessionID, instances); inst != nil {
		sessionID = inst.ID
	} else if link || sessionID == "" || !t.LinkedTo(device, sessionID) {
		// Unlink may name a session that no longer exists in this deck.
		out.Error(msg, code)
		os.Exit(1)
	}
	var links []worldstore.Link
	if link {
		links, err = client.Link(context.Background(), board.Owner, *t, device, sessionID)
	} else {
		links, err = client.Unlink(context.Background(), board.Owner, *t, device, sessionID)
	}
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	verb := "unlinked from"
	if link {
		verb = "linked to"
	}
	out.Success(fmt.Sprintf("session %s %s %s", sessionID, verb, t.Name), map[string]any{"success": true, "taskId": t.ID, "sessionId": sessionID, "links": links})
}

func handleTasksLaunch(profile string, args []string) {
	fs := flag.NewFlagSet("tasks launch", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	tool := fs.String("c", "claude", "Tool to run")
	group := fs.String("g", "", "Group path")
	title := fs.String("t", "", "Session title (default: task name)")
	out, client, ws, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	if t.ReadOnly || t.Lane == "done" {
		out.Error("task is read-only or already done", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	full, err := client.Get(context.Background(), board.Owner, t.ID)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	if *title == "" {
		*title = t.Name
	}
	sessionID, err := worldstore.Launch(context.Background(), worldstore.LaunchRequest{
		Exe: exe, Profile: profile, Path: fs.Arg(1), Group: *group, Tool: *tool,
		Title: *title, Prompt: worldstore.LaunchPrompt(*full), TaskID: t.ID,
	})
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	links, err := client.Link(context.Background(), board.Owner, *t, worldstore.DeviceID(ws.DeviceID), sessionID)
	if err != nil {
		out.Error(fmt.Sprintf("session %s started but linking failed: %s", sessionID, tasksErrorText(err)), tasksErrorCode(err))
		os.Exit(1)
	}
	out.Success(fmt.Sprintf("session %s started on %s and linked", sessionID, t.Name), map[string]any{"success": true, "taskId": t.ID, "sessionId": sessionID, "links": links})
}

func handleTasksInitLinks(args []string) {
	fs := flag.NewFlagSet("tasks init-links", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	yes := fs.Bool("yes", false, "Really create the field (changes the World Store schema)")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	out := NewCLIOutput(*jsonOut, false)
	client, _, err := worldStoreClient()
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	schema, err := client.InspectLinks(context.Background())
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	if schema.Ready {
		out.Success("agent-deck link field is ready", map[string]any{"success": true, "ready": true})
		return
	}
	if !*yes {
		out.Error("the agent-deck link field (任务｜agent-deck 会话关联) does not exist yet; creating it adds a relation to the 任务 type in World Store. Re-run with --yes to create it", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	board, err := client.Board(context.Background())
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	if err := client.InitializeLinks(context.Background(), board.Owner); err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	out.Success("agent-deck link field created", map[string]any{"success": true, "ready": true})
}
