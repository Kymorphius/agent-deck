package session

import (
	"strings"
	"testing"
	"time"
)

// fakeCodexPane simulates the parts of the Codex TUI the switch touches, with
// the behaviour observed on real 0.162/0.165 panes: /fast toggles the tier and
// shows it in the footer, Esc interrupts a turn (in Vim insert mode the first
// Esc only leaves insert mode), and the slash popup may swallow one Enter.
type fakeCodexPane struct {
	fast, running, menu bool
	vim                 string // "", "insert", "normal"
	composer            string
	popupEnters         int  // Enters the slash popup consumes before running /fast
	stuckFooter         bool // /fast does not change the footer
	stubbornTurn        bool // Esc does not stop the turn

	keys          []string
	idleEscapes   int
	continued     bool
	normalTyping  bool
	pendingPopups int
}

func (p *fakeCodexPane) CapturePaneFresh() (string, error) {
	var b strings.Builder
	b.WriteString("• Earlier answer.\n\n")
	if p.running {
		b.WriteString("• Working (12s • esc to interrupt)\n\n")
	}
	if p.menu {
		b.WriteString("  1. Yes\n  Press enter to confirm or esc to go back\n\n")
	}
	if p.composer == "" {
		b.WriteString("› \x1b[2mAsk Codex to do anything\x1b[0m\n\n")
	} else {
		b.WriteString("› " + p.composer + "\n\n")
	}
	footer := "  GPT-6.1-Sol medium"
	if p.fast {
		footer += " fast"
	}
	footer += " · ~/project · task · ← for agents"
	switch p.vim {
	case "insert":
		footer += "    Vim: Insert"
	case "normal":
		footer += "    Vim: Normal"
	}
	b.WriteString(footer + "\n  ? for shortcuts\n")
	return b.String(), nil
}

func (p *fakeCodexPane) SendKeys(text string) error {
	p.keys = append(p.keys, "text:"+text)
	if p.vim == "normal" {
		p.normalTyping = true
		return nil
	}
	p.composer += text
	if p.composer == "/fast" {
		p.pendingPopups = p.popupEnters
	}
	return nil
}

func (p *fakeCodexPane) SendNamedKey(key string) error {
	p.keys = append(p.keys, key)
	switch key {
	case "Escape":
		switch {
		case p.vim == "insert":
			p.vim = "normal"
		case p.running && !p.stubbornTurn:
			p.running = false
		case !p.running:
			p.idleEscapes++
		}
	case "i":
		if p.vim == "normal" {
			p.vim = "insert"
		}
	case "Enter":
		switch p.composer {
		case "/fast":
			if p.pendingPopups > 0 {
				p.pendingPopups--
				return nil
			}
			if !p.stuckFooter {
				p.fast = !p.fast
			}
			p.composer = ""
		case CodexFastContinuePrompt:
			p.composer, p.running, p.continued = "", true, true
		}
	}
	return nil
}

var fastTestTiming = codexFastTiming{settle: time.Millisecond, poll: time.Millisecond, interrupt: 50 * time.Millisecond, apply: 50 * time.Millisecond}

func TestSwitchCodexPaneFast(t *testing.T) {
	tests := []struct {
		name          string
		pane          fakeCodexPane
		want, dry     bool
		outcome       string
		continued     bool
		escapes       int
		fastAfter     bool
		noKeys        bool
		reasonContain string
	}{
		{name: "idle off to on: no Esc, no continue", pane: fakeCodexPane{}, want: true, outcome: CodexFastSwitched, fastAfter: true},
		{name: "idle on to off", pane: fakeCodexPane{fast: true}, want: false, outcome: CodexFastSwitched},
		{name: "running is interrupted then continued", pane: fakeCodexPane{running: true}, want: true, outcome: CodexFastSwitched, continued: true, escapes: 1, fastAfter: true},
		{name: "vim insert needs two Esc and i", pane: fakeCodexPane{running: true, vim: "insert"}, want: true, outcome: CodexFastSwitched, continued: true, escapes: 2, fastAfter: true},
		{name: "idle vim normal enters insert first", pane: fakeCodexPane{vim: "normal"}, want: true, outcome: CodexFastSwitched, fastAfter: true},
		{name: "slash popup takes one Enter", pane: fakeCodexPane{popupEnters: 1}, want: true, outcome: CodexFastSwitched, fastAfter: true},
		{name: "already in the wanted tier", pane: fakeCodexPane{fast: true, running: true}, want: true, outcome: CodexFastAlready, fastAfter: true, noKeys: true},
		{name: "draft is never touched", pane: fakeCodexPane{composer: "half typed"}, want: true, outcome: CodexFastSkipped, noKeys: true, reasonContain: "draft"},
		{name: "open menu is never answered", pane: fakeCodexPane{menu: true}, want: true, outcome: CodexFastSkipped, noKeys: true, reasonContain: "menu"},
		{name: "dry run sends nothing", pane: fakeCodexPane{running: true}, want: true, dry: true, outcome: CodexFastPlanned, noKeys: true, fastAfter: false},
		{name: "footer never changes", pane: fakeCodexPane{stuckFooter: true}, want: true, outcome: CodexFastFailed, reasonContain: "footer"},
		{name: "turn that will not stop", pane: fakeCodexPane{running: true, stubbornTurn: true}, want: true, outcome: CodexFastFailed, escapes: 1, reasonContain: "did not stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pane
			res := switchCodexPaneFast(&p, tt.want, tt.dry, fastTestTiming)
			if res.Outcome != tt.outcome {
				t.Fatalf("outcome = %q (%s), want %q; keys=%v", res.Outcome, res.Reason, tt.outcome, p.keys)
			}
			if tt.reasonContain != "" && !strings.Contains(res.Reason, tt.reasonContain) {
				t.Fatalf("reason %q does not mention %q", res.Reason, tt.reasonContain)
			}
			if res.Continued != tt.continued || p.continued != tt.continued {
				t.Fatalf("continued = %v/%v, want %v; keys=%v", res.Continued, p.continued, tt.continued, p.keys)
			}
			if p.idleEscapes != 0 {
				t.Fatalf("sent Esc to an idle composer %d times; keys=%v", p.idleEscapes, p.keys)
			}
			if p.normalTyping {
				t.Fatalf("typed while Vim was in normal mode; keys=%v", p.keys)
			}
			escapes := 0
			for _, k := range p.keys {
				if k == "Escape" {
					escapes++
				}
			}
			if escapes != tt.escapes {
				t.Fatalf("Esc count = %d, want %d; keys=%v", escapes, tt.escapes, p.keys)
			}
			if tt.noKeys && len(p.keys) != 0 {
				t.Fatalf("expected no input, got %v", p.keys)
			}
			if tt.outcome == CodexFastSwitched && p.fast != tt.fastAfter {
				t.Fatalf("pane fast = %v, want %v", p.fast, tt.fastAfter)
			}
		})
	}
}

// Real frames: a » composer with a dim placeholder is empty, while typed text
// on it is a draft; the footer's last token before " · " is the tier.
func TestReadCodexFooterRealShapes(t *testing.T) {
	idle := "  Worked for 4m 10s\n\n\x1b[1m»\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n\n  GPT-6.1-Sol ultra fast · ~/x · task · Main [default]\n  ? for shortcuts\n"
	st := readCodexFooter(idle)
	if !st.known || !st.fast || st.draft != "" || st.turnOn {
		t.Fatalf("idle » frame: %+v", st)
	}
	typed := strings.Replace(idle, "\x1b[2mAsk Codex to do anything\x1b[0m", "keep this", 1)
	if st := readCodexFooter(typed); st.draft != "keep this" {
		t.Fatalf("typed » frame draft = %q", st.draft)
	}
	history := "› an older prompt in the transcript\n\n• answer\n\n" + idle
	if st := readCodexFooter(history); st.draft != "" {
		t.Fatalf("transcript › row read as a draft: %q", st.draft)
	}
	slow := strings.Replace(idle, "ultra fast ·", "ultra ·", 1)
	if st := readCodexFooter(slow); !st.known || st.fast {
		t.Fatalf("standard tier frame: %+v", st)
	}
}
