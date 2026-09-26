package web

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
)

type choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type gate struct {
	ID      int      `json:"id"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Choices []choice `json:"choices"`
	reply   chan string
}

type snapshot struct {
	ID        string          `json:"id"`
	Prompt    string          `json:"prompt"`
	Active    bool            `json:"active"`
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Run       *artifact.Run   `json:"run,omitempty"`
	Gate      *gate           `json:"gate,omitempty"`
	Logs      []string        `json:"logs,omitempty"`
	Plan      string          `json:"plan,omitempty"`
	Review    string          `json:"review,omitempty"`
	Diff      string          `json:"diff,omitempty"`
	Prompts   *config.Prompts `json:"prompts,omitempty"`
}

// All browser state is copied under mu; the pipeline owns its mutable Run.
type session struct {
	mu sync.Mutex
	snapshot
	nextGate int
	cancel   context.CancelFunc
}

var _ ui.Gate = (*session)(nil)

func (s *session) view(detail bool) snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.snapshot
	if v.Run != nil {
		r := *v.Run
		v.Run = &r
	}
	if detail {
		v.Logs = append([]string(nil), v.Logs...)
	} else {
		v.Logs = nil
		v.Prompts = nil
	}
	return v
}

func (s *session) Info(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound memory and the size of each polling response, even for noisy agents.
	if len(msg) > 4096 {
		msg = msg[:4096] + " …"
	}
	s.Logs = append(s.Logs, msg)
	for len(s.Logs) > 300 {
		s.Logs = s.Logs[1:]
	}
}

func (s *session) Stage(k agent.Kind, msg string) {
	s.mu.Lock()
	s.Status = string(k) + ": " + msg
	s.mu.Unlock()
	s.Info(s.view(false).Status)
}
func (s *session) Line(ev agent.Event) { s.Info("[" + string(ev.Kind) + "] " + ev.Line) }
func (s *session) Close()              {}
func (s *session) RunUpdated(run artifact.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Run = &run
}

func (s *session) finish(err error, run *artifact.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Active, s.Gate = false, nil
	s.Status = "done"
	if run != nil {
		r := *run
		s.Run = &r
	}
	if err != nil {
		s.Error, s.Status = err.Error(), "failed"
	}
}

func (s *session) ask(ctx context.Context, kind, title, body string, choices []choice) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.nextGate++
	g := &gate{ID: s.nextGate, Kind: kind, Title: title, Body: body, Choices: choices, reply: make(chan string, 1)}
	s.Gate = g
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.Gate == g {
			s.Gate = nil
		}
		s.mu.Unlock()
	}()
	select {
	case answer := <-g.reply:
		return answer, ctx.Err()
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *session) answer(id int, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Active || s.Gate == nil || s.Gate.ID != id {
		return fmt.Errorf("this decision is no longer pending; refresh the run")
	}
	for _, c := range s.Gate.Choices {
		if c.Value == value {
			s.Gate.reply <- value
			s.Gate = nil // A second tap or stale phone cannot answer twice.
			return nil
		}
	}
	return fmt.Errorf("invalid choice for this decision")
}

func (s *session) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Active {
		s.Status, s.Gate = "stopping", nil
		s.cancel()
	}
}

func decision(value string) ui.Decision {
	switch value {
	case "approve":
		return ui.Approve
	case "fix":
		return ui.Fix
	default:
		return ui.Reject
	}
}

func (s *session) PlanGate(ctx context.Context, plan *contracts.Plan, _ string) (ui.Decision, error) {
	v, err := s.ask(ctx, "plan", "Approve this plan?", ui.RenderPlan(plan), []choice{{"approve", "Approve plan"}, {"reject", "Reject"}})
	return decision(v), err
}
func (s *session) ReviewGate(ctx context.Context, review *contracts.Review, _ string) (ui.Decision, error) {
	choices := []choice{{"fix", "Request fixes"}, {"reject", "Reject"}}
	if review.Pass() {
		choices = append([]choice{{"approve", "Accept review"}}, choices...)
	}
	v, err := s.ask(ctx, "review", "Review the changes", ui.RenderReview(review), choices)
	return decision(v), err
}
func (s *session) CommitGate(ctx context.Context, branch, _ string) (ui.CommitDecision, error) {
	v, err := s.ask(ctx, "commit", "Changes are ready", "Publish changes on "+branch+", or leave them staged.", []choice{{"stop", "Leave staged"}, {"commit", "Commit"}, {"commit+push", "Commit + push"}})
	switch v {
	case "commit":
		return ui.CommitOnly, err
	case "commit+push":
		return ui.CommitAndPush, err
	default:
		return ui.CommitStop, err
	}
}
func (s *session) WorktreeGate(ctx context.Context, branch string) (ui.WorktreeDecision, error) {
	v, err := s.ask(ctx, "worktree", "Branch already exists", branch, []choice{{"create", "Create a new branch"}, {"reuse", "Reuse existing branch"}})
	if v == "reuse" {
		return ui.WorktreeReuse, err
	}
	return ui.WorktreeCreate, err
}
func (s *session) SelectAgent(ctx context.Context, kind agent.Kind, failed string, options []string, preferred string, cause error) (string, error) {
	choices := []choice{{"retry", "Retry " + failed}}
	for _, name := range options {
		label := "Use " + name
		if name == preferred {
			label += " (configured fallback)"
		}
		choices = append(choices, choice{name, label})
	}
	choices = append(choices, choice{"stop", "Stop"})
	v, err := s.ask(ctx, "agent", string(kind)+" agent failed", cause.Error(), choices)
	if v == "stop" {
		v = ""
	}
	return v, err
}
func (s *session) RetryGate(ctx context.Context, step string, cause error) (bool, error) {
	v, err := s.ask(ctx, "retry", "Retry "+strings.TrimSpace(step)+"?", cause.Error(), []choice{{"retry", "Retry step"}, {"stop", "Stop"}})
	return v == "retry", err
}
