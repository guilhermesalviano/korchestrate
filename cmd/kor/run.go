package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/models"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/tui"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
)

func newRunCmd(configPath, repo, artifactsDir *string) *cobra.Command {
	var (
		yes, noTUI, allowDirty, keepWT, apply bool
		autopilot                             bool
		maxIter                               int
		name                                  string
		planPaths                             []string
		plannerModel, executorModel           string
		reviewerModel                         string
	)

	cmd := &cobra.Command{
		Use:   "run [prompt]",
		Short: "plan, execute and review a prompt end to end",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt, err := promptFrom(args, os.Stdin)
			if err != nil {
				return err
			}
			if strings.TrimSpace(prompt) == "" {
				return errors.New("a prompt is required (argument or stdin)")
			}
			cfg, err := resolveConfig(*configPath, *repo, *artifactsDir)
			if err != nil {
				return err
			}
			if maxIter > 0 {
				cfg.Loop.MaxIterations = maxIter
			}
			if plannerModel != "" {
				cfg.Models.Planner.Model = plannerModel
			}
			if executorModel != "" {
				cfg.Models.Executor.Model = executorModel
			}
			if reviewerModel != "" {
				cfg.Models.Reviewer.Model = reviewerModel
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			// Plan files pointed at by --plan or @file.md mentions in the
			// prompt skip the planner stage; the mentions leave the prompt.
			plan, prompt, err := planFiles(prompt, planPaths)
			if err != nil {
				return err
			}
			opts := pipeline.Options{
				Repo:         cfg.Repo,
				Prompt:       prompt,
				Name:         name,
				Plan:         plan,
				AllowDirty:   allowDirty,
				KeepWorktree: keepWT,
				Apply:        apply,
				Autopilot:    autopilot,
			}
			if noTUI || yes || !isTTY() {
				p := &pipeline.Pipeline{Cfg: cfg, Opts: opts}
				return runPlain(cmd.Context(), p, yes)
			}
			return launchDashboard(cmd.Context(), cfg, opts, prompt, name, true, planPaths)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "branch for this run (blank/current name uses the current checkout; a new name creates a worktree)")
	cmd.Flags().StringArrayVar(&planPaths, "plan", nil, "plan file(s) (.md or .json); skips the planner stage (mentions like @docs/plan.md in the prompt work too)")
	cmd.Flags().BoolVar(&yes, "yes", false, "auto-approve all gates")
	cmd.Flags().BoolVar(&autopilot, "autopilot", false, "never ask: approve every gate, then commit, push and open a PR")
	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "disable the TUI and use plain prompts")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "run even if the target repo has uncommitted changes")
	cmd.Flags().BoolVar(&keepWT, "keep-worktree", false, "keep the worktree and branch even if the run fails")
	cmd.Flags().BoolVar(&apply, "apply", false, "keep the worktree on success and mark the run applied")
	cmd.Flags().IntVar(&maxIter, "max-iterations", 0, "override the review/fix loop iteration cap")
	cmd.Flags().StringVar(&plannerModel, "planner-model", "", "override the planner model")
	cmd.Flags().StringVar(&executorModel, "executor-model", "", "override the executor model")
	cmd.Flags().StringVar(&reviewerModel, "reviewer-model", "", "override the reviewer model")
	return cmd
}

func newResumeCmd(configPath, artifactsDir *string) *cobra.Command {
	var yes, keepWT bool
	var from string
	cmd := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "resume a failed or interrupted run, rebuilding its worktree if it is gone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base := artifactBase(*artifactsDir)
			run, err := artifact.LoadByID(base, args[0])
			if err != nil {
				return err
			}
			cfg, err := loadRunConfig(run, *configPath)
			if err != nil {
				return err
			}
			switch agent.Kind(from) {
			case agent.Planner, agent.Executor, agent.Reviewer:
			default:
				return fmt.Errorf("--from must be planner, executor or reviewer, got %q", from)
			}
			if _, err := os.Stat(run.Worktree); err != nil && run.InPlace {
				return fmt.Errorf("worktree for %s is gone (%s); cannot resume", run.ID, run.Worktree)
			}
			p := &pipeline.Pipeline{
				Cfg:  cfg,
				Run:  run,
				Opts: pipeline.Options{Repo: run.Repo, Prompt: run.Prompt, KeepWorktree: keepWT, From: agent.Kind(from), Autopilot: run.Autopilot},
			}
			return runPlain(cmd.Context(), p, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "auto-approve all gates")
	cmd.Flags().BoolVar(&keepWT, "keep-worktree", true, "keep the worktree on failure")
	cmd.Flags().StringVar(&from, "from", "planner", "stage to resume at: planner, executor or reviewer")
	return cmd
}

func newDashboardCmd(configPath, repo, artifactsDir *string) *cobra.Command {
	var allowDirty, keepWT, apply, autopilot bool
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "open the interactive dashboard (prompt input, worktrees, ASCII pipeline)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDashboardDefault(cmd, *configPath, *repo, *artifactsDir, allowDirty, keepWT, apply, autopilot)
		},
	}
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "allow starting runs on a dirty repo")
	cmd.Flags().BoolVar(&keepWT, "keep-worktree", false, "keep the worktree and branch even if a run fails")
	cmd.Flags().BoolVar(&apply, "apply", false, "keep worktrees on success and mark runs applied")
	cmd.Flags().BoolVar(&autopilot, "autopilot", false, "start new runs in autopilot mode (switch with ctrl+a)")
	return cmd
}

// runDashboardDefault is shared by the root command and `kor tui`.
func runDashboardDefault(cmd *cobra.Command, configPath, repo, artifactsDir string, allowDirty, keepWT, apply, autopilot bool) error {
	if !isTTY() {
		return cmd.Help()
	}
	cfg, err := resolveConfig(configPath, repo, artifactsDir)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	opts := pipeline.Options{Repo: cfg.Repo, AllowDirty: allowDirty, KeepWorktree: keepWT, Apply: apply, Autopilot: autopilot}
	return launchDashboard(cmd.Context(), cfg, opts, "", "", false, nil)
}

// runPlain drives a single pipeline with the line-oriented gate.
func runPlain(ctx context.Context, p *pipeline.Pipeline, yes bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var gate ui.Gate = ui.NewPlain(yes)
	if yes {
		gate = ui.AutoApprove{Inner: gate}
	}
	p.Gate = gate
	defer gate.Close()
	return p.Execute(ctx)
}

// launchDashboard opens the interactive dashboard. template supplies the per-run
// options (Repo plus flags); every submitted prompt clones it. initialName backs
// the auto-started run when autoStart is set. planPaths are --plan files merged
// into every run started from the dashboard.
func launchDashboard(ctx context.Context, cfg *config.Config, template pipeline.Options, initialPrompt, initialName string, autoStart bool, planPaths []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Runs get their own context so quitting the dashboard cancels them: agents
	// run in their own process groups and would otherwise outlive us.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		closed bool
	)

	app := tui.NewApp(cfg, cfg.ArtifactsDir)
	app.SetInitial(initialPrompt, initialName, autoStart)
	app.SetAutopilot(template.Autopilot)
	app.PlanFromPrompt = func(prompt string) (*contracts.Plan, string, error) {
		return planFiles(prompt, planPaths)
	}
	app.OnStart = func(s *tui.Session, name, prompt string, choices models.Choices, plan *contracts.Plan) {
		mu.Lock()
		if closed {
			mu.Unlock()
			return
		}
		wg.Add(1)
		mu.Unlock()

		opts := template
		opts.Prompt = prompt
		opts.Plan = plan
		opts.Name = name
		opts.Autopilot = s.Autopilot()
		if opts.Repo == "" {
			opts.Repo = cfg.Repo
		}
		p := &pipeline.Pipeline{Cfg: applyChoices(cfg, choices), Opts: opts, Gate: s}
		go func() {
			defer wg.Done()
			err := p.Execute(runCtx)
			s.Finish(err, p.Run)
		}()
	}

	app.OnRetry = func(s *tui.Session, run *artifact.Run, from agent.Kind, choices models.Choices) {
		mu.Lock()
		if closed {
			mu.Unlock()
			return
		}
		wg.Add(1)
		mu.Unlock()

		runCfg, err := loadRunConfig(run, "")
		if err != nil {
			runCfg = cfg
		}
		runCfg.ArtifactsDir = cfg.ArtifactsDir
		opts := template
		opts.Repo, opts.Prompt, opts.From = run.Repo, run.Prompt, from
		opts.Autopilot = s.Autopilot()
		p := &pipeline.Pipeline{Cfg: applyChoices(runCfg, choices), Opts: opts, Run: run, Gate: s}
		go func() {
			defer wg.Done()
			err := p.Execute(runCtx)
			s.Finish(err, p.Run)
		}()
	}

	app.StartWeb = func() (tui.WebServer, error) {
		return startWeb(runCtx, cfg, "", defaultWebListen, template.AllowDirty)
	}

	prog := tea.NewProgram(app, tea.WithAltScreen())
	app.Attach(prog)
	_, runErr := prog.Run()

	if ws := app.Web(); ws != nil {
		if err := ws.Stop(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "kor: web dashboard:", err)
		}
	}

	mu.Lock()
	closed = true
	mu.Unlock()
	cancel()
	if !waitTimeout(&wg, 15*time.Second) {
		fmt.Fprintln(os.Stderr, "kor: timed out waiting for in-flight runs to stop")
	}
	if runErr != nil {
		return runErr
	}
	if err := app.LastError(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// applyChoices returns a per-run copy of cfg with the dashboard's pre-run
// provider/model/effort selection applied. Stage knobs the picker does not own
// (fallback, subagent, sandbox, extra args) are kept from cfg.
func applyChoices(cfg *config.Config, c models.Choices) *config.Config {
	out := *cfg
	out.Models = cfg.Models
	if c.Planner.Model != "" {
		if c.Planner.Agent != "" {
			out.Models.Planner.Agent = c.Planner.Agent
		}
		out.Models.Planner.Model = c.Planner.Model
		out.Models.Planner.Variant = c.Planner.Variant
	}
	if c.Executor.Model != "" {
		if c.Executor.Agent != "" {
			out.Models.Executor.Agent = c.Executor.Agent
		}
		out.Models.Executor.Model = c.Executor.Model
		out.Models.Executor.Variant = c.Executor.Variant
	}
	if c.Reviewer.Model != "" {
		if c.Reviewer.Agent != "" {
			out.Models.Reviewer.Agent = c.Reviewer.Agent
		}
		out.Models.Reviewer.Model = c.Reviewer.Model
		out.Models.Reviewer.Variant = c.Reviewer.Variant
	}
	return &out
}

func resolveConfig(configPath, repo, artifactsDir string) (*config.Config, error) {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	path := configPath
	if path == "" {
		path = config.Discover(repoAbs)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	// The --repo flag wins when set explicitly; otherwise honor the config file,
	// falling back to the flag value (default ".") when the config has none.
	switch {
	case repo != "" && repo != ".":
		cfg.Repo = repoAbs
	case cfg.Repo == "":
		cfg.Repo = repoAbs
	default:
		if abs, err := filepath.Abs(cfg.Repo); err == nil {
			cfg.Repo = abs
		}
	}
	if artifactsDir != "" {
		cfg.ArtifactsDir = artifactsDir
	}
	return cfg, nil
}

func loadRunConfig(run *artifact.Run, configPath string) (*config.Config, error) {
	if data, err := run.Read("config.resolved.yaml"); err == nil {
		cfg := config.Default()
		if err := yaml.Unmarshal(data, cfg); err == nil {
			cfg.Repo = run.Repo
			return cfg, nil
		}
	}
	return resolveConfig(configPath, run.Repo, "")
}

func artifactBase(artifactsDir string) string {
	if artifactsDir != "" {
		return artifactsDir
	}
	return config.DefaultArtifactsDir()
}

// planFiles resolves a run's plan inputs: the --plan files plus every
// @file.md/@file.json mention in the prompt. It returns the merged plan (nil
// when there are none) and the prompt with the mentions stripped.
func planFiles(prompt string, flags []string) (*contracts.Plan, string, error) {
	paths, rest := contracts.ExtractPlanFiles(prompt)
	paths = append(append([]string{}, flags...), paths...)
	if len(paths) == 0 {
		return nil, prompt, nil
	}
	plan, err := contracts.LoadPlans(paths)
	if err != nil {
		return nil, prompt, err
	}
	return plan, rest, nil
}

func promptFrom(args []string, stdin *os.File) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	if isTTY() {
		return "", nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func isTTY() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}
