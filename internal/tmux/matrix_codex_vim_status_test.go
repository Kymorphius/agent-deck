package tmux

import "testing"

// matrix/hardened: Vim's » composer must anchor the live row just like ›,
// even when the last submitted prompt still uses the transcript's › marker.
const matrixCodexVimFooter = "\n\n  GPT-6-Astra ultra fast · ~/example · Main [default]\n  ? for shortcuts"

func TestMatrixCodexVimComposerStatus(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		busy  bool
	}{
		{"live vim frame", "› inspect the example\n\n• Completed the previous step.\n\n" +
			"Checking environment setup (5m 09s • esc to interrupt)\n" +
			"  └ Tip: Use /vim to toggle Vim editing in the composer.\n\n\n" +
			"» Ask Codex to do anything" + matrixCodexVimFooter, true},
		{"live bare vim composer", "Working (3s • esc to interrupt)\n\n»" + matrixCodexVimFooter, true},
		{"live vim draft", "• Working (3s • esc to interrupt)\n\n» inspect the tests" + matrixCodexVimFooter, true},
		{"idle vim composer", "» Ask Codex to do anything" + matrixCodexVimFooter, false},
		{"idle bare vim composer", "»" + matrixCodexVimFooter, false},
		{"idle vim draft", "» inspect the tests" + matrixCodexVimFooter, false},
		{"historical busy before submitted turn", "• Working (3s • esc to interrupt)\n\n› inspect the example\n\n" +
			"• Completed the task.\n\n» Ask Codex to do anything" + matrixCodexVimFooter, false},
		{"quoted busy row", "• Working (3s • esc to interrupt)\n  is the status shape shown in the docs.\n\n" +
			"» Ask Codex to do anything" + matrixCodexVimFooter, false},
		{"quoted output", "• Ran cat status-example.txt\n  └ Working (3s • esc to interrupt)\n    » Ask Codex to do anything\n\n" +
			"» Ask Codex to do anything" + matrixCodexVimFooter, false},
		{"legacy empty composer", "Working (3s • esc to interrupt)\n\n›" + matrixCodexVimFooter, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexTurnRunning(tc.frame); got != tc.busy {
				t.Errorf("CodexTurnRunning = %v, want %v", got, tc.busy)
			}
			if got := NewPromptDetector("codex").HasPrompt(tc.frame); got == tc.busy {
				t.Errorf("HasPrompt = %v, want %v", got, !tc.busy)
			}
			want := FrameWaiting
			if tc.busy {
				want = FrameActive
			}
			if got := ClassifyPaneFrame("codex", tc.frame); got != want {
				t.Errorf("ClassifyPaneFrame = %s, want %s", got, want)
			}
		})
	}
}

func TestMatrixCodexVimComposerGetStatus(t *testing.T) {
	skipIfNoTmuxBinary(t)
	// matrix/hardened: Exercise the real pane capture path used by remote
	// status reads, with the busy row above the footer's three-line window.
	for _, busy := range []bool{false, true} {
		name := "idle"
		frame := "» Ask Codex to do anything" + matrixCodexVimFooter
		want := "waiting"
		if busy {
			name, want = "busy", "active"
			frame = "› inspect the example\n\n• Completed the previous step.\n\n" +
				"Checking environment setup (5m 09s • esc to interrupt)\n" +
				"  └ Tip: Use /vim to toggle Vim editing in the composer.\n\n" + frame
		}
		t.Run(name, func(t *testing.T) {
			s := startPaneWithContent(t, "matrix-codex-vim-"+name, frame, "codex", "? for shortcuts")
			if got, err := s.GetStatus(); err != nil || got != want {
				t.Fatalf("GetStatus = %q, %v; want %s", got, err, want)
			}
		})
	}
}

func TestMatrixCodexVimComposerBannerBoundaries(t *testing.T) {
	// matrix/hardened: A vim composer is not another submitted turn. A real
	// transcript prompt between it and an old banner must still clear history.
	const banner = "■ You've hit your usage limit. Try again at Oct 10th, 2026 8:03 AM.\n\n"
	for _, tc := range []struct {
		name, frame, want string
	}{
		{"current banner", banner + "» Ask Codex to do anything" + matrixCodexVimFooter, codexBannerUsageLimit},
		{"current banner with draft", banner + "» continue later" + matrixCodexVimFooter, codexBannerUsageLimit},
		{"older vim draft is not submitted", banner + "» unsubmitted draft\n\n» Ask Codex to do anything" + matrixCodexVimFooter, codexBannerUsageLimit},
		{"submitted after banner", banner + "› retry the example\n\n» Ask Codex to do anything" + matrixCodexVimFooter, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := scanCodexErrorBanner(tc.frame); got != tc.want {
				t.Fatalf("scanCodexErrorBanner = %q, want %q", got, tc.want)
			}
		})
	}
}
