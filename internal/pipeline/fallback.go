package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
)

// agentChoice is a resolved adapter plus the model to invoke it with.
type agentChoice struct {
	agent agent.Agent
	model string
}

// specFor returns the model spec configured for a pipeline role.
func (p *Pipeline) specFor(kind agent.Kind) config.ModelSpec {
	switch kind {
	case agent.Executor:
		return p.Cfg.Models.Executor.AsModel()
	case agent.Reviewer:
		return p.Cfg.Models.Reviewer
	default:
		return p.Cfg.Models.Planner
	}
}

// stageChoice resolves the adapter and model for a role, honoring any fallback
// the user selected earlier in this run.
func (p *Pipeline) stageChoice(kind agent.Kind) (agent.Agent, string, error) {
	if c, ok := p.overrides[kind]; ok {
		return c.agent, c.model, nil
	}
	spec := p.specFor(kind)
	a, err := p.agentFor(spec.Agent)
	if err != nil {
		return nil, "", err
	}
	return a, spec.Model, nil
}

// runStage invokes a stage's adapter and, when it fails, lets the user pick a
// replacement adapter that is then remembered for the rest of the run. The
// build closure receives the adapter and model so the request adapts too.
func (p *Pipeline) runStage(ctx context.Context, kind agent.Kind, build func(a agent.Agent, model string) (*agent.Result, error)) (*agent.Result, error) {
	spec := p.specFor(kind)
	preferred := strings.TrimSpace(spec.Fallback)
	tried := map[string]bool{}
	for {
		a, model, err := p.stageChoice(kind)
		if err != nil {
			return nil, err
		}
		tried[a.Name()] = true
		res, err := build(a, model)
		if err == nil || ctx.Err() != nil {
			return res, err
		}
		p.Gate.Info(fmt.Sprintf("%s agent %q failed: %v", kind, a.Name(), err))
		p.logError(string(kind), a.Name(), err)
		next, ok, ferr := p.pickFallback(ctx, kind, a.Name(), preferred, tried, err)
		if ferr != nil {
			return res, ferr
		}
		if !ok {
			return res, err
		}
		p.Gate.Info(fmt.Sprintf("retrying %s with %s (%s)", kind, next.agent.Name(), next.model))
		p.logf(artifact.LevelWarn, string(kind), "agent.fallback", "retrying %s with %s (%s)", kind, next.agent.Name(), next.model)
		if p.overrides == nil {
			p.overrides = map[agent.Kind]agentChoice{}
		}
		p.overrides[kind] = next
	}
}

// pickFallback asks the gate to choose an untried adapter after a failure.
func (p *Pipeline) pickFallback(ctx context.Context, kind agent.Kind, failed, preferred string, tried map[string]bool, cause error) (agentChoice, bool, error) {
	var options []string
	for _, n := range agent.Available() {
		if !tried[n] {
			options = append(options, n)
		}
	}
	if len(options) == 0 {
		return agentChoice{}, false, nil
	}
	var (
		chosen string
		err    error
	)
	if p.Opts.Autopilot {
		// The configured fallback when it is still untried, else the next one.
		chosen = options[0]
		for _, o := range options {
			if o == preferred {
				chosen = o
			}
		}
	} else if chosen, err = p.Gate.SelectAgent(ctx, kind, failed, options, preferred, cause); err != nil {
		return agentChoice{}, false, err
	}
	chosen = strings.ToLower(strings.TrimSpace(chosen))
	if chosen == "retry" {
		a, model, err := p.stageChoice(kind)
		return agentChoice{agent: a, model: model}, err == nil, err
	}
	if chosen == "" || tried[chosen] {
		return agentChoice{}, false, nil
	}
	a, err := p.agentFor(chosen)
	if err != nil {
		return agentChoice{}, false, err
	}
	model := config.DefaultModelFor(chosen)
	if model == "" {
		model = p.specFor(kind).Model
	}
	return agentChoice{agent: a, model: model}, true, nil
}
