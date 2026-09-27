package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
)

func TestLogsCommand(t *testing.T) {
	base := t.TempDir()
	run, err := artifact.New(base, "/repo", "x")
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Log(artifact.LogEntry{Level: artifact.LevelInfo, Stage: "run", Event: "run.start", Message: "started"})
	_ = run.Log(artifact.LogEntry{Level: artifact.LevelError, Stage: "executor", Agent: "codex", Event: "error", Message: "boom\nsecond line"})

	runLogs := func(args ...string) string {
		t.Helper()
		dir := base
		cmd := newLogsCmd(&dir)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{run.ID}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("logs %v: %v", args, err)
		}
		return out.String()
	}

	full := runLogs()
	for _, want := range []string{"INFO", "run.start", "ERROR", "executor", "boom ⏎ second line", "agent=codex"} {
		if !strings.Contains(full, want) {
			t.Errorf("full log missing %q:\n%s", want, full)
		}
	}

	errorsOnly := runLogs("--errors")
	if strings.Contains(errorsOnly, "run.start") || !strings.Contains(errorsOnly, "boom") {
		t.Errorf("--errors output wrong:\n%s", errorsOnly)
	}

	raw := strings.TrimSpace(runLogs("--json"))
	lines := strings.Split(raw, "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 json lines got %d:\n%s", len(lines), raw)
	}
	var entry artifact.LogEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil || entry.Level != artifact.LevelError {
		t.Fatalf("json line = %q err=%v", lines[1], err)
	}
}
