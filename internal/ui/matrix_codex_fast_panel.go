package ui

// matrix/hardened: alt+x switches every live Codex session on this machine
// and on every configured remote to /fast or back. Each host runs its own
// `agent-deck codex-fast` (cmd/agent-deck/matrix_codex_fast_cmd.go), locally or
// over the remote's SSH runner; the panel first shows a dry run, then asks
// before typing into any pane.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// A switch types into each pane and waits for its footer; panes run
// concurrently, so a host needs well under this.
const codexFastCommandTimeout = 3 * time.Minute

type codexFastHostMsg struct {
	seq         int
	target      string
	applied     bool // false: dry run
	results     []session.CodexFastResult
	summary     string
	err         error
	unsupported bool // the host's agent-deck predates codex-fast
}

type codexFastHost struct {
	target  string // "" = this machine
	run     adoptRunFunc
	loading bool
	applied bool
	results []session.CodexFastResult
	summary string
	err     string
	// matrix/hardened: an older agent-deck is a note, not a failure.
	unsupported bool
}

// CodexFastPanel is the alt+x overlay.
type CodexFastPanel struct {
	visible    bool
	width      int
	height     int
	seq        int
	hosts      []*codexFastHost
	confirming string // "on" / "off" awaiting y/n
	running    string // "on" / "off" in flight
}

func NewCodexFastPanel() *CodexFastPanel { return &CodexFastPanel{} }

func (p *CodexFastPanel) IsVisible() bool { return p != nil && p.visible }

func (p *CodexFastPanel) SetSize(w, h int) {
	if p == nil {
		return
	}
	p.width, p.height = w, h
}

func (p *CodexFastPanel) Hide() {
	if p == nil {
		return
	}
	p.visible, p.confirming = false, ""
}

// Open shows the panel for the given hosts and reads them with a dry run.
func (p *CodexFastPanel) Open(targets []string, runner func(string) adoptRunFunc) tea.Cmd {
	p.visible, p.confirming, p.running = true, "", ""
	p.hosts = p.hosts[:0]
	for _, t := range targets {
		p.hosts = append(p.hosts, &codexFastHost{target: t, run: runner(t)})
	}
	return p.dispatch("on", false)
}

// dispatch runs `codex-fast <want>` on every host; dry runs only read.
func (p *CodexFastPanel) dispatch(want string, apply bool) tea.Cmd {
	p.seq++
	seq := p.seq
	var cmds []tea.Cmd
	for _, h := range p.hosts {
		h.loading, h.err = true, ""
		target, run := h.target, h.run
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), codexFastCommandTimeout)
			defer cancel()
			args := []string{"codex-fast", want, "--json"}
			if !apply {
				args = append(args, "--dry-run")
			}
			out, err := run(ctx, args...)
			var resp struct {
				Results []session.CodexFastResult `json:"results"`
				Summary string                    `json:"summary"`
			}
			if jerr := json.Unmarshal(out, &resp); jerr != nil {
				if err == nil {
					err = jerr
				}
				err = adoptCommandError(out, err)
				if codexFastUnsupported(err) {
					return codexFastHostMsg{seq: seq, target: target, applied: apply, unsupported: true}
				}
				err = fmt.Errorf("%v (is agent-deck there older than codex-fast?)", err)
				return codexFastHostMsg{seq: seq, target: target, applied: apply, err: err}
			}
			// A failed session exits non-zero but still reports every result.
			return codexFastHostMsg{seq: seq, target: target, applied: apply, results: resp.Results, summary: resp.Summary}
		})
	}
	return tea.Batch(cmds...)
}

func (p *CodexFastPanel) handleHost(m codexFastHostMsg) {
	if m.seq != p.seq {
		return
	}
	for _, h := range p.hosts {
		if h.target != m.target {
			continue
		}
		h.loading, h.applied, h.unsupported = false, m.applied, m.unsupported
		if m.unsupported {
			h.err, h.results, h.summary = "", nil, ""
		} else if m.err != nil {
			h.err, h.results, h.summary = m.err.Error(), nil, ""
		} else {
			h.err, h.results, h.summary = "", m.results, m.summary
		}
	}
	if !p.busy() {
		p.running = ""
	}
}

func (p *CodexFastPanel) busy() bool {
	for _, h := range p.hosts {
		if h.loading {
			return true
		}
	}
	return false
}

// pending counts sessions a switch to want would change, by running state.
func (p *CodexFastPanel) pending(want bool) (running, idle int) {
	for _, h := range p.hosts {
		for _, r := range h.results {
			if r.WasFast == want || r.Outcome == session.CodexFastSkipped || r.Outcome == session.CodexFastFailed {
				continue
			}
			if r.WasRunning {
				running++
			} else {
				idle++
			}
		}
	}
	return running, idle
}

func (p *CodexFastPanel) Update(msg tea.Msg) (*CodexFastPanel, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	if p.confirming != "" {
		switch key.String() {
		case "y", "Y", "enter":
			want := p.confirming
			p.confirming, p.running = "", want
			return p, p.dispatch(want, true)
		case "n", "N", "esc", "q":
			p.confirming = ""
		}
		return p, nil
	}
	switch key.String() {
	case "esc", "q":
		p.Hide() // a switch in flight keeps going on each host
	case "r":
		if !p.busy() {
			return p, p.dispatch("on", false)
		}
	case "o":
		if !p.busy() {
			p.confirming = "on"
		}
	case "f":
		if !p.busy() {
			p.confirming = "off"
		}
	}
	return p, nil
}

func (p *CodexFastPanel) View() string {
	if p == nil || !p.visible {
		return ""
	}
	width := min(max(50, p.width-2), 110)
	inner := width - 2
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ColorBorder).Padding(0, 1).Width(width)
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	text := lipgloss.NewStyle().Foreground(ColorText)
	red := lipgloss.NewStyle().Foreground(ColorRed).Bold(true)
	green := lipgloss.NewStyle().Foreground(ColorGreen)
	yellow := lipgloss.NewStyle().Foreground(ColorYellow)
	sel := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	rule := dim.Render(strings.Repeat("─", inner))

	head := "live Codex sessions"
	switch {
	case p.running != "":
		head += " · switching " + p.running + "…"
	case p.busy():
		head += " · reading…"
	}
	var sb strings.Builder
	sb.WriteString(title.Render("CODEX FAST") + "  " + dim.Render(kanbanFit(head, inner-12)) + "\n" + rule + "\n")

	lines := 0
	maxLines := max(4, p.height-12)
	for _, h := range p.hosts {
		where := "this machine"
		if h.target != "" {
			where = "remote " + h.target
		}
		status := ""
		switch {
		case h.loading:
			status = dim.Render(" …")
		case h.unsupported:
			status = dim.Render(" agent-deck there is too old for codex-fast; skipped")
		case h.err != "":
			status = red.Render(" " + kanbanFit(h.err, inner-cellWidth(where)-2))
		case len(h.results) == 0:
			status = dim.Render(" no live Codex sessions")
		case h.applied:
			status = dim.Render(" " + h.summary)
		}
		sb.WriteString(text.Bold(true).Render(where) + status + "\n")
		lines++
		for _, r := range h.results {
			if lines >= maxLines {
				break
			}
			tier := dim.Render("std ")
			if (h.applied && r.Fast) || (!h.applied && r.WasFast) {
				tier = red.Render("fast")
			}
			state := dim.Render("idle   ")
			if r.WasRunning {
				state = yellow.Render("running")
			}
			note := ""
			switch {
			case r.Outcome == session.CodexFastFailed:
				note = red.Render(" ✗ " + r.Reason)
			case r.Outcome == session.CodexFastSkipped:
				note = yellow.Render(" skip: " + r.Reason)
			case h.applied && r.Outcome == session.CodexFastSwitched:
				note = green.Render(" ✓ switched")
				if r.Continued {
					note += green.Render(", continued")
				}
				if r.Reason != "" {
					note += yellow.Render(" (" + r.Reason + ")")
				}
			}
			row := "  " + tier + " " + state + " " + r.Title
			sb.WriteString(kanbanFit(row, inner-cellWidth(note)) + note + "\n")
			lines++
		}
	}
	sb.WriteString(rule + "\n")
	switch {
	case p.confirming != "":
		want := p.confirming == "on"
		running, idle := p.pending(want)
		verb := "ON (2x speed, more usage)"
		if !want {
			verb = "OFF"
		}
		if running+idle == 0 {
			sb.WriteString(sel.Render(kanbanFit("Nothing to change. [n] back", inner)))
			break
		}
		sb.WriteString(sel.Render(kanbanFit(fmt.Sprintf("Turn fast %s for %d running + %d idle sessions?", verb, running, idle), inner)) + "\n")
		sb.WriteString(sel.Render(kanbanFit(fmt.Sprintf("Running turns are interrupted and sent %q.  [y] yes  [n] no", session.CodexFastContinuePrompt), inner)))
	case p.running != "" || p.busy():
		sb.WriteString(dim.Render(kanbanFit("Working on each host…  [Esc] Close (keeps running)", inner)))
	default:
		sb.WriteString(dim.Render(kanbanFit("[o] All fast ON  [f] All fast OFF  [r] Refresh  [Esc] Close", inner)))
	}
	return lipgloss.Place(p.width, p.height, lipgloss.Center, lipgloss.Center, box.Render(sb.String()))
}

// codexFastTargets is this machine plus every configured remote, sorted.
func codexFastTargets() []string {
	targets := []string{""}
	cfg, _ := session.LoadUserConfig()
	if cfg == nil {
		return targets
	}
	names := make([]string, 0, len(cfg.Remotes))
	for name := range cfg.Remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	return append(targets, names...)
}

// codexFastUnsupported recognises the refusals of an agent-deck that predates
// codex-fast: the remote channel's verb allowlist, or the CLI itself.
func codexFastUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "verb not allowed") || strings.Contains(msg, "not a recognized command")
}
