package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
)

func adapterFor(name string) (agent.Agent, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude":
		return agent.Claude{}, nil
	case "codex":
		return agent.Codex{}, nil
	case "opencode", "":
		return agent.OpenCode{}, nil
	case "antigravity":
		return agent.Antigravity{}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q (want claude|codex|opencode|antigravity)", name)
	}
}

func (p *Pipeline) addUsage(res *agent.Result) {
	if res == nil {
		return
	}
	p.Run.Usage.Add(res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.CostUSD)
	_ = p.Run.Save()
	p.notify()
}

func writeEvents(run *artifact.Run, name string, res *agent.Result) {
	if res == nil || len(res.Events) == 0 {
		return
	}
	var b strings.Builder
	for _, ev := range res.Events {
		b.Write(ev)
		b.WriteByte('\n')
	}
	_ = run.Write(name, []byte(b.String()))
}

// plan runs the planner with one validation retry.
func (p *Pipeline) plan(ctx context.Context) (*contracts.Plan, error) {
	if err := p.Run.SetState(artifact.StatePlanning); err != nil {
		return nil, err
	}
	p.Gate.Stage(agent.Planner, "planning with "+p.Cfg.Models.Planner.Model)

	base := fmt.Sprintf("Repository root: %s\n\nUser request:\n%s\n", p.worktreePath, p.Opts.Prompt)
	var correction string
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		spec := p.Cfg.Models.Planner
		res, err := p.runStage(ctx, agent.Planner, func(a agent.Agent, model string) (*agent.Result, error) {
			return a.Run(ctx, agent.Request{
				Dir:          p.worktreePath,
				Prompt:       base + correction,
				System:       p.Cfg.Prompts.Resolved().Planner,
				Model:        model,
				Variant:      spec.Variant,
				ExtraArgs:    spec.ExtraArgs,
				SchemaInline: contracts.PlanSchema,
				BudgetUSD:    p.Cfg.PlannerBudgetUSD,
				Timeout:      p.Cfg.Timeouts.Planner.Duration(),
				Observe:      p.Gate.Line,
			})
		})
		writeEvents(p.Run, "planner.events.jsonl", res)
		p.addUsage(res)
		if err != nil {
			lastErr = err
			correction = "\n\n(Your previous attempt failed; return ONLY valid JSON matching the schema.)"
			continue
		}
		if len(res.Structured) == 0 {
			lastErr = errors.New("planner returned no structured output")
			correction = "\n\n(You did not return JSON. Return ONLY the JSON object matching the schema.)"
			continue
		}
		var plan contracts.Plan
		if err := contracts.DecodeObject(res.Structured, &plan); err != nil {
			lastErr = fmt.Errorf("decode plan: %w", err)
			correction = fmt.Sprintf("\n\n(Your JSON was invalid: %v. Return corrected JSON only.)", err)
			continue
		}
		if err := plan.Validate(); err != nil {
			lastErr = fmt.Errorf("invalid plan: %w", err)
			correction = fmt.Sprintf("\n\n(Your plan was invalid: %v. Return corrected JSON only.)", err)
			continue
		}
		return &plan, nil
	}
	return nil, fmt.Errorf("planner failed: %w", lastErr)
}

// execute runs the executor. It returns a report when one was produced.
func (p *Pipeline) execute(ctx context.Context, plan *contracts.Plan, iter int, fix string) (*contracts.ExecReport, error) {
	if err := p.Run.SetState(artifact.StateExecuting); err != nil {
		return nil, err
	}
	p.Gate.Stage(agent.Executor, fmt.Sprintf("executing iteration %d with %s", iter, p.Cfg.Models.Executor.Model))

	schemaPath := p.Run.Path("codex-report.schema.json")
	if err := os.WriteFile(schemaPath, []byte(contracts.ExecReportSchema), 0o644); err != nil {
		return nil, err
	}
	outFile := p.Run.Path(fmt.Sprintf("executor.last.%d.txt", iter))

	spec := p.Cfg.Models.Executor
	res, err := p.runStage(ctx, agent.Executor, func(a agent.Agent, model string) (*agent.Result, error) {
		return a.Run(ctx, agent.Request{
			Dir:          p.worktreePath,
			Prompt:       renderExecutor(p.Opts.Prompt, plan, iter, fix),
			System:       p.Cfg.Prompts.Resolved().Executor,
			Model:        model,
			Variant:      spec.Variant,
			ExtraArgs:    spec.ExtraArgs,
			SchemaFile:   schemaPath,
			OutFile:      outFile,
			Sandbox:      spec.Sandbox,
			ApproveForMe: spec.ApproveForMe,
			Bypass:       spec.Bypass,
			Timeout:      p.Cfg.Timeouts.Executor.Duration(),
			Observe:      p.Gate.Line,
		})
	})
	writeEvents(p.Run, fmt.Sprintf("executor.events.%d.jsonl", iter), res)
	p.addUsage(res)
	if err != nil {
		return nil, err
	}
	if len(res.Structured) == 0 {
		return nil, nil
	}
	var report contracts.ExecReport
	if err := contracts.DecodeObject(res.Structured, &report); err != nil {
		p.Gate.Info("warning: could not decode executor report: " + err.Error())
		return nil, nil
	}
	return &report, nil
}

// review runs the reviewer with one validation retry.
func (p *Pipeline) review(ctx context.Context, plan *contracts.Plan, diff string, iter int) (*contracts.Review, error) {
	if err := p.Run.SetState(artifact.StateReviewing); err != nil {
		return nil, err
	}
	p.Gate.Stage(agent.Reviewer, fmt.Sprintf("reviewing iteration %d with %s", iter, p.Cfg.Models.Reviewer.Model))

	base := renderReviewer(p.Opts.Prompt, plan, diff)
	var correction string
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		spec := p.Cfg.Models.Reviewer
		res, err := p.runStage(ctx, agent.Reviewer, func(a agent.Agent, model string) (*agent.Result, error) {
			return a.Run(ctx, agent.Request{
				Dir:       p.worktreePath,
				Prompt:    base + correction,
				System:    p.Cfg.Prompts.Resolved().Reviewer,
				Model:     model,
				Variant:   spec.Variant,
				Agent:     spec.SubAgent,
				ExtraArgs: spec.ExtraArgs,
				Timeout:   p.Cfg.Timeouts.Reviewer.Duration(),
				Observe:   p.Gate.Line,
			})
		})
		writeEvents(p.Run, fmt.Sprintf("reviewer.events.%d.jsonl", iter), res)
		p.addUsage(res)
		if err != nil {
			lastErr = err
			continue
		}
		if len(res.Structured) == 0 {
			lastErr = errors.New("reviewer returned no structured output")
			correction = "\n\n(You did not return JSON. Return ONLY the JSON verdict object.)"
			continue
		}
		var review contracts.Review
		if err := contracts.DecodeObject(res.Structured, &review); err != nil {
			lastErr = fmt.Errorf("decode review: %w", err)
			correction = fmt.Sprintf("\n\n(Your JSON was invalid: %v. Return corrected JSON only.)", err)
			continue
		}
		if err := review.Validate(); err != nil {
			lastErr = fmt.Errorf("invalid review: %w", err)
			correction = fmt.Sprintf("\n\n(Your review was invalid: %v. Return corrected JSON only.)", err)
			continue
		}
		return &review, nil
	}
	return nil, fmt.Errorf("reviewer failed: %w", lastErr)
}

const maxDiffBytes = 120000

func capDiff(s string) string {
	if len(s) <= maxDiffBytes {
		return s
	}
	return s[:maxDiffBytes] + "\n\n[diff truncated by orchestrator]\n"
}

func renderExecutor(request string, plan *contracts.Plan, iter int, fix string) string {
	planJSON, _ := json.MarshalIndent(plan, "", "  ")
	var b strings.Builder
	fmt.Fprintf(&b, "## Original request\n%s\n\n", request)
	fmt.Fprintf(&b, "## Approved plan (JSON)\n%s\n\n", planJSON)
	b.WriteString("## Acceptance criteria\n")
	for _, c := range plan.AcceptanceCriteria {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	if strings.TrimSpace(fix) != "" {
		fmt.Fprintf(&b, "\n## Fix required (iteration %d)\nThe reviewer found these issues; fix them and nothing else:\n%s\n", iter, fix)
	}
	b.WriteString("\nImplement the plan now and return the JSON report.")
	return b.String()
}

func renderReviewer(request string, plan *contracts.Plan, diff string) string {
	planJSON, _ := json.MarshalIndent(plan, "", "  ")
	var b strings.Builder
	fmt.Fprintf(&b, "## Original request\n%s\n\n", request)
	fmt.Fprintf(&b, "## Approved plan (JSON)\n%s\n\n", planJSON)
	b.WriteString("## Acceptance criteria to verify\n")
	for _, c := range plan.AcceptanceCriteria {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	fmt.Fprintf(&b, "\n## Diff under review\n```diff\n%s\n```\n", capDiff(diff))
	b.WriteString("\nInspect the worktree as needed, then return ONLY the JSON review object.")
	return b.String()
}
