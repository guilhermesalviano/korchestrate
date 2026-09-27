package pipeline

import (
	"fmt"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
)

// logEntry appends one entry to the run's audit timeline. Logging is
// best-effort: it must never fail a run.
func (p *Pipeline) logEntry(e artifact.LogEntry) {
	if p.Run == nil {
		return
	}
	_ = p.Run.Log(e)
}

// logf is the short form for informational timeline entries.
func (p *Pipeline) logf(level, stage, event, format string, args ...any) {
	p.logEntry(artifact.LogEntry{
		Level:   level,
		Stage:   stage,
		Event:   event,
		Message: fmt.Sprintf(format, args...),
	})
}

// markStage remembers which stage and agent last started, so a failure
// returned to Execute's handler can be attributed to the right one.
func (p *Pipeline) markStage(stage, agentName, model string) {
	p.stage, p.stageAgent, p.stageModel = stage, agentName, model
}

// logError records a recovered or fatal failure in the run's error history
// and timeline.
func (p *Pipeline) logError(stage, agentName string, err error) {
	if p.Run == nil || err == nil {
		return
	}
	_ = p.Run.AddError(stage, agentName, err)
}

// logStageStart records the beginning of one agent attempt. It is called from
// the runStage closure so a fallback agent logs its own start.
func (p *Pipeline) logStageStart(kind agent.Kind, agentName, model string, iter int) {
	p.markStage(string(kind), agentName, model)
	p.logEntry(artifact.LogEntry{
		Level: artifact.LevelInfo, Stage: string(kind), Agent: agentName, Model: model,
		Iter: iter, Event: "stage.start", Message: "starting " + string(kind),
	})
}

// logStageRetry records a same-agent retry, usually after malformed output.
func (p *Pipeline) logStageRetry(kind agent.Kind, iter int, reason string) {
	p.logEntry(artifact.LogEntry{
		Level: artifact.LevelWarn, Stage: string(kind), Agent: p.stageAgent,
		Model: p.stageModel, Iter: iter, Event: "stage.retry", Message: reason,
	})
}

// logStageEnd records the outcome, wall time, exit code and usage of the
// stage attempt marked by the last logStageStart. res is nil when the adapter
// failed before returning one.
func (p *Pipeline) logStageEnd(kind agent.Kind, iter int, started time.Time, res *agent.Result, err error) {
	e := artifact.LogEntry{
		Stage: string(kind), Agent: p.stageAgent, Model: p.stageModel, Iter: iter,
		Event: "stage.end", Message: string(kind) + " finished",
		DurationMS: time.Since(started).Milliseconds(),
	}
	if res != nil {
		if res.Duration > 0 {
			e.DurationMS = res.Duration.Milliseconds()
		}
		code := res.ExitCode
		e.ExitCode = &code
		if u := res.Usage; u.InputTokens > 0 || u.OutputTokens > 0 || u.CostUSD > 0 {
			e.Usage = &artifact.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: u.CostUSD}
		}
	}
	if err != nil {
		e.Level = artifact.LevelError
		e.Event = "stage.failed"
		e.Message = string(kind) + " failed: " + err.Error()
		p.logEntry(e)
		p.logError(string(kind), p.stageAgent, err)
		return
	}
	e.Level = artifact.LevelInfo
	p.logEntry(e)
}
