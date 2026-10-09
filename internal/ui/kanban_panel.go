package ui

// matrix/hardened: World Store task board (alt+t). Tasks live only in World
// Store; this panel reads them when it opens or on `r`, and writes only after
// the user confirms one action. A write whose outcome is unknown locks every
// further write until a fresh read (never retried automatically).

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

// kanbanSession is what the board needs to know about an agent-deck session.
type kanbanSession struct {
	ID     string
	Title  string
	Path   string
	Group  string
	Status session.Status
}

// kanbanEnv is everything the panel needs to reach World Store and launch.
type kanbanEnv struct {
	client  *worldstore.Client
	device  string
	exe     string
	profile string
}

type kanbanConfirm struct {
	kind    string // move, done, link, unlink, launch
	task    worldstore.Task
	choices []string
	choice  int
}

type kanbanLoadedMsg struct {
	seq   int
	board *worldstore.Board
	err   error
}

type kanbanWrittenMsg struct {
	notice string
	err    error
}

type kanbanJumpMsg struct{ sessionID string }

// KanbanPanel is the World Store task board overlay.
type KanbanPanel struct {
	visible bool
	width   int
	height  int

	env      *kanbanEnv
	setupErr string
	// context is the session selected when the board opened: the one `L`
	// links and whose path `n` launches in.
	context *kanbanSession
	lookup  func(id string) (kanbanSession, bool)

	board  *worldstore.Board
	cols   []worldstore.Column
	col    int
	rows   []int
	scroll []int

	seq     int
	loading bool
	busy    bool
	locked  bool
	notice  string
	err     string
	confirm *kanbanConfirm

	filter        string
	filterEditing bool
}

// NewKanbanPanel creates a hidden board.
func NewKanbanPanel() *KanbanPanel { return &KanbanPanel{} }

// IsVisible reports whether the board is shown.
func (p *KanbanPanel) IsVisible() bool { return p != nil && p.visible }

// SetSize records the terminal size.
func (p *KanbanPanel) SetSize(w, h int) {
	if p == nil {
		return
	}
	p.width, p.height = w, h
}

// Hide closes the board.
func (p *KanbanPanel) Hide() {
	if p == nil {
		return
	}
	p.visible = false
	p.confirm = nil
	p.filterEditing = false
}

// Open shows the board and starts a read. env is nil when World Store is
// not configured; setupErr then says why.
func (p *KanbanPanel) Open(env *kanbanEnv, setupErr string, ctx *kanbanSession, lookup func(string) (kanbanSession, bool)) tea.Cmd {
	p.visible = true
	p.env, p.setupErr, p.context, p.lookup = env, setupErr, ctx, lookup
	p.confirm, p.filterEditing, p.notice, p.err = nil, false, "", ""
	return p.reload()
}

func (p *KanbanPanel) reload() tea.Cmd {
	if p.env == nil || p.loading {
		return nil
	}
	p.loading = true
	p.seq++
	seq, client := p.seq, p.env.client
	return func() tea.Msg {
		board, err := client.Board(context.Background())
		return kanbanLoadedMsg{seq: seq, board: board, err: err}
	}
}

func (p *KanbanPanel) handleLoaded(msg kanbanLoadedMsg) {
	if msg.seq != p.seq {
		return
	}
	p.loading = false
	if msg.err != nil {
		p.err = kanbanErrorText(msg.err)
		if worldstore.CodeOf(msg.err) == worldstore.CodeOwnerChanged {
			p.board, p.cols = nil, nil
		}
		return
	}
	if p.board != nil && p.board.Owner != msg.board.Owner {
		p.notice = "World Store account or space changed; showing the current one"
	}
	selected := p.selectedID()
	p.board, p.locked, p.err = msg.board, false, ""
	p.rebuild(selected)
}

func (p *KanbanPanel) handleWritten(msg kanbanWrittenMsg) tea.Cmd {
	p.busy = false
	if msg.err != nil {
		p.err = kanbanErrorText(msg.err)
		switch worldstore.CodeOf(msg.err) {
		case worldstore.CodeWriteUncertain:
			p.locked = true
			return nil
		case worldstore.CodeConflict, worldstore.CodeNotFound, worldstore.CodeOwnerChanged:
			return p.reload()
		}
		return nil
	}
	p.notice = msg.notice
	return p.reload()
}

func kanbanErrorText(err error) string {
	switch worldstore.CodeOf(err) {
	case worldstore.CodeWriteUncertain:
		return "Write not confirmed — it may have been applied. Press r and check before doing anything else. (" + err.Error() + ")"
	case worldstore.CodeConflict:
		return "Task changed in World Store since it was read; reloaded, review and try again."
	case worldstore.CodeSourceUnavailable:
		if strings.Contains(err.Error(), "关联") {
			return "agent-deck link field not set up in World Store: run `agent-deck tasks init-links --yes` once."
		}
	case worldstore.CodeUnavailable:
		return "World Store unreachable — is the app running with one window open? (" + err.Error() + ")"
	}
	return err.Error()
}

// rebuild regroups the board and keeps the cursor on the same task.
func (p *KanbanPanel) rebuild(selectedID string) {
	if p.board == nil {
		p.cols = nil
		return
	}
	p.cols = worldstore.Columns(p.board.Tasks, p.filter)
	if len(p.rows) != len(p.cols) {
		p.rows, p.scroll = make([]int, len(p.cols)), make([]int, len(p.cols))
	}
	if p.col >= len(p.cols) {
		p.col = 0
	}
	for c := range p.cols {
		for i, t := range p.cols[c].Tasks {
			if t.ID == selectedID {
				p.col, p.rows[c] = c, i
			}
		}
		if p.rows[c] >= len(p.cols[c].Tasks) {
			p.rows[c] = max(0, len(p.cols[c].Tasks)-1)
		}
	}
	// A filter can empty the current column; land on the first one with tasks.
	if len(p.cols[p.col].Tasks) == 0 {
		for c := range p.cols {
			if len(p.cols[c].Tasks) > 0 {
				p.col = c
				break
			}
		}
	}
}

func (p *KanbanPanel) selected() *worldstore.Task {
	if p.col < 0 || p.col >= len(p.cols) {
		return nil
	}
	ts := p.cols[p.col].Tasks
	if r := p.rows[p.col]; r >= 0 && r < len(ts) {
		return &ts[r]
	}
	return nil
}

func (p *KanbanPanel) selectedID() string {
	if t := p.selected(); t != nil {
		return t.ID
	}
	return ""
}

// Update handles keys and the panel's own async results.
func (p *KanbanPanel) Update(msg tea.Msg) (*KanbanPanel, tea.Cmd) {
	if p == nil || !p.visible {
		return p, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	k := key.String()

	if p.confirm != nil {
		switch k {
		case "esc", "q":
			p.confirm, p.notice = nil, "Cancelled; nothing written."
		case "enter":
			return p, p.execute()
		case "left", "h", "shift+tab":
			if n := len(p.confirm.choices); n > 0 {
				p.confirm.choice = (p.confirm.choice + n - 1) % n
			}
		case "right", "l", "tab":
			if n := len(p.confirm.choices); n > 0 {
				p.confirm.choice = (p.confirm.choice + 1) % n
			}
		}
		return p, nil
	}

	if p.filterEditing {
		selected := p.selectedID()
		switch {
		case k == "esc":
			p.filter, p.filterEditing = "", false
		case k == "enter":
			p.filterEditing = false
		case k == "backspace":
			if r := []rune(p.filter); len(r) > 0 {
				p.filter = string(r[:len(r)-1])
			}
		case key.Type == tea.KeyRunes || key.Type == tea.KeySpace:
			text := string(key.Runes)
			if key.Type == tea.KeySpace {
				text = " "
			}
			if len([]rune(p.filter)) < 60 {
				p.filter += kanbanClean(text)
			}
		}
		p.rebuild(selected)
		return p, nil
	}

	p.notice = ""
	switch k {
	case "esc":
		if p.filter != "" {
			selected := p.selectedID()
			p.filter = ""
			p.rebuild(selected)
			return p, nil
		}
		p.Hide()
	case "q", "alt+t":
		p.Hide()
	case "h", "left":
		p.moveCol(-1)
	case "l", "right":
		p.moveCol(1)
	case "j", "down":
		p.moveRow(1)
	case "k", "up":
		p.moveRow(-1)
	case "g", "home":
		p.moveRow(-1 << 20)
	case "G", "end":
		p.moveRow(1 << 20)
	case "r":
		if p.busy {
			p.notice = "Waiting for the write to finish."
			return p, nil
		}
		p.err = ""
		return p, p.reload()
	case "/":
		p.filterEditing = true
	case "m", "x", "L", "U", "n":
		p.startConfirm(k)
	case "enter":
		return p, p.jump()
	}
	return p, nil
}

func (p *KanbanPanel) moveCol(d int) {
	if len(p.cols) == 0 {
		return
	}
	p.col = (p.col + d + len(p.cols)) % len(p.cols)
}

func (p *KanbanPanel) moveRow(d int) {
	if p.col >= len(p.cols) {
		return
	}
	n := len(p.cols[p.col].Tasks)
	p.rows[p.col] = max(0, min(n-1, p.rows[p.col]+d))
}

// startConfirm checks an action is allowed and opens its confirmation.
func (p *KanbanPanel) startConfirm(k string) {
	fail := func(s string) { p.notice = s }
	t := p.selected()
	switch {
	case p.env == nil:
		fail("World Store is not configured.")
		return
	case p.busy || p.loading:
		fail("Busy — wait for the current read or write.")
		return
	case p.locked:
		fail("An earlier write is unconfirmed. Press r and check the task first.")
		return
	case t == nil:
		fail("No task selected.")
		return
	case t.ReadOnly:
		fail("Imported task — read-only here; edit it in World Store.")
		return
	}
	c := &kanbanConfirm{task: *t}
	switch k {
	case "m":
		if t.Lane == "review" || t.Lane == "done" {
			fail("Status " + t.Status + " can only be changed in World Store.")
			return
		}
		c.kind = "move"
		for _, s := range worldstore.MovableStatuses {
			if s != t.Status {
				c.choices = append(c.choices, s)
			}
		}
	case "x":
		if t.Lane == "review" || t.Lane == "done" {
			fail("Complete " + t.Status + " tasks in World Store.")
			return
		}
		c.kind = "done"
	case "L", "U":
		if p.context == nil {
			fail("Open the board with a session selected to link it.")
			return
		}
		if t.LinksUnknown {
			fail("This task's stored links are in an unknown format; check it in World Store.")
			return
		}
		linked := t.LinkedTo(p.env.device, p.context.ID)
		if k == "L" && linked {
			fail("Already linked to " + p.context.Title + ".")
			return
		}
		if k == "U" && !linked {
			fail(p.context.Title + " is not linked to this task.")
			return
		}
		c.kind = map[string]string{"L": "link", "U": "unlink"}[k]
	case "n":
		if t.Lane == "done" {
			fail("Task is already done.")
			return
		}
		c.kind = "launch"
	}
	p.confirm = c
}

func (p *KanbanPanel) confirmText() string {
	c := p.confirm
	name := kanbanClean(c.task.Name)
	switch c.kind {
	case "move":
		var opts []string
		for i, s := range c.choices {
			if i == c.choice {
				s = "[" + s + "]"
			}
			opts = append(opts, s)
		}
		return fmt.Sprintf("Move %q from %s to  %s  (←/→ choose)", name, c.task.Status, strings.Join(opts, " "))
	case "done":
		return fmt.Sprintf("Mark %q complete (records completion time)", name)
	case "link":
		return fmt.Sprintf("Link session %q to %q", p.context.Title, name)
	case "unlink":
		return fmt.Sprintf("Unlink session %q from %q", p.context.Title, name)
	case "launch":
		where := "the default path"
		if p.context != nil && p.context.Path != "" {
			where = p.context.Path
		}
		return fmt.Sprintf("Start a claude session on %q in %s and link it", name, where)
	}
	return ""
}

// execute runs the confirmed action in the background.
func (p *KanbanPanel) execute() tea.Cmd {
	c := p.confirm
	p.confirm = nil
	if c == nil || p.env == nil || p.board == nil || p.busy || p.locked {
		return nil
	}
	p.busy, p.err = true, ""
	env, owner, task := *p.env, p.board.Owner, c.task
	var ctxSession kanbanSession
	if p.context != nil {
		ctxSession = *p.context
	}
	choice := ""
	if len(c.choices) > 0 {
		choice = c.choices[c.choice]
	}
	return func() tea.Msg {
		bg := context.Background()
		name := kanbanClean(task.Name)
		switch c.kind {
		case "move":
			if _, err := env.client.SetStatus(bg, owner, task, choice); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			return kanbanWrittenMsg{notice: name + " → " + choice}
		case "done":
			if _, err := env.client.Complete(bg, owner, task, ""); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			return kanbanWrittenMsg{notice: name + " → " + worldstore.StatusDone}
		case "link":
			if _, err := env.client.Link(bg, owner, task, env.device, ctxSession.ID); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			return kanbanWrittenMsg{notice: "Linked " + ctxSession.Title}
		case "unlink":
			if _, err := env.client.Unlink(bg, owner, task, env.device, ctxSession.ID); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			return kanbanWrittenMsg{notice: "Unlinked " + ctxSession.Title}
		case "launch":
			full, err := env.client.Get(bg, owner, task.ID)
			if err != nil {
				return kanbanWrittenMsg{err: err}
			}
			ctx, cancel := context.WithTimeout(bg, 5*time.Minute)
			defer cancel()
			id, err := worldstore.Launch(ctx, worldstore.LaunchRequest{
				Exe: env.exe, Profile: env.profile, Path: ctxSession.Path, Group: ctxSession.Group,
				Title: name, Prompt: worldstore.LaunchPrompt(*full), TaskID: task.ID, NoParent: true,
			})
			if err != nil {
				return kanbanWrittenMsg{err: err}
			}
			if _, err := env.client.Link(bg, owner, task, env.device, id); err != nil {
				return kanbanWrittenMsg{err: fmt.Errorf("session %s started but not linked: %w", id, err)}
			}
			return kanbanWrittenMsg{notice: "Started and linked session " + id}
		}
		return kanbanWrittenMsg{}
	}
}

// jump returns to the first linked session that exists in this deck.
func (p *KanbanPanel) jump() tea.Cmd {
	t := p.selected()
	if t == nil || p.env == nil || p.lookup == nil {
		return nil
	}
	for _, l := range t.AgentDeckLinks {
		if l.DeviceID != p.env.device {
			continue
		}
		if s, ok := p.lookup(l.SessionID); ok {
			id := s.ID
			return func() tea.Msg { return kanbanJumpMsg{sessionID: id} }
		}
	}
	if len(t.AgentDeckLinks) > 0 {
		p.notice = "Linked sessions are not in this deck (another machine, profile, or deleted)."
	} else {
		p.notice = "No linked session. L links the current session, n starts one."
	}
	return nil
}

// kanbanClean drops control characters so stored text cannot move the cursor.
func kanbanClean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func kanbanFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(kanbanClean(s), w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

func kanbanStatusColor(st session.Status) lipgloss.Color {
	switch st {
	case session.StatusRunning, session.StatusStarting:
		return ColorGreen
	case session.StatusWaiting:
		return ColorYellow
	case session.StatusError:
		return ColorRed
	}
	return ColorTextDim
}

// linkMarker is the card's session dot: the liveliest linked session's color,
// hollow when no linked session is in this deck.
func (p *KanbanPanel) linkMarker(t worldstore.Task) (string, lipgloss.Color) {
	if t.LinksUnknown {
		return "?", ColorRed
	}
	if len(t.AgentDeckLinks) == 0 {
		return " ", ColorTextDim
	}
	rank := map[session.Status]int{session.StatusRunning: 4, session.StatusStarting: 4, session.StatusWaiting: 3, session.StatusError: 2}
	best, found := session.Status(""), false
	for _, l := range t.AgentDeckLinks {
		if p.env == nil || l.DeviceID != p.env.device || p.lookup == nil {
			continue
		}
		if s, ok := p.lookup(l.SessionID); ok {
			if !found || rank[s.Status] > rank[best] {
				best = s.Status
			}
			found = true
		}
	}
	if !found {
		return "○", ColorTextDim
	}
	return "●", kanbanStatusColor(best)
}

// View renders the board.
func (p *KanbanPanel) View() string {
	if p == nil || !p.visible {
		return ""
	}
	width := max(40, p.width-2)
	inner := width - 2
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ColorBorder).Padding(0, 1).Width(width)
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	red := lipgloss.NewStyle().Foreground(ColorRed)
	rule := dim.Render(strings.Repeat("─", inner))

	var sb strings.Builder
	head := "World Store"
	if p.board != nil {
		head += fmt.Sprintf(" · %d tasks", len(p.board.Tasks))
	}
	if p.context != nil {
		head += " · session: " + kanbanClean(p.context.Title)
	}
	if p.loading {
		head += " · reading…"
	}
	if p.busy {
		head += " · writing…"
	}
	sb.WriteString(title.Render("TASKS") + "  " + dim.Render(kanbanFit(head, inner-7)) + "\n" + rule + "\n")

	if p.env == nil {
		sb.WriteString(red.Render("World Store is not configured.") + "\n")
		for _, line := range strings.Split(p.setupErr, "\n") {
			sb.WriteString(dim.Render(kanbanFit(line, inner)) + "\n")
		}
		sb.WriteString(rule + "\n" + dim.Render("[Esc] Close"))
		return box.Render(sb.String())
	}

	// Rows: border 2, header 2, column titles 1, rule 1, detail 3, rule 1, footer 2.
	bodyH := max(3, p.height-12)
	if len(p.cols) == 0 {
		msg := "Reading World Store…"
		if !p.loading {
			msg = "No board loaded. Press r to read."
		}
		sb.WriteString(dim.Render(msg) + "\n" + strings.Repeat("\n", bodyH))
	} else {
		sb.WriteString(p.renderColumns(inner, bodyH))
	}
	sb.WriteString(rule + "\n")
	sb.WriteString(p.renderDetail(inner))
	sb.WriteString(rule + "\n")
	sb.WriteString(p.renderFooter(inner))
	return box.Render(sb.String())
}

func (p *KanbanPanel) renderColumns(inner, bodyH int) string {
	n := len(p.cols)
	colW := (inner - (n - 1)) / n
	narrow := colW < 16
	shown := make([]int, 0, n)
	if narrow {
		colW = inner
		shown = append(shown, p.col)
	} else {
		for c := range p.cols {
			shown = append(shown, c)
		}
	}
	sep := lipgloss.NewStyle().Foreground(ColorBorder).Render("│")
	headActive := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	headIdle := lipgloss.NewStyle().Bold(true).Foreground(ColorText)
	sel := lipgloss.NewStyle().Background(ColorSurface).Foreground(ColorText).Bold(true)
	normal := lipgloss.NewStyle().Foreground(ColorText)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)

	var sb strings.Builder
	if narrow {
		var tabs []string
		for c, col := range p.cols {
			label := fmt.Sprintf("%s %d", col.Title, len(col.Tasks)+col.Hidden)
			if c == p.col {
				tabs = append(tabs, headActive.Render("["+label+"]"))
			} else {
				tabs = append(tabs, dim.Render(label))
			}
		}
		sb.WriteString(strings.Join(tabs, " ") + "\n")
	} else {
		var heads []string
		for _, c := range shown {
			col := p.cols[c]
			style := headIdle
			if c == p.col {
				style = headActive
			}
			heads = append(heads, style.Render(kanbanFit(fmt.Sprintf("%s (%d)", col.Title, len(col.Tasks)+col.Hidden), colW)))
		}
		sb.WriteString(strings.Join(heads, sep) + "\n")
	}

	for _, c := range shown {
		r, s := p.rows[c], p.scroll[c]
		if r < s {
			s = r
		}
		if r >= s+bodyH {
			s = r - bodyH + 1
		}
		p.scroll[c] = max(0, s)
	}
	for line := 0; line < bodyH; line++ {
		var cells []string
		for _, c := range shown {
			col := p.cols[c]
			i := p.scroll[c] + line
			switch {
			case i < len(col.Tasks):
				t := col.Tasks[i]
				mark, color := p.linkMarker(t)
				text := kanbanFit(t.Name, colW-2)
				style, cursor := normal, " "
				if c == p.col && i == p.rows[c] {
					// The glyph keeps the cursor visible without colors.
					style, cursor = sel, "▸"
				}
				cells = append(cells, lipgloss.NewStyle().Foreground(color).Render(mark)+style.Render(cursor+text))
			case i == len(col.Tasks) && col.Hidden > 0:
				cells = append(cells, dim.Render(kanbanFit(fmt.Sprintf("  … %d older", col.Hidden), colW)))
			default:
				cells = append(cells, strings.Repeat(" ", colW))
			}
		}
		sb.WriteString(strings.Join(cells, sep) + "\n")
	}
	return sb.String()
}

func (p *KanbanPanel) renderDetail(inner int) string {
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	t := p.selected()
	if t == nil {
		return "\n\n\n"
	}
	meta := []string{t.Status}
	if t.Category != "" {
		meta = append(meta, t.Category)
	}
	if t.PlanDate != nil {
		meta = append(meta, "计划 "+time.UnixMilli(*t.PlanDate).Local().Format("2006-01-02"))
	}
	if t.CompletedAt != nil {
		meta = append(meta, "完成 "+time.UnixMilli(*t.CompletedAt).Local().Format("2006-01-02"))
	}
	if t.ReadOnly {
		meta = append(meta, "read-only")
	}
	var sessions []string
	switch {
	case t.LinksUnknown:
		sessions = append(sessions, "links: unknown format")
	default:
		for _, l := range t.AgentDeckLinks {
			label := l.SessionID
			if p.env != nil && l.DeviceID == p.env.device && p.lookup != nil {
				if s, ok := p.lookup(l.SessionID); ok {
					label = kanbanClean(s.Title) + " (" + string(s.Status) + ")"
				} else {
					label += " (not in deck)"
				}
			} else {
				label += " @" + l.DeviceID
			}
			sessions = append(sessions, label)
		}
	}
	line3 := "sessions: none"
	if len(sessions) > 0 {
		line3 = "sessions: " + strings.Join(sessions, ", ")
	}
	return lipgloss.NewStyle().Bold(true).Foreground(ColorText).Render(kanbanFit(t.Name, inner)) + "\n" +
		dim.Render(kanbanFit(strings.Join(meta, " · "), inner)) + "\n" +
		dim.Render(kanbanFit(line3, inner)) + "\n"
}

func (p *KanbanPanel) renderFooter(inner int) string {
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	accent := lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)
	red := lipgloss.NewStyle().Foreground(ColorRed)
	var status string
	switch {
	case p.confirm != nil:
		status = accent.Render(kanbanFit(p.confirmText(), inner))
	case p.filterEditing:
		status = accent.Render(kanbanFit("Filter: "+p.filter+"▏", inner))
	case p.err != "":
		status = red.Render(kanbanFit(p.err, inner))
	case p.notice != "":
		status = accent.Render(kanbanFit(p.notice, inner))
	case p.filter != "":
		status = dim.Render(kanbanFit("Filter: "+p.filter+"  (Esc clears)", inner))
	default:
		status = ""
	}
	keys := "[h/l j/k] Move  [m] Status  [x] Done  [L/U] Link/unlink session  [n] New session  [Enter] Go to session  [/] Filter  [r] Reload  [Esc] Close"
	if p.confirm != nil {
		keys = "[Enter] Confirm  [Esc] Cancel"
	} else if p.filterEditing {
		keys = "[Enter] Keep filter  [Esc] Clear"
	}
	return status + "\n" + dim.Render(kanbanFit(keys, inner))
}
