package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/models"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
)

const (
	arrowW     = 5  // width of the connector between stage cards
	minCardW   = 16 // below this the pipeline is drawn vertically
	entryLines = 3  // rows per run in the sidebar (two lines plus a gap)
)

// View implements tea.Model.
func (a *App) View() string {
	w, h := max(a.width, 1), max(a.height, 1)
	if w < 12 || h < 8 {
		return fitView("Enlarge terminal", w, h)
	}

	if a.setup.active {
		return fitView(a.renderSetup(w, h), w, h)
	}

	if a.help {
		return fitView(a.renderHelp(w, h), w, h)
	}

	if a.full && a.current() != nil {
		a.asideFits = false // "d" opens the Diff tab instead
		footer := a.renderFullFooter(w)
		bodyH := max(h-lipgloss.Height(footer), 3)
		return fitView(lipgloss.JoinVertical(lipgloss.Left, a.renderMain(w, bodyH), footer), w, h)
	}

	sideW, mainW, asideW := a.layout(w)
	a.asideFits = asideW > 0 // read by the "d" key handler

	header := a.renderHeader(w)
	footer := a.renderFooter(w)
	bodyH := max(h-lipgloss.Height(header)-lipgloss.Height(footer), 3)
	if a.showSidebar && w < 80 {
		body := a.renderSidebar(w, bodyH)
		return fitView(lipgloss.JoinVertical(lipgloss.Left, header, body, footer), w, h)
	}

	if !a.showAside {
		mainW, asideW = mainW+asideW, 0
	}
	var cols []string
	if sideW > 0 {
		cols = append(cols, a.renderSidebar(sideW, bodyH))
	}
	cols = append(cols, a.renderMain(mainW, bodyH))
	if asideW > 0 {
		cols = append(cols, a.renderAside(asideW, bodyH))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, cols...)
	return fitView(lipgloss.JoinVertical(lipgloss.Left, header, body, footer), w, h)
}

// layout splits the width into sidebar, main pane and diff aside. The aside
// only appears when the main pane keeps enough room for the pipeline cards.
func (a *App) layout(w int) (side, main, aside int) {
	const minMain = 64
	if a.showSidebar && w >= 80 {
		side = min(max(w/5, 24), 38)
	}
	aside = min(max(w*36/100, 46), 110)
	if w-side-aside >= minMain {
		return side, w - side - aside, aside
	}
	return side, w - side, 0
}

// fitView bounds the frame to the actual terminal, including very small sizes.
func fitView(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	lines = lines[:min(len(lines), h)]
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, line := range lines {
		lines[i] = truncate(line, w)
	}
	return strings.Join(lines, "\n")
}

// renderAside shows the selected run's whole diff: the live worktree while it
// runs, the staged diff.patch once it has finished.
func (a *App) renderAside(w, h int) string {
	inner := w - 4
	rows := h - 2
	e := a.current()
	if e == nil {
		return panel("DIFF", emptyNote("Select a run to see its changes.", inner), w, h, cFaint)
	}
	e.ensureDiff()

	c := &a.asideCache
	if !(c.entry == e && c.width == inner && c.ver == e.ver && c.lines != nil) {
		var lines []string
		if strings.TrimSpace(e.Diff) == "" {
			msg := "No changes yet — they appear here as the executor edits the worktree."
			if !e.Live {
				msg = "This run left no diff."
			}
			lines = emptyNote(msg, inner)
		} else {
			lines = diffBody(e.Diff, inner)
			for len(lines) > 0 && lines[0] == "" {
				lines = lines[1:]
			}
		}
		*c = contentCache{entry: e, width: inner, ver: e.ver, lines: lines}
	}
	lines := c.lines

	a.asideH = rows
	a.asideMax = max(0, len(lines)-rows)
	a.asideScroll = min(a.asideScroll, a.asideMax)
	view := lines[a.asideScroll:min(len(lines), a.asideScroll+rows)]

	title := "DIFF"
	if strings.TrimSpace(e.Diff) != "" {
		title += " " + diffSummary(e.Diff)
	}
	if e.Live {
		title += " " + cyanStyle.Render("● live")
	}
	if a.asideMax > 0 {
		title += " " + mutedStyle.Render(fmt.Sprintf("%d%%", a.asideScroll*100/a.asideMax))
	}
	return panel(title, view, w, h, cFaint)
}

// --- header -----------------------------------------------------------------

func (a *App) renderHeader(w int) string {
	left := gradient(" ◬ PROMPTER ILLUMINATI", gradFrom, gradTo, true) +
		mutedStyle.Render("  ·  "+filepath.Base(a.cfg.Repo))
	if w < 80 {
		left = goldStyle.Bold(true).Render(" kor") + mutedStyle.Render(" · "+filepath.Base(a.cfg.Repo))
	}

	var live, waiting, done, failed int
	for _, e := range a.entries {
		switch {
		case e.Gate != nil:
			waiting++
		case e.Live:
			live++
		case e.State == artifact.StateDone:
			done++
		case e.State == artifact.StateFailed || e.State == artifact.StateAborted:
			failed++
		}
	}
	var stats []string
	if live > 0 {
		stats = append(stats, cyanStyle.Render(fmt.Sprintf("%s %d running", spin[a.frame%len(spin)], live)))
	}
	if waiting > 0 {
		stats = append(stats, amberStyle.Bold(true).Render(fmt.Sprintf("◆ %d waiting", waiting)))
	}
	if done > 0 {
		stats = append(stats, greenStyle.Render(fmt.Sprintf("✔ %d", done)))
	}
	if failed > 0 {
		stats = append(stats, redStyle.Render(fmt.Sprintf("✘ %d", failed)))
	}
	right := strings.Join(stats, mutedStyle.Render("  ")) + " "
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

// --- sidebar ----------------------------------------------------------------

func (a *App) renderSidebar(w, h int) string {
	inner := max(1, w-4)
	rows := h - 2
	title := "WORKTREES"
	if n := len(a.entries); n > 0 {
		title += mutedStyle.Render(fmt.Sprintf(" %d", n))
	}
	// The mascot naps at the bottom when the list still has room for two runs.
	listRows := rows
	if rows >= koalaH+2*entryLines {
		listRows = rows - koalaH
	}
	withKoala := func(lines []string) []string {
		if listRows == rows {
			return lines
		}
		for len(lines) < listRows {
			lines = append(lines, "")
		}
		for _, l := range koala(moodSleep, a.frame, "", 0) {
			lines = append(lines, lipgloss.PlaceHorizontal(inner, lipgloss.Center, l))
		}
		return lines
	}

	if len(a.entries) == 0 {
		lines := []string{"",
			mutedStyle.Render("No runs yet."), "",
			textStyle.Render("type a prompt below"),
			textStyle.Render("and press ") + goldStyle.Render("enter"),
		}
		return panel(title, withKoala(lines), w, h, cFaint)
	}

	visible := max(1, (listRows+1)/entryLines)
	if a.cursor < a.listTop {
		a.listTop = a.cursor
	}
	if a.cursor >= a.listTop+visible {
		a.listTop = a.cursor - visible + 1
	}
	a.listTop = min(a.listTop, max(0, len(a.entries)-visible))

	var lines []string
	for i := a.listTop; i < len(a.entries) && i < a.listTop+visible; i++ {
		e := a.entries[i]
		sel := i == a.cursor
		bar := "  "
		titleSt := textStyle
		if sel {
			bar = goldStyle.Render("▌ ")
			titleSt = boldStyle
		}
		icon := a.entryIcon(e)
		prompt := strings.Join(strings.Fields(e.Prompt), " ")
		if prompt == "" {
			prompt = e.title()
		}
		l1 := bar + icon + " " + titleSt.Render(truncate(prompt, inner-4))

		meta := []string{a.entryStateText(e)}
		if e.Live {
			meta = append(meta, elapsed(e.duration()))
		} else if e.Run != nil {
			meta = append(meta, ago(e.Run.CreatedAt))
		}
		if e.Run != nil && e.Run.Usage.CostUSD > 0 {
			meta = append(meta, fmt.Sprintf("$%.2f", e.Run.Usage.CostUSD))
		}
		if e.Autopilot {
			meta = append(meta, "autopilot")
		}
		l2 := bar + "  " + mutedStyle.Render(truncate(strings.Join(meta, " · "), inner-4))
		lines = append(lines, l1, l2, "")
	}
	if a.listTop > 0 {
		lines[len(lines)-1] = mutedStyle.Render(fmt.Sprintf("  ↑ %d more", a.listTop))
	}
	if rest := len(a.entries) - a.listTop - visible; rest > 0 {
		lines = append(lines[:min(len(lines), listRows-1)], mutedStyle.Render(fmt.Sprintf("  ↓ %d more", rest)))
	}
	return panel(title, withKoala(lines), w, h, cFaint)
}

func (a *App) entryIcon(e *Entry) string {
	switch {
	case e.Gate != nil:
		if a.frame/4%2 == 0 {
			return amberStyle.Bold(true).Render("◆")
		}
		return amberStyle.Render("◇")
	case e.Live:
		return cyanStyle.Render(spin[a.frame%len(spin)])
	case e.State == artifact.StateDone:
		return greenStyle.Render("✔")
	case e.State == artifact.StateFailed:
		return redStyle.Render("✘")
	case e.State == artifact.StateAborted:
		return amberStyle.Render("⊘")
	default:
		return mutedStyle.Render("◌")
	}
}

func (a *App) entryStateText(e *Entry) string {
	switch {
	case e.deleting:
		return "deleting…"
	case e.Gate != nil:
		return "needs you"
	case e.State == artifact.StateGatePlan || e.State == artifact.StateGateReview:
		return "gate"
	case !e.Live && e.State != artifact.StateDone && e.State != artifact.StateFailed && e.State != artifact.StateAborted:
		return "interrupted"
	}
	return strings.ReplaceAll(string(e.State), "_", " ")
}

// --- main pane --------------------------------------------------------------

func (a *App) renderMain(w, h int) string {
	inner := max(1, w-4)
	rows := h - 2
	e := a.current()
	if e == nil {
		return panel("PIPELINE", a.welcome(inner, rows), w, h, cFaint)
	}

	gate := []string(nil)
	if e.Gate != nil {
		gate = a.renderGate(e, inner)
	}

	var top []string
	for i, l := range wrap(strings.Join(strings.Fields(e.Prompt), " "), inner) {
		if i == 2 {
			top[1] = truncate(top[1], inner-1) + "…"
			break
		}
		top = append(top, boldStyle.Render(l))
	}
	top = append(top, a.metaLine(e, inner), "")
	if len(gate) > 0 {
		top = append(top, gate...)
		top = append(top, "")
	}
	if e.Autopilot && !e.Live && e.State == artifact.StateDone {
		for i, l := range wrap(pipeline.EndMessage(e.Run), inner-2) {
			mark := "  "
			if i == 0 {
				mark = greenStyle.Bold(true).Render("✔ ")
			}
			top = append(top, mark+greenStyle.Render(l))
		}
		top = append(top, "")
	}
	if e.ErrText != "" {
		errLines := wrap(sanitize(e.ErrText), inner-2)
		if len(errLines) > 2 {
			errLines = append(errLines[:2], "…")
		}
		for i, l := range errLines {
			mark := "  "
			if i == 0 {
				mark = redStyle.Bold(true).Render("✘ ")
			}
			top = append(top, mark+redStyle.Render(l))
		}
		top = append(top, "")
	}

	// The pipeline flow is pinned to the bottom of the pane, below the tabbed
	// content, so it stays visible while the log/plan/review scrolls above it.
	flow := strings.Split(a.renderFlow(e, inner), "\n")
	if w < 80 || rows-len(top)-len(flow)-2 < 1 {
		flow = a.compactFlow(e, inner)
	}
	shellRow := 0
	if a.tab == tabSupport {
		shellRow = 1 // the command line sits under the scrollback
	}
	viewH := max(rows-len(top)-len(flow)-2-shellRow, 1)
	content := a.contentLines(e, inner)
	maxOff := max(0, len(content)-viewH)
	off := min(a.scroll, maxOff)
	if a.follow {
		off = maxOff
	}
	a.lastMax, a.viewH = maxOff, viewH

	lines := append(top, a.tabBar(e, inner, off, maxOff), faintStyle.Render(strings.Repeat("─", inner)))
	chunk := content[off:min(len(content), off+viewH)]
	lines = append(lines, chunk...)
	for i := len(chunk); i < viewH; i++ {
		lines = append(lines, "")
	}
	if shellRow > 0 {
		lines = append(lines, a.shellPrompt(e, inner))
	}
	lines = append(lines, flow...)

	title := "RUN " + mutedStyle.Render(truncate(e.title(), inner-8))
	if e.Autopilot {
		title = "RUN " + cyanStyle.Bold(true).Render("AUTOPILOT") + " " + mutedStyle.Render(truncate(e.title(), inner-18))
	}
	border := cFaint
	if e.Gate != nil {
		border = cAmber
	}
	return panel(title, lines, w, h, border)
}

// compactFlow lists the stages one per line for terminals too narrow or too
// short for the full flow diagram.
func (a *App) compactFlow(e *Entry, w int) []string {
	rows := make([]string, 0, len(stageOrder))
	for _, k := range stageOrder {
		glyph, word, col := a.stageStatus(e.Stages[k], e.Live)
		rows = append(rows, truncate(
			lipgloss.NewStyle().Foreground(roleColor[k]).Render(strings.ToUpper(string(k)))+" "+
				lipgloss.NewStyle().Foreground(col).Render(glyph+" "+word)+" "+
				mutedStyle.Render(choiceText(a.stageChoice(e, k))), w))
	}
	return rows
}

func (a *App) metaLine(e *Entry, w int) string {
	var parts []string
	if branch := e.branch(); branch != "" {
		parts = append(parts, violetStyle.Render("⎇ "+branch))
	}
	if e.Iter > 0 {
		parts = append(parts, amberStyle.Render(fmt.Sprintf("iter %d", e.Iter)))
	}
	if e.Run != nil {
		u := e.Run.Usage
		if u.InputTokens+u.OutputTokens > 0 {
			parts = append(parts, mutedStyle.Render(fmt.Sprintf("%s tok", tokens(u.InputTokens+u.OutputTokens))))
		}
		if u.CostUSD > 0 {
			parts = append(parts, goldStyle.Render(fmt.Sprintf("$%.2f", u.CostUSD)))
		}
		if e.Run.Commit != "" {
			parts = append(parts, greenStyle.Render("● "+shortSHA(e.Run.Commit)))
		}
	}
	if d := e.duration(); d > 0 {
		parts = append(parts, mutedStyle.Render("⏱ "+elapsed(d)))
	}
	if len(parts) == 0 {
		parts = append(parts, mutedStyle.Render("starting…"))
	}
	return truncate(strings.Join(parts, mutedStyle.Render("  ·  ")), w)
}

// --- pipeline flow ----------------------------------------------------------

func (a *App) renderFlow(e *Entry, w int) string {
	cardW := (w - 2*arrowW) / 3
	if cardW < minCardW {
		return a.renderFlowVertical(e, w)
	}
	parts := make([]string, 0, 5)
	for i, k := range stageOrder {
		if i > 0 {
			col := cFaint
			if e.Stages[stageOrder[i-1]].done {
				col = cGreen
			}
			parts = append(parts, lipgloss.NewStyle().Foreground(col).Render(" ━━▶ "))
		}
		parts = append(parts, a.stageCard(e, k, cardW))
	}
	row := lipgloss.JoinHorizontal(lipgloss.Center, parts...)
	flow := row + "\n" + a.loopLine(e, cardW)
	pad := strings.Repeat(" ", max(0, (w-lipgloss.Width(row))/2))
	return pad + strings.ReplaceAll(flow, "\n", "\n"+pad)
}

// loopLine draws the fix-loop return path from the reviewer back to the
// executor underneath the cards.
func (a *App) loopLine(e *Entry, cardW int) string {
	execMid := cardW + arrowW + cardW/2
	revMid := 2*(cardW+arrowW) + cardW/2
	span := revMid - execMid - 2 // between "╰◀" and "╯"
	label := fmt.Sprintf(" ↺ fix loop · max %d ", a.cfg.Loop.MaxIterations)
	st := faintStyle
	labelSt := mutedStyle
	if e.Iter > 0 {
		label = fmt.Sprintf(" ↺ fix loop · iteration %d ", e.Iter)
		st, labelSt = amberStyle, amberStyle.Bold(true)
	}
	for _, short := range []string{fmt.Sprintf(" ↺ fix loop #%d ", e.Iter), fmt.Sprintf(" ↺ %d ", e.Iter)} {
		if lipgloss.Width(label) <= span {
			break
		}
		label = short
	}
	left := (span - lipgloss.Width(label)) / 2
	right := span - lipgloss.Width(label) - left
	if left < 0 || right < 0 {
		return ""
	}
	return strings.Repeat(" ", execMid) +
		st.Render("╰◀"+strings.Repeat("─", left)) + labelSt.Render(label) +
		st.Render(strings.Repeat("─", right)+"╯")
}

func (a *App) stageCard(e *Entry, k agent.Kind, w int) string {
	si := e.Stages[k]
	glyph, word, col := a.stageStatus(si, e.Live)

	border := cFaint
	switch {
	case si.failed:
		border = cRed
	case si.done:
		border = cGreen
	case si.status != "" && e.Live:
		border = cCyan
		if a.frame/5%2 == 1 {
			border = cViolet
		}
	case si.status != "":
		border = cAmber
	}
	inner := w - 2
	role := lipgloss.NewStyle().Foreground(roleColor[k]).Bold(true).
		Render(roleGlyph[k] + " " + strings.ToUpper(string(k)))
	model := mutedStyle.Render(truncate(choiceText(a.stageChoice(e, k)), inner))
	status := lipgloss.NewStyle().Foreground(col).Render(truncate(glyph+" "+word, inner))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Width(inner).
		Align(lipgloss.Center).
		Render(role + "\n" + model + "\n" + status)
}

func (a *App) renderFlowVertical(e *Entry, w int) string {
	var rows []string
	for i, k := range stageOrder {
		glyph, word, col := a.stageStatus(e.Stages[k], e.Live)
		role := lipgloss.NewStyle().Foreground(roleColor[k]).Bold(true).
			Render(fmt.Sprintf("%s %-8s", roleGlyph[k], strings.ToUpper(string(k))))
		st := lipgloss.NewStyle().Foreground(col).Render(glyph + " " + word)
		model := mutedStyle.Render(a.stageChoice(e, k).Model)
		rows = append(rows, truncate(role+"  "+st+"  "+model, w))
		if i < len(stageOrder)-1 {
			rows = append(rows, faintStyle.Render("  │"))
		}
	}
	if e.Iter > 0 {
		rows = append(rows, amberStyle.Render(fmt.Sprintf("  ↺ fix loop · iteration %d", e.Iter)))
	}
	return strings.Join(rows, "\n")
}

func (a *App) stageStatus(si *StageInfo, live bool) (glyph, word string, col lipgloss.AdaptiveColor) {
	switch {
	case si.failed:
		return "✘", statusWord(si.status, "failed"), cRed
	case si.done:
		return "✔", "done", cGreen
	case si.status != "" && live:
		return spin[a.frame%len(spin)], statusWord(si.status, "running"), cCyan
	case si.status != "":
		return "◌", "stalled", cAmber
	default:
		return "·", "pending", cMuted
	}
}

func statusWord(status, fallback string) string {
	s := strings.TrimSpace(status)
	switch {
	case s == "":
		return fallback
	case strings.Contains(strings.ToLower(s), "awaiting"):
		return "your turn"
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

// stageChoice returns the provider/model/effort a run's stage card shows: the
// run's own snapshot when known, otherwise the session's current selection.
func (a *App) stageChoice(e *Entry, k agent.Kind) models.Choice {
	if e != nil {
		if c := e.Models.For(k); c.Model != "" {
			return c
		}
	}
	return a.choices.For(k)
}

// --- gate, tabs and content ---------------------------------------------------

func (a *App) renderGate(e *Entry, w int) []string {
	bar := amberStyle.Render("┃ ")
	var title string
	var chips []string
	switch e.Gate.kind {
	case gateRetry:
		title = strings.ToUpper(e.Gate.step) + " FAILED"
		chips = []string{chip("t", "retry", cGreen), chip("s", "stop", cRed), chip("p", "push", cCyan)}
	case gateAgent:
		title = fmt.Sprintf("%s FAILED — retry or change agent", strings.ToUpper(string(e.Gate.agentKind)))
		chips = []string{chip("t", "retry", cGreen), chip("↑↓", "choose", cCyan), chip("enter", "use", cGreen), chip("esc", "stop", cRed)}
	case gatePlan:
		title = "PLAN READY — approve to start the executor"
		chips = []string{chip("a", "approve", cGreen), chip("r", "reject", cRed)}
	case gateCommit:
		title = "CHANGES STAGED — commit or push " + e.Gate.branch
		chips = []string{chip("c", "commit", cGreen), chip("p", "commit + push", cCyan), chip("s", "skip", cMuted)}
	case gateWorktree:
		title = "BRANCH " + e.Gate.branch + " ALREADY EXISTS — keep its worktree or create a new one"
		chips = []string{chip("k", "keep existing", cGreen), chip("c", "create new", cCyan)}
	case gateReview:
		if e.Gate.review != nil && e.Gate.review.Pass() {
			title = "REVIEW PASSED — approve to commit the branch"
			chips = []string{chip("a", "approve", cGreen), chip("f", "another fix pass", cAmber), chip("r", "reject", cRed)}
		} else {
			title = "REVIEW FAILED — send the issues back or stop"
			chips = []string{chip("f", "fix", cAmber), chip("r", "reject", cRed)}
		}
	}
	if e.Gate.kind == gateAgent {
		lines := []string{bar + amberStyle.Bold(true).Render(truncate("◆ "+title, w-2))}
		if e.Gate.cause != nil {
			lines = append(lines, bar+faintStyle.Render(truncate(fmt.Sprintf("%s failed: %v", e.Gate.failed, e.Gate.cause), w-2)))
		}
		for i, o := range e.Gate.options {
			cur := i == e.Gate.cursor
			mark := "  "
			if cur {
				mark = goldStyle.Render("❯ ")
			}
			label := fmt.Sprintf("%d. %s", i+1, o)
			if o == e.Gate.preferred {
				label += " (preferred)"
			}
			style := textStyle
			if cur {
				style = goldStyle.Bold(true)
			}
			lines = append(lines, bar+mark+style.Render(truncate(label, w-4)))
		}
		for _, line := range packHints(chips, max(1, w-2)) {
			lines = append(lines, bar+line)
		}
		return lines
	}
	lines := []string{bar + amberStyle.Bold(true).Render(truncate("◆ "+title, w-2))}
	if e.Gate.kind == gateRetry && e.Gate.cause != nil {
		lines = append(lines, bar+redStyle.Render(truncate(sanitize(e.Gate.cause.Error()), w-2)))
	}
	for _, line := range packHints(chips, max(1, w-2)) {
		lines = append(lines, bar+line)
	}
	return lines
}

func (a *App) tabBar(e *Entry, w, off, maxOff int) string {
	if w < 60 {
		return truncate(goldStyle.Bold(true).Render(fmt.Sprintf("%d %s", a.tab+1, tabNames[a.tab]))+mutedStyle.Render("  · tab/1–5 views"), w)
	}
	var parts []string
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		switch tab(i) {
		case tabPlan:
			if e.Plan != nil {
				label += fmt.Sprintf(" %d", len(e.Plan.Steps))
			}
		case tabSupport:
			if e.shellStop != nil {
				label += " ●"
			}
		case tabReview:
			if e.Review != nil {
				if e.Review.Pass() {
					label += " ✔"
				} else {
					label += " ✘"
				}
			}
		}
		if tab(i) == a.tab {
			parts = append(parts, tabActive.Render(label))
		} else {
			parts = append(parts, tabInactive.Render(label))
		}
	}
	left := strings.Join(parts, "   ")

	right := ""
	switch {
	case maxOff == 0:
	case a.follow:
		right = cyanStyle.Render("● live")
	default:
		right = mutedStyle.Render(fmt.Sprintf("%d%%", off*100/maxOff))
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

// contentLines renders the selected tab, cached until the entry changes.
func (a *App) contentLines(e *Entry, w int) []string {
	switch a.tab {
	case tabActivity:
		e.ensureLogs()
	case tabDiff:
		e.ensureDiff()
	}
	c := &a.cache
	if c.entry == e && c.tab == a.tab && c.width == w && c.ver == e.ver && c.lines != nil {
		return c.lines
	}
	var lines []string
	switch a.tab {
	case tabActivity:
		if len(e.Logs) == 0 {
			lines = emptyNote("No activity recorded for this run.", w)
		} else {
			lines = renderActivity(e.Logs, w)
		}
	case tabPlan:
		lines = renderPlan(e.Plan, w)
	case tabReview:
		lines = renderReview(e.Review, w)
	case tabDiff:
		lines = renderDiff(e.Diff, w)
	case tabSupport:
		lines = renderShell(e, w)
	}
	*c = contentCache{entry: e, tab: a.tab, width: w, ver: e.ver, lines: lines}
	return lines
}

func (a *App) welcome(w, h int) []string {
	if w < 60 || h < 18 {
		return []string{
			gradient("korchestrate", gradFrom, gradTo, true),
			"",
			textStyle.Render("type a prompt below"),
			mutedStyle.Render("blank name: current checkout"),
			keyHint("n", "new prompt"),
			keyHint("m", "models") + "  " + keyHint("b", "runs"),
		}
	}
	art := []string{
		"      ▲      ",
		"     ╱ ╲     ",
		"    ╱ ◉ ╲    ",
		"   ╱─────╲   ",
		"  ╱       ╲  ",
		" ▔▔▔▔▔▔▔▔▔▔▔ ",
	}
	var out []string
	for _, l := range art {
		out = append(out, gradient(l, gradFrom, gradTo, true))
	}
	out = append(out, "",
		gradient("korchestrate", gradFrom, gradTo, true),
		mutedStyle.Render("one prompt · three minds · your chosen branch"),
		"",
		lipgloss.NewStyle().Foreground(cViolet).Render("plan")+faintStyle.Render(" ━▶ ")+
			lipgloss.NewStyle().Foreground(cCyan).Render("execute")+faintStyle.Render(" ━▶ ")+
			lipgloss.NewStyle().Foreground(cGold).Render("review"),
		mutedStyle.Render(fmt.Sprintf("%s · %s · %s",
			a.choices.Planner.Model, a.choices.Executor.Model, a.choices.Reviewer.Model)),
		"",
		textStyle.Render("press ")+goldStyle.Bold(true).Render("n")+textStyle.Render(" and describe a change"),
		textStyle.Render("press ")+goldStyle.Bold(true).Render("m")+textStyle.Render(" to pick provider · model · effort"),
	)
	for i, l := range out {
		out[i] = lipgloss.PlaceHorizontal(w, lipgloss.Center, l)
	}
	if pad := (h - len(out)) / 2; pad > 0 {
		out = append(make([]string, pad), out...)
	}
	return out
}

// --- footer -----------------------------------------------------------------

func (a *App) renderFooter(w int) string {
	if e := a.confirmDel; e != nil {
		message := "Removes its worktree, branch and run history."
		if e.Run != nil && e.Run.InPlace {
			message = "Removes run history; keeps your checkout and branch."
		}
		lines := []string{redStyle.Bold(true).Render("⚠ Delete " + e.branch() + "?")}
		lines = append(lines, wrap(message, max(1, w-4))...)
		if e.Run != nil && !e.Run.InPlace && e.Run.Commit != "" && !e.Run.Pushed {
			lines = append(lines, amberStyle.Render("Commit "+shortSHA(e.Run.Commit)+" was never pushed and will be lost."))
		}
		lines = append(lines, chip("y", "delete", cRed)+"  "+chip("esc", "keep", cMuted))
		for i, l := range lines {
			lines[i] = truncate(l, w-4)
		}
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cRed).
			Padding(0, 1).Width(w - 2)
		return box.Render(strings.Join(lines, "\n")) + "\n"
	}
	if a.confirmQuit {
		n := a.liveCount()
		msg := amberStyle.Bold(true).Render(fmt.Sprintf("⚠ %d run(s) in flight.", n)) +
			textStyle.Render(" Quitting cancels active runs. ") +
			chip("q", "quit", cRed) + "  " + chip("esc", "stay", cMuted)
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAmber).
			Padding(0, 1).Width(w - 2)
		return box.Render(strings.Join(wrap(msg, max(1, w-4)), "\n")) + "\n"
	}

	border := cFaint
	if a.inputFocus {
		border = cGold
	}
	avail := max(1, w-4)
	nameActive := a.inputFocus && a.field == fieldName
	promptActive := a.inputFocus && a.field == fieldPrompt
	body := inputLine("name", string(a.inputName), nameActive, "blank: current checkout", avail) + "\n" +
		inputLine("prompt", string(a.input), promptActive, "describe the change you want…", avail) + "\n" +
		a.modeLine(avail)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(w - 2).Render(body)

	hints := []string{keyHint("h", "keys")}
	e := a.current()
	if e != nil {
		hints = append(hints, keyHint("o", "full view"))
	}
	if w < 80 {
		lines := packHints(hints, w-2)
		lines = lines[:min(len(lines), 3)]
		if a.notice != "" {
			lines = wrap(amberStyle.Render("⚠ "+a.notice), max(1, w-2))
		}
		if web := a.webLine(w); web != "" {
			lines = append([]string{web}, lines...)
		}
		if !a.inputFocus && e != nil {
			return strings.Join(lines, "\n")
		}
		return box + "\n" + strings.Join(lines, "\n")
	}
	if web := a.webLine(w); web != "" {
		box += "\n" + web
	}
	if a.notice != "" {
		return box + "\n" + truncate(" "+amberStyle.Render("⚠ "+a.notice), w)
	}
	return box + "\n" + truncate(" "+strings.Join(hints, mutedStyle.Render("  ·  ")), w)
}

// modeLine shows the mode the next run starts in.
func (a *App) modeLine(avail int) string {
	mode := textStyle.Render("default") + mutedStyle.Render(" · asks before each step")
	if a.autopilot {
		mode = cyanStyle.Bold(true).Render("autopilot") + mutedStyle.Render(" · no questions; commits, pushes and opens a PR")
	}
	return truncate("  "+faintStyle.Render("mode:")+" "+mode+"  "+keyHint("ctrl+a", "switch"), avail)
}

// renderFullFooter keeps the full-screen run view to one hint line, falling
// back to the regular footer while typing a prompt or confirming.
func (a *App) renderFullFooter(w int) string {
	if a.inputFocus || a.confirmDel != nil || a.confirmQuit {
		return a.renderFooter(w)
	}
	if a.notice != "" {
		return truncate(" "+amberStyle.Render("⚠ "+a.notice), w)
	}
	hints := []string{keyHint("o", "exit full view"), keyHint("n", "new run"), keyHint("h", "keys")}
	footer := truncate(" "+strings.Join(hints, mutedStyle.Render("  ·  ")), w)
	if web := a.webLine(w); web != "" {
		footer = web + "\n" + footer
	}
	return footer
}

// packHints wraps whole controls so their key and label stay together.
func packHints(hints []string, w int) []string {
	var lines []string
	line := ""
	for _, hint := range hints {
		if line != "" && lipgloss.Width(line)+2+lipgloss.Width(hint) > w {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += "  "
		}
		line += truncate(hint, w)
	}
	return append(lines, line)
}

// --- layout helpers ---------------------------------------------------------

// inputLine renders one labeled field of the footer input.
func inputLine(label, val string, active bool, placeholder string, avail int) string {
	mark := "  "
	labelSt := faintStyle.Render(label + ":")
	if active {
		mark = goldStyle.Bold(true).Render("❯ ")
		labelSt = goldStyle.Render(label + ":")
	}
	prefix := mark + labelSt + " "
	inner := avail - lipgloss.Width(mark) - lipgloss.Width(label) - 2
	if inner < 4 {
		inner = 4
	}
	var content string
	switch {
	case val == "" && active:
		content = lipgloss.NewStyle().Reverse(true).Render(" ") +
			mutedStyle.Render(" "+truncate(placeholder, max(0, inner-2)))
	case val == "":
		content = mutedStyle.Render(truncate(placeholder, inner))
	default:
		text := tail(val, inner)
		if active {
			content = textStyle.Render(text) + lipgloss.NewStyle().Reverse(true).Render(" ")
		} else {
			content = mutedStyle.Render(text)
		}
	}
	return truncate(prefix+content, avail)
}

// panel draws a rounded box of exactly w×h cells with the title set into the
// top border.
func panel(title string, lines []string, w, h int, border lipgloss.AdaptiveColor) string {
	bs := lipgloss.NewStyle().Foreground(border)
	inner := w - 2
	t := " " + truncate(title, inner-4) + " "
	tw := lipgloss.Width(t)
	top := bs.Render("╭─") + goldStyle.Bold(true).Render(t) + bs.Render(strings.Repeat("─", max(0, inner-1-tw))+"╮")

	out := make([]string, 0, h)
	out = append(out, top)
	cw := inner - 2
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = truncate(lines[i], cw)
		}
		pad := max(0, cw-lipgloss.Width(l))
		out = append(out, bs.Render("│")+" "+l+strings.Repeat(" ", pad)+" "+bs.Render("│"))
	}
	out = append(out, bs.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// tail keeps the rightmost cells of s that fit in n, marking the cut with "…".
func tail(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > n {
		r = r[1:]
	}
	return "…" + string(r)
}

func shortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
