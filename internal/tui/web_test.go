package tui

import (
	"context"
	"strings"
	"testing"
)

type fakeWeb struct {
	active  bool
	stopped bool
}

func (f *fakeWeb) Links() []string {
	return []string{"http://127.0.0.1:8787/#token=x", "http://192.168.1.2:8787/#token=x"}
}
func (f *fakeWeb) Active() bool               { return f.active }
func (f *fakeWeb) Stop(context.Context) error { f.stopped = true; return nil }

func TestWebKeyStartsShowsAndStopsDashboard(t *testing.T) {
	a := NewApp(testConfig(), t.TempDir())
	a.width, a.height = 100, 30
	ws := &fakeWeb{active: true}
	a.StartWeb = func() (WebServer, error) { return ws, nil }

	a.handleKey(key("w"))
	if a.web == nil || !strings.Contains(a.View(), "192.168.1.2:8787") {
		t.Fatalf("web link not shown:\n%s", a.View())
	}
	// An active browser run needs a second press before it is cancelled.
	if _, cmd := a.handleKey(key("w")); cmd != nil || !a.confirmWeb {
		t.Fatal("expected confirmation before stopping an active run")
	}
	_, cmd := a.handleKey(key("w"))
	if cmd == nil {
		t.Fatal("expected stop command")
	}
	a.Update(cmd())
	if !ws.stopped || a.web != nil || strings.Contains(a.View(), "192.168.1.2") {
		t.Fatal("web dashboard not stopped")
	}
}

func TestWebRunCountsAsLiveOnQuit(t *testing.T) {
	a := NewApp(testConfig(), t.TempDir())
	a.web = &fakeWeb{active: true}
	if _, cmd := a.handleKey(key("q")); cmd != nil || !a.confirmQuit {
		t.Fatal("quitting with an active browser run should ask first")
	}
}
