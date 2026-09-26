// Package config loads and defaults the kor orchestrator configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings such as "15m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalYAML renders the duration as a string such as "15m0s".
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ModelSpec configures one agent invocation.
type ModelSpec struct {
	Agent     string   `yaml:"agent"`    // adapter: claude | codex | opencode | antigravity
	Model     string   `yaml:"model"`    // provider/model
	Variant   string   `yaml:"variant"`  // reasoning effort, provider-specific
	SubAgent  string   `yaml:"subagent"` // opencode agent name
	Fallback  string   `yaml:"fallback"` // adapter offered when Agent fails
	ExtraArgs []string `yaml:"extra_args"`
}

// ExecutorSpec adds executor-only sandbox controls.
type ExecutorSpec struct {
	Agent        string   `yaml:"agent"`
	Model        string   `yaml:"model"`
	Variant      string   `yaml:"variant"`
	Fallback     string   `yaml:"fallback"`
	ExtraArgs    []string `yaml:"extra_args"`
	Sandbox      string   `yaml:"sandbox"`
	ApproveForMe bool     `yaml:"approve_for_me"`
	Bypass       bool     `yaml:"bypass"`
}

// AsModel returns the shared model fields.
func (e ExecutorSpec) AsModel() ModelSpec {
	return ModelSpec{Agent: e.Agent, Model: e.Model, Variant: e.Variant, Fallback: e.Fallback, ExtraArgs: e.ExtraArgs}
}

// DefaultModelFor returns the built-in model for an adapter, used when a run
// falls back to an agent that was not configured for the stage.
func DefaultModelFor(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude":
		return "opus"
	case "codex":
		return "gpt-6-sol"
	case "opencode":
		return "opencode-go/deepseek-v4.1-flash"
	case "antigravity":
		return "gemini-3.1-pro-high"
	default:
		return ""
	}
}

// Config is the resolved orchestrator configuration.
type Config struct {
	Repo    string  `yaml:"repo"`
	Prompts Prompts `yaml:"prompts"`

	Models struct {
		Planner  ModelSpec    `yaml:"planner"`
		Executor ExecutorSpec `yaml:"executor"`
		Reviewer ModelSpec    `yaml:"reviewer"`
	} `yaml:"models"`

	Loop struct {
		MaxIterations int `yaml:"max_iterations"`
	} `yaml:"loop"`

	Gates struct {
		AfterPlan   bool `yaml:"after_plan"`
		AfterReview bool `yaml:"after_review"`
	} `yaml:"gates"`

	Timeouts struct {
		Planner  Duration `yaml:"planner"`
		Executor Duration `yaml:"executor"`
		Reviewer Duration `yaml:"reviewer"`
	} `yaml:"timeouts"`

	PlannerBudgetUSD float64 `yaml:"planner_budget_usd"`
	ArtifactsDir     string  `yaml:"artifacts_dir"`

	// Apply commits the branch and (with OpenPR) opens a PR after approval.
	Apply  bool `yaml:"apply"`
	OpenPR bool `yaml:"open_pr"`
}

// Default returns the built-in configuration.
func Default() *Config {
	c := &Config{}
	c.Prompts = DefaultPrompts()
	c.Models.Planner = ModelSpec{Agent: "claude", Model: "opus", Fallback: "codex"}
	c.Models.Executor = ExecutorSpec{
		Agent:        "codex",
		Model:        "gpt-6-sol",
		Fallback:     "opencode",
		Sandbox:      "workspace-write",
		ApproveForMe: true,
	}
	c.Models.Reviewer = ModelSpec{Agent: "opencode", Model: "opencode-go/deepseek-v4.1-flash", Variant: "high", SubAgent: "plan", Fallback: "claude"}
	c.Loop.MaxIterations = 3
	c.Gates.AfterPlan = true
	c.Gates.AfterReview = true
	c.Timeouts.Planner = Duration(15 * time.Minute)
	c.Timeouts.Executor = Duration(45 * time.Minute)
	c.Timeouts.Reviewer = Duration(15 * time.Minute)
	c.PlannerBudgetUSD = 5
	c.ArtifactsDir = DefaultArtifactsDir()
	return c
}

// DefaultArtifactsDir returns ~/.local/state/korchestrate/runs.
func DefaultArtifactsDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "kor-runs")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "korchestrate", "runs")
}

// Load reads a JSON or YAML config file, merging it over defaults. A missing path is
// not an error (defaults are returned).
func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	// JSON is also accepted by the YAML decoder, including string durations.
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	return c, nil
}

// Discover looks for kor.yaml or config.json in repo, then config.yaml or
// config.json in the user config dir. Existing YAML files take precedence
// over JSON files in the same directory.
func Discover(repo string) (path string) {
	candidates := []string{}
	if repo != "" {
		candidates = append(candidates, filepath.Join(repo, "kor.yaml"), filepath.Join(repo, "config.json"))
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		candidates = append(candidates, filepath.Join(base, "kor", "config.yaml"), filepath.Join(base, "kor", "config.json"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "kor", "config.yaml"), filepath.Join(home, ".config", "kor", "config.json"))
	}
	for _, cand := range candidates {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Models.Planner.Agent == "" {
		c.Models.Planner = d.Models.Planner
	}
	if c.Models.Executor.Agent == "" {
		c.Models.Executor = d.Models.Executor
	} else if c.Models.Executor.Sandbox == "" {
		c.Models.Executor.Sandbox = d.Models.Executor.Sandbox
	}
	if c.Models.Reviewer.Agent == "" {
		c.Models.Reviewer = d.Models.Reviewer
	}
	if c.Loop.MaxIterations <= 0 {
		c.Loop.MaxIterations = d.Loop.MaxIterations
	}
	if c.Timeouts.Planner == 0 {
		c.Timeouts.Planner = d.Timeouts.Planner
	}
	if c.Timeouts.Executor == 0 {
		c.Timeouts.Executor = d.Timeouts.Executor
	}
	if c.Timeouts.Reviewer == 0 {
		c.Timeouts.Reviewer = d.Timeouts.Reviewer
	}
	if c.ArtifactsDir == "" {
		c.ArtifactsDir = d.ArtifactsDir
	}
}

// Validate checks that the configuration is internally consistent.
func (c *Config) Validate() error {
	if err := c.Prompts.Validate(); err != nil {
		return err
	}
	if c.Repo == "" {
		return fmt.Errorf("repo is empty")
	}
	if c.Models.Planner.Model == "" || c.Models.Executor.Model == "" || c.Models.Reviewer.Model == "" {
		return fmt.Errorf("planner/executor/reviewer models must be set")
	}
	switch c.Models.Executor.Sandbox {
	case "", "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("invalid executor sandbox %q", c.Models.Executor.Sandbox)
	}
	return nil
}

// MarshalYAML renders the resolved config for the run artifact.
func (c *Config) MarshalYAML() ([]byte, error) {
	resolved := *c
	resolved.Prompts = c.Prompts.Resolved()
	return yaml.Marshal(&resolved)
}
