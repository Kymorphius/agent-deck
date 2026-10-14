//go:build windows

package main

// matrix/hardened: explicitly opt-in online acceptance owns one private Codex
// process, StateDB, pipe and project. Credentials are supplied in a separately
// protected QA CODEX_HOME by the operator; this test never opens default auth.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

const (
	// A fresh QA home has no model catalog or startup caches. Let Codex finish
	// configuration before sending any prompt; ordinary UI/IPC checks stay 15s.
	nativeCodexStartupTimeout = 90 * time.Second
	nativeCodexModelTimeout   = 120 * time.Second
)

func nativeCodexQAHome(t *testing.T) string {
	t.Helper()
	path := os.Getenv("CODEX_HOME")
	const prefix = "agent-deck-native-codex-online-"
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), prefix) {
		t.Fatal("CODEX_HOME must explicitly name a protected QA clone: agent-deck-native-codex-online-<UUID>")
	}
	if _, err := uuid.Parse(strings.TrimPrefix(filepath.Base(path), prefix)); err != nil {
		t.Fatal("CODEX_HOME QA clone must have a UUID suffix")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.EqualFold(filepath.Clean(path), canonical) {
		t.Fatal("CODEX_HOME QA clone must be an existing canonical directory without links")
	}
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		t.Fatal("CODEX_HOME QA clone directory is unavailable")
	}
	// Only inspect metadata. No configuration, authentication or token values
	// enter test output. The operator owns creation and removal of this clone.
	for _, name := range []string{"config.toml", "auth.json"} {
		info, err := os.Lstat(filepath.Join(path, name))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("QA clone requires a regular %s supplied by the operator", name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(path, "sessions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot inspect QA rollout directory")
	}
	if len(entries) != 0 {
		t.Fatal("QA CODEX_HOME must have no pre-existing sessions; use a fresh config/auth-only clone")
	}
	return path
}

func nativeCodexStartUI(t *testing.T, cli string, config options, cwd string) *nativeFleetUI {
	t.Helper()
	process, err := conpty.Start(conpty.Options{
		Executable: cli,
		Args:       []string{"ui", "--tool", "codex", "--store", "statedb", "--pipe", config.Pipe, "--state-dir", config.StateDir, "--cwd", cwd},
		Env:        os.Environ(), Dir: cwd, Cols: 160, Rows: 45,
	})
	if err != nil {
		t.Fatalf("start shared-store native Codex UI: %v", err)
	}
	frame, err := terminalhost.New(process, 160, 45)
	if err != nil {
		_ = process.Close()
		t.Fatalf("decode native Codex UI: %v", err)
	}
	t.Cleanup(func() { nativeClose(t, "online Codex UI console", frame.Close) })
	return &nativeFleetUI{viewer: &nativeViewer{process: process}, frame: frame}
}

func nativeCodexFrame(t *testing.T, ui *nativeFleetUI, label string, wanted ...string) {
	t.Helper()
	nativeAwait(t, label, func() (bool, string) {
		screen := ui.screen()
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		for _, value := range wanted {
			if !strings.Contains(screen, value) {
				return false, fmt.Sprintf("missing expected fixture text %q; screen withheld", value)
			}
		}
		return true, "expected fixture text present"
	})
}

func nativeCodexBlockedScreen(screen string) string {
	lower := strings.ToLower(screen)
	for _, phrase := range []string{
		"sign in with chatgpt", "provide your own api key", "paste your api key",
		"authentication failed", "unauthorized", "invalid api key", "not logged in",
		"failed to refresh token", "quota exceeded", "usage limit reached",
	} {
		if strings.Contains(lower, phrase) {
			return "Codex login/provider setup or authentication failed (" + phrase + ")"
		}
	}
	if strings.Contains(lower, "windows sandbox") &&
		(strings.Contains(lower, "set up") || strings.Contains(lower, "setup") || strings.Contains(lower, "enable") || strings.Contains(lower, "elevat")) {
		return "Codex requires Windows sandbox setup; no setup choice or permission bypass was submitted"
	}
	if strings.Contains(lower, "create a sandbox") || strings.Contains(lower, "set up the sandbox") {
		return "Codex startup would create a sandbox; no setup choice was submitted"
	}
	return ""
}

func nativeCodexComposer(screen string) bool {
	lower := strings.ToLower(screen)
	if !strings.Contains(lower, "codex") || !strings.Contains(screen, "›") {
		return false
	}
	modelPresent, modelLoading := nativeCodexModelHeader(screen)
	if !modelPresent || modelLoading {
		return false
	}
	// matrix/hardened: selection menus also use ›. Require the real composer
	// placeholder or its footer, and exclude every onboarding confirmation.
	if strings.Contains(lower, "press enter to continue") || strings.Contains(lower, "do you trust the contents") || strings.Contains(lower, "yes, continue") {
		return false
	}
	return strings.Contains(lower, "ask codex to do anything") ||
		strings.Contains(lower, "ask a follow-up question") ||
		(strings.Contains(lower, "for shortcuts") && strings.Contains(lower, "context left"))
}

func nativeCodexModelHeader(screen string) (present, loading bool) {
	for _, line := range strings.Split(strings.ToLower(screen), "\n") {
		index := strings.Index(line, "model:")
		if index < 0 {
			continue
		}
		value := strings.TrimSpace(line[index+len("model:"):])
		if strings.Trim(value, " │") != "" {
			present = true
			loading = loading || strings.Contains(value, "loading")
		}
	}
	return present, loading
}

func nativeCodexScreenCategory(screen string) string {
	if reason := nativeCodexBlockedScreen(screen); reason != "" {
		return reason
	}
	lower := strings.ToLower(screen)
	_, modelLoading := nativeCodexModelHeader(screen)
	switch {
	case strings.Contains(lower, "trust") && strings.Contains(lower, "yes, continue"):
		return "directory-trust-choice"
	case strings.Contains(lower, "update available") && strings.Contains(lower, "press"):
		return "update-choice"
	case strings.Contains(lower, "press enter to continue"):
		return "startup-choice"
	case modelLoading:
		return "startup-model-loading"
	case strings.Contains(lower, "stream disconnected") || strings.Contains(lower, "error sending request") || strings.Contains(lower, "failed to connect"):
		return "provider-transport-error"
	case strings.Contains(lower, "retrying") || strings.Contains(lower, "reconnecting"):
		return "provider-retry"
	case nativeCodexComposer(screen):
		return "composer"
	case strings.Contains(lower, "codex"):
		return "codex-screen-without-confirmed-composer"
	default:
		return "unclassified-screen"
	}
}

func nativeCodexDialogFacts(screen, project string) string {
	lower := strings.ToLower(screen)
	selectedYes := false
	for _, line := range strings.Split(lower, "\n") {
		line = strings.TrimSpace(line)
		selectedYes = selectedYes || (strings.HasPrefix(line, "›") && strings.Contains(line, "yes, continue"))
	}
	modelPresent, modelLoading := nativeCodexModelHeader(screen)
	return fmt.Sprintf("own_project_visible=%v trust_heading=%v yes_continue=%v selected_yes=%v continue_hint=%v model_header_present=%v model_loading=%v",
		strings.Contains(lower, strings.ToLower(project)), strings.Contains(lower, "do you trust"),
		strings.Contains(lower, "yes, continue"), selectedYes, strings.Contains(lower, "to continue"), modelPresent, modelLoading)
}

func nativeCodexAwaitReady(t *testing.T, ui *nativeFleetUI, project string) {
	t.Helper()
	deadline := time.Now().Add(nativeCodexStartupTimeout)
	trusted := false
	lastCategory := "not-yet-observed"
	lastFacts := "not-yet-observed"
	var composerSince time.Time
	for time.Now().Before(deadline) {
		screen := ui.screen()
		lastCategory = nativeCodexScreenCategory(screen)
		lastFacts = nativeCodexDialogFacts(screen, project)
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		lower := strings.ToLower(screen)
		if strings.Contains(lower, "trust") && strings.Contains(lower, "yes, continue") && strings.Contains(lower, strings.ToLower(project)) {
			composerSince = time.Time{}
			if trusted {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// The trust prompt must visibly identify this test's owned project.
			// This changes only the explicit QA clone, never the default config.
			if strings.Contains(lower, "yes, continue") {
				nativeWrite(t, ui.viewer, "\r")
				trusted = true
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		if nativeCodexComposer(screen) {
			if composerSince.IsZero() {
				composerSince = time.Now()
			}
			// Initial placeholders can precede asynchronous trust onboarding.
			// Observe a stable real composer before sending the first draft.
			if time.Since(composerSince) >= time.Second {
				return
			}
		} else {
			composerSince = time.Time{}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Codex did not expose its stable native composer within the startup deadline; screen_category=%s trusted_owned_project=%v %s; screen contents withheld", lastCategory, trusted, lastFacts)
}

func nativeCodexSubmit(t *testing.T, ui *nativeFleetUI, prompt string) {
	t.Helper()
	// matrix/hardened: verify human typing separately from bracketed paste.
	// Codex classifies an 8 ms char burst as paste; space events by 30 ms.
	for _, char := range prompt {
		nativeWrite(t, ui.viewer, string(char))
		time.Sleep(30 * time.Millisecond)
	}
	nativeCodexCommitDraft(t, ui, prompt)
}

func nativeCodexSubmitPaste(t *testing.T, ui *nativeFleetUI, pipe string, info terminalhost.SessionInfo, prompt string) {
	t.Helper()
	nativeAwait(t, "exact inner Codex and reopened viewer both enable bracketed paste", func() (bool, string) {
		inner := nativeCall(t, pipe, terminalhost.Request{Op: "capture", SessionID: info.ID, RunID: info.RunID})
		outer := ui.frame.Dispatch(terminalhost.Request{Op: "capture"})
		if inner.SessionID != info.ID || inner.RunID != info.RunID || inner.PID != info.PID || outer.PID != ui.viewer.process.PID() {
			t.Fatal("bracketed-paste mode capture disagrees with exact inner/outer native process ownership")
		}
		return inner.BracketedPaste && outer.BracketedPaste,
			fmt.Sprintf("inner_bracketed_paste=%v outer_bracketed_paste=%v", inner.BracketedPaste, outer.BracketedPaste)
	})
	// The same one-Enter acknowledgement applies after real bracketed paste.
	// Never resend a prompt after uncertainty about its submission.
	nativeWrite(t, ui.viewer, "\x1b[200~"+prompt+"\x1b[201~")
	nativeCodexCommitDraft(t, ui, prompt)
}

func nativeCodexCommitDraft(t *testing.T, ui *nativeFleetUI, prompt string) {
	t.Helper()
	nativeAwait(t, "exact harmless prompt is visible in the last native composer row", func() (bool, string) {
		screen := ui.screen()
		return nativeCodexDraftContains(screen, prompt), nativeCodexDraftFacts(screen, prompt)
	})
	time.Sleep(time.Second)
	screen := ui.screen()
	if !nativeCodexDraftContains(screen, prompt) || nativeCodexScreenCategory(screen) == "directory-trust-choice" || nativeCodexBlockedScreen(screen) != "" {
		t.Fatalf("composer changed before the single Enter; no submission sent; %s %s", nativeCodexScreenCategory(screen), nativeCodexDraftFacts(screen, prompt))
	}
	nativeWrite(t, ui.viewer, "\r")
	nativeAwait(t, "single Codex Enter clears the exact draft rather than adding a newline", func() (bool, string) {
		screen := ui.screen()
		category := nativeCodexScreenCategory(screen)
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		if category == "directory-trust-choice" || category == "startup-choice" || category == "update-choice" {
			t.Fatalf("startup choice appeared after the single Enter; no retry submitted; %s", category)
		}
		return !nativeCodexDraftContains(screen, prompt), nativeCodexDraftFacts(screen, prompt)
	})
}

func nativeCodexDraftContains(screen, prompt string) bool {
	lastComposerRow := ""
	for _, line := range strings.Split(screen, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "›") {
			lastComposerRow = line
		}
	}
	return strings.Contains(lastComposerRow, prompt)
}

func nativeCodexDraftFacts(screen, prompt string) string {
	promptRows, arrowRows := 0, 0
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, prompt) {
			promptRows++
		}
		if strings.HasPrefix(strings.TrimSpace(line), "›") {
			arrowRows++
		}
	}
	return fmt.Sprintf("draft_has_prompt=%v prompt_rows=%d arrow_rows=%d", nativeCodexDraftContains(screen, prompt), promptRows, arrowRows)
}

type nativeCodexRollout struct {
	path       string
	threadID   string
	assistant  []string
	user       []string
	toolCalled bool
}

func nativeCodexReadRollout(path, project string) (nativeCodexRollout, error) {
	result := nativeCodexRollout{path: path}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return result, errors.New("QA rollout is unavailable, linked or exceeds its size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return result, errors.New("cannot open owned QA rollout")
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, (8<<20)+1))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	line, total := 0, 0
	for scanner.Scan() {
		line++
		total += len(scanner.Bytes()) + 1
		if total > 8<<20 {
			return result, errors.New("QA rollout exceeded its total read limit")
		}
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		// The live writer may leave its last JSON line temporarily incomplete.
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if line == 1 {
			var meta struct {
				ID           string `json:"id"`
				SessionID    string `json:"session_id"`
				CWD          string `json:"cwd"`
				ThreadSource string `json:"thread_source"`
				ParentID     string `json:"parent_thread_id"`
			}
			if event.Type != "session_meta" || json.Unmarshal(event.Payload, &meta) != nil ||
				!strings.EqualFold(filepath.Clean(meta.CWD), filepath.Clean(project)) || meta.ParentID != "" || meta.ThreadSource == "subagent" {
				return result, errors.New("QA rollout header does not identify this exact project/user thread")
			}
			result.threadID = meta.ID
			if result.threadID == "" {
				result.threadID = meta.SessionID
			}
			if _, err := uuid.Parse(result.threadID); err != nil || (meta.SessionID != "" && meta.SessionID != result.threadID) {
				return result, errors.New("QA rollout header has invalid or conflicting thread identity")
			}
			continue
		}
		if event.Type != "response_item" {
			continue
		}
		var response struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(event.Payload, &response) != nil {
			continue
		}
		if strings.HasSuffix(response.Type, "_call") {
			result.toolCalled = true
		}
		if response.Type != "message" || (response.Role != "assistant" && response.Role != "user") {
			continue
		}
		var text strings.Builder
		for _, content := range response.Content {
			if content.Type == "output_text" || content.Type == "input_text" {
				text.WriteString(content.Text)
			}
		}
		if response.Role == "assistant" {
			result.assistant = append(result.assistant, strings.TrimSpace(text.String()))
		} else {
			result.user = append(result.user, text.String())
		}
	}
	if err := scanner.Err(); err != nil {
		return result, errors.New("QA rollout exceeded its bounded JSON line reader")
	}
	if result.threadID == "" {
		return result, errors.New("QA rollout header is not complete yet")
	}
	return result, nil
}

func nativeCodexFindRollout(home, project string) (nativeCodexRollout, error) {
	var result nativeCodexRollout
	count, matches := 0, 0
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 128 || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("QA rollout traversal rejected a link or exceeded its entry limit")
		}
		relative, err := filepath.Rel(filepath.Join(home, "sessions"), path)
		if err != nil || len(strings.Split(relative, string(filepath.Separator))) > 5 {
			return errors.New("QA rollout traversal exceeded its depth limit")
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		candidate, err := nativeCodexReadRollout(path, project)
		if err != nil {
			return err
		}
		matches++
		result = candidate
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return result, errors.New("QA rollout has not been created yet")
	}
	if err != nil {
		return result, err
	}
	if matches != 1 {
		return result, errors.New("QA rollout discovery requires exactly one owned user thread")
	}
	return result, nil
}

func nativeCodexAwaitReply(t *testing.T, ui *nativeFleetUI, home, project, priorThread, prompt, expected string) nativeCodexRollout {
	t.Helper()
	deadline := time.Now().Add(nativeCodexModelTimeout)
	var lastError error
	lastCategory := "not-yet-observed"
	lastFacts := "not-yet-observed"
	lastUsers, lastAssistants := 0, 0
	lastExactUsers := 0
	promptVisible, replyVisible := false, false
	lastDraftFacts := "not-yet-observed"
	for time.Now().Before(deadline) {
		screen := ui.screen()
		lastCategory, promptVisible = nativeCodexScreenCategory(screen), strings.Contains(screen, prompt)
		replyVisible, lastDraftFacts = strings.Contains(screen, expected), nativeCodexDraftFacts(screen, prompt)
		lastFacts = nativeCodexDialogFacts(screen, project)
		if reason := nativeCodexBlockedScreen(screen); reason != "" {
			t.Fatal(reason)
		}
		rollout, err := nativeCodexFindRollout(home, project)
		lastError = err
		if err == nil {
			lastUsers, lastAssistants = len(rollout.user), len(rollout.assistant)
			if priorThread != "" && rollout.threadID != priorThread {
				t.Fatal("reattached Codex changed its native rollout thread identity")
			}
			if rollout.toolCalled {
				t.Fatal("harmless reply-only Codex prompt unexpectedly invoked a tool")
			}
			userSeen, assistantSeen := false, false
			lastExactUsers = 0
			for _, text := range rollout.user {
				// A correct model reply alone cannot prove paste framing: the
				// model might ignore leaked CSI markers. Require exact user text.
				if strings.TrimSpace(text) == prompt {
					userSeen = true
					lastExactUsers++
				}
			}
			for _, text := range rollout.assistant {
				assistantSeen = assistantSeen || text == expected
			}
			if userSeen && assistantSeen && strings.Contains(screen, expected) {
				return rollout
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Codex reply was not independently confirmed within %s; rollout_reader=%v user_messages=%d exact_user_matches=%d assistant_messages=%d screen_category=%s prompt_visible=%v reply_visible=%v %s %s; no auth/config or raw screen contents logged", nativeCodexModelTimeout, lastError, lastUsers, lastExactUsers, lastAssistants, lastCategory, promptVisible, replyVisible, lastFacts, lastDraftFacts)
	return nativeCodexRollout{}
}

func nativeCodexReadState(t *testing.T, db *statedb.StateDB, info terminalhost.SessionInfo, image string) {
	t.Helper()
	rows, err := db.LoadInstances()
	if err != nil || len(rows) != 1 {
		t.Fatalf("original StateDB must contain exactly the private Codex descriptor: count=%d error=%v", len(rows), err)
	}
	row := rows[0]
	backend, err := nativestatestore.DecodeBackend(row.ToolData)
	if err != nil || backend == nil || row.ID != info.ID || row.Title != info.Name || row.Tool != "codex" || row.GroupPath != nativestatestore.DefaultGroup || row.TmuxSession != "" || !strings.EqualFold(row.ProjectPath, info.Dir) {
		t.Fatal("original StateDB readback disagrees with the exact Codex session identity")
	}
	if !strings.EqualFold(filepath.Clean(backend.Launch.Executable), filepath.Clean(image)) || len(backend.Launch.Args) != 0 {
		t.Fatal("Codex descriptor did not launch the actual native executable with its default bare arguments")
	}
	if len(backend.Launch.Env) != 0 {
		t.Fatal("UI-created Codex descriptor persisted inherited environment")
	}
}

func TestWindowsNativeCodexOnlineLifecycle(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" || os.Getenv("AGENTDECK_NATIVE_CODEX_ONLINE_TEST") != "1" {
		t.Skip("set both AGENTDECK_NATIVE_TEST=1 and AGENTDECK_NATIVE_CODEX_ONLINE_TEST=1 to authorize two harmless real model turns")
	}
	cli := nativeBootstrapCLI(t)
	home := nativeCodexQAHome(t)
	project := filepath.Join(t.TempDir(), uuid.NewString())
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	// Windows TempDir may use an ADMINI~1 alias, while Codex displays the long
	// directory name in its trust prompt and rollout header. Supply one spelling
	// to the native UI, descriptor, process cwd and independent readbacks.
	shortPath, err := windows.UTF16PtrFromString(project)
	if err != nil {
		t.Fatal(err)
	}
	longPath := make([]uint16, 32768)
	longLength, err := windows.GetLongPathName(shortPath, &longPath[0], uint32(len(longPath)))
	if err != nil || longLength == 0 || longLength >= uint32(len(longPath)) {
		t.Fatalf("normalize exact owned project path: %v", err)
	}
	project = windows.UTF16ToString(longPath[:longLength])
	sentinel := []byte("owned harmless Codex lifecycle fixture\n")
	sentinelPath := filepath.Join(project, "keep.txt")
	if err := os.WriteFile(sentinelPath, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	config := options{Store: "statedb", StateDir: filepath.Join(t.TempDir(), "manager-"+uuid.NewString()), Pipe: winipc.PipePrefix + "codex-online-" + uuid.NewString()}
	var identity terminalhost.Response
	nativeBootstrapCleanup(t, config, &identity)
	ui := nativeCodexStartUI(t, cli, config, project)
	nativeCodexFrame(t, ui, "private online Codex list", "Windows 原生会话", "没有会话")
	identity = nativeBootstrapIdentity(t, config)
	managerHandle := nativeProcessHandle(t, uint32(identity.HostPID))
	name := filepath.Base(project)
	nativeWrite(t, ui.viewer, "n")
	nativeCodexFrame(t, ui, "new-session form defaults to Codex and the owned project", "新建会话", "› 名称：", name, project, "[Codex]", "留空为默认分组")
	nativeWrite(t, ui.viewer, "\r")
	info := nativeFleetFind(t, config.Pipe, name, "running")
	if info.Tool != "codex" || info.Group != nativestatestore.DefaultGroup || info.ID == "" || info.RunID == "" || info.PID == 0 || info.HostPID != identity.HostPID || !strings.EqualFold(info.Dir, project) {
		t.Fatal("UI-created Codex does not belong to the exact shared-store background manager")
	}
	codexHandle := nativeProcessHandle(t, info.PID)
	imageBuffer := make([]uint16, 32768)
	imageLength := uint32(len(imageBuffer))
	if err := windows.QueryFullProcessImageName(codexHandle, 0, &imageBuffer[0], &imageLength); err != nil {
		t.Fatalf("query exact native Codex process image: %v", err)
	}
	image := windows.UTF16ToString(imageBuffer[:imageLength])
	if !strings.EqualFold(filepath.Base(image), "codex.exe") {
		t.Fatal("managed Codex PID is not the actual native codex.exe")
	}
	db, err := statedb.OpenReadOnlyLive(filepath.Join(config.StateDir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativeClose(t, "online StateDB readback", db.Close) })
	nativeCodexReadState(t, db, info, image)
	if _, err := os.Stat(filepath.Join(config.StateDir, "registry.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("online shared-store manager created a duplicate prototype registry")
	}
	nativeCodexAwaitReady(t, ui, project)
	makePrompt := func() (string, string) {
		// matrix/hardened: test real I/O rather than random-string reversal
		// ability. The transformed proof is still absent from typed input.
		entropy := uuid.New()
		letters := make([]byte, 12)
		for index := range letters {
			letters[index] = 'a' + entropy[index]%26
		}
		nonce := string(letters)
		prompt := "Reply with only the uppercase form of " + nonce + ". Do not use tools or modify any files."
		expected := strings.ToUpper(nonce)
		if strings.Contains(prompt, expected) {
			t.Fatal("reply proof must not already appear in typed input")
		}
		return prompt, expected
	}
	firstPrompt, firstReply := makePrompt()
	nativeCodexSubmit(t, ui, firstPrompt)
	first := nativeCodexAwaitReply(t, ui, home, project, "", firstPrompt, firstReply)
	nativeFleetSameRun(t, info, nativeFleetFind(t, config.Pipe, name, "running"))
	nativeWrite(t, ui.viewer, "\x11")
	nativeCodexFrame(t, ui, "leave Codex after real reply", "Windows 原生会话", name, "已返回列表")
	nativeWrite(t, ui.viewer, "q")
	nativeWaitExit(t, ui.viewer.process, 0)
	nativeAssertProcess(t, managerHandle, true)
	nativeAssertProcess(t, codexHandle, true)
	nativeFleetSameRun(t, info, nativeFleetFind(t, config.Pipe, name, "running"))
	reopened := nativeCodexStartUI(t, cli, config, project)
	nativeCodexFrame(t, reopened, "reopen original running Codex list", "Windows 原生会话", name)
	after := nativeBootstrapIdentity(t, config)
	if after.RunID != identity.RunID || after.HostPID != identity.HostPID {
		t.Fatal("reopening online UI changed the background manager identity")
	}
	nativeWrite(t, reopened.viewer, "\r")
	nativeCodexFrame(t, reopened, "reattach restores independently verified assistant reply", firstReply)
	nativeFleetSameRun(t, info, nativeFleetFind(t, config.Pipe, name, "running"))
	secondPrompt, secondReply := makePrompt()
	nativeCodexSubmitPaste(t, reopened, config.Pipe, info, secondPrompt)
	second := nativeCodexAwaitReply(t, reopened, home, project, first.threadID, secondPrompt, secondReply)
	if second.path != first.path {
		t.Fatal("reattached online Codex wrote its second reply to a different rollout")
	}
	nativeWrite(t, reopened.viewer, "\x11")
	nativeCodexFrame(t, reopened, "return after second real Codex reply", "Windows 原生会话", "已返回列表", name)
	nativeWrite(t, reopened.viewer, "s")
	nativeCodexFrame(t, reopened, "confirm exact online Codex stop", "停止此会话？", name)
	nativeWrite(t, reopened.viewer, "\r")
	nativeCodexFrame(t, reopened, "exact online Codex run stopped", "会话已停止。", "已停止")
	stopped := nativeFleetFind(t, config.Pipe, name, "stopped")
	if stopped.ID != info.ID || stopped.RunID != info.RunID || stopped.PID != info.PID {
		t.Fatal("stop changed the original Codex descriptor/run identity")
	}
	nativeAssertProcess(t, codexHandle, false)
	nativeAssertProcess(t, managerHandle, true)
	nativeCodexReadState(t, db, stopped, image)
	nativeFleetProjectUnchanged(t, sentinelPath, sentinel)
	nativeWrite(t, reopened.viewer, "q")
	nativeWaitExit(t, reopened.viewer.process, 0)
	ctx, cancel := context.WithTimeout(context.Background(), nativeAttachTimeout)
	defer cancel()
	final, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "fleet-list"})
	if err != nil || len(final.Sessions) != 1 || final.Sessions[0].ID != info.ID || final.Sessions[0].State != "stopped" {
		t.Fatal("Codex stop did not retain exactly the private shared-store descriptor")
	}
	t.Logf("human typing, bracketed paste, two real assistant replies and same-thread reattach verified; manager_pid=%d codex_pid=%d thread_id=%s; headless console evidence, no visible desktop assertion", identity.HostPID, info.PID, first.threadID)
}
