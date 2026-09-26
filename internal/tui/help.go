package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// helpKey is one shortcut shown in the key cheatsheet.
type helpKey struct {
	key   string
	label string
}

// helpGroup collects shortcuts under a heading in the cheatsheet.
type helpGroup struct {
	title string
	keys  []helpKey
}

var helpGroups = []helpGroup{
	{"RUNS", []helpKey{
		{"n / i / /", "new run"},
		{"↑↓  k j", "select run"},
		{"enter", "open"},
		{"o", "full-screen run"},
		{"b", "toggle worktrees"},
		{"t", "retry failed run at tab's stage"},
		{"x", "delete run"},
		{"m", "provider · model · effort"},
		{"ctrl+a", "default / autopilot mode"},
	}},
	{"VIEWS", []helpKey{
		{"tab / →", "next view"},
		{"shift+tab / ←", "previous view"},
		{"1–5", "activity · plan · review · diff · support"},
		{"d", "diff aside"},
		{"pgup / pgdn", "scroll"},
		{"g / G", "top / bottom"},
		{"[ / ]", "scroll diff aside"},
	}},
	{"A RUN WAITS", []helpKey{
		{"a", "approve"},
		{"f", "fix / another fix pass"},
		{"r", "reject"},
		{"t", "retry"},
		{"s", "stop / skip"},
		{"c", "commit"},
		{"p", "commit + push"},
		{"k", "keep existing worktree"},
		{"enter / esc", "next · use / back · stop"},
	}},
	{"PROMPT", []helpKey{
		{"enter", "next / run"},
		{"tab", "switch field"},
		{"esc", "back"},
		{"@plan.md", "skip the planner"},
		{"ctrl+u", "clear field"},
		{"ctrl+w", "delete word"},
	}},
	{"SUPPORT TAB", []helpKey{
		{"enter / !", "type a command"},
		{"enter", "run in the run's checkout"},
		{"ctrl+c", "stop command"},
		{"↑ ↓", "history"},
		{"esc", "leave the command line"},
	}},
	{"GENERAL", []helpKey{
		{"h", "this help"},
		{"w", "start / stop web dashboard"},
		{"q / ctrl+c", "quit"},
	}},
}

// renderHelp draws the full key-hint cheatsheet centered on the terminal. It
// balances the groups across the fewest columns that fit the terminal height.
func (a *App) renderHelp(w, h int) string {
	boxW := min(76, max(w, 1))
	inner := max(1, boxW-4)
	avail := max(3, h-3) // rows left for the groups after border and title

	blocks := make([][]string, 0, len(helpGroups))
	total := 0
	for _, g := range helpGroups {
		block := []string{textStyle.Bold(true).Render(g.title)}
		for _, k := range g.keys {
			block = append(block, "  "+keyHint(k.key, k.label))
		}
		blocks = append(blocks, block)
		total += len(block)
	}

	maxCols := max(1, min(3, inner/24))
	target := min(avail, 18)
	ncols := 1
	for ncols < maxCols && (total+ncols-1)/ncols > target {
		ncols++
	}

	colLines := make([][]string, ncols)
	heights := make([]int, ncols)
	for _, b := range blocks {
		c := 0
		for j := 1; j < ncols; j++ {
			if heights[j] < heights[c] {
				c = j
			}
		}
		colLines[c] = append(colLines[c], b...)
		heights[c] += len(b)
	}

	colW := max(1, (inner-2*(ncols-1))/ncols)
	cells := make([]string, 0, ncols)
	for i, lines := range colLines {
		for j, l := range lines {
			lines[j] = truncate(l, colW)
		}
		style := lipgloss.NewStyle().Width(colW)
		if i < ncols-1 {
			style = style.Width(colW + 2)
		}
		cells = append(cells, style.Render(strings.Join(lines, "\n")))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, cells...)

	title := goldStyle.Bold(true).Render("◆ KEYS") + mutedStyle.Render("  ·  press any key to close")
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cGold).
		Padding(0, 1).Width(boxW - 2).
		Render(title + "\n" + body)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}
