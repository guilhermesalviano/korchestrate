package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// WebServer is a web dashboard started from the TUI with "w".
type WebServer interface {
	// Links returns the dashboard links; the LAN link is last.
	Links() []string
	// Active reports whether a browser-started run is in flight.
	Active() bool
	// Stop shuts the server down and cancels its active run.
	Stop(ctx context.Context) error
}

type webStoppedMsg struct{ err error }

// Web returns the web dashboard left running, if any, so the host can stop it
// after the program exits.
func (a *App) Web() WebServer { return a.web }

// toggleWeb starts the web dashboard, or stops it off the UI goroutine. A
// browser run in flight needs a second "w" to confirm cancelling it.
func (a *App) toggleWeb() tea.Cmd {
	confirm := a.confirmWeb
	a.confirmWeb = false
	switch {
	case a.StartWeb == nil:
		a.notice = "web dashboard is unavailable here; run kor web"
	case a.webStopping:
		a.notice = "web dashboard is stopping…"
	case a.web == nil:
		ws, err := a.StartWeb()
		if err != nil {
			a.notice = "web dashboard: " + err.Error()
			return nil
		}
		a.web = ws
		a.notice = "web dashboard on · anyone on your local network can use it to control agents here"
	case a.web.Active() && !confirm:
		a.confirmWeb = true
		a.notice = "a browser run is active; press w again to cancel it and stop the web dashboard"
	default:
		a.webStopping = true
		ws := a.web
		return func() tea.Msg { return webStoppedMsg{ws.Stop(context.Background())} }
	}
	return nil
}

func (a *App) webStopped(msg webStoppedMsg) {
	a.web, a.webStopping = nil, false
	a.notice = "web dashboard stopped"
	if msg.err != nil {
		a.notice = "web dashboard stopped: " + msg.err.Error()
	}
}

// webLine shows where to open the running web dashboard.
func (a *App) webLine(w int) string {
	if a.web == nil {
		return ""
	}
	label := cyanStyle.Bold(true).Render("● web")
	if a.webStopping {
		label = mutedStyle.Render("○ web stopping")
	}
	links := a.web.Links()
	parts := []string{label}
	if len(links) > 0 {
		parts = append(parts, textStyle.Render(links[len(links)-1]))
	}
	parts = append(parts, keyHint("w", "stop"))
	return truncate(" "+strings.Join(parts, "  "), w)
}
