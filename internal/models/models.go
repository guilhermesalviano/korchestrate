// Package models discovers the providers, models and reasoning efforts
// available to the agent CLIs, so the dashboard can offer them before a run
// starts. Discovery never fails: each adapter falls back to the built-in
// defaults when its source is unavailable.
package models

import (
	"sort"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/config"
)

// Choice is one stage's provider/model/effort selection. Agent is the adapter
// (claude | codex | opencode | antigravity), Model the CLI model id and Variant the
// reasoning effort (codex model_reasoning_effort, opencode --variant).
type Choice struct {
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Variant string `json:"variant"`
}

// Choices holds the selection for the three pipeline stages.
type Choices struct {
	Planner  Choice `json:"planner"`
	Executor Choice `json:"executor"`
	Reviewer Choice `json:"reviewer"`
}

// For returns the choice for a pipeline stage.
func (c Choices) For(k agent.Kind) Choice {
	switch k {
	case agent.Planner:
		return c.Planner
	case agent.Executor:
		return c.Executor
	default:
		return c.Reviewer
	}
}

// ModelInfo is one selectable model. Efforts is the list of reasoning efforts
// the model accepts; empty means effort selection does not apply.
type ModelInfo struct {
	ID            string   `json:"id"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
}

// AgentInfo is the catalog of models for one adapter.
type AgentInfo struct {
	Agent  string      `json:"agent"`
	Models []ModelInfo `json:"models"`
}

// Catalog is everything discovery found, in canonical adapter order.
type Catalog struct {
	Agents []AgentInfo `json:"agents"`
}

// Agent returns the catalog entry for an adapter, or nil.
func (c *Catalog) Agent(name string) *AgentInfo {
	if c == nil {
		return nil
	}
	for i := range c.Agents {
		if c.Agents[i].Agent == name {
			return &c.Agents[i]
		}
	}
	return nil
}

// Find returns the catalog entry for one model, or nil.
func (c *Catalog) Find(agentName, model string) *ModelInfo {
	a := c.Agent(agentName)
	if a == nil {
		return nil
	}
	for i := range a.Models {
		if a.Models[i].ID == model {
			return &a.Models[i]
		}
	}
	return nil
}

// Efforts returns the reasoning efforts accepted by a model, or nil when the
// model or agent takes no effort.
func (c *Catalog) Efforts(agentName, model string) []string {
	if m := c.Find(agentName, model); m != nil {
		return m.Efforts
	}
	return nil
}

// ProviderOrder lists the adapters in the canonical selection order.
// Optional adapters whose CLI is not installed are left out.
func ProviderOrder() []string { return agent.Available() }

// ChoicesFromConfig seeds the picker with the resolved configuration.
func ChoicesFromConfig(cfg *config.Config) Choices {
	return Choices{
		Planner: Choice{
			Agent:   cfg.Models.Planner.Agent,
			Model:   cfg.Models.Planner.Model,
			Variant: cfg.Models.Planner.Variant,
		},
		Executor: Choice{
			Agent:   cfg.Models.Executor.Agent,
			Model:   cfg.Models.Executor.Model,
			Variant: cfg.Models.Executor.Variant,
		},
		Reviewer: Choice{
			Agent:   cfg.Models.Reviewer.Agent,
			Model:   cfg.Models.Reviewer.Model,
			Variant: cfg.Models.Reviewer.Variant,
		},
	}
}

// Apply returns a per-run copy of cfg with the chosen provider, model and
// effort. A stage without a model keeps the configured one.
func (c Choices) Apply(cfg *config.Config) *config.Config {
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

// DefaultModel returns the model to preselect for an adapter: the built-in
// default when the catalog knows it, else the catalog's first model.
func (c *Catalog) DefaultModel(agentName string) string {
	def := config.DefaultModelFor(agentName)
	if c != nil {
		if c.Find(agentName, def) != nil {
			return def
		}
		if a := c.Agent(agentName); a != nil && len(a.Models) > 0 {
			return a.Models[0].ID
		}
	}
	return def
}

// Discover collects the catalog from every adapter source. Sources that are
// unavailable contribute their fallback entries; Discover always returns a
// usable catalog.
func Discover() *Catalog {
	c := &Catalog{Agents: []AgentInfo{claudeCatalog(), DiscoverCodex(), DiscoverOpencode()}}
	if agent.Installed("antigravity") {
		c.Agents = append(c.Agents, DiscoverAntigravity())
	}
	for _, name := range ProviderOrder() {
		if c.Agent(name) == nil {
			c.Agents = append(c.Agents, AgentInfo{Agent: name})
		}
	}
	return c
}

// claudeCatalog lists the Claude Code model aliases. The CLI has no model
// listing and no effort flag, so the aliases are static and carry no efforts.
func claudeCatalog() AgentInfo {
	ids := []string{"opus", "sonnet", "fable", "haiku"}
	models := make([]ModelInfo, len(ids))
	for i, id := range ids {
		models[i] = ModelInfo{ID: id}
	}
	return AgentInfo{Agent: "claude", Models: models}
}

// sortedKeys returns the map's keys in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stripANSI removes ANSI escape sequences from CLI output.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j
			continue
		}
		// Two-byte escape (e.g. ESC ]): skip the next byte.
		i++
	}
	return b.String()
}
