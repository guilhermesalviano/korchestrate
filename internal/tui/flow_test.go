package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestFlowArrowAnimatesIntoRunningStage(t *testing.T) {
	// Tests have no terminal; force color so the lit segment is visible.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	a := NewApp(testConfig(), t.TempDir())
	done := &StageInfo{done: true}
	running := &StageInfo{status: "executing iteration 0"}
	seen := map[string]bool{}
	for frame := 0; frame < 8; frame++ {
		a.frame = frame
		arrow := a.flowArrow(done, running, true)
		if w := lipgloss.Width(arrow); w != arrowW {
			t.Fatalf("frame %d: arrow width %d, want %d", frame, w, arrowW)
		}
		seen[arrow] = true
	}
	if len(seen) < 3 {
		t.Fatalf("arrow should animate across frames, saw %d variants", len(seen))
	}
	// Idle, pending and waiting connectors stay still.
	for _, next := range []*StageInfo{{}, {status: "awaiting plan approval"}, {done: true}} {
		first := a.flowArrow(done, next, true)
		a.frame++
		if a.flowArrow(done, next, true) != first {
			t.Fatalf("arrow into %+v should not animate", next)
		}
	}
}

func TestWaitingStageBlinksGold(t *testing.T) {
	a := NewApp(testConfig(), t.TempDir())
	si := &StageInfo{status: "awaiting plan approval"}
	glyphs := map[string]bool{}
	for frame := 0; frame < 8; frame++ {
		a.frame = frame
		glyph, word, col := a.stageStatus(si, true)
		if word != "your turn" || col != cGold {
			t.Fatalf("waiting stage = %q %v", word, col)
		}
		glyphs[glyph] = true
	}
	if len(glyphs) != 2 {
		t.Fatalf("waiting glyph should blink, saw %v", glyphs)
	}
}
