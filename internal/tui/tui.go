// Package tui implements the Bubble Tea dashboard: a prompt input, a list of
// worktrees/runs on the left and, for the selected run, the plan → execute →
// review pipeline with tabbed activity, plan, review and diff views.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/models"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
	"github.com/guilhermesalviano/korchestrate/internal/worktree"
)

// tab is one of the detail views for the selected run.
type tab int

const (
	tabActivity tab = iota
	tabPlan
	tabReview
	tabDiff
	tabSupport
	tabCount
)

var tabNames = [tabCount]string{"Activity", "Plan", "Review", "Diff", "Support"}

// inputField selects which footer field receives typing while the input is
// focused. Leaving the name blank uses the current checkout directly.
type inputField int

const (
	fieldName inputField = iota
	fieldPrompt
)

const maxLogs = 1000

// App is the dashboard model. It implements tea.Model and owns the sessions
// that pipeline runs use as their ui.Gate.
type App struct {
	// OnStart is called when the user submits a prompt. Implementations should
	// run the pipeline with the chosen provider/model/effort setup and call
	// Session.Finish when done. plan is the pre-parsed plan when the prompt
	// pointed at plan files, nil when the planner stage should run.
	OnStart func(s *Session, name, prompt string, choices models.Choices, plan *contracts.Plan)

	// PlanFromPrompt resolves @file.md plan mentions in a submitted prompt.
	// A failed resolution blocks the run and explains itself via notice.
	PlanFromPrompt func(prompt string) (*contracts.Plan, string, error)

	// OnRetry resumes a finished, failed run in its existing worktree at the
	// given stage and must call Session.Finish when done.
	OnRetry func(s *Session, run *artifact.Run, from agent.Kind, choices models.Choices)

	// StartWeb serves the web dashboard, toggled with "w"; nil disables it.
	StartWeb func() (WebServer, error)

	cfg         *config.Config
	entries     []*Entry
	cursor      int
	listTop     int
	showSidebar bool

	// choices is the sticky pre-run provider/model/effort selection, edited
	// through the setup overlay; catalog backs it with discovered models.
	choices models.Choices
	catalog *models.Catalog
	setup   setupUI

	// autopilot is the sticky mode for new runs, toggled with ctrl+a: off
	// stops at every gate for confirmation, on runs straight through to a PR.
	autopilot bool
	// full expands the selected run to the whole terminal, toggled with "o".
	full bool

	input      []rune
	inputName  []rune
	inputFocus bool
	field      inputField

	// The Support tab's command line and its session-wide history.
	shellInput  []rune
	shellFocus  bool
	shellHist   []string
	shellHistAt int

	tab     tab
	scroll  int  // content offset from the top
	follow  bool // keep the content pinned to the bottom
	lastMax int  // max scroll offset seen by the last render
	viewH   int  // content viewport height seen by the last render
	cache   contentCache

	confirmQuit bool
	confirmDel  *Entry // run awaiting a delete confirmation
	notice      string // one-shot footer message, cleared by the next key
	help        bool   // key-hint cheatsheet overlay, toggled with "h"

	web         WebServer // running web dashboard, nil when off
	webStopping bool
	confirmWeb  bool // a second "w" cancels the active browser run

	// discard tears down a finished run's worktree, branch and artifacts.
	discard func(*artifact.Run) (warn, err error)

	// The right aside shows the selected run's full diff.
	showAside   bool
	asideFits   bool // whether the last render had room for the aside
	asideScroll int
	asideMax    int
	asideH      int
	asideCache  contentCache

	width       int
	height      int
	frame       int
	initial     string
	initialName string
	autoRun     bool

	mu      sync.Mutex
	lastErr error
	prog    *tea.Program
}

type contentCache struct {
	entry *Entry
	tab   tab
	width int
	ver   int
	lines []string
}

// NewApp builds the dashboard, loading historical runs from baseDir.
func NewApp(cfg *config.Config, baseDir string) *App {
	a := &App{cfg: cfg, width: 100, height: 30, follow: true, showAside: true, discard: pipeline.Discard}
	a.choices = models.ChoicesFromConfig(cfg)
	runs, _ := artifact.List(baseDir)
	for _, r := range runs {
		a.entries = append(a.entries, entryFromRun(r))
	}
	return a
}

// Attach connects the app to its running program.
func (a *App) Attach(p *tea.Program) { a.prog = p }

// SetInitial pre-fills the name and prompt; when auto is true the run starts
// immediately after the program starts.
func (a *App) SetInitial(prompt, name string, auto bool) {
	a.initial = prompt
	a.initialName = name
	a.autoRun = auto
}

// SetAutopilot preselects the mode new runs start in.
func (a *App) SetAutopilot(on bool) { a.autopilot = on }

// LastError returns the last pipeline error observed.
func (a *App) LastError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

func (a *App) recordError(err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
}

func (a *App) send(m tea.Msg) {
	if a.prog != nil {
		a.prog.Send(m)
	}
}

// --- Session: the ui.Gate handed to a pipeline run --------------------------

// Session routes a single pipeline run's gate calls into the dashboard.
type Session struct {
	entry     *Entry
	app       *App
	publish   chan publishRequest
	done      chan struct{}
	publisher func() error
}

// Autopilot reports whether the run skips every gate and ends with a PR.
func (s *Session) Autopilot() bool { return s.entry.Autopilot }

func (s *Session) Stage(k agent.Kind, msg string) {
	s.app.send(stageEventMsg{entry: s.entry, kind: k, text: msg})
}
func (s *Session) Line(ev agent.Event) { s.app.send(lineEventMsg{entry: s.entry, ev: ev}) }
func (s *Session) Info(msg string)     { s.app.send(infoEventMsg{entry: s.entry, text: msg}) }
func (s *Session) Close()              {}

// RunUpdated implements pipeline.RunObserver.
func (s *Session) RunUpdated(run artifact.Run) {
	s.app.send(runEventMsg{entry: s.entry, run: run})
}

func (s *Session) PlanGate(ctx context.Context, plan *contracts.Plan, diff string) (ui.Decision, error) {
	return s.gate(ctx, &gateReq{kind: gatePlan, plan: plan, diff: diff})
}

func (s *Session) ReviewGate(ctx context.Context, review *contracts.Review, diff string) (ui.Decision, error) {
	return s.gate(ctx, &gateReq{kind: gateReview, review: review, diff: diff})
}

// CommitGate asks whether to commit and/or push the staged changes.
func (s *Session) CommitGate(ctx context.Context, branch, worktree string) (ui.CommitDecision, error) {
	req := &gateReq{
		kind:        gateCommit,
		branch:      branch,
		worktree:    worktree,
		commitReply: make(chan ui.CommitDecision, 1),
	}
	s.app.send(gateEventMsg{entry: s.entry, req: req})
	return awaitSession(ctx, s, (<-chan ui.CommitDecision)(req.commitReply))
}

func (s *Session) SelectAgent(ctx context.Context, kind agent.Kind, failed string, options []string, preferred string, cause error) (string, error) {
	req := &gateReq{
		kind:       gateAgent,
		agentKind:  kind,
		failed:     failed,
		options:    options,
		preferred:  preferred,
		cause:      cause,
		agentReply: make(chan string, 1),
	}
	for i, o := range options {
		if o == preferred {
			req.cursor = i
		}
	}
	s.app.send(gateEventMsg{entry: s.entry, req: req})
	return awaitSession(ctx, s, (<-chan string)(req.agentReply))
}

// WorktreeGate asks whether to reuse an existing worktree/branch or create a
// new one when the requested branch name is taken.
func (s *Session) WorktreeGate(ctx context.Context, branch string) (ui.WorktreeDecision, error) {
	req := &gateReq{
		kind:          gateWorktree,
		branch:        branch,
		worktreeReply: make(chan ui.WorktreeDecision, 1),
	}
	s.app.send(gateEventMsg{entry: s.entry, req: req})
	return awaitSession(ctx, s, (<-chan ui.WorktreeDecision)(req.worktreeReply))
}

func (s *Session) gate(ctx context.Context, req *gateReq) (ui.Decision, error) {
	req.reply = make(chan ui.Decision, 1)
	s.app.send(gateEventMsg{entry: s.entry, req: req})
	return awaitSession(ctx, s, (<-chan ui.Decision)(req.reply))
}

// Finish reports the pipeline outcome and updates the entry.
func (s *Session) Finish(err error, run *artifact.Run) {
	if s.done != nil {
		close(s.done)
	}
	s.app.recordError(err)
	s.app.send(doneEventMsg{entry: s.entry, err: err, run: run})
}

// --- Entry ------------------------------------------------------------------

// StageInfo is the display state of one pipeline stage.
type StageInfo struct {
	status string
	done   bool
	failed bool
}

var stageOrder = []agent.Kind{agent.Planner, agent.Executor, agent.Reviewer}

// Entry is one run shown in the worktree list.
type Entry struct {
	ID      string
	Prompt  string
	Repo    string
	Name    string
	Models  models.Choices
	Live    bool
	Run     *artifact.Run
	State   artifact.State
	Session *Session
	Stages  map[agent.Kind]*StageInfo
	Iter    int
	Gate    *gateReq
	ErrText string
	Plan    *contracts.Plan
	Review  *contracts.Review
	Diff    string
	Logs    []logLine
	Shell   []shellLine // Support tab scrollback
	Started time.Time
	Ended   time.Time

	// Autopilot runs never stop at a gate; they finish with a pull request.
	Autopilot bool

	ver        int // bumped on every change; keys the render cache
	logsLoaded bool
	diffAt     time.Time
	diffBusy   bool               // a live snapshot is in flight
	deleting   bool               // a delete is in flight
	publishing bool               // commit and push is queued or running
	shellStop  context.CancelFunc // non-nil while a Support command runs
}

// branch is the run's git branch, or the requested name before it exists.
func (e *Entry) branch() string {
	if e.Run != nil && e.Run.Branch != "" {
		return e.Run.Branch
	}
	return e.Name
}

func newStages() map[agent.Kind]*StageInfo {
	return map[agent.Kind]*StageInfo{agent.Planner: {}, agent.Executor: {}, agent.Reviewer: {}}
}

func newEntry(prompt, repo string) *Entry {
	return &Entry{
		ID:         "live-" + shortID(prompt),
		Prompt:     prompt,
		Repo:       repo,
		Live:       true,
		State:      artifact.StatePreflight,
		Stages:     newStages(),
		Started:    time.Now(),
		logsLoaded: true,
	}
}

func entryFromRun(r *artifact.Run) *Entry {
	e := &Entry{
		ID:      r.ID,
		Prompt:  r.Prompt,
		Repo:    r.Repo,
		Name:    r.Branch,
		Run:     r,
		State:   r.State,
		Iter:    r.Iteration,
		ErrText: r.Error,
		Stages:  newStages(),
		Started: r.CreatedAt,
		Ended:   r.UpdatedAt,
	}
	e.Autopilot = r.Autopilot
	e.hydrate()
	return e
}

func (e *Entry) touch() { e.ver++ }

// hydrate derives stage display state and loads plan/review artifacts.
func (e *Entry) hydrate() {
	done := func(k agent.Kind) { e.Stages[k].done = true }
	switch e.State {
	case artifact.StatePlanning:
		e.Stages[agent.Planner].status = "planning"
	case artifact.StateGatePlan:
		e.Stages[agent.Planner].status = "awaiting approval"
		done(agent.Planner)
	case artifact.StateExecuting:
		done(agent.Planner)
		e.Stages[agent.Executor].status = fmt.Sprintf("executing (iter %d)", e.Iter)
	case artifact.StateReviewing, artifact.StateGateReview:
		done(agent.Planner)
		done(agent.Executor)
		e.Stages[agent.Reviewer].status = fmt.Sprintf("reviewing (iter %d)", e.Iter)
	case artifact.StateCommitting, artifact.StatePublishing, artifact.StateDone:
		for _, k := range stageOrder {
			done(k)
		}
	case artifact.StateFailed, artifact.StateAborted:
		reached := e.reachedStage()
		for _, k := range stageOrder {
			si := e.Stages[k]
			if k == reached {
				si.failed, si.done, si.status = true, false, string(e.State)
				break
			}
			si.done = true
		}
	}
	e.loadArtifacts()
	e.touch()
}

// reachedStage reports the furthest stage a finished run got to: from live
// stage statuses when we watched it, otherwise from the artifacts on disk.
func (e *Entry) reachedStage() agent.Kind {
	for i := len(stageOrder) - 1; i >= 0; i-- {
		if e.Stages[stageOrder[i]].status != "" {
			return stageOrder[i]
		}
	}
	if e.Run == nil {
		return agent.Planner
	}
	has := func(pattern string) bool {
		m, _ := filepath.Glob(e.Run.Path(pattern))
		return len(m) > 0
	}
	switch {
	case has("review.json"), has("reviewer.events.*"):
		return agent.Reviewer
	case has("diff.patch"), has("executor.*"):
		return agent.Executor
	}
	return agent.Planner
}

// loadArtifacts reads plan.json and review.json when the run directory is
// known. Both files are small.
func (e *Entry) loadArtifacts() {
	if e.Run == nil || e.Run.Dir == "" {
		return
	}
	if data, err := e.Run.Read("plan.json"); err == nil {
		var p contracts.Plan
		if json.Unmarshal(data, &p) == nil {
			e.Plan = &p
		}
	}
	if data, err := e.Run.Read("review.json"); err == nil {
		var r contracts.Review
		if json.Unmarshal(data, &r) == nil {
			e.Review = &r
		}
	}
	if data, err := e.Run.Read("config.resolved.yaml"); err == nil {
		var doc struct {
			Models struct {
				Planner  config.ModelSpec    `yaml:"planner"`
				Executor config.ExecutorSpec `yaml:"executor"`
				Reviewer config.ModelSpec    `yaml:"reviewer"`
			} `yaml:"models"`
		}
		if yaml.Unmarshal(data, &doc) == nil {
			e.Models = models.Choices{
				Planner:  models.Choice{Agent: doc.Models.Planner.Agent, Model: doc.Models.Planner.Model, Variant: doc.Models.Planner.Variant},
				Executor: models.Choice{Agent: doc.Models.Executor.Agent, Model: doc.Models.Executor.Model, Variant: doc.Models.Executor.Variant},
				Reviewer: models.Choice{Agent: doc.Models.Reviewer.Agent, Model: doc.Models.Reviewer.Model, Variant: doc.Models.Reviewer.Variant},
			}
		}
	}
}

// ensureLogs replays persisted agent events for runs we did not watch live.
func (e *Entry) ensureLogs() {
	if e.logsLoaded || e.Run == nil {
		return
	}
	e.logsLoaded = true
	e.Logs = replayEvents(e.Run.Dir)
	e.touch()
}

// ensureDiff loads diff.patch once for finished runs. Live runs are refreshed
// from the worktree itself by snapshotDiff.
func (e *Entry) ensureDiff() {
	if e.Live || e.Run == nil || e.Run.Dir == "" || !e.diffAt.IsZero() {
		return
	}
	e.diffAt = time.Now()
	if data, err := os.ReadFile(e.Run.Path("diff.patch")); err == nil && string(data) != e.Diff {
		e.Diff = string(data)
		e.touch()
	}
}

func (e *Entry) push(l logLine) {
	if strings.TrimSpace(l.text) == "" {
		return
	}
	if l.at.IsZero() {
		l.at = time.Now()
	}
	e.Logs = append(e.Logs, l)
	if len(e.Logs) > maxLogs {
		e.Logs = e.Logs[len(e.Logs)-maxLogs:]
	}
	e.touch()
}

func (e *Entry) title() string {
	if e.Run != nil && e.Run.ID != "" {
		return e.Run.ID
	}
	return e.ID
}

func (e *Entry) duration() time.Duration {
	if e.Started.IsZero() {
		return 0
	}
	if e.Live {
		return time.Since(e.Started)
	}
	if e.Ended.Before(e.Started) {
		return 0
	}
	return e.Ended.Sub(e.Started)
}

// --- messages ---------------------------------------------------------------

type gateKind int

const (
	gatePlan gateKind = iota
	gateReview
	gateCommit
	gateAgent
	gateWorktree
	gateRetry
)

type gateReq struct {
	kind   gateKind
	plan   *contracts.Plan
	review *contracts.Review
	diff   string
	reply  chan ui.Decision

	// gateCommit fields: publishing staged changes after a passed review.
	branch      string
	worktree    string
	commitReply chan ui.CommitDecision

	// gateAgent fields: choosing a replacement adapter after a stage failed.
	agentKind  agent.Kind
	failed     string
	options    []string
	preferred  string
	cause      error
	cursor     int
	agentReply chan string

	// gateWorktree fields: reusing or replacing an existing worktree/branch.
	worktreeReply chan ui.WorktreeDecision
	step          string
	retryReply    chan bool
}

type (
	stageEventMsg struct {
		entry *Entry
		kind  agent.Kind
		text  string
	}
	lineEventMsg struct {
		entry *Entry
		ev    agent.Event
	}
	infoEventMsg struct {
		entry *Entry
		text  string
	}
	runEventMsg struct {
		entry *Entry
		run   artifact.Run
	}
	gateEventMsg struct {
		entry *Entry
		req   *gateReq
	}
	doneEventMsg struct {
		entry *Entry
		err   error
		run   *artifact.Run
	}
	diffMsg struct {
		entry *Entry
		diff  string
		err   error
	}
	catalogMsg struct {
		catalog *models.Catalog
	}
	deletedMsg struct {
		entry     *Entry
		warn, err error
	}
	tickMsg time.Time
)

const diffRefresh = 1500 * time.Millisecond

// snapshotDiff reads the live worktree diff off the UI goroutine. Only the
// selected run is polled so idle runs cost nothing.
func (a *App) snapshotDiff() tea.Cmd {
	e := a.current()
	if e == nil || !e.Live || e.diffBusy || e.Run == nil || e.Run.Worktree == "" ||
		time.Since(e.diffAt) < diffRefresh {
		return nil
	}
	e.diffBusy, e.diffAt = true, time.Now()
	dir := e.Run.Worktree
	return func() tea.Msg {
		if _, err := os.Stat(dir); err != nil {
			return diffMsg{entry: e, err: err}
		}
		d, err := worktree.Snapshot(dir)
		return diffMsg{entry: e, diff: d, err: err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// loadCatalog discovers provider/model/effort catalogs off the UI thread.
func loadCatalog() tea.Msg {
	return catalogMsg{catalog: models.Discover()}
}

// Init implements tea.Model.
func (a *App) Init() tea.Cmd {
	cmds := []tea.Cmd{tick(), loadCatalog}
	if a.initial != "" && a.autoRun {
		cmds = append(cmds, a.startRun(a.initialName, a.initial))
	} else {
		a.input = []rune(a.initial)
		a.inputName = []rune(a.initialName)
		a.inputFocus = true
		a.field = fieldName
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tickMsg:
		a.frame++
		return a, tea.Batch(tick(), a.snapshotDiff())

	case diffMsg:
		a.mutate(t.entry, func(e *Entry) {
			e.diffBusy = false
			// A snapshot landing after the run finished would show the
			// post-commit (empty) tree; diff.patch is authoritative then.
			if t.err == nil && e.Live && t.diff != e.Diff {
				e.Diff = t.diff
				e.touch()
			}
		})

	case tea.WindowSizeMsg:
		if t.Width < 80 && a.width >= 80 {
			a.showSidebar = false
		}
		a.width, a.height = t.Width, t.Height
		return a, nil

	case catalogMsg:
		a.catalog = t.catalog
		return a, nil

	case stageEventMsg:
		a.mutate(t.entry, func(e *Entry) {
			e.State = stateFromStage(t.kind, e.State)
			applyStage(e, t.kind, t.text)
			e.loadArtifacts()
			e.push(logLine{kind: t.kind, level: levelStage, text: t.text})
		})

	case lineEventMsg:
		if l, ok := summarizeEvent(t.ev); ok {
			a.mutate(t.entry, func(e *Entry) { e.push(l) })
		}

	case infoEventMsg:
		a.mutate(t.entry, func(e *Entry) { e.push(logLine{level: levelInfo, text: sanitize(t.text)}) })

	case runEventMsg:
		a.mutate(t.entry, func(e *Entry) {
			run := t.run
			e.Run = &run
			if run.Iteration > e.Iter {
				e.Iter = run.Iteration
			}
			e.touch()
		})

	case gateEventMsg:
		a.mutate(t.entry, func(e *Entry) {
			e.Gate = t.req
			switch t.req.kind {
			case gatePlan:
				e.Plan = t.req.plan
			case gateReview:
				e.Review = t.req.review
				if t.req.diff != "" {
					e.Diff = t.req.diff
				}
			case gateRetry:
				if k, ok := stageForStep(t.req.step); ok {
					si := e.Stages[k]
					si.failed, si.done = true, false
					if strings.TrimSpace(si.status) == "" {
						si.status = t.req.step + " failed"
					}
				}
			}
			e.touch()
		})
		if a.current() == t.entry {
			a.inputFocus = false
			switch t.req.kind {
			case gatePlan:
				a.setTab(tabPlan)
			case gateReview:
				a.setTab(tabReview)
			case gateCommit:
				a.setTab(tabDiff)
			}
		}

	case doneEventMsg:
		a.mutate(t.entry, func(e *Entry) {
			e.Gate = nil
			e.Live = false
			e.Ended = time.Now()
			if t.run != nil {
				e.Run = t.run
				e.State = t.run.State
				e.Iter = t.run.Iteration
			}
			if t.err != nil {
				e.ErrText = t.err.Error()
				if e.State != artifact.StateAborted {
					e.State = artifact.StateFailed
				}
			}
			e.hydrate()
			e.diffAt = time.Time{}
			e.push(logLine{level: levelStage, text: "run finished: " + string(e.State)})
			if e.Autopilot {
				a.notice = pipeline.EndMessage(e.Run)
			}
		})

	case deletedMsg:
		if t.err != nil {
			a.mutate(t.entry, func(e *Entry) {
				e.deleting = false
				e.ErrText = "delete failed: " + t.err.Error()
				e.touch()
			})
			return a, nil
		}
		a.removeEntry(t.entry)
		if t.warn != nil {
			a.notice = "deleted, but " + t.warn.Error()
		}

	case shellOutMsg:
		a.mutate(t.entry, func(e *Entry) { e.pushShell(t.line) })
		return a, waitShell(t.ch)
	case shellDoneMsg:
		a.finishShell(t)

	case tea.KeyMsg:
		return a.handleKey(t)
	case webStoppedMsg:
		a.webStopped(t)
	case publishResultMsg:
		a.mutate(t.entry, func(e *Entry) {
			e.publishing = false
			if t.run != nil {
				e.Run = t.run
			}
			message := "Committed and pushed " + e.branch()
			if t.err != nil {
				message = t.err.Error()
			}
			e.push(logLine{level: levelInfo, text: message})
			a.notice = message
			e.touch()
		})
	}
	return a, nil
}

func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a.notice = ""
	if a.help {
		a.help = false
		return a, nil
	}
	if a.confirmQuit {
		switch msg.String() {
		case "q", "y", "ctrl+c":
			a.stopShells()
			return a, tea.Quit
		default:
			a.confirmQuit = false
		}
		return a, nil
	}

	if e := a.confirmDel; e != nil {
		a.confirmDel = nil
		if msg.String() == "y" {
			return a, a.deleteRun(e)
		}
		return a, nil
	}

	if a.setup.active {
		return a.handleSetupKey(msg)
	}

	if a.inputFocus {
		if msg.String() == "ctrl+p" {
			return a, a.requestPublish()
		}
		switch msg.Type {
		case tea.KeyCtrlA:
			a.autopilot = !a.autopilot
		case tea.KeyEsc:
			a.inputFocus = false
		case tea.KeyTab:
			if a.field == fieldName {
				a.field = fieldPrompt
			} else {
				a.field = fieldName
			}
		case tea.KeyCtrlC:
			return a.quit()
		case tea.KeyEnter:
			if a.field == fieldName {
				a.field = fieldPrompt
				return a, nil
			}
			name := strings.TrimSpace(string(a.inputName))
			prompt := strings.TrimSpace(string(a.input))
			if prompt == "" {
				return a, nil
			}
			// A blank name uses the current checkout.
			cmd := a.startRun(name, prompt)
			if cmd != nil {
				a.input, a.inputName = nil, nil
				a.inputFocus = false
			}
			return a, cmd
		case tea.KeyBackspace:
			a.deleteChar()
		case tea.KeyCtrlU:
			a.clearField()
		case tea.KeyCtrlW:
			a.deleteWord()
		case tea.KeySpace:
			a.typeRunes(" ")
		case tea.KeyRunes:
			a.typeRunes(string(msg.Runes))
		}
		return a, nil
	}

	if e := a.current(); e != nil && a.tab == tabSupport {
		if a.shellFocus {
			return a.handleShellKey(e, msg)
		}
		if msg.String() == "!" || (msg.String() == "enter" && e.Gate == nil) {
			a.shellFocus = true
			return a, nil
		}
	}

	if msg.String() != "w" {
		a.confirmWeb = false
	}
	switch msg.String() {
	case "h":
		a.help = true
		return a, nil
	case "w":
		return a, a.toggleWeb()
	case "ctrl+a":
		a.autopilot = !a.autopilot
		a.notice = "new runs start in " + modeName(a.autopilot) + " mode"
		return a, nil
	case "o":
		a.toggleFull()
		return a, nil
	case "b":
		// The run list remains accessible while a gate is waiting.
		a.full = false
		a.showSidebar = !a.showSidebar
		return a, nil
	}
	if a.showSidebar && a.width < 80 {
		switch msg.String() {
		case "p", "ctrl+p":
			return a, a.requestPublish()
		case "up", "k":
			a.move(-1)
		case "down", "j":
			a.move(1)
		case "enter", "esc":
			a.showSidebar = false
		case "q", "ctrl+c":
			return a.quit()
		}
		return a, nil
	}
	if msg.String() == "p" || msg.String() == "ctrl+p" {
		if e := a.current(); e != nil && e.Gate != nil && e.Gate.kind == gateCommit {
			a.answerCommit(ui.CommitAndPush)
			return a, nil
		}
		return a, a.requestPublish()
	}
	if e := a.current(); e != nil && e.Gate != nil {
		// Waiting for a decision must not prevent inspecting or scrolling
		// the selected run. Agent selection keeps its own tab/number keys.
		switch msg.String() {
		case "d":
			a.setTab(tabDiff)
			return a, nil
		case "pgdown", "ctrl+d", "J", "shift+down":
			a.scrollBy(max(1, a.viewH/2))
			return a, nil
		case "pgup", "ctrl+u", "K", "shift+up":
			a.scrollBy(-max(1, a.viewH/2))
			return a, nil
		case "tab", "right", "l":
			if e.Gate.kind != gateAgent {
				a.setTab((a.tab + 1) % tabCount)
				return a, nil
			}
		case "shift+tab", "left":
			if e.Gate.kind != gateAgent {
				a.setTab((a.tab + tabCount - 1) % tabCount)
				return a, nil
			}
		case "1", "2", "3", "4", "5":
			if e.Gate.kind != gateAgent {
				a.setTab(tab(msg.String()[0] - '1'))
				return a, nil
			}
		}
		switch e.Gate.kind {
		case gateRetry:
			switch msg.String() {
			case "t", "enter":
				a.answerRetry(true)
			case "s", "esc":
				a.answerRetry(false)
			case "ctrl+c", "q":
				return a.quit()
			}
			return a, nil
		case gateAgent:
			return a.handleAgentKey(e.Gate, msg)
		case gateCommit:
			return a.handleCommitKey(e.Gate, msg)
		case gateWorktree:
			return a.handleWorktreeKey(e.Gate, msg)
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return a.quit()
	case "n", "/", "i":
		a.inputFocus = true
		a.field = fieldName
	case "m":
		a.setup = setupUI{active: true}
	case "up", "k":
		a.move(-1)
	case "down", "j":
		a.move(1)
	case "tab", "right", "l":
		a.setTab((a.tab + 1) % tabCount)
	case "shift+tab", "left":
		a.setTab((a.tab + tabCount - 1) % tabCount)
	case "1", "2", "3", "4", "5":
		a.setTab(tab(msg.String()[0] - '1'))
	case "pgdown", "ctrl+d", "J", "shift+down":
		a.scrollBy(max(1, a.viewH/2))
	case "pgup", "ctrl+u", "K", "shift+up":
		a.scrollBy(-max(1, a.viewH/2))
	case "g", "home":
		a.scroll, a.follow = 0, false
	case "G", "end":
		a.scroll, a.follow = a.lastMax, true
	case "d":
		if a.asideFits {
			a.showAside = !a.showAside
		} else {
			a.setTab(tabDiff)
		}
	case "]":
		a.asideScroll = min(a.asideScroll+max(1, a.asideH/2), a.asideMax)
	case "[":
		a.asideScroll = max(a.asideScroll-max(1, a.asideH/2), 0)
	case "t":
		return a, a.retryRun()
	case "x", "delete":
		a.askDelete()
	case "a":
		a.answer(ui.Approve)
	case "f":
		a.answer(ui.Fix)
	case "r":
		a.answer(ui.Reject)
	}
	return a, nil
}

// typeRunes appends text to the focused input field.
func (a *App) typeRunes(s string) {
	if a.field == fieldName {
		a.inputName = append(a.inputName, []rune(s)...)
		return
	}
	a.input = append(a.input, []rune(s)...)
}

// deleteChar removes the last rune of the focused field.
func (a *App) deleteChar() {
	if a.field == fieldName {
		if len(a.inputName) > 0 {
			a.inputName = a.inputName[:len(a.inputName)-1]
		}
		return
	}
	if len(a.input) > 0 {
		a.input = a.input[:len(a.input)-1]
	}
}

// clearField empties the focused field.
func (a *App) clearField() {
	if a.field == fieldName {
		a.inputName = nil
		return
	}
	a.input = nil
}

// deleteWord removes the last word of the focused field.
func (a *App) deleteWord() {
	if a.field == fieldName {
		a.inputName = []rune(dropLastWord(string(a.inputName)))
		return
	}
	a.input = []rune(dropLastWord(string(a.input)))
}

func dropLastWord(s string) string {
	s = strings.TrimRight(s, " ")
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		return s[:i+1]
	}
	return ""
}

// quit exits immediately when nothing is running; otherwise it asks first,
// because quitting cancels every in-flight run.
func (a *App) quit() (tea.Model, tea.Cmd) {
	if a.liveCount() == 0 {
		a.stopShells()
		return a, tea.Quit
	}
	a.confirmQuit = true
	return a, nil
}

// askDelete asks before deleting the selected run. A live run's worktree is
// still in use, so it has to finish first.
func (a *App) askDelete() {
	e := a.current()
	switch {
	case e == nil || e.deleting:
	case e.publishing:
		a.notice = "wait for publishing to finish before deleting this run"
	case e.Live:
		a.notice = "can't delete a running worktree; wait for it to finish"
	default:
		a.confirmDel = e
	}
}

// deleteRun removes a finished run's worktree, branch and artifacts off the
// UI goroutine.
func (a *App) deleteRun(e *Entry) tea.Cmd {
	e.deleting = true
	e.touch()
	run, discard := e.Run, a.discard
	return func() tea.Msg {
		if run == nil {
			return deletedMsg{entry: e} // never got past preflight: nothing on disk
		}
		warn, err := discard(run)
		return deletedMsg{entry: e, warn: warn, err: err}
	}
}

// removeEntry drops e from the list, keeping the cursor on a neighbour.
func (a *App) removeEntry(e *Entry) {
	for i, x := range a.entries {
		if x != e {
			continue
		}
		selected := i == a.cursor
		a.entries = append(a.entries[:i], a.entries[i+1:]...)
		if i < a.cursor || a.cursor >= len(a.entries) {
			a.cursor = max(0, a.cursor-1)
		}
		if selected {
			a.asideScroll = 0
			a.resetScroll()
		}
		return
	}
}

// liveCount includes the web dashboard's run, which quitting also cancels.
func (a *App) liveCount() int {
	n := 0
	if a.web != nil && a.web.Active() {
		n++
	}
	for _, e := range a.entries {
		if e.Live {
			n++
		}
	}
	return n
}

// startRun appends a live entry and asks the host to run the pipeline. The
// entry snapshots the current model choices so the run keeps displaying what
// it was started with. A prompt pointing at plan files (@plan.md) skips the
// planner; when the files cannot be resolved nothing starts and the typed
// input is kept for editing.
func (a *App) startRun(name, prompt string) tea.Cmd {
	plan := (*contracts.Plan)(nil)
	if a.PlanFromPrompt != nil {
		p, clean, err := a.PlanFromPrompt(prompt)
		if err != nil {
			a.notice = err.Error()
			return nil
		}
		plan, prompt = p, clean
	}
	e := newEntry(prompt, a.cfg.Repo)
	e.Name = strings.TrimSpace(name)
	e.Models = a.choices
	e.Autopilot = a.autopilot
	e.Plan = plan
	s := &Session{entry: e, app: a, publish: make(chan publishRequest, 1), done: make(chan struct{})}
	e.Session = s
	a.entries = append([]*Entry{e}, a.entries...)
	a.cursor = 0
	a.setTab(tabActivity)
	return func() tea.Msg {
		if a.OnStart != nil {
			a.OnStart(s, e.Name, prompt, a.choices, plan)
		}
		return nil
	}
}

// retryRun resumes the selected failed run at the stage the active tab shows:
// plan → planner, review → reviewer, diff → executor, activity → the stage
// that failed. Earlier stages' results are reused.
func (a *App) retryRun() tea.Cmd {
	e := a.current()
	switch {
	case e == nil || e.Live || e.deleting || e.publishing:
		return nil
	case e.State != artifact.StateFailed && e.State != artifact.StateAborted:
		a.notice = "only failed runs can be retried"
		return nil
	case e.Run == nil:
		a.notice = "this run never started; submit it again"
		return nil
	case a.OnRetry == nil:
		return nil
	}
	if _, err := os.Stat(e.Run.Worktree); err != nil && e.Run.InPlace {
		a.notice = "can't retry: the run's checkout is gone"
		return nil
	}
	from := retryStage(a.tab, e)
	if from != agent.Planner {
		if _, err := e.Run.Read("plan.json"); err != nil {
			from = agent.Planner
		}
	}
	for _, k := range stageOrder {
		si := e.Stages[k]
		switch {
		case stageIndex(k) < stageIndex(from):
			*si = StageInfo{done: true, status: si.status}
		case k == from:
			*si = StageInfo{status: "retrying…"}
		default:
			*si = StageInfo{}
		}
	}
	s := &Session{entry: e, app: a, publish: make(chan publishRequest, 1), done: make(chan struct{})}
	e.Session = s
	e.Live, e.Gate, e.ErrText = true, nil, ""
	e.Ended = time.Time{}
	e.push(logLine{level: levelStage, text: "retrying from " + string(from)})
	e.touch()
	// The current picker selection applies, so a failing model can be
	// swapped (m) before retrying.
	e.Models = a.choices
	run, choices := e.Run, a.choices
	return func() tea.Msg {
		a.OnRetry(s, run, from, choices)
		return nil
	}
}

// retryStage picks the stage a retry resumes at for the active tab.
func retryStage(t tab, e *Entry) agent.Kind {
	switch t {
	case tabPlan:
		return agent.Planner
	case tabReview:
		return agent.Reviewer
	case tabDiff:
		return agent.Executor
	}
	for _, k := range stageOrder {
		if e.Stages[k].failed {
			return k
		}
	}
	return e.reachedStage()
}

// toggleFull expands the selected run to the whole terminal or restores the
// normal layout.
func (a *App) toggleFull() {
	if !a.full && a.current() == nil {
		a.notice = "select a run to open it full screen"
		return
	}
	a.full = !a.full
	if a.full && a.width < 80 {
		a.showSidebar = false
	}
}

func modeName(autopilot bool) string {
	if autopilot {
		return "autopilot"
	}
	return "default"
}

func (a *App) move(delta int) {
	if len(a.entries) == 0 {
		return
	}
	next := min(max(a.cursor+delta, 0), len(a.entries)-1)
	if next != a.cursor {
		a.cursor = next
		a.asideScroll = 0
		a.resetScroll()
	}
}

func (a *App) setTab(t tab) {
	a.tab = t
	a.shellFocus = false
	a.resetScroll()
}

func (a *App) resetScroll() {
	a.scroll = 0
	a.follow = a.tab == tabActivity || a.tab == tabSupport
}

func (a *App) scrollBy(d int) {
	cur := a.scroll
	if a.follow {
		cur = a.lastMax
	}
	cur = min(max(cur+d, 0), a.lastMax)
	a.scroll = cur
	a.follow = cur >= a.lastMax && a.tab == tabActivity
}

// handleAgentKey drives the adapter-selection prompt shown after a stage fails.
func (a *App) handleAgentKey(g *gateReq, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	move := func(d int) {
		if n := len(g.options); n > 0 {
			g.cursor = (g.cursor + d + n) % n
			if e := a.current(); e != nil {
				e.touch()
			}
		}
	}
	switch msg.String() {
	case "t":
		a.answerAgent("retry")
	case "ctrl+c":
		return a.quit()
	case "up", "k", "shift+tab":
		move(-1)
	case "down", "j", "tab":
		move(1)
	case "enter":
		if g.cursor >= 0 && g.cursor < len(g.options) {
			a.answerAgent(g.options[g.cursor])
		} else {
			a.answerAgent("")
		}
	case "esc":
		a.answerAgent("")
	default:
		if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			if i := int(s[0] - '1'); i < len(g.options) {
				a.answerAgent(g.options[i])
			}
		}
	}
	return a, nil
}

// handleCommitKey drives the publish gate shown after a passed review.
func (a *App) handleCommitKey(g *gateReq, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return a.quit()
	case "c", "enter":
		a.answerCommit(ui.CommitOnly)
	case "p":
		a.answerCommit(ui.CommitAndPush)
	case "s", "esc":
		a.answerCommit(ui.CommitStop)
	}
	return a, nil
}

// handleWorktreeKey drives the keep-or-create prompt shown when the run's
// default branch already exists.
func (a *App) handleWorktreeKey(g *gateReq, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return a.quit()
	case "k", "enter":
		a.answerWorktree(ui.WorktreeReuse)
	case "c", "n", "esc":
		a.answerWorktree(ui.WorktreeCreate)
	}
	return a, nil
}

// answerWorktree resolves a pending keep-or-create worktree prompt.
func (a *App) answerWorktree(d ui.WorktreeDecision) {
	e := a.current()
	if e == nil || e.Gate == nil || e.Gate.kind != gateWorktree || e.Gate.worktreeReply == nil {
		return
	}
	e.Gate.worktreeReply <- d
	e.Gate = nil
	e.touch()
}

// answerCommit resolves a pending publish gate.
func (a *App) answerCommit(d ui.CommitDecision) {
	e := a.current()
	if e == nil || e.Gate == nil || e.Gate.kind != gateCommit || e.Gate.commitReply == nil {
		return
	}
	e.Gate.commitReply <- d
	e.Gate = nil
	e.touch()
}

// answerAgent resolves a pending adapter-selection prompt. An empty name aborts.
func (a *App) answerAgent(name string) {
	e := a.current()
	if e == nil || e.Gate == nil || e.Gate.kind != gateAgent || e.Gate.agentReply == nil {
		return
	}
	e.Gate.agentReply <- name
	e.Gate = nil
	e.touch()
}

// answer resolves the selected run's pending gate. Keys that do not apply to
// the current gate are ignored.
func (a *App) answer(d ui.Decision) {
	e := a.current()
	if e == nil || e.Gate == nil || e.Gate.reply == nil {
		return
	}
	if !gateAllows(e.Gate, d) {
		return
	}
	e.Gate.reply <- d
	e.Gate = nil
	e.touch()
	if d != ui.Reject {
		a.setTab(tabActivity)
	}
}

// gateAllows reports whether decision d is offered at gate g. A plan can only
// be approved or rejected; a failed review can only be fixed or rejected.
func gateAllows(g *gateReq, d ui.Decision) bool {
	switch g.kind {
	case gateAgent, gateCommit, gateWorktree:
		return false
	case gatePlan:
		return d != ui.Fix
	case gateReview:
		if g.review != nil && !g.review.Pass() {
			return d != ui.Approve
		}
	}
	return true
}

func (a *App) current() *Entry {
	if a.cursor < 0 || a.cursor >= len(a.entries) {
		return nil
	}
	return a.entries[a.cursor]
}

func (a *App) mutate(e *Entry, fn func(*Entry)) {
	for _, x := range a.entries {
		if x == e {
			fn(e)
			return
		}
	}
}

func stateFromStage(k agent.Kind, cur artifact.State) artifact.State {
	switch k {
	case agent.Planner:
		return artifact.StatePlanning
	case agent.Executor:
		return artifact.StateExecuting
	case agent.Reviewer:
		return artifact.StateReviewing
	}
	return cur
}

// stageForStep maps a retry gate's step label to the pipeline stage it belongs
// to, so the flow card can show the failure and refresh on retry.
func stageForStep(step string) (agent.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(step)) {
	case "planner":
		return agent.Planner, true
	case "executor":
		return agent.Executor, true
	case "reviewer", "review fixes":
		return agent.Reviewer, true
	}
	return "", false
}

func applyStage(e *Entry, k agent.Kind, text string) {
	si := e.Stages[k]
	si.status = text
	low := strings.ToLower(text)
	switch k {
	case agent.Planner:
		if strings.Contains(low, "awaiting plan") {
			e.Stages[agent.Planner].done = true
		}
	case agent.Executor:
		// A new executor pass (fix loop) supersedes the previous review.
		e.Stages[agent.Planner].done = true
		si.done, si.failed = false, false
		*e.Stages[agent.Reviewer] = StageInfo{}
		if it, ok := parseIteration(text); ok {
			e.Iter = it
		}
	case agent.Reviewer:
		e.Stages[agent.Executor].done = true
		switch {
		case strings.Contains(low, "passed"):
			e.Stages[agent.Reviewer].done = true
			e.Stages[agent.Reviewer].failed = false
		case strings.Contains(low, "failed"):
			e.Stages[agent.Reviewer].failed = true
			e.Stages[agent.Reviewer].done = false
		default:
			e.Stages[agent.Reviewer].done = false
			e.Stages[agent.Reviewer].failed = false
		}
	}
}

func parseIteration(text string) (int, bool) {
	i := strings.Index(strings.ToLower(text), "iteration ")
	if i < 0 {
		return 0, false
	}
	rest := text[i+len("iteration "):]
	n := 0
	seen := false
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		seen = true
	}
	return n, seen
}

func shortID(prompt string) string {
	return artifact.Slug(prompt, 24) + "-" + time.Now().Format("150405")
}
