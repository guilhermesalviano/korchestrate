// Package agent adapts the supported agent CLIs (claude, codex, opencode,
// antigravity) behind one interface so the pipeline can treat them uniformly.
package agent

import (
	"context"
	"encoding/json"
	"time"
)

// Kind identifies the pipeline role an adapter plays.
type Kind string

const (
	Planner  Kind = "planner"
	Executor Kind = "executor"
	Reviewer Kind = "reviewer"
)

// Known lists the adapter names the orchestrator can invoke.
func Known() []string { return []string{"claude", "codex", "opencode", "antigravity"} }

// Usage captures token/cost accounting when the CLI reports it.
type Usage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Event is one streamed line of agent output.
type Event struct {
	Kind   Kind
	Stream string // "stdout", "stderr" or "status"
	Line   string
}

// Observer receives streamed events plus synthetic status lines.
type Observer func(Event)

// Status emits a synthetic, human-readable progress line.
func (o Observer) Status(k Kind, msg string) {
	if o != nil {
		o(Event{Kind: k, Stream: "status", Line: msg})
	}
}

// Request describes a single agent invocation.
type Request struct {
	// Role is the pipeline stage being served; adapters that can fill
	// several roles use it for their mode and progress labels.
	Role   Kind
	Dir    string
	Prompt string
	System string

	Model   string
	Variant string
	Agent   string // opencode agent name (e.g. "plan")

	SchemaInline string // claude --json-schema (inline JSON)
	SchemaFile   string // codex --output-schema (path)
	OutFile      string // codex -o output-last-message path

	Sandbox      string // codex -s
	ApproveForMe bool   // codex --approve-for-me
	Bypass       bool   // codex --dangerously-bypass-approvals-and-sandbox
	BudgetUSD    float64

	Timeout   time.Duration
	ExtraArgs []string
	Env       []string
	Observe   Observer
}

// promptWithSystem is used by adapters that carry base instructions in the
// prompt argument rather than a separate system-prompt flag.
func (r Request) promptWithSystem() string {
	if r.System == "" {
		return r.Prompt
	}
	return r.System + "\n\n" + r.Prompt
}

// roleOr returns the requested role, or def when the caller did not set one.
func (r Request) roleOr(def Kind) Kind {
	if r.Role != "" {
		return r.Role
	}
	return def
}

// Result is the normalized outcome of an agent invocation.
type Result struct {
	ExitCode   int
	Final      string
	Structured json.RawMessage
	Events     []json.RawMessage
	Stderr     string
	Duration   time.Duration
	Usage      Usage
}

// Agent is implemented by each CLI adapter.
type Agent interface {
	Name() string
	Kind() Kind
	Run(ctx context.Context, r Request) (*Result, error)
}
