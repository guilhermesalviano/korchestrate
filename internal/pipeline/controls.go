package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/worktree"
)

// Interactive gates may keep a failed step open until the user retries it.
type retryGate interface {
	RetryGate(context.Context, string, error) (bool, error)
}

func retryStep[T any](ctx context.Context, p *Pipeline, step string, fn func() (T, error)) (T, error) {
	for {
		value, err := fn()
		if err == nil || ctx.Err() != nil {
			return value, err
		}
		gate, ok := p.Gate.(retryGate)
		if !ok || p.Opts.Autopilot {
			return value, err
		}
		p.Gate.Info(step + " failed: " + err.Error())
		again, gateErr := gate.RetryGate(ctx, step, err)
		if gateErr != nil {
			return value, gateErr
		}
		if !again {
			return value, err
		}
		p.Gate.Info("retrying " + step)
		p.logf(artifact.LevelInfo, step, "step.retry", "retrying %s", step)
	}
}

type publishControls interface {
	SetPublishHandler(func() error)
	ProcessControls()
}

func (p *Pipeline) processControls() {
	if gate, ok := p.Gate.(publishControls); ok {
		gate.ProcessControls()
	}
}

// PublishRun publishes an idle run. Active runs use the pipeline's own handler
// at a checkpoint so committing never races an agent's writes.
func PublishRun(run *artifact.Run) error {
	unlock, err := worktree.LockCheckout(run.Worktree)
	if err != nil {
		return err
	}
	defer unlock()
	return publishRun(run)
}

func publishRun(run *artifact.Run) error {
	branch, err := worktree.CurrentBranch(run.Worktree)
	if err != nil {
		return err
	}
	if branch != run.Branch {
		return fmt.Errorf("checkout is on %q, expected %q", branch, run.Branch)
	}
	diff, err := worktree.Snapshot(run.Worktree)
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) != "" {
		if err := run.Write("diff.patch", []byte(diff)); err != nil {
			return err
		}
	}
	message := commitMessage(context.Background(), runCommitSpec(run), run.Worktree, run.Prompt, diff, run.Prompt, run.ID)
	commit, err := worktree.Commit(run.Worktree, message)
	if err != nil {
		return err
	}
	if commit != "" {
		run.Commit, run.Pushed = commit, false
		_ = run.Log(artifact.LogEntry{Level: artifact.LevelInfo, Stage: "commit", Event: "commit.done",
			Message: "committed " + shortSHA(commit) + " on " + run.Branch})
	}
	if err := run.Save(); err != nil {
		return err
	}
	if err := worktree.Push(run.Worktree, run.Branch); err != nil {
		_ = run.AddError("push", "", err)
		return fmt.Errorf("commit kept on %s; push failed (press p to retry): %w", run.Branch, err)
	}
	run.Pushed = true
	_ = run.Log(artifact.LogEntry{Level: artifact.LevelInfo, Stage: "push", Event: "push.done",
		Message: "pushed " + run.Branch + " to origin"})
	return run.Save()
}
