package ui

// matrix/hardened: World Store board "send to an existing session" (key d).
// The user works session by session; a long-lived session they trust is
// their "agent". The picker lists this deck's sessions by recent activity;
// the confirmed dispatch sends the task as a message (queued: the session
// may be busy), links the task to the session unless already linked, and
// starts the task in World Store. Multica dispatch is a separate path.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

type kanbanPicker struct {
	task      worldstore.Task
	all       []kanbanSession
	filter    string
	filtering bool
	cursor    int
	scroll    int
}

func (pk *kanbanPicker) shown() []kanbanSession {
	f := strings.ToLower(strings.TrimSpace(pk.filter))
	if f == "" {
		return pk.all
	}
	var out []kanbanSession
	for _, s := range pk.all {
		if strings.Contains(strings.ToLower(s.Title+" "+s.Group+" "+s.Path), f) {
			out = append(out, s)
		}
	}
	return out
}

// openPicker checks the selected task can be dispatched and lists sessions.
func (p *KanbanPanel) openPicker() {
	t := p.selected()
	switch {
	case p.env == nil:
		p.notice = "World Store is not configured."
		return
	case p.busy || p.loading:
		p.notice = "Busy — wait for the current read or write."
		return
	case p.locked:
		p.notice = "An earlier write is unconfirmed. Press r and check the task first."
		return
	case t == nil:
		p.notice = "No task selected."
		return
	case p.board != nil && !p.board.CanWrite:
		p.notice = "World Store reports this space as read-only."
		return
	case p.sessions == nil:
		p.notice = "Session list unavailable here."
		return
	}
	if err := worldstore.CheckDispatch(*t, p.board.LinksReady); err != nil {
		p.notice = kanbanErrText(err)
		return
	}
	list := p.sessions()
	if len(list) == 0 {
		p.notice = "No sessions in this deck to send to."
		return
	}
	p.picker = &kanbanPicker{task: *t, all: list}
}

func kanbanErrText(err error) string {
	var e *worldstore.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func (p *KanbanPanel) updatePicker(key tea.KeyMsg) tea.Cmd {
	pk := p.picker
	k := key.String()
	if pk.filtering {
		switch {
		case k == "esc":
			pk.filter, pk.filtering = "", false
		case k == "enter":
			pk.filtering = false
		case k == "backspace":
			if r := []rune(pk.filter); len(r) > 0 {
				pk.filter = string(r[:len(r)-1])
			}
		case key.Type == tea.KeyRunes || key.Type == tea.KeySpace:
			text := string(key.Runes)
			if key.Type == tea.KeySpace {
				text = " "
			}
			if len([]rune(pk.filter)) < 60 {
				pk.filter += kanbanClean(text)
			}
		}
		pk.cursor, pk.scroll = 0, 0
		return nil
	}
	shown := pk.shown()
	switch k {
	case "esc", "q":
		if pk.filter != "" {
			pk.filter, pk.cursor, pk.scroll = "", 0, 0
			return nil
		}
		p.picker, p.notice = nil, "Cancelled; nothing sent."
	case "j", "down":
		pk.cursor = min(len(shown)-1, pk.cursor+1)
	case "k", "up":
		pk.cursor = max(0, pk.cursor-1)
	case "g", "home":
		pk.cursor = 0
	case "G", "end":
		pk.cursor = max(0, len(shown)-1)
	case "/":
		pk.filtering = true
	case "enter":
		if pk.cursor < 0 || pk.cursor >= len(shown) {
			return nil
		}
		target := shown[pk.cursor]
		p.picker = nil
		p.confirm = &kanbanConfirm{kind: "dispatch", task: pk.task, target: &target, operationID: worldstore.NewOperationID()}
	}
	return nil
}

func (p *KanbanPanel) dispatchConfirmText(c *kanbanConfirm) string {
	name, to := kanbanClean(c.task.Name), kanbanClean(c.target.Title)
	if p.env != nil && c.task.LinkedTo(p.env.device, c.target.ID) {
		return fmt.Sprintf("Send %q to %q again (already linked: message only, then start if it is not running)", name, to)
	}
	return fmt.Sprintf("Send %q to session %q, link them, and start the task in World Store", name, to)
}

// dispatchMsg runs the confirmed dispatch off the UI goroutine.
func dispatchMsg(ctx context.Context, env kanbanEnv, owner worldstore.Owner, task worldstore.Task, target kanbanSession, operationID string, running *worldstore.Execution) tea.Msg {
	// The plan text the message carries is read fresh; status, links and
	// execution stay as the board read them (the baselines of the writes).
	if full, err := env.client.Get(ctx, owner, task.ID); err == nil {
		task.PlanText, task.Category = full.PlanText, full.Category
	} else {
		return kanbanWrittenMsg{err: fmt.Errorf("nothing sent: %w", err)}
	}
	send := env.send
	if send == nil {
		send = func(ctx context.Context, id, text string) error {
			return worldstore.SendToSession(ctx, env.exe, env.profile, id, text)
		}
	}
	sendCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := env.client.Dispatch(sendCtx, worldstore.DispatchRequest{
		Owner: owner, Task: task, DeviceID: env.device, SessionID: target.ID, OperationID: operationID, Running: running,
		Send: func(ctx context.Context, text string) error { return send(ctx, target.ID, text) },
	})
	if err != nil {
		return kanbanWrittenMsg{err: err}
	}
	notice := "Sent to " + kanbanClean(target.Title) + ": " + res.Done()
	if res.StartSkipped != "" {
		notice += " (not started: " + res.StartSkipped + ")"
	}
	return kanbanWrittenMsg{notice: notice}
}

func kanbanAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (p *KanbanPanel) renderPicker(inner, bodyH int) string {
	pk := p.picker
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	sel := lipgloss.NewStyle().Background(ColorSurface).Foreground(ColorText).Bold(true)
	head := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	shown := pk.shown()
	var sb strings.Builder
	title := "Send 「" + kanbanClean(pk.task.Name) + "」 to which session?"
	if pk.filter != "" || pk.filtering {
		title += "  filter: " + pk.filter
		if pk.filtering {
			title += "▏"
		}
	}
	sb.WriteString(head.Render(kanbanFit(title, inner)) + "\n")
	rows := max(1, bodyH)
	if pk.cursor < pk.scroll {
		pk.scroll = pk.cursor
	}
	if pk.cursor >= pk.scroll+rows {
		pk.scroll = pk.cursor - rows + 1
	}
	groupW := min(24, max(10, inner/4))
	for line := 0; line < rows; line++ {
		i := pk.scroll + line
		if i >= len(shown) {
			if i == 0 {
				sb.WriteString(dim.Render(kanbanFit("  No session matches the filter.", inner)))
			}
			sb.WriteString("\n")
			continue
		}
		s := shown[i]
		mark := " "
		if p.env != nil && pk.task.LinkedTo(p.env.device, s.ID) {
			mark = "↔"
		}
		status := fmt.Sprintf("%-8s %4s", string(s.Status), kanbanAge(s.LastActivity))
		titleW := max(8, inner-groupW-len(status)-6)
		text := " " + mark + " " + kanbanFit(s.Title, titleW) + " " + kanbanFit(s.Group, groupW) + " " + status
		dot := lipgloss.NewStyle().Foreground(kanbanStatusColor(s.Status)).Render("●")
		if i == pk.cursor {
			sb.WriteString(dot + sel.Render(kanbanFit("▸"+text[1:], inner-1)) + "\n")
		} else {
			sb.WriteString(dot + kanbanFit(text, inner-1) + "\n")
		}
	}
	return sb.String()
}
