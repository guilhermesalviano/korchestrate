package config

import (
	"fmt"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/contracts"
)

const MaxBasePromptBytes = 16 << 10

// Prompts contains reusable instructions for each pipeline role. The original
// request, plan, diff and retry feedback are supplied separately at runtime.
type Prompts struct {
	Planner  string `yaml:"planner" json:"planner"`
	Executor string `yaml:"executor" json:"executor"`
	Reviewer string `yaml:"reviewer" json:"reviewer"`
}

func DefaultPrompts() Prompts {
	return Prompts{Planner: contracts.PlannerPrompt, Executor: contracts.ExecutorPrompt, Reviewer: contracts.ReviewerPrompt}
}

// Resolved lets old configs and omitted/blank fields keep the built-in prompts.
func (p Prompts) Resolved() Prompts {
	defaults := DefaultPrompts()
	if strings.TrimSpace(p.Planner) == "" {
		p.Planner = defaults.Planner
	}
	if strings.TrimSpace(p.Executor) == "" {
		p.Executor = defaults.Executor
	}
	if strings.TrimSpace(p.Reviewer) == "" {
		p.Reviewer = defaults.Reviewer
	}
	return p
}

func (p Prompts) Validate() error {
	for _, stage := range []struct{ name, text string }{{"planner", p.Planner}, {"executor", p.Executor}, {"reviewer", p.Reviewer}} {
		if len(stage.text) > MaxBasePromptBytes {
			return fmt.Errorf("%s base prompt exceeds %d bytes", stage.name, MaxBasePromptBytes)
		}
	}
	return nil
}
