package tmux

import (
	"strings"
	"testing"
)

// matrix/hardened: Codex truncates the combined header and interrupt hint,
// so a synthetic long label can occupy the full 127-column live status row.
var matrixCodexClippedHeader = string([]rune("Inspecting " + strings.Repeat("synthetic example files ", 8))[:126]) + "…"

const matrixCodexStatusTip = "  └ Tip: Use /vim to toggle Vim editing in the composer.\n"
const matrixCodexStatusQueue = "• Queued follow-up inputs\n" +
	"  ↳ inspect the synthetic example\n" +
	"    before continuing\n" +
	"    shift + ← edit last queued message\n"

func TestMatrixCodexTruncatedStatusFrame(t *testing.T) {
	for _, composer := range []string{"›", "»"} {
		footer := "\n" + composer + " Ask Codex to do anything" + matrixCodexVimFooter
		for _, tc := range []struct {
			name, body string
			busy       bool
		}{
			{"hidden spinner live Tip", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip, true},
			{"wrapped live Tip", matrixCodexClippedHeader + "\n  └ Tip: Use /vim to toggle Vim editing\n    in the composer.\n", true},
			{"wrapped Tip with empty continuation", matrixCodexClippedHeader + "\n  └ Tip: Use /vim to toggle Vim editing\n    \n    in the composer.\n", true},
			{"live Tip above complete queue", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n" + matrixCodexStatusQueue, true},
			{"solid spinner with strong hint", "• Working (3s • esc to interrupt)\n" + matrixCodexStatusTip, true},
			{"hollow spinner with strong hint", "◦ Working (3s • esc to interrupt)\n" + matrixCodexStatusTip, true},
			{"hidden spinner with strong hint", "Working (3s • esc to interrupt)\n" + matrixCodexStatusTip, true},
			{"idle composer", "", false},
			{"solid bullet quoted clipped label", "• " + matrixCodexClippedHeader + "\n" + matrixCodexStatusTip, false},
			{"hollow bullet quoted clipped label", "◦ " + matrixCodexClippedHeader + "\n" + matrixCodexStatusTip, false},
			{"indented clipped header", "  " + matrixCodexClippedHeader + "\n" + matrixCodexStatusTip, false},
			{"no status details", matrixCodexClippedHeader + "\n", false},
			{"ordinary history Tip", matrixCodexClippedHeader + "\n  Tip: Use /vim to toggle Vim editing in the composer.\n", false},
			{"generic status details", matrixCodexClippedHeader + "\n  └ Completed the example.\n", false},
			{"empty Tip", matrixCodexClippedHeader + "\n  └ Tip: \n", false},
			{"unclipped label", "Inspecting synthetic example files\n" + matrixCodexStatusTip, false},
			{"lowercase label", "inspecting synthetic example files…\n" + matrixCodexStatusTip, false},
			{"quoted tool output", "• Ran cat example.txt\n  └ " + matrixCodexClippedHeader + "\n    └ Tip: Use /vim to toggle Vim editing.\n", false},
			{"historical busy before later reply", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n› next request\n\n• Completed the task.\n", false},
			{"worked separator", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n─ Worked for 3s ─────\n", false},
			{"bare separator", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n─────────────\n", false},
			{"menu option after details", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n  1. Yes\n  2. No\n", false},
			{"footer after details", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n  GPT-6-Astra · ~/example · Main [default]\n", false},
			{"error banner after details", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n■ Conversation interrupted - use /feedback if something went wrong\n", false},
			{"Tip separated from header", matrixCodexClippedHeader + "\n\n" + matrixCodexStatusTip, false},
			{"Tip behind another details block", matrixCodexClippedHeader + "\n  └ Completed an example.\n" + matrixCodexStatusTip, false},
			{"another details block after Tip", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "  └ Completed an example.\n", false},
			{"fake details continuation", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "  quoted continuation without four spaces\n", false},
			{"malformed queue", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n• Queued follow-up inputs\n", false},
			{"continuation without queue header", matrixCodexClippedHeader + "\n" + matrixCodexStatusTip + "\n  ↳ queued text without header\n", false},
		} {
			t.Run(composer+"/"+tc.name, func(t *testing.T) {
				frame := tc.body + footer
				if got := CodexTurnRunning(frame); got != tc.busy {
					t.Errorf("CodexTurnRunning = %v, want %v", got, tc.busy)
				}
				if got := NewPromptDetector("codex").HasPrompt(frame); got == tc.busy {
					t.Errorf("HasPrompt = %v, want %v", got, !tc.busy)
				}
				verdict := ClassifyPaneFrame("codex", frame)
				if (verdict == FrameActive) != tc.busy {
					t.Errorf("ClassifyPaneFrame = %s, want busy=%v", verdict, tc.busy)
				}
			})
		}
	}
}
