package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
)

type fakeAgent struct {
	name string
	kind agent.Kind
	fn   func(ctx context.Context, r agent.Request) (*agent.Result, error)
}

func (f fakeAgent) Name() string     { return f.name }
func (f fakeAgent) Kind() agent.Kind { return f.kind }
func (f fakeAgent) Run(ctx context.Context, r agent.Request) (*agent.Result, error) {
	return f.fn(ctx, r)
}

type recordingGate struct {
	planGates, reviewGates, worktreeGates int
	infos                                 []string
}

func (g *recordingGate) Stage(agent.Kind, string) {}
func (g *recordingGate) Line(agent.Event)         {}
func (g *recordingGate) Info(m string)            { g.infos = append(g.infos, m) }
func (g *recordingGate) Close()                   {}
func (g *recordingGate) PlanGate(context.Context, *contracts.Plan, string) (ui.Decision, error) {
	g.planGates++
	return ui.Approve, nil
}
func (g *recordingGate) ReviewGate(context.Context, *contracts.Review, string) (ui.Decision, error) {
	g.reviewGates++
	return ui.Approve, nil
}
func (g *recordingGate) CommitGate(context.Context, string, string) (ui.CommitDecision, error) {
	return ui.CommitOnly, nil
}
func (g *recordingGate) WorktreeGate(context.Context, string) (ui.WorktreeDecision, error) {
	g.worktreeGates++
	return ui.WorktreeCreate, nil
}
func (g *recordingGate) SelectAgent(context.Context, agent.Kind, string, []string, string, error) (string, error) {
	return "", nil
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")
	return dir
}

func baseConfig(t *testing.T, repo string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Repo = repo
	cfg.ArtifactsDir = t.TempDir()
	cfg.Loop.MaxIterations = 1
	cfg.Timeouts.Planner = config.Duration(0)
	cfg.Timeouts.Executor = config.Duration(0)
	cfg.Timeouts.Reviewer = config.Duration(0)
	return cfg
}

func planJSON(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"summary":"add feature","steps":[{"id":"1","description":"write feature.txt"}],"acceptance_criteria":["feature.txt exists"]}`)
}

func TestExecuteHappyPath(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &recordingGate{}

	execRuns := 0
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(_ context.Context, _ agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				execRuns++
				if err := os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"wrote feature"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Reviewer, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				if !strings.Contains(r.Prompt, "feature.txt") {
					t.Error("review prompt missing diff context")
				}
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"looks good"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p.Run.State != artifact.StateDone {
		t.Fatalf("state = %s, want done", p.Run.State)
	}
	if p.Run.Branch != "test-run" {
		t.Fatalf("branch = %q, want the worktree name", p.Run.Branch)
	}
	if p.Run.Commit == "" {
		t.Fatal("expected a commit")
	}
	if gate.planGates != 1 || gate.reviewGates != 1 {
		t.Fatalf("gates plan=%d review=%d", gate.planGates, gate.reviewGates)
	}
	if execRuns != 1 {
		t.Fatalf("executor ran %d times", execRuns)
	}
	for _, f := range []string{"plan.json", "review.json", "diff.patch", "run.json"} {
		if _, err := p.Run.Read(f); err != nil {
			t.Errorf("missing artifact %s: %v", f, err)
		}
	}
	if data, _ := p.Run.Read("diff.patch"); !strings.Contains(string(data), "feature.txt") {
		t.Errorf("diff.patch missing feature.txt:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(p.Run.Worktree, "feature.txt")); err != nil {
		t.Errorf("worktree missing file: %v", err)
	}
}

func TestExecuteFixLoopThenPass(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	cfg.Loop.MaxIterations = 2
	gate := &recordingGate{}

	execRuns, reviewRuns := 0, 0
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				execRuns++
				if execRuns > 1 && !strings.Contains(r.Prompt, "Fix required") {
					t.Error("second executor run missing fix instructions")
				}
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"x"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Reviewer, func(context.Context, agent.Request) (*agent.Result, error) {
				reviewRuns++
				if reviewRuns == 1 {
					return &agent.Result{Structured: json.RawMessage(`{"verdict":"fail","summary":"needs work","issues":[{"severity":"blocker","description":"missing tests"}]}`)}, nil
				}
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"fixed"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if execRuns != 2 || reviewRuns != 2 {
		t.Fatalf("exec=%d review=%d, want 2/2", execRuns, reviewRuns)
	}
	if p.Run.Iteration != 1 {
		t.Fatalf("iteration = %d, want 1", p.Run.Iteration)
	}
}

type fallbackGate struct {
	recordingGate
	choose string
}

func (g *fallbackGate) SelectAgent(context.Context, agent.Kind, string, []string, string, error) (string, error) {
	return g.choose, nil
}

func TestExecutorFallsBackOnFailure(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &fallbackGate{choose: "opencode"}

	codexRuns, opencodeExecRuns := 0, 0
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(context.Context, agent.Request) (*agent.Result, error) {
				codexRuns++
				return nil, errors.New("codex not working")
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				if !strings.Contains(r.OutFile, "executor.last") {
					return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
				}
				opencodeExecRuns++
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"wrote"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if codexRuns != 1 || opencodeExecRuns != 1 {
		t.Fatalf("codex=%d opencode=%d, want 1/1", codexRuns, opencodeExecRuns)
	}
	if p.Run.State != artifact.StateDone {
		t.Fatalf("state = %s, want done", p.Run.State)
	}
}

func TestNamedBranchCollision(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &recordingGate{}
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{name, agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{name, agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"x"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{name, agent.Reviewer, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	// A named run creates its requested branch.
	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "feature"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p.Run.Branch != "feature" {
		t.Fatalf("branch = %q, want feature", p.Run.Branch)
	}

	// Choosing create on a collision keeps the old branch and uses a suffix.
	p2 := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature again", Name: "feature"}, Gate: gate, AgentFactory: factory}
	if err := p2.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p2.Run.Branch != "feature-2" {
		t.Fatalf("branch = %q, want feature-2", p2.Run.Branch)
	}
	if gate.worktreeGates != 1 {
		t.Fatalf("worktree gate ran %d times, want 1", gate.worktreeGates)
	}
}

type reuseGate struct {
	recordingGate
	reuse bool
}

func (g *reuseGate) WorktreeGate(context.Context, string) (ui.WorktreeDecision, error) {
	g.worktreeGates++
	if g.reuse {
		return ui.WorktreeReuse, nil
	}
	return ui.WorktreeCreate, nil
}

func passFactory(t *testing.T) func(string) (agent.Agent, error) {
	t.Helper()
	return func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{name, agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{name, agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"x"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{name, agent.Reviewer, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}}, nil
		}
		return nil, nil
	}
}

func TestReuseExistingWorktree(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	factory := passFactory(t)

	// A named run creates the main-2 worktree and keeps it.
	p1 := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "first feature", Name: "main-2"}, Gate: &reuseGate{}, AgentFactory: factory}
	if err := p1.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p1.Run.Branch != "main-2" {
		t.Fatalf("first branch = %q, want main-2", p1.Run.Branch)
	}

	// A second run with the same name can reuse its worktree.
	gate := &reuseGate{reuse: true}
	p2 := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "second feature", Name: "main-2"}, Gate: gate, AgentFactory: factory}
	if err := p2.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p2.Run.Branch != "main-2" {
		t.Fatalf("second branch = %q, want the reused main-2", p2.Run.Branch)
	}
	if p2.Run.Worktree != p1.Run.Worktree {
		t.Fatalf("worktree = %q, want the reused %q", p2.Run.Worktree, p1.Run.Worktree)
	}
	if gate.worktreeGates != 1 {
		t.Fatalf("worktree gate ran %d times, want 1", gate.worktreeGates)
	}
	if _, err := os.Stat(filepath.Join(p2.Run.Worktree, "feature.txt")); err != nil {
		t.Errorf("reused worktree missing feature.txt: %v", err)
	}
}

func TestReuseBranchAfterWorktreeRemoved(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)

	// A leftover main-2 branch whose worktree directory was deleted without
	// git knowing: the registration is stale.
	stale := filepath.Join(t.TempDir(), "stale")
	gitRun(t, repo, "worktree", "add", "-b", "main-2", stale, "HEAD")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "again", Name: "main-2"}, Gate: &reuseGate{reuse: true}, AgentFactory: passFactory(t)}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p.Run.Branch != "main-2" {
		t.Fatalf("branch = %q, want the reused main-2", p.Run.Branch)
	}
	if p.Run.Worktree != p.Run.Path("worktree") {
		t.Fatalf("worktree = %q, want a fresh checkout at %q", p.Run.Worktree, p.Run.Path("worktree"))
	}
}

type rejectPlanGate struct{ recordingGate }

func (g *rejectPlanGate) PlanGate(context.Context, *contracts.Plan, string) (ui.Decision, error) {
	return ui.Reject, nil
}

func TestProvidedPlanSkipsPlanner(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &recordingGate{}

	plannerRuns := 0
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				plannerRuns++
				return nil, errors.New("planner must not run when a plan is provided")
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"wrote feature"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Reviewer, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				if !strings.Contains(r.Prompt, "predefined summary") {
					t.Error("review prompt missing the provided plan")
				}
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	plan := &contracts.Plan{
		Summary:            "predefined summary",
		Steps:              []contracts.PlanStep{{ID: "1", Description: "write feature.txt"}},
		AcceptanceCriteria: []string{"feature.txt exists"},
	}
	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run", Plan: plan}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if plannerRuns != 0 {
		t.Fatalf("planner ran %d times, want 0", plannerRuns)
	}
	if p.Run.State != artifact.StateDone {
		t.Fatalf("state = %s, want done", p.Run.State)
	}
	if data, err := p.Run.Read("plan.json"); err != nil || !strings.Contains(string(data), "predefined summary") {
		t.Errorf("plan.json artifact missing the provided plan: %v", err)
	}
	if gate.planGates != 1 {
		t.Fatalf("plan gate ran %d times, want 1 (provided plans still need approval)", gate.planGates)
	}
}

func TestRejectedPlanStaysAborted(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	factory := func(name string) (agent.Agent, error) {
		return fakeAgent{name, agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
			return &agent.Result{Structured: planJSON(t)}, nil
		}}, nil
	}
	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"}, Gate: &rejectPlanGate{}, AgentFactory: factory}
	if err := p.Execute(context.Background()); err == nil {
		t.Fatal("expected rejection error")
	}
	saved, err := artifact.Load(p.Run.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != artifact.StateAborted || saved.Error == "" {
		t.Fatalf("persisted state = %s (error %q), want aborted with error", saved.State, saved.Error)
	}
}

func TestFailedReviewResumesAtReviewer(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &recordingGate{}

	planRuns, execRuns, reviewRuns := 0, 0, 0
	reviewOK := false
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				planRuns++
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				execRuns++
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"x"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Reviewer, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				reviewRuns++
				if !reviewOK {
					return &agent.Result{Structured: json.RawMessage(`{"verdict":"","summary":""}`)}, nil
				}
				if !strings.Contains(r.Prompt, "feature.txt") {
					t.Error("resumed review is missing the executor's diff")
				}
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid review") {
		t.Fatalf("first run err = %v, want invalid review", err)
	}
	if p.Run.State != artifact.StateFailed {
		t.Fatalf("state = %s, want failed", p.Run.State)
	}
	if _, err := os.Stat(filepath.Join(p.Run.Worktree, "feature.txt")); err != nil {
		t.Fatalf("a failed run must keep its worktree for retry: %v", err)
	}

	reviewOK = true
	planRuns, execRuns, reviewRuns = 0, 0, 0
	retry := &Pipeline{Cfg: cfg, Run: p.Run, Opts: Options{Repo: repo, Prompt: "add feature", From: agent.Reviewer}, Gate: gate, AgentFactory: factory}
	if err := retry.Execute(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if planRuns != 0 || execRuns != 0 || reviewRuns != 1 {
		t.Fatalf("retry ran plan=%d exec=%d review=%d, want 0/0/1", planRuns, execRuns, reviewRuns)
	}
	if retry.Run.State != artifact.StateDone || retry.Run.Error != "" {
		t.Fatalf("state=%s error=%q, want done with no error", retry.Run.State, retry.Run.Error)
	}
}

func TestResumeRebuildsDeletedWorktree(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	gate := &recordingGate{}

	execRuns, reviewOK := 0, false
	factory := func(name string) (agent.Agent, error) {
		switch name {
		case "claude":
			return fakeAgent{"claude", agent.Planner, func(context.Context, agent.Request) (*agent.Result, error) {
				return &agent.Result{Structured: planJSON(t)}, nil
			}}, nil
		case "codex":
			return fakeAgent{"codex", agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				execRuns++
				_ = os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","summary":"x"}`)}, nil
			}}, nil
		case "opencode":
			return fakeAgent{"opencode", agent.Reviewer, func(_ context.Context, r agent.Request) (*agent.Result, error) {
				if !reviewOK {
					return &agent.Result{Structured: json.RawMessage(`{"verdict":"","summary":""}`)}, nil
				}
				if _, err := os.Stat(filepath.Join(r.Dir, "feature.txt")); err != nil {
					t.Error("rebuilt worktree is missing the executor's changes")
				}
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}}, nil
		}
		return nil, nil
	}

	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "test-run"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err == nil {
		t.Fatal("first run should fail")
	}
	if p.Run.Base == "" {
		t.Fatal("the run should record its base commit")
	}
	// Runs failed by older versions lost their worktree and branch.
	gitRun(t, repo, "worktree", "remove", "--force", p.Run.Worktree)
	gitRun(t, repo, "branch", "-D", p.Run.Branch)

	reviewOK, execRuns = true, 0
	retry := &Pipeline{Cfg: cfg, Run: p.Run, Opts: Options{Repo: repo, Prompt: "add feature", From: agent.Reviewer}, Gate: gate, AgentFactory: factory}
	if err := retry.Execute(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if execRuns != 0 {
		t.Fatalf("executor ran %d times; the patch should have restored its work", execRuns)
	}
	if retry.Run.State != artifact.StateDone || retry.Run.Commit == "" {
		t.Fatalf("state=%s commit=%q, want a committed done run", retry.Run.State, retry.Run.Commit)
	}
}

// Any installed adapter can plan; the executor gets the plan as markdown.
func TestPlannerCanBeOpencodeOrAntigravity(t *testing.T) {
	for _, plannerName := range []string{"opencode", "antigravity"} {
		t.Run(plannerName, func(t *testing.T) {
			repo := setupRepo(t)
			cfg := baseConfig(t, repo)
			cfg.Models.Planner = config.ModelSpec{Agent: plannerName, Model: "m"}
			cfg.Models.Reviewer.Agent = "claude"
			var planReq agent.Request
			var execPrompt string
			factory := func(name string) (agent.Agent, error) {
				return fakeAgent{name, agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
					switch r.Role {
					case agent.Planner:
						planReq = r
						return &agent.Result{Structured: planJSON(t)}, nil
					case agent.Executor:
						execPrompt = r.Prompt
						return &agent.Result{}, os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644)
					default:
						return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
					}
				}}, nil
			}
			p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "plan-" + plannerName}, Gate: &recordingGate{}, AgentFactory: factory}
			if err := p.Execute(context.Background()); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if planReq.Role != agent.Planner || planReq.SchemaInline == "" {
				t.Fatalf("planner request: %+v", planReq)
			}
			if plannerName == "opencode" && planReq.Agent != "plan" {
				t.Fatalf("opencode must plan with its read-only agent, got %q", planReq.Agent)
			}
			md, err := p.Run.Read("plan.md")
			if err != nil || !strings.Contains(string(md), "1. write feature.txt") {
				t.Fatalf("plan.md: %v\n%s", err, md)
			}
			for _, want := range []string{p.Run.Path("plan.md"), "1. write feature.txt", "Acceptance criteria", "- feature.txt exists"} {
				if !strings.Contains(execPrompt, want) {
					t.Fatalf("executor prompt missing %q:\n%s", want, execPrompt)
				}
			}
			if strings.Contains(execPrompt, `"acceptance_criteria"`) {
				t.Fatal("executor should get the markdown plan, not JSON")
			}
		})
	}
}

func TestResumeUsesEditedPlanMarkdown(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	var execPrompts []string
	factory := func(name string) (agent.Agent, error) {
		return fakeAgent{name, agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
			switch r.Role {
			case agent.Planner:
				return &agent.Result{Structured: planJSON(t)}, nil
			case agent.Executor:
				execPrompts = append(execPrompts, r.Prompt)
				return &agent.Result{}, os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte(strings.Repeat("x", len(execPrompts))), 0o644)
			default:
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}
		}}, nil
	}
	gate := &recordingGate{}
	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "edit-plan"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// An untouched plan.md resumes with the exact recorded plan.
	if plan, edited, err := loadRunPlan(p.Run); err != nil || edited || plan.Summary != "add feature" {
		t.Fatalf("unedited plan: %+v %v %v", plan, edited, err)
	}
	edited := "# Plan\n\n## Summary\n\nAdd feature, differently\n\n## Steps\n\n1. write feature.txt with a greeting\n\n## Acceptance criteria\n\n- feature.txt greets\n"
	if err := p.Run.Write("plan.md", []byte(edited)); err != nil {
		t.Fatal(err)
	}
	retry := &Pipeline{Cfg: cfg, Run: p.Run, Opts: Options{Repo: repo, Prompt: "add feature", From: agent.Executor}, Gate: gate, AgentFactory: factory}
	if err := retry.Execute(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	last := execPrompts[len(execPrompts)-1]
	if !strings.Contains(last, "write feature.txt with a greeting") || !strings.Contains(last, "feature.txt greets") {
		t.Fatalf("resume ignored the edited plan.md:\n%s", last)
	}
	if !strings.Contains(strings.Join(gate.infos, "\n"), "using your edited plan.md") {
		t.Fatalf("missing notice: %v", gate.infos)
	}
	var saved contracts.Plan
	data, _ := p.Run.Read("plan.json")
	if json.Unmarshal(data, &saved) != nil || saved.Summary != "Add feature, differently" {
		t.Fatalf("plan.json not updated from plan.md: %s", data)
	}
}

func TestReviewerGetsExecutorChecks(t *testing.T) {
	repo := setupRepo(t)
	cfg := baseConfig(t, repo)
	var reviewPrompts []string
	factory := func(name string) (agent.Agent, error) {
		return fakeAgent{name, agent.Executor, func(_ context.Context, r agent.Request) (*agent.Result, error) {
			switch r.Role {
			case agent.Planner:
				return &agent.Result{Structured: planJSON(t)}, nil
			case agent.Executor:
				if err := os.WriteFile(filepath.Join(r.Dir, "feature.txt"), []byte("ok\n"), 0o644); err != nil {
					return nil, err
				}
				return &agent.Result{Structured: json.RawMessage(`{"status":"done","changed_files":["feature.txt"],"summary":"wrote feature","commands":["npm test → passed (3 tests)"],"known_gaps":[]}`)}, nil
			default:
				reviewPrompts = append(reviewPrompts, r.Prompt)
				return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"ok"}`)}, nil
			}
		}}, nil
	}
	gate := &recordingGate{}
	p := &Pipeline{Cfg: cfg, Opts: Options{Repo: repo, Prompt: "add feature", Name: "checks"}, Gate: gate, AgentFactory: factory}
	if err := p.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// A review resumed on its own reads the saved report.
	retry := &Pipeline{Cfg: cfg, Run: p.Run, Opts: Options{Repo: repo, Prompt: "add feature", From: agent.Reviewer}, Gate: gate, AgentFactory: factory}
	if err := retry.Execute(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(reviewPrompts) != 2 {
		t.Fatalf("reviews = %d", len(reviewPrompts))
	}
	for i, prompt := range reviewPrompts {
		for _, want := range []string{"## Executor's report", "- npm test → passed (3 tests)", "do not run them again"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("review %d missing %q:\n%s", i, want, prompt)
			}
		}
	}
}
