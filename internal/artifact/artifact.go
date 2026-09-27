// Package artifact persists run state and every stage artifact to disk so runs
// are inspectable, resumable and auditable.
package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// State is the pipeline state persisted in run.json.
type State string

const (
	StatePreflight  State = "preflight"
	StateWorktree   State = "worktree"
	StatePlanning   State = "planning"
	StateGatePlan   State = "gate_plan"
	StateExecuting  State = "executing"
	StateReviewing  State = "reviewing"
	StateGateReview State = "gate_review"
	StatePublishing State = "publishing"
	StateCommitting State = "committing"
	StateDone       State = "done"
	StateFailed     State = "failed"
	StateAborted    State = "aborted"
)

// Usage aggregates token/cost accounting across stages.
type Usage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Add accumulates another usage value.
func (u *Usage) Add(in, out int, cost float64) {
	u.InputTokens += in
	u.OutputTokens += out
	u.CostUSD += cost
}

// RunError is one failure recorded during a run, kept as an append-only
// history: fallbacks, retries and failed pushes are worth auditing even when
// the run eventually succeeded. Error always holds the most recent fatal one.
type RunError struct {
	At      time.Time `json:"at"`
	Stage   string    `json:"stage,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Message string    `json:"message"`
}

// maxRunErrors bounds the persisted history, keeping the newest entries.
const maxRunErrors = 100

// Run is the persisted record of one orchestrator run.
type Run struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	// Base is the commit the run's branch started from, used to rebuild a
	// deleted worktree on retry.
	Base string `json:"base,omitempty"`
	// InPlace runs use the user's checkout; cleanup must never remove it.
	InPlace   bool       `json:"in_place,omitempty"`
	Dir       string     `json:"dir"`
	State     State      `json:"state"`
	Iteration int        `json:"iteration"`
	Commit    string     `json:"commit,omitempty"`
	Pushed    bool       `json:"pushed,omitempty"`
	Error     string     `json:"error,omitempty"`
	Errors    []RunError `json:"errors,omitempty"`
	Usage     Usage      `json:"usage"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`

	// Autopilot runs skip every gate, then commit, push and open a PR.
	Autopilot bool   `json:"autopilot,omitempty"`
	PR        string `json:"pr,omitempty"` // pull request URL opened by autopilot
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug converts a prompt into a short filesystem-safe identifier.
func Slug(prompt string, max int) string {
	s := slugRe.ReplaceAllString(strings.ToLower(prompt), "-")
	s = strings.Trim(s, "-")
	if len(s) > max {
		s = strings.Trim(s[:max], "-")
	}
	if s == "" {
		s = "run"
	}
	return s
}

// New creates the run directory and persists an initial run.json.
func New(baseDir, repo, prompt string) (*Run, error) {
	id := time.Now().Format("20060102-150405") + "-" + Slug(prompt, 40)
	dir := filepath.Join(baseDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	now := time.Now()
	r := &Run{
		ID:        id,
		Prompt:    prompt,
		Repo:      repo,
		Dir:       dir,
		State:     StatePreflight,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.Save(); err != nil {
		return nil, err
	}
	if err := r.Write("prompt.txt", []byte(prompt+"\n")); err != nil {
		return nil, err
	}
	return r, nil
}

// Path joins path parts onto the run directory.
func (r *Run) Path(parts ...string) string {
	return filepath.Join(append([]string{r.Dir}, parts...)...)
}

// Write writes a named artifact under the run directory.
func (r *Run) Write(name string, data []byte) error {
	p := r.Path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Read reads a named artifact.
func (r *Run) Read(name string) ([]byte, error) { return os.ReadFile(r.Path(name)) }

// SetState updates and persists the run state, recording the transition in
// the run's audit timeline.
func (r *Run) SetState(s State) error {
	r.State = s
	if err := r.Save(); err != nil {
		return err
	}
	_ = r.Log(LogEntry{Level: LevelInfo, Stage: "run", Event: "state", Message: string(s)})
	return nil
}

// AddError appends a failure to the run's error history and timeline, and
// persists it. Use it for failures the run survives (an agent falling back,
// a failed push); Fail records the fatal one. Consecutive duplicates (the
// same failure logged again by the final handler) are collapsed.
func (r *Run) AddError(stage, agentName string, err error) error {
	if r == nil || err == nil {
		return nil
	}
	msg := err.Error()
	e := RunError{At: time.Now(), Stage: stage, Agent: agentName, Message: msg}
	if n := len(r.Errors); n > 0 && r.Errors[n-1].Message == msg {
		return nil
	}
	r.Errors = append(r.Errors, e)
	if len(r.Errors) > maxRunErrors {
		r.Errors = r.Errors[len(r.Errors)-maxRunErrors:]
	}
	_ = r.Log(LogEntry{Level: LevelError, Stage: stage, Agent: agentName, Event: "error", Message: msg})
	return r.Save()
}

// Fail marks the run failed with a message, adding it to the error history.
func (r *Run) Fail(err error) error {
	r.State = StateFailed
	if err != nil {
		r.Error = err.Error()
		if rerr := r.AddError("", "", err); rerr != nil {
			return rerr
		}
	}
	return r.Save()
}

// Save atomically writes run.json.
func (r *Run) Save() error {
	r.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.Path("run.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.Path("run.json"))
}

// Load reads a run from its directory.
func Load(dir string) (*Run, error) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.Dir == "" {
		r.Dir = dir
	}
	return &r, nil
}

// LoadByID resolves a run by exact ID or unique prefix under baseDir.
func LoadByID(baseDir, id string) (*Run, error) {
	if dir := filepath.Join(baseDir, id); fileExists(filepath.Join(dir, "run.json")) {
		return Load(dir)
	}
	runs, err := List(baseDir)
	if err != nil {
		return nil, err
	}
	var match *Run
	for _, r := range runs {
		if strings.HasPrefix(r.ID, id) {
			if match != nil {
				return nil, fmt.Errorf("ambiguous run id %q", id)
			}
			match = r
		}
	}
	if match == nil {
		return nil, fmt.Errorf("run %q not found", id)
	}
	return match, nil
}

// List returns all runs under baseDir, newest first.
func List(baseDir string) ([]*Run, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var runs []*Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if r, err := Load(filepath.Join(baseDir, e.Name())); err == nil {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
