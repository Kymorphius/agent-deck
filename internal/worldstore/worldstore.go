// Package worldstore connects agent-deck to the World Store task board
// (personal-panel). matrix/hardened: local-only integration added on our fork.
//
// World Store is the only home of the tasks: agent-deck keeps no copy. Every
// call runs the personal-panel task bridge (`node <bridge>`), which reads one
// named JSON operation on stdin and talks to the running World Store app over
// its private per-user Unix socket. No network, no tokens, nothing polled in
// the background: reads happen when the board opens or the user refreshes.
//
// Writes follow the bridge's contract: every write carries the owner and the
// task revision it was based on (stale writes fail with CONFLICT), and a write
// whose outcome is unknown returns WRITE_UNCERTAIN. Callers must never retry
// that automatically; they re-read and let the user decide.
package worldstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Source is the link source agent-deck writes. World Store keeps these links
// in their own relation (任务｜agent-deck 会话关联), apart from Codex links.
const Source = "agent-deck"

// Task statuses the bridge accepts for a plain status change. 已完成 goes
// through Complete instead, which also records the completion time.
var MovableStatuses = []string{"待办", "进行中", "暂停", "已取消"}

// StatusDone is the completed status.
const StatusDone = "已完成"

// Bridge error codes (see personal-panel src/multica-task-bridge.cjs).
const (
	CodeBadRequest        = "BAD_REQUEST"
	CodeConflict          = "CONFLICT"
	CodeNotFound          = "NOT_FOUND"
	CodeReadOnly          = "READ_ONLY"
	CodeOwnerChanged      = "OWNER_CHANGED"
	CodeReadFailed        = "READ_FAILED"
	CodeSourceUnavailable = "SOURCE_UNAVAILABLE"
	CodeWriteUncertain    = "WRITE_UNCERTAIN"
	// CodeUnavailable is ours: the bridge could not be run or answered garbage.
	CodeUnavailable = "UNAVAILABLE"
)

// Error is a bridge failure with its machine-readable code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// CodeOf returns the bridge error code of err, or "" when err is not one.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Owner identifies the World Store account and space a read came from.
// Writes must echo it so a switched space never receives them.
type Owner struct {
	AccountID string `json:"accountId"`
	SpaceID   string `json:"spaceId"`
}

// Link is one recorded task → agent-deck session association.
type Link struct {
	Source    string `json:"source"`
	DeviceID  string `json:"deviceId"`
	SessionID string `json:"sessionId"`
	LinkedAt  int64  `json:"linkedAt"`
}

// Task is the bridge's board projection of one World Store task.
type Task struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Lane         string `json:"lane"`
	Category     string `json:"category"`
	PlanDate     *int64 `json:"planDate"`
	SelectedDate *int64 `json:"selectedDate"`
	CompletedAt  *int64 `json:"completedAt"`
	ReadOnly     bool   `json:"readOnly"`
	Revision     string `json:"revision"`
	PlanText     string `json:"planText,omitempty"`
	ResultText   string `json:"resultText,omitempty"`
	// AgentDeckLinks is nil when the stored value is not a known format;
	// LinksUnknown distinguishes that from "no links".
	AgentDeckLinks []Link `json:"agentDeckLinks,omitempty"`
	LinksUnknown   bool   `json:"linksUnknown,omitempty"`
}

// UnmarshalJSON keeps the bridge's three states for agentDeckLinks apart:
// absent (relation not set up / empty), null (unknown format), array.
func (t *Task) UnmarshalJSON(data []byte) error {
	type plain Task
	// Links shadows plain.AgentDeckLinks (shallower field wins), so the raw
	// value is inspected here instead of being decoded straight into the slice.
	var aux struct {
		plain
		Links json.RawMessage `json:"agentDeckLinks"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*t = Task(aux.plain)
	switch raw := bytes.TrimSpace(aux.Links); {
	case len(raw) == 0:
	case bytes.Equal(raw, []byte("null")):
		t.LinksUnknown = true
	default:
		if err := json.Unmarshal(raw, &t.AgentDeckLinks); err != nil {
			t.LinksUnknown = true
		}
	}
	return nil
}

// LinkedTo reports whether the task records sessionID from deviceID.
func (t Task) LinkedTo(deviceID, sessionID string) bool {
	for _, l := range t.AgentDeckLinks {
		if l.DeviceID == deviceID && l.SessionID == sessionID {
			return true
		}
	}
	return false
}

// Board is one complete read of the task list.
type Board struct {
	Owner     Owner  `json:"owner"`
	UpdatedAt string `json:"updatedAt"`
	Tasks     []Task `json:"tasks"`
}

// Find returns the task with id, or nil.
func (b *Board) Find(id string) *Task {
	if b == nil {
		return nil
	}
	for i := range b.Tasks {
		if b.Tasks[i].ID == id {
			return &b.Tasks[i]
		}
	}
	return nil
}

// LinkSchema is the state of the agent-deck link relation in World Store.
type LinkSchema struct {
	Exists   bool `json:"exists"`
	Attached bool `json:"attached"`
	Ready    bool `json:"ready"`
}

// Runner executes the bridge with one request on stdin and returns stdout.
// A non-nil error with non-empty stdout still carries the bridge's answer.
type Runner func(ctx context.Context, stdin []byte) (stdout []byte, err error)

// Client talks to World Store through the personal-panel task bridge.
type Client struct {
	run          Runner
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// Config is what the client needs from [worldstore] in config.toml.
type Config struct {
	// Bridge is the absolute path to personal-panel's
	// scripts/multica-task-bridge.cjs.
	Bridge string
	// Node is the node binary; empty means "node" from PATH.
	Node string
}

// ErrNotConfigured is returned when [worldstore].bridge is unset.
var ErrNotConfigured = errors.New("World Store is not configured: set [worldstore] bridge in config.toml to personal-panel's scripts/multica-task-bridge.cjs")

// New validates cfg and returns a client that runs the real bridge.
func New(cfg Config) (*Client, error) {
	bridge := strings.TrimSpace(cfg.Bridge)
	if bridge == "" {
		return nil, ErrNotConfigured
	}
	if strings.HasPrefix(bridge, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			bridge = filepath.Join(home, bridge[2:])
		}
	}
	if !filepath.IsAbs(bridge) {
		return nil, fmt.Errorf("[worldstore] bridge must be an absolute path, got %q", cfg.Bridge)
	}
	if st, err := os.Stat(bridge); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("[worldstore] bridge %s is not a readable file", bridge)
	}
	node := strings.TrimSpace(cfg.Node)
	if node == "" {
		node = "node"
	}
	return NewWithRunner(execRunner(node, bridge)), nil
}

// NewWithRunner builds a client over any runner (tests use a fake).
func NewWithRunner(run Runner) *Client {
	// The bridge waits up to 180 s per native request and asks callers for a
	// longer process timeout; a write is a native request plus a read-back.
	return &Client{run: run, ReadTimeout: 200 * time.Second, WriteTimeout: 6 * time.Minute}
}

func execRunner(node, bridge string) Runner {
	return func(ctx context.Context, stdin []byte) ([]byte, error) {
		cmd := exec.CommandContext(ctx, node, bridge)
		// The bridge resolves its own modules; run it from the repo so a
		// relative require never depends on agent-deck's cwd.
		cmd.Dir = filepath.Dir(filepath.Dir(bridge))
		cmd.Stdin = bytes.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		// A bridge that never started cannot have written anything; one that
		// started and then died or timed out might have (call decides).
		if err := cmd.Start(); err != nil {
			return nil, &Error{Code: CodeUnavailable, Message: "task bridge did not start: " + err.Error()}
		}
		err := cmd.Wait()
		if err != nil && stdout.Len() == 0 {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 500 {
				msg = msg[:500]
			}
			if msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		return stdout.Bytes(), err
	}
}

func (c *Client) call(ctx context.Context, write bool, req map[string]any, out any) error {
	timeout := c.ReadTimeout
	if write {
		timeout = c.WriteTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return &Error{Code: CodeBadRequest, Message: err.Error()}
	}
	stdout, runErr := c.run(ctx, body)
	if runErr != nil && len(stdout) == 0 {
		if CodeOf(runErr) != "" {
			return runErr
		}
		if write {
			return &Error{Code: CodeWriteUncertain, Message: "task bridge interrupted: " + runErr.Error() + "; re-read before acting, do not retry blindly"}
		}
		return &Error{Code: CodeUnavailable, Message: runErr.Error()}
	}
	var failure struct {
		OK    *bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &failure); err != nil {
		if write {
			return &Error{Code: CodeWriteUncertain, Message: "task bridge answer unreadable; re-read before acting"}
		}
		return &Error{Code: CodeUnavailable, Message: "task bridge answer unreadable"}
	}
	if failure.OK != nil && !*failure.OK {
		return &Error{Code: failure.Error.Code, Message: failure.Error.Message}
	}
	if runErr != nil {
		return &Error{Code: CodeUnavailable, Message: runErr.Error()}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		if write {
			return &Error{Code: CodeWriteUncertain, Message: "task bridge answer unreadable; re-read before acting"}
		}
		return &Error{Code: CodeUnavailable, Message: "task bridge answer unreadable: " + err.Error()}
	}
	return nil
}

// Board reads every task, following the bridge's pagination.
func (c *Client) Board(ctx context.Context) (*Board, error) {
	board := &Board{}
	cursor := ""
	for page := 0; page < 100; page++ {
		req := map[string]any{"op": "task.list", "limit": 200}
		if cursor != "" {
			req["cursor"] = cursor
			req["owner"] = board.Owner
		}
		var resp struct {
			Owner      Owner   `json:"owner"`
			UpdatedAt  string  `json:"updatedAt"`
			Tasks      []Task  `json:"tasks"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := c.call(ctx, false, req, &resp); err != nil {
			return nil, err
		}
		if resp.Owner.AccountID == "" || resp.Owner.SpaceID == "" {
			return nil, &Error{Code: CodeSourceUnavailable, Message: "World Store did not confirm its account and space"}
		}
		if page == 0 {
			board.Owner, board.UpdatedAt = resp.Owner, resp.UpdatedAt
		} else if resp.Owner != board.Owner {
			return nil, &Error{Code: CodeOwnerChanged, Message: "World Store account or space changed while reading"}
		}
		board.Tasks = append(board.Tasks, resp.Tasks...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return board, nil
		}
		cursor = *resp.NextCursor
	}
	return nil, &Error{Code: CodeReadFailed, Message: "task list did not end after 100 pages"}
}

// Get reads one task with its plan and result text.
func (c *Client) Get(ctx context.Context, owner Owner, id string) (*Task, error) {
	var resp struct {
		Task Task `json:"task"`
	}
	if err := c.call(ctx, false, map[string]any{"op": "task.get", "owner": owner, "id": id}, &resp); err != nil {
		return nil, err
	}
	return &resp.Task, nil
}

// SetStatus moves a task to one of MovableStatuses.
func (c *Client) SetStatus(ctx context.Context, owner Owner, t Task, status string) (*Task, error) {
	movable := false
	for _, s := range MovableStatuses {
		movable = movable || s == status
	}
	if !movable {
		return nil, &Error{Code: CodeBadRequest, Message: fmt.Sprintf("status %q cannot be set directly (allowed: %s; use complete for %s)", status, strings.Join(MovableStatuses, ", "), StatusDone)}
	}
	var resp struct {
		Task Task `json:"task"`
	}
	req := map[string]any{"op": "task.update", "owner": owner, "id": t.ID, "revision": t.Revision, "patch": map[string]any{"status": status}}
	if err := c.call(ctx, true, req, &resp); err != nil {
		return nil, err
	}
	return &resp.Task, nil
}

// Complete marks a task done. resultText is optional ("" leaves it as is).
func (c *Client) Complete(ctx context.Context, owner Owner, t Task, resultText string) (*Task, error) {
	var resp struct {
		Task Task `json:"task"`
	}
	req := map[string]any{"op": "task.complete", "owner": owner, "id": t.ID, "revision": t.Revision}
	if strings.TrimSpace(resultText) != "" {
		req["resultText"] = resultText
	}
	if err := c.call(ctx, true, req, &resp); err != nil {
		return nil, err
	}
	return &resp.Task, nil
}

// InspectLinks reports whether the agent-deck link relation is ready.
func (c *Client) InspectLinks(ctx context.Context) (*LinkSchema, error) {
	var resp struct {
		Schema LinkSchema `json:"schema"`
	}
	if err := c.call(ctx, false, map[string]any{"op": "task.links.inspect", "source": Source}, &resp); err != nil {
		return nil, err
	}
	return &resp.Schema, nil
}

// InitializeLinks creates and attaches the agent-deck link relation. It
// changes the World Store schema, so callers must have the user's explicit
// confirmation.
func (c *Client) InitializeLinks(ctx context.Context, owner Owner) error {
	return c.call(ctx, true, map[string]any{"op": "task.links.initialize", "source": Source, "owner": owner, "confirmed": true}, nil)
}

// Link records sessionID (on deviceID) against the task.
func (c *Client) Link(ctx context.Context, owner Owner, t Task, deviceID, sessionID string) ([]Link, error) {
	return c.link(ctx, "task.linkExecution", owner, t, deviceID, sessionID)
}

// Unlink removes a recorded session from the task.
func (c *Client) Unlink(ctx context.Context, owner Owner, t Task, deviceID, sessionID string) ([]Link, error) {
	return c.link(ctx, "task.unlinkExecution", owner, t, deviceID, sessionID)
}

func (c *Client) link(ctx context.Context, op string, owner Owner, t Task, deviceID, sessionID string) ([]Link, error) {
	var resp struct {
		Links []Link `json:"links"`
	}
	req := map[string]any{"op": op, "source": Source, "owner": owner, "id": t.ID, "revision": t.Revision, "deviceId": deviceID, "sessionId": sessionID, "confirmed": true}
	if err := c.call(ctx, true, req, &resp); err != nil {
		return nil, err
	}
	return resp.Links, nil
}

// DeviceID is how this machine is named in links: the configured value, else
// the short hostname. It only has to be stable on this machine.
func DeviceID(configured string) string {
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "local"
	}
	return strings.TrimSuffix(host, ".local")
}
