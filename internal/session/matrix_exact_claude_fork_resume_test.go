package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: exercise the actual persisted fork-builder shape, not a
// hand-written approximation. No provider process or real user state is used.
func matrixCompletedClaudeForkFixture(t *testing.T, extraArgs ...string) (*Instance, *Instance, string) {
	t.Helper()
	parent, home, _ := matrixExactClaudeFixture(t)
	parent.ExtraArgs = append([]string(nil), extraArgs...)
	child, command, err := parent.CreateForkedInstanceWithOptions("old fork; $literal", "", &ClaudeOptions{SessionMode: "new", SkipPermissions: true, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if got := child.consumeForkStartCommand(); got != command || child.IsForkAwaitingStart {
		t.Fatal("fixture did not consume the original one-shot fork")
	}
	path := filepath.Join(home, ".claude", "projects", SlugifyClaudeProjectPath(child.ProjectPath), child.ClaudeSessionID+".jsonl")
	var history strings.Builder
	for _, role := range []string{"user", "assistant"} {
		row := map[string]any{"type": role, "sessionId": child.ClaudeSessionID, "cwd": child.ProjectPath, "isSidechain": false, "message": map[string]any{"role": role, "content": "own fork history"}}
		line, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		history.Write(line)
		history.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(history.String()), 0600); err != nil {
		t.Fatal(err)
	}
	child.Title = "renamed evaluation"
	return parent, child, path
}

func TestMatrixExactClaudeCompletedForkPreservesEqualModelArgument(t *testing.T) {
	_, child, _ := matrixCompletedClaudeForkFixture(t, "--model=haiku")
	plan, err := newExactClaudeResumePlan(child)
	if err != nil {
		t.Fatal(err)
	}
	command, err := plan.command(child)
	if err != nil || !strings.Contains(command, "--model=haiku") || strings.Contains(command, "--fork-session") {
		t.Fatalf("builder-generated equal argument was not safely resumed: %s %v", command, err)
	}
}

func TestMatrixExactClaudeForkLiteralArgumentGrammar(t *testing.T) {
	for _, command := range []string{
		`--name 'foo\';true`,
		`--name 'foo\\';true`,
		`--name 'unterminated`,
		"--name trailing\\",
		`--name "$(printf bad)"`,
		"--name valid\nprintf bad",
	} {
		if args, ok := exactClaudeForkLiteralArgs(command); ok {
			t.Fatalf("non-literal historical command accepted: %q -> %v", command, args)
		}
	}
	if _, ok := exactClaudeForkLiteralArgs(`--name 'literal\$value; name' --model=sonnet`); !ok {
		t.Fatal("quoted literal metacharacters were rejected")
	}
}

func TestMatrixExactClaudeCompletedForkResumesOwnConversation(t *testing.T) {
	parent, child, _ := matrixCompletedClaudeForkFixture(t)
	originalID, originalCommand := child.ClaudeSessionID, child.Command
	plan, err := newExactClaudeResumePlan(child)
	if err != nil {
		t.Fatal(err)
	}
	command, err := plan.command(child)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "--resume "+originalID) || !strings.Contains(command, "--model sonnet") || !strings.Contains(command, "--name 'renamed evaluation'") {
		t.Fatalf("original child launch settings missing: %s", command)
	}
	for _, forbidden := range []string{parent.ClaudeSessionID, "--session-id", "--fork-session", "old fork; $literal", filepath.Join("identity", parent.ID, "identity.md")} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("historical fork input %q leaked into resume: %s", forbidden, command)
		}
	}
	if child.ClaudeSessionID != originalID || child.Command != originalCommand {
		t.Fatal("strict fork recovery changed its durable identity or historical command")
	}
}

func TestMatrixExactClaudeCompletedForkRejectsUnprovenScripts(t *testing.T) {
	for _, kind := range []string{"wrong-child", "invalid-parent", "self-parent", "duplicate-selector", "duplicate-flag", "shell-suffix", "shell-pipe", "expansion", "prompt", "custom-executable", "shell-wrapper", "foreign-cwd", "foreign-instance", "unterminated", "awaiting-first-start", "missing-own-history", "foreign-history"} {
		t.Run(kind, func(t *testing.T) {
			parent, child, path := matrixCompletedClaudeForkFixture(t)
			originalID := child.ClaudeSessionID
			switch kind {
			case "wrong-child":
				child.Command = strings.Replace(child.Command, "--session-id \""+child.ClaudeSessionID+"\"", "--session-id \"aaaaaaaa-2222-4333-8444-555555555555\"", 1)
			case "invalid-parent":
				child.Command = strings.Replace(child.Command, "--resume "+parent.ClaudeSessionID, "--resume invalid", 1)
			case "self-parent":
				child.Command = strings.Replace(child.Command, "--resume "+parent.ClaudeSessionID, "--resume "+child.ClaudeSessionID, 1)
			case "duplicate-selector":
				child.Command += " --resume " + parent.ClaudeSessionID
			case "duplicate-flag":
				child.Command += " --model other"
			case "shell-suffix":
				child.Command += "; printf unwanted"
			case "shell-pipe":
				child.Command += " | cat"
			case "expansion":
				child.Command += " --add-dir \"$(printf unwanted)\""
			case "prompt":
				child.Command += " continue working"
			case "custom-executable":
				child.Command = strings.Replace(child.Command, "exec claude ", "exec custom-claude ", 1)
			case "shell-wrapper":
				child.Command = "bash -c " + child.Command
			case "foreign-cwd":
				child.Command = strings.Replace(child.Command, "cd '"+child.ProjectPath+"'", "cd '/foreign'", 1)
			case "foreign-instance":
				child.Command = strings.Replace(child.Command, "AGENTDECK_INSTANCE_ID="+child.ID, "AGENTDECK_INSTANCE_ID=foreign", 1)
			case "unterminated":
				child.Command += " --add-dir 'unterminated"
			case "awaiting-first-start":
				child.IsForkAwaitingStart = true
			case "missing-own-history":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "foreign-history":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), child.ClaudeSessionID, parent.ClaudeSessionID)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := newExactClaudeResumePlan(child); err == nil {
				t.Fatalf("unproven historical fork %s accepted", kind)
			}
			if child.ClaudeSessionID != originalID {
				t.Fatal("refusal replaced the child identity")
			}
		})
	}
}

func TestMatrixExactClaudeCompletedForkCommandChangeInvalidatesPlan(t *testing.T) {
	parent, child, _ := matrixCompletedClaudeForkFixture(t)
	plan, err := newExactClaudeResumePlan(child)
	if err != nil {
		t.Fatal(err)
	}
	// A syntactically valid replacement still changes the frozen historical
	// binding, even though neither parent's ID would reach the resume command.
	child.Command = strings.Replace(child.Command, "--resume "+parent.ClaudeSessionID, "--resume aaaaaaaa-2222-4333-8444-555555555555", 1)
	if command, err := plan.command(child); err == nil || command != "" {
		t.Fatalf("changed historical fork command accepted: %s %v", command, err)
	}
}
