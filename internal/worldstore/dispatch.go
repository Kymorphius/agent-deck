package worldstore

// matrix/hardened: hand a World Store task to an existing agent-deck session.
// A long-lived session the user trusts plays the role a Multica agent plays
// elsewhere: the task goes to it as a message, the task records the session,
// and the task starts in World Store. Multica dispatch is untouched.

import (
	"context"
	"errors"
	"strings"
)

// DispatchPrompt is the message a session receives for a dispatched task.
func DispatchPrompt(t Task) string {
	var sb strings.Builder
	sb.WriteString("World Store 任务：")
	sb.WriteString(t.Name)
	if t.Category != "" {
		sb.WriteString("\n分类：" + t.Category)
	}
	if plan := strings.TrimSpace(t.PlanText); plan != "" {
		sb.WriteString("\n计划说明：\n" + plan)
	}
	sb.WriteString("\n\n这个任务交给你处理。请先确认目标和范围，再开始处理。" +
		"做完时回报：做了什么、结果如何、还有什么没收尾；需要我决定的事先问我。")
	return sb.String()
}

// DispatchRequest is one dispatch of a task to a session.
type DispatchRequest struct {
	Owner     Owner
	Task      Task
	DeviceID  string
	SessionID string
	// Send queues the message for the session (agent-deck session send
	// --queue, text on stdin): the session may be busy.
	Send func(ctx context.Context, text string) error
	// OperationID is fixed when the user confirms, so a start is one World
	// Store operation however it ends.
	OperationID string
	// Running is the execution running anywhere in the space, if any:
	// World Store allows one, so the start is skipped (not failed) then.
	Running *Execution
}

// DispatchResult records which steps ran.
type DispatchResult struct {
	Sent          bool
	Linked        bool
	AlreadyLinked bool
	Started       bool
	// StartSkipped explains a start that was not attempted.
	StartSkipped string
}

// Done says which steps completed, for messages.
func (r DispatchResult) Done() string {
	var steps []string
	if r.Sent {
		steps = append(steps, "message sent")
	}
	switch {
	case r.Linked:
		steps = append(steps, "linked")
	case r.AlreadyLinked:
		steps = append(steps, "already linked")
	}
	if r.Started {
		steps = append(steps, "started")
	}
	if len(steps) == 0 {
		return "nothing done"
	}
	return strings.Join(steps, ", ")
}

// DispatchError is a failed step after the earlier ones succeeded. It wraps
// the step's error, so Uncertain and CodeOf still see World Store's code.
type DispatchError struct {
	Step   string // send, link or start
	Result DispatchResult
	Err    error
}

func (e *DispatchError) Error() string {
	return e.Result.Done() + "; " + e.Step + " failed: " + e.Err.Error()
}

func (e *DispatchError) Unwrap() error { return e.Err }

// CheckDispatch refuses a dispatch before anything is sent.
func CheckDispatch(t Task, linksReady bool) error {
	switch {
	case t.ReadOnly:
		return &Error{Code: CodeBadInput, Message: "imported task is read-only"}
	case t.Status == StatusDone || t.Status == StatusCancelled:
		return &Error{Code: CodeBadInput, Message: "task is already " + t.Status}
	case !linksReady:
		return &Error{Code: CodeBadInput, Message: "agent-deck link field not set up in World Store: run `agent-deck tasks init --yes` once"}
	case t.LinksUnknown:
		return &Error{Code: CodeBadInput, Message: "this task's stored links are in an unknown format; check it in World Store"}
	}
	return nil
}

// Dispatch sends the task to the session, links them (unless already
// linked) and starts the task (unless it already has an execution, or
// another task's execution is running). Steps run in that order and stop at
// the first failure; nothing is retried.
func (c *Client) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	var r DispatchResult
	if err := CheckDispatch(req.Task, true); err != nil {
		return r, err
	}
	if req.Send == nil || req.SessionID == "" {
		return r, &Error{Code: CodeBadInput, Message: "no session to send to"}
	}
	if err := req.Send(ctx, DispatchPrompt(req.Task)); err != nil {
		return r, &DispatchError{Step: "send", Result: r, Err: err}
	}
	r.Sent = true
	if req.Task.LinkedTo(req.DeviceID, req.SessionID) {
		r.AlreadyLinked = true
	} else {
		if _, err := c.Link(ctx, req.Owner, req.Task, req.DeviceID, req.SessionID); err != nil {
			return r, &DispatchError{Step: "link", Result: r, Err: err}
		}
		r.Linked = true
	}
	switch {
	case req.Task.Execution != nil:
		r.StartSkipped = "the task already has an execution (" + req.Task.Execution.Status + ")"
	case req.Running != nil:
		r.StartSkipped = "another task's execution is running"
	case req.Task.Status == StatusCycle:
		r.StartSkipped = "recurring tasks start rounds in World Store"
	default:
		if err := c.Start(ctx, req.Owner, req.Task, req.OperationID); err != nil {
			if CodeOf(err) == CodeRedlineReview || CodeOf(err) == CodeRedlineUnavailable {
				err = &Error{Code: CodeOf(err), Message: "red lines must be reviewed first: start this task in World Store's TUI"}
			}
			return r, &DispatchError{Step: "start", Result: r, Err: err}
		}
		r.Started = true
	}
	return r, nil
}

// IsDispatchError reports whether err came from a step after the first.
func IsDispatchError(err error) (*DispatchError, bool) {
	var d *DispatchError
	ok := errors.As(err, &d)
	return d, ok
}
