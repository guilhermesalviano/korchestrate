package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
)

// silentGate fails the test when autopilot asks for any confirmation.
type silentGate struct {
	recordingGate
	t *testing.T
}

func (g *silentGate) PlanGate(context.Context, *contracts.Plan, string) (ui.Decision, error) {
	g.t.Error("autopilot asked for plan approval")
	return ui.Approve, nil
}
func (g *silentGate) ReviewGate(context.Context, *contracts.Review, string) (ui.Decision, error) {
	g.t.Error("autopilot asked for review approval")
	return ui.Approve, nil
}
func (g *silentGate) CommitGate(context.Context, string, string) (ui.CommitDecision, error) {
	g.t.Error("autopilot asked before committing")
	return ui.CommitStop, nil
}
func (g *silentGate) WorktreeGate(context.Context, string) (ui.WorktreeDecision, error) {
	g.t.Error("autopilot asked about an existing worktree")
	return ui.WorktreeReuse, nil
}
func (g *silentGate) RetryGate(context.Context, string, error) (bool, error) {
	g.t.Error("autopilot asked to retry a step")
	return false, nil
}

func stubPR(t *testing.T, fn func(dir, branch, title, body string) (string, error)) {
	t.Helper()
	prev := openPR
	openPR = func(_ context.Context, dir, branch, title, body string) (string, error) {
		return fn(dir, branch, title, body)
	}
	t.Cleanup(func() { openPR = prev })
}

// withOrigin gives repo a bare origin so pushes succeed.
func withOrigin(t *testing.T, repo string) {
	t.Helper()
	origin := t.TempDir()
	gitRun(t, origin, "init", "--bare", "-b", "main")
	gitRun(t, repo, "remote", "add", "origin", origin)
}

func autopilotFactory(t *testing.T, verdicts ...string) (func(string) (agent.Agent, error), *int) {
	execRuns := 0
	reviews := 0
	return func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				execRuns++
				body := strings.Repeat("ok\n", execRuns)
				return &agent.Result{}, os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte(body), 0o644)
			}}, nil
		default:
			return fakeAgent{"opencode", agent.Reviewer, func(context.Context, agent.Request) (*agent.Result, error) {
				v := verdicts[min(reviews, len(verdicts)-1)]
				reviews++
				raw, _ := json.Marshal(map[string]any{"verdict": v, "summary": "review " + v,
					"issues": []map[string]string{{"file": "feature.txt", "description": "needs work"}}})
				if v == "pass" {
					raw, _ = json.Marshal(map[string]string{"verdict": v, "summary": "looks good"})
				}
				return &agent.Result{Structured: raw}, nil
			}}, nil
		}
	}, &execRuns
}

func TestAutopilotCommitsPushesAndOpensPR(t *testing.T) {
	repo := setupRepo(t)
	withOrigin(t, repo)
	cfg := baseConfig(t, repo)
	cfg.Loop.MaxIterations = 2
	gate := &silentGate{t: t}
	factory, execRuns := autopilotFactory(t, "fail", "pass")

	var gotBranch, gotTitle, gotBody string
	stubPR(t, func(_, branch, title, body string) (string, error) {
		gotBranch, gotTitle, gotBody = branch, title, body
		return "https://github.com/o/r/pull/7", nil
	})

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "auto", Autopilot: true}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if *execRuns != 2 {
		t.Fatalf("executor ran %d times, want a fix pass after the failed review", *execRuns)
	}
	if p.Run.State != artifact.StateDone || p.Run.Commit == "" || !p.Run.Pushed {
		t.Fatalf("run = state %s commit %q pushed %v", p.Run.State, p.Run.Commit, p.Run.Pushed)
	}
	if !p.Run.Autopilot || p.Run.PR != "https://github.com/o/r/pull/7" {
		t.Fatalf("run autopilot=%v pr=%q", p.Run.Autopilot, p.Run.PR)
	}
	if gotBranch != "auto" || gotTitle != "add feature" {
		t.Fatalf("PR branch=%q title=%q", gotBranch, gotTitle)
	}
	for _, want := range []string{"add feature", "feature.txt exists", "looks good", p.Run.ID} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("PR body missing %q:\n%s", want, gotBody)
		}
	}
	last := gate.infos[len(gate.infos)-1]
	if !strings.Contains(last, "autopilot finished") || !strings.Contains(last, "pull/7") {
		t.Fatalf("end message = %q", last)
	}
}

func TestAutopilotStopsWhenReviewsKeepFailing(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &silentGate{t: t}
	factory, _ := autopilotFactory(t, "fail")
	stubPR(t, func(string, string, string, string) (string, error) {
		t.Error("a failed run must not open a PR")
		return "", nil
	})

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "auto", Autopilot: true}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err == nil {
		t.Fatal("expected the run to fail")
	}
	if p.Run.State != artifact.StateFailed {
		t.Fatalf("state = %s, want failed", p.Run.State)
	}
	if msg := EndMessage(p.Run); !strings.Contains(msg, "autopilot stopped") {
		t.Fatalf("end message = %q", msg)
	}
}

func TestAutopilotPicksNextFreeBranch(t *testing.T) {
	repo := setupRepo(t)
	gitRun(t, repo, "branch", "auto")
	cfg := baseConfig(t, repo)
	factory, _ := autopilotFactory(t, "pass")
	stubPR(t, func(string, string, string, string) (string, error) { return "", nil })

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "auto", Autopilot: true}, Gate: &silentGate{t: t}, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p.Run.Branch != "auto-2" {
		t.Fatalf("branch = %q, want auto-2", p.Run.Branch)
	}
	// No origin: the push fails but the commit is kept and reported.
	if p.Run.Pushed || !strings.Contains(EndMessage(p.Run), "push failed") {
		t.Fatalf("pushed=%v end=%q", p.Run.Pushed, EndMessage(p.Run))
	}
}

func TestAutopilotFallsBackWhenReviewsStayInvalid(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &silentGate{t: t}

	var opencodeRuns, claudeReviewRuns int
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				if strings.Contains(r.Prompt, "## Diff under review") {
					claudeReviewRuns++
					return &agent.Result{Final: `{"verdict":"pass","summary":"recovered"}`,
						Structured: json.RawMessage(`{"verdict":"pass","summary":"recovered"}`)}, nil
				}
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				return &agent.Result{}, os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
			}}, nil
		default:
			return fakeAgent{"opencode", agent.Reviewer, func(context.Context, agent.Request) (*agent.Result, error) {
				opencodeRuns++
				return &agent.Result{Final: `{"summary":"no verdict"}`,
					Structured: json.RawMessage(`{"summary":"no verdict"}`),
					Events:     []json.RawMessage{json.RawMessage(`{"type":"text"}`)}}, nil
			}}, nil
		}
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "auto", Autopilot: true}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if opencodeRuns != 2 {
		t.Fatalf("opencode reviewer ran %d times, want 2 attempts before falling back", opencodeRuns)
	}
	if claudeReviewRuns != 1 {
		t.Fatalf("claude fallback reviewer ran %d times, want 1", claudeReviewRuns)
	}
	if p.Run.State != artifact.StateDone {
		t.Fatalf("state = %s, want done", p.Run.State)
	}
	// Every attempt must leave its raw output behind for debugging.
	for _, name := range []string{
		"reviewer.last.0.1.opencode.txt",
		"reviewer.last.0.2.opencode.txt",
		"reviewer.last.0.1.claude.txt",
		"reviewer.events.0.1.opencode.jsonl",
	} {
		if _, err := os.Stat(p.Run.Path(name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
}

func TestPRTitleIsCommitSubject(t *testing.T) {
	if got := prTitle("feat: add pages\n\nbody\n\nkor run x"); got != "feat: add pages" {
		t.Fatalf("title = %q", got)
	}
}

func TestAutopilotOnDefaultBranchPushesWithoutPR(t *testing.T) {
	repo := setupRepo(t)
	withOrigin(t, repo)
	cfg := baseConfig(t, repo)
	gate := &silentGate{t: t}
	factory, _ := autopilotFactory(t, "pass")
	stubPR(t, func(_, branch, _, _ string) (string, error) {
		t.Errorf("opened a pull request from the default branch %q", branch)
		return "", nil
	})

	// A blank name works in the current checkout, which is on main.
	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Autopilot: true}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !p.Run.InPlace || p.Run.Branch != "main" || !p.Run.Pushed || p.Run.PR != "" {
		t.Fatalf("run in_place=%v branch=%q pushed=%v pr=%q", p.Run.InPlace, p.Run.Branch, p.Run.Pushed, p.Run.PR)
	}
	if !strings.Contains(strings.Join(gate.infos, "\n"), "main is the default branch") {
		t.Fatalf("missing explanation: %v", gate.infos)
	}
	if msg := EndMessage(p.Run); !strings.Contains(msg, "no PR: main is the default branch") {
		t.Fatalf("end message: %q", msg)
	}
}
