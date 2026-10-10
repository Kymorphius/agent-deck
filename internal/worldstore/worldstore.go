// Package worldstore connects agent-deck to the World Store task board
// (personal-panel). matrix/hardened: local-only integration added on our fork.
//
// World Store is the only home of the tasks: agent-deck keeps no copy. Every
// call runs personal-panel's AI data entry (`node scripts/worldstore-ai.cjs`),
// which reads one JSON request on stdin and talks to the official headless
// Anytype service on this machine. No network, no tokens, nothing polled in
// the background: reads happen when the board opens or the user refreshes.
//
// Status changes follow World Store's own flow instead of editing the status
// field: "start" creates a running execution record, pause/resume act on that
// execution, and complete closes the task and its executions. Every write
// carries the owner and the baselines it was based on; a write whose outcome
// is unknown (UNCONFIRMED / WRITE_UNCERTAIN) must never be retried
// automatically — callers re-read and let the user decide.
package worldstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
// in their own relation, apart from Codex links.
const Source = "agent-deck"

// LinkField is the native relation holding agent-deck links.
const LinkField = "任务｜agent-deck 会话关联"

// Native field names read from the snapshot.
const (
	fieldStatus      = "任务｜状态"
	fieldCategory    = "任务｜分类"
	fieldPlanDate    = "任务｜计划日期"
	fieldPlanText    = "任务｜计划说明"
	fieldResult      = "任务｜处理结果"
	fieldSelected    = "任务｜今日选择"
	fieldCompletedAt = "任务｜完成时间"
	execStatus       = "执行｜状态"
	execTask         = "执行｜关联任务"
	execStartedAt    = "执行｜开始时间"
)

// Task and execution statuses.
const (
	StatusDone      = "已完成"
	StatusCancelled = "已取消"
	StatusCycle     = "周期进行中"
	ExecRunning     = "进行中"
	ExecPaused      = "暂停"
)

// Error codes. Most come from World Store (docs/ai-interface.md); the last
// two are ours.
const (
	CodeInvalidRequest     = "INVALID_REQUEST"
	CodeOwnerChanged       = "OWNER_CHANGED"
	CodeSourceUnavailable  = "SOURCE_UNAVAILABLE"
	CodeNotFound           = "NOT_FOUND"
	CodeBusy               = "BUSY"
	CodeUnconfirmed        = "UNCONFIRMED"
	CodeWriteUncertain     = "WRITE_UNCERTAIN"
	CodeAlreadyCreated     = "ALREADY_CREATED"
	CodeRedlineReview      = "REDLINE_REVIEW_REQUIRED"
	CodeRedlineUnavailable = "REDLINE_SOURCE_UNAVAILABLE"
	CodeNativeError        = "NATIVE_ERROR"
	// CodeUnavailable: the entry could not be run or answered garbage.
	CodeUnavailable = "UNAVAILABLE"
	// CodeBadInput: refused here before anything was sent.
	CodeBadInput = "BAD_INPUT"
)

// Error is a World Store failure with its machine-readable code.
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

// CodeOf returns the error code of err, or "" when err is not an *Error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Uncertain reports whether err means a write may have been applied.
func Uncertain(err error) bool {
	c := CodeOf(err)
	return c == CodeUnconfirmed || c == CodeWriteUncertain
}

// Owner identifies the World Store account and space a read came from.
// Writes echo it so a switched space never receives them.
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

// Execution is the task's open execution record (running or paused).
type Execution struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt,omitempty"`
}

// Task is agent-deck's projection of one native World Store task.
// Dates are Unix seconds.
type Task struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	Lane         string     `json:"lane"`
	Category     string     `json:"category,omitempty"`
	PlanDate     *int64     `json:"planDate,omitempty"`
	SelectedDate *int64     `json:"selectedDate,omitempty"`
	CompletedAt  *int64     `json:"completedAt,omitempty"`
	ReadOnly     bool       `json:"readOnly,omitempty"`
	PlanText     string     `json:"planText,omitempty"`
	ResultText   string     `json:"resultText,omitempty"`
	Execution    *Execution `json:"execution,omitempty"`
	// LinksRaw is the stored link value exactly as read: the baseline a
	// link/unlink write must quote.
	LinksRaw       string `json:"-"`
	AgentDeckLinks []Link `json:"agentDeckLinks,omitempty"`
	LinksUnknown   bool   `json:"linksUnknown,omitempty"`
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

// Board is one complete read of the tasks and their open executions.
type Board struct {
	Owner     Owner  `json:"owner"`
	UpdatedAt string `json:"updatedAt"`
	Tasks     []Task `json:"tasks"`
	// LinksReady is true once the agent-deck link relation exists.
	LinksReady bool `json:"linksReady"`
	// CanWrite is World Store's own check of write permission on the space.
	CanWrite bool `json:"canWrite"`
	// Running is the execution currently running anywhere in the space
	// (World Store allows one), or nil.
	Running *Execution `json:"running,omitempty"`
	// RunningTaskID is the task Running belongs to.
	RunningTaskID string `json:"runningTaskId,omitempty"`
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

// Runner executes the entry with one request on stdin and returns stdout.
// A non-nil error with non-empty stdout still carries World Store's answer.
type Runner func(ctx context.Context, stdin []byte) (stdout []byte, err error)

// Client talks to World Store through personal-panel's AI data entry.
type Client struct {
	run          Runner
	now          func() time.Time
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// Config is what the client needs from [worldstore] in config.toml.
type Config struct {
	// Bridge is the absolute path to personal-panel's scripts/worldstore-ai.cjs.
	Bridge string
	// Node is the node binary; empty means "node" from PATH.
	Node string
}

// ErrNotConfigured is returned when [worldstore].bridge is unset.
var ErrNotConfigured = errors.New("World Store is not configured: set [worldstore] bridge in config.toml to personal-panel's scripts/worldstore-ai.cjs")

// New validates cfg and returns a client that runs the real entry.
func New(cfg Config) (*Client, error) {
	entry := strings.TrimSpace(cfg.Bridge)
	if entry == "" {
		return nil, ErrNotConfigured
	}
	if strings.HasPrefix(entry, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			entry = filepath.Join(home, entry[2:])
		}
	}
	if !filepath.IsAbs(entry) {
		return nil, fmt.Errorf("[worldstore] bridge must be an absolute path, got %q", cfg.Bridge)
	}
	// The old desktop maintenance bridge answers in a different shape and
	// needs the desktop app's socket, which World Store no longer runs.
	if filepath.Base(entry) == "multica-task-bridge.cjs" {
		return nil, fmt.Errorf("[worldstore] bridge points at the retired desktop bridge; use %s", filepath.Join(filepath.Dir(entry), "worldstore-ai.cjs"))
	}
	if st, err := os.Stat(entry); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("[worldstore] bridge %s is not a readable file", entry)
	}
	node := strings.TrimSpace(cfg.Node)
	if node == "" {
		node = "node"
	}
	return NewWithRunner(execRunner(node, entry)), nil
}

// NewWithRunner builds a client over any runner (tests use a fake).
func NewWithRunner(run Runner) *Client {
	// A cold read may first start the headless service; a write is a native
	// call plus World Store's own read-back.
	return &Client{run: run, now: time.Now, ReadTimeout: 200 * time.Second, WriteTimeout: 6 * time.Minute}
}

func execRunner(node, entry string) Runner {
	return func(ctx context.Context, stdin []byte) ([]byte, error) {
		cmd := exec.CommandContext(ctx, node, entry)
		// Run from the repo so the entry finds its own runtime files.
		cmd.Dir = filepath.Dir(filepath.Dir(entry))
		cmd.Stdin = bytes.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		// stderr carries connection progress only; keep the tail for errors.
		cmd.Stderr = &stderr
		// An entry that never started cannot have written anything; one that
		// started and then died or timed out might have (call decides).
		if err := cmd.Start(); err != nil {
			return nil, &Error{Code: CodeUnavailable, Message: "World Store entry did not start: " + err.Error()}
		}
		err := cmd.Wait()
		if err != nil && stdout.Len() == 0 {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 500 {
				msg = msg[len(msg)-500:]
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
		return &Error{Code: CodeBadInput, Message: err.Error()}
	}
	stdout, runErr := c.run(ctx, body)
	unknown := func(why string) error {
		if write {
			return &Error{Code: CodeUnconfirmed, Message: why + "; the write may have been applied — re-read before acting, do not retry blindly"}
		}
		return &Error{Code: CodeUnavailable, Message: why}
	}
	if runErr != nil && len(bytes.TrimSpace(stdout)) == 0 {
		if CodeOf(runErr) != "" {
			return runErr
		}
		return unknown("World Store entry interrupted: " + runErr.Error())
	}
	var resp struct {
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &resp); err != nil || resp.OK == nil {
		return unknown("World Store answer unreadable")
	}
	if !*resp.OK {
		return &Error{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return unknown("World Store answer unreadable: " + err.Error())
	}
	return nil
}

// nativeRow is one record as the snapshot returns it.
type nativeRow struct {
	ID       string                     `json:"id"`
	Name     string                     `json:"name"`
	Imported bool                       `json:"imported"`
	Fields   map[string]json.RawMessage `json:"fields"`
}

func (r nativeRow) str(name string) string {
	var s string
	if raw, ok := r.Fields[name]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func (r nativeRow) date(name string) *int64 {
	raw, ok := r.Fields[name]
	if !ok {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f <= 0 {
		return nil
	}
	v := int64(f)
	if v >= 100000000000 { // milliseconds from an older writer
		v /= 1000
	}
	return &v
}

func (r nativeRow) ids(name string) []string {
	raw, ok := r.Fields[name]
	if !ok {
		return nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

var statusLanes = map[string]string{"待办": "todo", "待处理": "todo", "进行中": "doing", "暂停": "paused", StatusDone: "done", StatusCancelled: "cancelled"}

// laneOf places a task: closed statuses win, then an open execution, then
// the status itself; statuses agent-deck does not know go to "review".
func laneOf(status string, open *Execution) string {
	if status == StatusDone || status == StatusCancelled {
		return statusLanes[status]
	}
	if open != nil {
		if open.Status == ExecRunning {
			return "doing"
		}
		return "paused"
	}
	if lane, ok := statusLanes[status]; ok {
		return lane
	}
	return "review"
}

// parseLinks decodes the stored link value: ok=false means unknown format.
func parseLinks(raw string) ([]Link, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, true
	}
	var v struct {
		Version int    `json:"version"`
		Links   []Link `json:"links"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil || v.Version != 1 {
		return nil, false
	}
	for _, l := range v.Links {
		if l.Source != Source || l.DeviceID == "" || l.SessionID == "" {
			return nil, false
		}
	}
	return v.Links, true
}

func projectTask(r nativeRow, open *Execution) Task {
	t := Task{
		ID: r.ID, Name: r.Name, Status: r.str(fieldStatus), Category: r.str(fieldCategory),
		PlanDate: r.date(fieldPlanDate), SelectedDate: r.date(fieldSelected), CompletedAt: r.date(fieldCompletedAt),
		ReadOnly: r.Imported, PlanText: r.str(fieldPlanText), ResultText: r.str(fieldResult), Execution: open,
	}
	t.Lane = laneOf(t.Status, open)
	t.LinksRaw = r.str(LinkField)
	links, known := parseLinks(t.LinksRaw)
	t.AgentDeckLinks, t.LinksUnknown = links, !known
	return t
}

// Board reads the snapshot: every task plus the executions that decide
// whether a task is running or paused.
func (c *Client) Board(ctx context.Context) (*Board, error) {
	var snap struct {
		AccountID       string   `json:"accountId"`
		SpaceID         string   `json:"spaceId"`
		UpdatedAt       string   `json:"updatedAt"`
		Pending         bool     `json:"pending"`
		AvailableFields []string `json:"availableFields"`
		CanWrite        bool     `json:"tuiCanWrite"`
		Groups          struct {
			Tasks      *[]nativeRow `json:"tasks"`
			Executions *[]nativeRow `json:"executions"`
		} `json:"groups"`
	}
	if err := c.call(ctx, false, map[string]any{"op": "snapshot"}, &snap); err != nil {
		return nil, err
	}
	if snap.AccountID == "" || snap.SpaceID == "" {
		return nil, &Error{Code: CodeSourceUnavailable, Message: "World Store did not confirm its account and space"}
	}
	// A null group means "not confirmed yet", never "empty".
	if snap.Pending || snap.Groups.Tasks == nil || snap.Groups.Executions == nil {
		return nil, &Error{Code: CodeSourceUnavailable, Message: "World Store is still reading its data; refresh in a moment"}
	}
	board := &Board{Owner: Owner{snap.AccountID, snap.SpaceID}, UpdatedAt: snap.UpdatedAt, CanWrite: snap.CanWrite}
	for _, f := range snap.AvailableFields {
		board.LinksReady = board.LinksReady || f == LinkField
	}
	open := map[string]*Execution{}
	for _, e := range *snap.Groups.Executions {
		st := e.str(execStatus)
		if st != ExecRunning && st != ExecPaused {
			continue
		}
		ex := &Execution{ID: e.ID, Status: st}
		if d := e.date(execStartedAt); d != nil {
			ex.StartedAt = *d
		}
		for _, taskID := range e.ids(execTask) {
			// Prefer the running one, then the most recently started.
			if cur := open[taskID]; cur == nil || (cur.Status != ExecRunning && (st == ExecRunning || ex.StartedAt > cur.StartedAt)) {
				open[taskID] = ex
			}
			if st == ExecRunning {
				board.Running, board.RunningTaskID = ex, taskID
			}
		}
	}
	for _, r := range *snap.Groups.Tasks {
		board.Tasks = append(board.Tasks, projectTask(r, open[r.ID]))
	}
	return board, nil
}

// Get reads one task (without execution state) for its plan text.
func (c *Client) Get(ctx context.Context, owner Owner, id string) (*Task, error) {
	var resp struct {
		Task nativeRow `json:"task"`
	}
	if err := c.call(ctx, false, map[string]any{"op": "task.get", "owner": owner, "id": id}, &resp); err != nil {
		return nil, err
	}
	t := projectTask(resp.Task, nil)
	return &t, nil
}

func (c *Client) action(ctx context.Context, owner Owner, payload map[string]any) error {
	payload["now"] = c.now().Unix()
	return c.call(ctx, true, map[string]any{"op": "task.action", "owner": owner, "confirm": true, "payload": payload}, nil)
}

// NewOperationID returns a fresh id for a start request. Reusing the same id
// for a retry lets World Store recognise the execution it already created.
func NewOperationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "agentdeck-" + hex.EncodeToString(b)
}

// Start begins work on a task: World Store creates a running execution.
// It may refuse with REDLINE_REVIEW_REQUIRED when red lines must be reviewed
// first; agent-deck cannot do that review, World Store's TUI can.
func (c *Client) Start(ctx context.Context, owner Owner, t Task, operationID string) error {
	if t.Status == StatusDone || t.Status == StatusCancelled {
		return &Error{Code: CodeBadInput, Message: "task is already closed"}
	}
	if t.Execution != nil {
		return &Error{Code: CodeBadInput, Message: "task already has an open execution; pause or resume it instead"}
	}
	return c.action(ctx, owner, map[string]any{"mode": "start", "taskId": t.ID, "expectedTaskStatus": t.Status, "operationId": operationID})
}

// Pause pauses the task's running execution.
func (c *Client) Pause(ctx context.Context, owner Owner, t Task) error {
	if t.Execution == nil || t.Execution.Status != ExecRunning {
		return &Error{Code: CodeBadInput, Message: "task has no running execution"}
	}
	return c.action(ctx, owner, map[string]any{"mode": "pause", "executionId": t.Execution.ID, "expectedStatus": ExecRunning})
}

// Resume continues the task's paused execution.
func (c *Client) Resume(ctx context.Context, owner Owner, t Task) error {
	if t.Execution == nil || t.Execution.Status != ExecPaused {
		return &Error{Code: CodeBadInput, Message: "task has no paused execution"}
	}
	return c.action(ctx, owner, map[string]any{"mode": "resume", "executionId": t.Execution.ID, "expectedStatus": ExecPaused})
}

// Complete marks a task done; World Store also closes its open executions.
// resultText is optional ("" leaves the stored result as is).
func (c *Client) Complete(ctx context.Context, owner Owner, t Task, resultText string) error {
	switch t.Status {
	case StatusDone, StatusCancelled:
		return &Error{Code: CodeBadInput, Message: "task is already closed"}
	case StatusCycle:
		return &Error{Code: CodeBadInput, Message: "recurring tasks end a round in World Store instead of completing"}
	}
	payload := map[string]any{"mode": "complete", "taskId": t.ID, "expectedTaskStatus": t.Status}
	if strings.TrimSpace(resultText) != "" {
		payload["result"], payload["expectedResult"] = resultText, t.ResultText
	}
	return c.action(ctx, owner, payload)
}

// linkUnsupported turns "this World Store does not know the op" into a
// message the user can act on.
func linkUnsupported(err error) error {
	if CodeOf(err) == CodeInvalidRequest {
		return &Error{Code: CodeInvalidRequest, Message: "this World Store does not support agent-deck session links yet (task.link); update personal-panel"}
	}
	return err
}

// InspectLinks reports whether the agent-deck link relation is ready.
func (c *Client) InspectLinks(ctx context.Context) (*LinkSchema, error) {
	var s LinkSchema
	if err := c.call(ctx, false, map[string]any{"op": "task.links.inspect", "source": Source}, &s); err != nil {
		return nil, linkUnsupported(err)
	}
	return &s, nil
}

// InitializeLinks creates and attaches the agent-deck link relation. It
// changes the World Store schema, so callers need the user's explicit yes.
func (c *Client) InitializeLinks(ctx context.Context, owner Owner) error {
	err := c.call(ctx, true, map[string]any{"op": "task.links.initialize", "source": Source, "owner": owner, "confirm": true}, nil)
	return linkUnsupported(err)
}

// Link records sessionID (on deviceID) against the task.
func (c *Client) Link(ctx context.Context, owner Owner, t Task, deviceID, sessionID string) ([]Link, error) {
	return c.link(ctx, "task.link", owner, t, deviceID, sessionID)
}

// Unlink removes a recorded session from the task.
func (c *Client) Unlink(ctx context.Context, owner Owner, t Task, deviceID, sessionID string) ([]Link, error) {
	return c.link(ctx, "task.unlink", owner, t, deviceID, sessionID)
}

func (c *Client) link(ctx context.Context, op string, owner Owner, t Task, deviceID, sessionID string) ([]Link, error) {
	if t.LinksUnknown {
		return nil, &Error{Code: CodeBadInput, Message: "stored links are in an unknown format; check the task in World Store"}
	}
	var resp struct {
		Links []Link `json:"links"`
	}
	req := map[string]any{
		"op": op, "owner": owner, "confirm": true, "id": t.ID,
		"payload":  map[string]any{"source": Source, "deviceId": deviceID, "sessionId": sessionID, "now": c.now().Unix()},
		"expected": map[string]any{LinkField: t.LinksRaw},
	}
	if err := c.call(ctx, true, req, &resp); err != nil {
		return nil, linkUnsupported(err)
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
