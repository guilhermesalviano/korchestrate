package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
		started := time.Now()
		res, err := p.runStage(ctx, agent.Planner, func(a agent.Agent, model string) (*agent.Result, error) {
			p.logStageStart(agent.Planner, a.Name(), model, 0)
			return a.Run(ctx, agent.Request{
				Role:         agent.Planner,
				Dir:          p.worktreePath,
				Prompt:       base + correction,
				System:       p.Cfg.Prompts.Resolved().Planner,
				Model:        model,
				Variant:      spec.Variant,
				Agent:        plannerSubAgent(a.Name(), spec.SubAgent),
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
			p.logStageEnd(agent.Planner, 0, started, res, err)
			lastErr = err
			correction = "\n\n(Your previous attempt failed; return ONLY valid JSON matching the schema.)"
			continue
		}
		if len(res.Structured) == 0 {
			lastErr = errors.New("planner returned no structured output")
			p.logStageRetry(agent.Planner, 0, lastErr.Error())
			correction = "\n\n(You did not return JSON. Return ONLY the JSON object matching the schema.)"
			continue
		}
		var plan contracts.Plan
		if err := contracts.DecodeObject(res.Structured, &plan); err != nil {
			lastErr = fmt.Errorf("decode plan: %w", err)
			p.logStageRetry(agent.Planner, 0, lastErr.Error())
			correction = fmt.Sprintf("\n\n(Your JSON was invalid: %v. Return corrected JSON only.)", err)
			continue
		}
		if err := plan.Validate(); err != nil {
			lastErr = fmt.Errorf("invalid plan: %w", err)
			p.logStageRetry(agent.Planner, 0, lastErr.Error())
			correction = fmt.Sprintf("\n\n(Your plan was invalid: %v. Return corrected JSON only.)", err)
			continue
		}
		p.logStageEnd(agent.Planner, 0, started, res, nil)
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
	started := time.Now()
	res, err := p.runStage(ctx, agent.Executor, func(a agent.Agent, model string) (*agent.Result, error) {
		p.logStageStart(agent.Executor, a.Name(), model, iter)
		return a.Run(ctx, agent.Request{
			Role:         agent.Executor,
			Dir:          p.worktreePath,
			Prompt:       renderExecutor(p.Opts.Prompt, plan, p.Run.Path("plan.md"), iter, fix),
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
		p.logStageEnd(agent.Executor, iter, started, res, err)
		return nil, err
	}
	var report *contracts.ExecReport
	if len(res.Structured) == 0 {
		p.logf(artifact.LevelWarn, "executor", "executor.no_report", "executor returned no structured report")
	} else {
		var decoded contracts.ExecReport
		if derr := contracts.DecodeObject(res.Structured, &decoded); derr != nil {
			p.Gate.Info("warning: could not decode executor report: " + derr.Error())
			p.logf(artifact.LevelWarn, "executor", "executor.report_invalid", "could not decode executor report: %v", derr)
		} else {
			report = &decoded
		}
	}
	p.logStageEnd(agent.Executor, iter, started, res, nil)
	return report, nil
}

// review runs the reviewer with one validation retry per agent. Adapter
// failures and reviews that never satisfy the contract are reported as stage
// errors so runStage can offer the configured fallback agent.
func (p *Pipeline) review(ctx context.Context, plan *contracts.Plan, diff string, report *contracts.ExecReport, iter int) (*contracts.Review, error) {
	if err := p.Run.SetState(artifact.StateReviewing); err != nil {
		return nil, err
	}
	p.Gate.Stage(agent.Reviewer, fmt.Sprintf("reviewing iteration %d with %s", iter, p.Cfg.Models.Reviewer.Model))

	schemaPath := p.Run.Path("review.schema.json")
	if err := os.WriteFile(schemaPath, []byte(contracts.ReviewSchema), 0o644); err != nil {
		return nil, err
	}
	base := renderReviewer(p.Opts.Prompt, plan, diff, report)

	started := time.Now()
	res, err := p.runStage(ctx, agent.Reviewer, func(a agent.Agent, model string) (*agent.Result, error) {
		var correction string
		var lastErr error
		for attempt := 1; attempt <= 2; attempt++ {
			spec := p.Cfg.Models.Reviewer
			tag := fmt.Sprintf("%d.%d.%s", iter, attempt, a.Name())
			outFile := p.Run.Path("reviewer.last." + tag + ".txt")
			p.logStageStart(agent.Reviewer, a.Name(), model, iter)
			res, err := a.Run(ctx, agent.Request{
				Role:         agent.Reviewer,
				Dir:          p.worktreePath,
				Prompt:       base + correction,
				System:       p.Cfg.Prompts.Resolved().Reviewer,
				Model:        model,
				Variant:      spec.Variant,
				Agent:        spec.SubAgent,
				ExtraArgs:    spec.ExtraArgs,
				SchemaInline: contracts.ReviewSchema,
				SchemaFile:   schemaPath,
				OutFile:      outFile,
				Timeout:      p.Cfg.Timeouts.Reviewer.Duration(),
				Observe:      p.Gate.Line,
			})
			writeEvents(p.Run, "reviewer.events."+tag+".jsonl", res)
			p.addUsage(res)
			if res != nil && res.Final != "" {
				_ = p.Run.Write("reviewer.last."+tag+".txt", []byte(res.Final))
			}
			if err != nil {
				// A broken adapter is runStage's call to make: let it offer a
				// fallback instead of burning the correction retry.
				return nil, err
			}
			if len(res.Structured) == 0 {
				lastErr = errors.New("reviewer returned no structured output")
				p.logStageRetry(agent.Reviewer, iter, lastErr.Error())
				correction = "\n\n(You did not return JSON. Return ONLY the JSON verdict object.)"
				continue
			}
			var review contracts.Review
			if err := contracts.DecodeObject(res.Structured, &review); err != nil {
				lastErr = fmt.Errorf("decode review: %w", err)
				p.logStageRetry(agent.Reviewer, iter, lastErr.Error())
				correction = fmt.Sprintf("\n\n(Your JSON was invalid: %v. Return corrected JSON only.)", err)
				continue
			}
			if err := review.Validate(); err != nil {
				lastErr = fmt.Errorf("invalid review: %w", err)
				p.logStageRetry(agent.Reviewer, iter, lastErr.Error())
				correction = fmt.Sprintf("\n\n(Your review was invalid: %v. Return corrected JSON only, with a top-level \"verdict\" of pass or fail.)", err)
				continue
			}
			return res, nil
		}
		return nil, fmt.Errorf("reviewer failed: %w", lastErr)
	})
	if err != nil {
		p.logStageEnd(agent.Reviewer, iter, started, res, err)
		return nil, err
	}
	var review contracts.Review
	if err := contracts.DecodeObject(res.Structured, &review); err != nil {
		p.logStageEnd(agent.Reviewer, iter, started, res, nil)
		return nil, fmt.Errorf("reviewer failed: decode review: %w", err)
	}
	p.logStageEnd(agent.Reviewer, iter, started, res, nil)
	return &review, nil
}

const maxDiffBytes = 120000

func capDiff(s string) string {
	if len(s) <= maxDiffBytes {
		return s
	}
	return s[:maxDiffBytes] + "\n\n[diff truncated by orchestrator]\n"
}

// plannerSubAgent keeps opencode read-only while planning: its default agent
// can edit files, so "plan" is used unless the config names another.
func plannerSubAgent(adapter, configured string) string {
	if configured == "" && adapter == "opencode" {
		return "plan"
	}
	return configured
}

// renderExecutor hands the executor the approved plan as markdown, the same
// document saved as plan.md in the run directory.
func renderExecutor(request string, plan *contracts.Plan, planFile string, iter int, fix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Original request\n%s\n\n", request)
	fmt.Fprintf(&b, "## Approved plan (also saved at %s)\n\n%s\n", planFile, demoteHeadings(plan.Markdown()))
	if strings.TrimSpace(fix) != "" {
		fmt.Fprintf(&b, "\n## Fix required (iteration %d)\nThe reviewer found these issues; fix them and nothing else:\n%s\n", iter, fix)
	}
	b.WriteString("\nImplement the plan now and return the JSON report.")
	return b.String()
}

func renderReviewer(request string, plan *contracts.Plan, diff string, report *contracts.ExecReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Original request\n%s\n\n", request)
	fmt.Fprintf(&b, "## Approved plan\n\n%s\n", demoteHeadings(plan.Markdown()))
	b.WriteString("Verify every item under Acceptance criteria.\n")
	b.WriteString("\n## Executor's report\n")
	if report == nil {
		b.WriteString("The executor returned no report, so no checks are known to have run.\n")
	} else {
		fmt.Fprintf(&b, "Status: %s\n%s\n", report.Status, strings.TrimSpace(report.Summary))
		b.WriteString("\nChecks the executor ran, with their results (do not run them again):\n")
		if len(report.Commands) == 0 {
			b.WriteString("- none\n")
		}
		for _, c := range report.Commands {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		if len(report.KnownGaps) > 0 {
			b.WriteString("\nKnown gaps:\n")
			for _, g := range report.KnownGaps {
				fmt.Fprintf(&b, "- %s\n", g)
			}
		}
	}
	fmt.Fprintf(&b, "\n## Diff under review\n```diff\n%s\n```\n", capDiff(diff))
	b.WriteString("\nInspect the worktree as needed, then return ONLY the JSON review object.")
	return b.String()
}

// demoteHeadings nests a markdown document under the prompt's own "##"
// sections.
func demoteHeadings(md string) string {
	lines := strings.Split(md, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			lines[i] = "##" + line
		}
	}
	return strings.Join(lines, "\n")
}
