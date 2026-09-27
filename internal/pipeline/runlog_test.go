package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
)

func logEntries(t *testing.T, run *artifact.Run) []artifact.LogEntry {
	t.Helper()
	entries, err := artifact.ReadLog(run.Dir)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return entries
}

func findEntry(entries []artifact.LogEntry, event, stage string) *artifact.LogEntry {
	for i := range entries {
		if entries[i].Event == event && (stage == "" || entries[i].Stage == stage) {
			return &entries[i]
		}
	}
	return nil
}

func lastEntry(entries []artifact.LogEntry, event, stage string) *artifact.LogEntry {
	var found *artifact.LogEntry
	for i := range entries {
		if entries[i].Event == event && (stage == "" || entries[i].Stage == stage) {
			found = &entries[i]
		}
	}
	return found
}

func TestRunLogRecordsHappyPath(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t), Duration: 2 * time.Second,
					Usage: agent.Usage{InputTokens: 11, OutputTokens: 3}}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"x"}`),
					Duration: 3 * time.Second, Usage: agent.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.25}}, nil
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Reviewer, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`),
					Duration: time.Second}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"},
		Gate: &recordingGate{}, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	entries := logEntries(t, p.Run)

	if e := findEntry(entries, "run.start", "run"); e == nil || !strings.Contains(e.Message, "test-run") {
		t.Fatalf("run.start entry = %+v", e)
	}
	if e := findEntry(entries, "run.done", "run"); e == nil {
		t.Fatalf("run.done entry = %+v", e)
	}
	for _, stage := range []string{"planner", "executor", "reviewer"} {
		if findEntry(entries, "stage.start", stage) == nil {
			t.Errorf("missing stage.start for %s", stage)
		}
		end := findEntry(entries, "stage.end", stage)
		if end == nil {
			t.Errorf("missing stage.end for %s", stage)
			continue
		}
		if end.DurationMS == 0 {
			t.Errorf("stage.end for %s has no duration", stage)
		}
	}
	end := findEntry(entries, "stage.end", "executor")
	if end == nil || end.Agent != "codex" || end.ExitCode == nil || *end.ExitCode != 0 {
		t.Fatalf("executor stage.end = %+v", end)
	}
	if end.Usage == nil || end.Usage.InputTokens != 100 || end.Usage.CostUSD != 0.25 {
		t.Fatalf("executor usage = %+v", end.Usage)
	}
	if e := findEntry(entries, "review.verdict", "reviewer"); e == nil || !strings.Contains(e.Message, "passed") {
		t.Fatalf("review verdict = %+v", e)
	}
	if len(p.Run.Errors) != 0 {
		t.Fatalf("happy path recorded errors: %+v", p.Run.Errors)
	}
}

func TestRunLogRecordsFailure(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	factory := func(name string) (agent.Agent, error) {
		return fakeAgent{name, agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
			return nil, errors.New("planner exploded")
		}}, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "x", Name: "test-run"},
		Gate: &recordingGate{}, AgentFactory: factory}
	if err := p.Execute(context.Background()); err == nil {
		t.Fatal("expected the run to fail")
	}
	if p.Run.State != artifact.StateFailed {
		t.Fatalf("state = %s, want failed", p.Run.State)
	}

	run, err := artifact.Load(p.Run.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Errors) == 0 {
		t.Fatal("expected an error history in run.json")
	}
	if run.Error == "" {
		t.Fatal("expected the fatal error to be set")
	}

	entries := logEntries(t, p.Run)
	var messages []string
	for _, e := range entries {
		if e.Level == artifact.LevelError {
			messages = append(messages, e.Message)
		}
	}
	if !strings.Contains(strings.Join(messages, "\n"), "planner exploded") {
		t.Fatalf("error entries missing the failure: %+v", messages)
	}
	if findEntry(entries, "stage.failed", "planner") == nil {
		t.Fatalf("missing stage.failed entry: %+v", entries)
	}
	if findEntry(entries, "run.failed", "run") == nil {
		t.Fatalf("missing run.failed entry: %+v", entries)
	}
}

func TestRunLogRecordsFallback(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &fallbackGate{choose: "opencode"}
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(context.Context, agent.Request) (*agent.Result, error) {
				return nil, errors.New("codex not working")
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				if !strings.Contains(r.OutFile, "executor.last") {
					return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
				}
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"wrote"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"},
		Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(p.Run.Errors) == 0 || p.Run.Errors[0].Agent != "codex" {
		t.Fatalf("recovered fallback failure missing from history: %+v", p.Run.Errors)
	}
	entries := logEntries(t, p.Run)
	if e := findEntry(entries, "agent.fallback", "executor"); e == nil || !strings.Contains(e.Message, "opencode") {
		t.Fatalf("fallback entry = %+v", e)
	}
	if e := lastEntry(entries, "stage.start", "executor"); e == nil || e.Agent != "opencode" {
		t.Fatalf("last executor start = %+v", e)
	}
}
