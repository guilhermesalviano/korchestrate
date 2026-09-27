package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunLogRoundTrip(t *testing.T) {
	base := t.TempDir()
	r, err := New(base, "/repo", "x")
	if err != nil {
		t.Fatal(err)
	}
	code := 1
	if err := r.Log(LogEntry{
		Level: LevelInfo, Stage: "executor", Agent: "codex", Model: "gpt", Iter: 1,
		Event: "stage.end", Message: "executor finished", DurationMS: 1234, ExitCode: &code,
		Usage: &Usage{InputTokens: 10, OutputTokens: 4, CostUSD: 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Log(LogEntry{Level: LevelError, Stage: "push", Event: "error", Message: "boom"}); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadLog(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries got %d", len(entries))
	}
	first := entries[0]
	if first.Time.IsZero() || first.Level != LevelInfo || first.Stage != "executor" ||
		first.Agent != "codex" || first.Model != "gpt" || first.Iter != 1 || first.DurationMS != 1234 {
		t.Fatalf("unexpected first entry: %+v", first)
	}
	if first.ExitCode == nil || *first.ExitCode != 1 {
		t.Fatalf("exit code = %v", first.ExitCode)
	}
	if first.Usage == nil || first.Usage.InputTokens != 10 || first.Usage.CostUSD != 0.5 {
		t.Fatalf("usage = %+v", first.Usage)
	}
	if entries[1].Level != LevelError || entries[1].Message != "boom" {
		t.Fatalf("unexpected second entry: %+v", entries[1])
	}
}

func TestReadLogMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()
	entries, err := ReadLog(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing log: entries=%v err=%v", entries, err)
	}
	data := "{not json}\n" + `{"level":"info","message":"ok"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, LogFile), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err = ReadLog(dir)
	if err != nil || len(entries) != 1 || entries[0].Message != "ok" {
		t.Fatalf("malformed tolerance: entries=%+v err=%v", entries, err)
	}
}

func TestSetStateLogsTransition(t *testing.T) {
	base := t.TempDir()
	r, err := New(base, "/repo", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetState(StatePlanning); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadLog(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Event != "state" || entries[0].Message != string(StatePlanning) {
		t.Fatalf("state transition log = %+v", entries)
	}
}

func TestErrorHistory(t *testing.T) {
	base := t.TempDir()
	r, err := New(base, "/repo", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddError("executor", "codex", errors.New("codex failed: exit 1")); err != nil {
		t.Fatal(err)
	}
	// A consecutive duplicate (the same failure reaching the final handler)
	// must not be recorded twice.
	if err := r.AddError("executor", "codex", errors.New("codex failed: exit 1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Fail(errors.New("review did not pass")); err != nil {
		t.Fatal(err)
	}

	got, err := Load(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) != 2 {
		t.Fatalf("want 2 errors got %d: %+v", len(got.Errors), got.Errors)
	}
	if got.Errors[0].Stage != "executor" || got.Errors[0].Agent != "codex" {
		t.Fatalf("first error = %+v", got.Errors[0])
	}
	if got.Errors[1].Message != "review did not pass" || got.Error != "review did not pass" {
		t.Fatalf("fatal error = %+v / %q", got.Errors[1], got.Error)
	}

	entries, err := ReadLog(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	errorsLogged := 0
	for _, e := range entries {
		if e.Level == LevelError {
			errorsLogged++
		}
	}
	if errorsLogged != 2 {
		t.Fatalf("want 2 error log entries got %d: %+v", errorsLogged, entries)
	}
}
