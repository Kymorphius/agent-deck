package session

// matrix/hardened: switch every live Codex session of this profile to /fast
// on or off in one action. Codex's /fast is a toggle with no argument (an
// argument is sent to the model as chat), so each pane's current state is read
// from its footer ("GPT-6.1-Sol medium fast · …") and /fast is typed only where
// it differs. A running turn is interrupted first and gets "继续" afterwards;
// an idle session is only switched. A pane with an operator draft, an open
// menu or an unreadable footer is left alone. Codex itself persists the last
// toggle to config.toml's service_tier, so new sessions follow the last switch.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// CodexFastContinuePrompt is sent to a session whose turn was interrupted to
// switch it, so the work resumes under the new tier.
const CodexFastContinuePrompt = "继续"

// Outcomes of one session's switch.
const (
	CodexFastSwitched = "switched"
	CodexFastAlready  = "already"
	CodexFastSkipped  = "skipped"
	CodexFastFailed   = "failed"
	CodexFastPlanned  = "planned" // dry run: would switch
)

// CodexFastResult is one session's outcome.
type CodexFastResult struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Outcome     string `json:"outcome"`
	WasFast     bool   `json:"was_fast"`
	Fast        bool   `json:"fast"`
	WasRunning  bool   `json:"was_running"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Continued   bool   `json:"continued,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// codexFooterState is what the frame below the composer says.
type codexFooterState struct {
	fast   bool
	vim    string // "", "normal" or "insert"
	known  bool
	draft  string
	menu   bool
	turnOn bool
}

// codexComposerLine reports whether line is a Codex composer row (› or », the
// latter with Vim editing), as tmux.codexComposerText does.
func codexComposerLine(line string) bool {
	for _, glyph := range []string{"›", "»"} {
		if line == glyph || strings.HasPrefix(line, glyph+" ") {
			return true
		}
	}
	return false
}

// readCodexFooter reads the live frame. raw keeps ANSI so a dim placeholder
// is not mistaken for an operator draft (send.ComposerDraft's contract).
func readCodexFooter(raw string) codexFooterState {
	plain := tmux.StripANSI(raw)
	var st codexFooterState
	st.turnOn = tmux.CodexTurnRunning(plain)
	st.menu = tmux.NewPromptDetector("codex").ClassifySubstate(plain) == tmux.SubstateInteractiveMenu
	// Lines are split per row on the raw capture so the dim placeholder can be
	// told apart from typed text; send.ComposerDraft does not know the »
	// composer and falls back to an older › transcript row.
	rawLines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(rawLines) > 60 {
		rawLines = rawLines[len(rawLines)-60:]
	}
	lines := make([]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = tmux.StripANSI(l)
	}
	composer := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if codexComposerLine(lines[i]) {
			composer = i
			break
		}
	}
	if composer < 0 {
		return st
	}
	draft := []string{codexComposerBody(rawLines[composer], lines[composer])}
	for n, line := range lines[composer+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		head, _, found := strings.Cut(strings.TrimSpace(line), " · ")
		if !found || !strings.HasPrefix(line, "  ") {
			// A wrapped draft row, not the footer.
			draft = append(draft, codexComposerBody(rawLines[composer+1+n], line))
			continue
		}
		st.draft = strings.TrimSpace(strings.Join(draft, " "))
		fields := strings.Fields(head)
		if len(fields) == 0 {
			return st
		}
		st.known = true
		st.fast = fields[len(fields)-1] == "fast"
		switch {
		case strings.Contains(line, "Vim: Normal"):
			st.vim = "normal"
		case strings.Contains(line, "Vim: Insert"):
			st.vim = "insert"
		}
		return st
	}
	return st
}

// codexComposerBody is the typed text of one composer row: the glyph is
// dropped, and text drawn entirely dim is Codex's placeholder, not input.
func codexComposerBody(raw, plain string) string {
	body := strings.TrimSpace(plain)
	for _, glyph := range []string{"›", "»"} {
		if b, ok := strings.CutPrefix(body, glyph); ok {
			body = strings.TrimSpace(b)
			break
		}
	}
	if body == "" {
		return ""
	}
	// Remove the glyph's own styling, then check whether every visible rune
	// that follows sits inside a dim (SGR 2) run.
	rest := raw
	if at := strings.IndexAny(rest, "›»"); at >= 0 {
		_, size := utf8.DecodeRuneInString(rest[at:])
		rest = rest[at+size:]
	}
	if codexAllDim(rest) {
		return ""
	}
	return body
}

// codexAllDim reports whether all visible text in s is drawn with SGR 2.
func codexAllDim(s string) bool {
	dim, sawText := false, false
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j >= len(s) {
				break
			}
			if s[j] == 'm' {
				for _, p := range strings.Split(s[i+2:j], ";") {
					switch p {
					case "2":
						dim = true
					case "", "0", "22":
						dim = false
					}
				}
			}
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsSpace(r) {
			sawText = true
			if !dim {
				return false
			}
		}
		i += size
	}
	return sawText
}

// codexFastPane is the pane surface the switch needs; *tmux.Session satisfies it.
type codexFastPane interface {
	CapturePaneFresh() (string, error)
	SendKeys(text string) error
	SendNamedKey(key string) error
}

// codexFastTiming is overridable so tests run without real delays.
type codexFastTiming struct {
	settle    time.Duration // after typing, before Enter (lets the slash popup settle)
	poll      time.Duration
	interrupt time.Duration // how long a turn may take to stop after Esc
	apply     time.Duration // how long the footer may take to show the new tier
}

var defaultCodexFastTiming = codexFastTiming{settle: 800 * time.Millisecond, poll: 250 * time.Millisecond, interrupt: 10 * time.Second, apply: 4 * time.Second}

// switchCodexPaneFast brings one pane to want. It never clears a draft,
// answers a menu, or sends Esc to an idle composer (two Escs open Codex's
// edit-previous-message mode).
func switchCodexPaneFast(pane codexFastPane, want, dryRun bool, tm codexFastTiming) CodexFastResult {
	var res CodexFastResult
	capture := func() (codexFooterState, error) {
		raw, err := pane.CapturePaneFresh()
		if err != nil {
			return codexFooterState{}, err
		}
		return readCodexFooter(raw), nil
	}
	waitFor := func(limit time.Duration, ok func(codexFooterState) bool) (codexFooterState, bool) {
		deadline := time.Now().Add(limit)
		for {
			st, err := capture()
			if err == nil && ok(st) {
				return st, true
			}
			if time.Now().After(deadline) {
				return st, false
			}
			time.Sleep(tm.poll)
		}
	}
	fail := func(reason string) CodexFastResult {
		res.Outcome, res.Reason = CodexFastFailed, reason
		return res
	}
	skip := func(reason string) CodexFastResult {
		res.Outcome, res.Reason = CodexFastSkipped, reason
		res.Fast = res.WasFast
		return res
	}

	st, err := capture()
	if err != nil {
		return fail("capture pane: " + err.Error())
	}
	res.WasFast, res.WasRunning = st.fast, st.turnOn
	switch {
	case !st.known:
		return skip("cannot read the Codex footer")
	case st.fast == want:
		res.Outcome, res.Fast = CodexFastAlready, want
		return res
	case st.menu:
		return skip("a menu or approval is open")
	case st.draft != "":
		return skip("composer has an unsent draft")
	}
	if dryRun {
		res.Outcome, res.Fast = CodexFastPlanned, want
		return res
	}

	if st.turnOn {
		// In Vim insert mode the first Esc only leaves insert mode.
		escapes := 1
		if st.vim == "insert" {
			escapes = 2
		}
		for n := 0; n < escapes; n++ {
			if err := pane.SendNamedKey("Escape"); err != nil {
				return fail("interrupt: " + err.Error())
			}
			time.Sleep(tm.poll)
		}
		after, stopped := waitFor(tm.interrupt, func(s codexFooterState) bool { return !s.turnOn })
		if !stopped {
			return fail("the turn did not stop after Esc")
		}
		res.Interrupted = true
		if after.draft != "" {
			// Codex may hand queued input back to the composer; leave it.
			return fail("interrupted, but the composer now holds a draft; switch and continue by hand")
		}
		st = after
	}
	if st.vim == "normal" {
		if err := pane.SendNamedKey("i"); err != nil {
			return fail("enter insert mode: " + err.Error())
		}
		time.Sleep(tm.poll)
	}

	if err := pane.SendKeys("/fast"); err != nil {
		return fail("type /fast: " + err.Error())
	}
	time.Sleep(tm.settle)
	typed, err := capture()
	if err != nil || typed.draft != "/fast" {
		// Something else owns the composer now; do not submit it.
		return fail("composer did not show /fast; nothing submitted")
	}
	applied := func(s codexFooterState) bool { return s.known && s.fast == want && s.draft == "" }
	if err := pane.SendNamedKey("Enter"); err != nil {
		return fail("submit /fast: " + err.Error())
	}
	final, ok := waitFor(tm.apply, func(s codexFooterState) bool { return applied(s) || s.draft == "/fast" })
	if !ok || (!applied(final) && final.draft == "/fast") {
		// The first Enter accepted the slash popup's completion.
		if err := pane.SendNamedKey("Enter"); err != nil {
			return fail("submit /fast: " + err.Error())
		}
		final, ok = waitFor(tm.apply, applied)
	}
	if !ok || !applied(final) {
		return fail("the footer did not show the new tier")
	}
	res.Outcome, res.Fast = CodexFastSwitched, want

	if res.Interrupted {
		if err := pane.SendKeys(CodexFastContinuePrompt); err != nil {
			res.Reason = "switched; sending 继续 failed: " + err.Error()
			return res
		}
		time.Sleep(tm.settle)
		typed, err := capture()
		if err != nil || typed.draft != CodexFastContinuePrompt {
			res.Reason = "switched; 继续 was not submitted (composer changed)"
			return res
		}
		if err := pane.SendNamedKey("Enter"); err != nil {
			res.Reason = "switched; submitting 继续 failed: " + err.Error()
			return res
		}
		res.Continued = true
	}
	return res
}

// SwitchCodexFast switches every live, local Codex session in instances to
// want. Panes are handled concurrently; results are sorted by title.
func SwitchCodexFast(instances []*Instance, want, dryRun bool) []CodexFastResult {
	type job struct {
		inst *Instance
		pane *tmux.Session
	}
	var jobs []job
	for _, inst := range instances {
		if inst == nil || !IsCodexCompatible(inst.Tool) || inst.IsSSH() || inst.IsArchived() {
			continue
		}
		pane := inst.GetTmuxSession()
		if pane == nil || !pane.Exists() {
			continue
		}
		jobs = append(jobs, job{inst, pane})
	}
	results := make([]CodexFastResult, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for n, j := range jobs {
		wg.Add(1)
		go func(n int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := switchCodexPaneFast(j.pane, want, dryRun, defaultCodexFastTiming)
			r.ID, r.Title = j.inst.ID, j.inst.Title
			results[n] = r
		}(n, j)
	}
	wg.Wait()
	sort.SliceStable(results, func(a, b int) bool { return results[a].Title < results[b].Title })
	return results
}

// CodexFastSummary is the one-line tally shown by the CLI and the TUI.
func CodexFastSummary(results []CodexFastResult) string {
	counts := map[string]int{}
	continued := 0
	for _, r := range results {
		counts[r.Outcome]++
		if r.Continued {
			continued++
		}
	}
	if len(results) == 0 {
		return "no live Codex sessions"
	}
	parts := []string{}
	for _, k := range []string{CodexFastPlanned, CodexFastSwitched, CodexFastAlready, CodexFastSkipped, CodexFastFailed} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	if continued > 0 {
		parts = append(parts, fmt.Sprintf("%d continued", continued))
	}
	return strings.Join(parts, ", ")
}

// ErrCodexFastPartial marks a run where at least one session failed.
var ErrCodexFastPartial = errors.New("some Codex sessions could not be switched")
