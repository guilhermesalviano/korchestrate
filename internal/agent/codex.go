package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/contracts"
)

// Codex adapts the `codex exec` CLI. It is used as the executor.
type Codex struct{}

func (Codex) Name() string { return "codex" }
func (Codex) Kind() Kind   { return Executor }

func (c Codex) Run(ctx context.Context, r Request) (*Result, error) {
	args := append(buildArgs(r), r.ExtraArgs...)
	args = append(args, r.promptWithSystem())

	r.Observe.Status(Executor, "codex exec started ("+r.Model+", sandbox="+r.Sandbox+")")
	proc := Exec(ctx, ProcSpec{
		Bin:     "codex",
		Args:    args,
		Dir:     r.Dir,
		Env:     r.Env,
		Timeout: r.Timeout,
		OnLine:  lineObserver(r.Observe, Executor),
	})

	res := &Result{
		ExitCode: proc.ExitCode,
		Stderr:   proc.Stderr,
		Duration: proc.Duration,
	}
	var errorDetail string
	var turnFailed bool
	for _, line := range strings.Split(proc.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if json.Valid([]byte(line)) {
			res.Events = append(res.Events, json.RawMessage(line))
			var event struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Error   struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &event) == nil {
				switch event.Type {
				case "error":
					if event.Message != "" {
						errorDetail = event.Message
					}
				case "turn.failed":
					turnFailed = true
					if event.Error.Message != "" {
						errorDetail = event.Error.Message
					}
				}
			}
		}
	}
	if proc.Err != nil {
		if errorDetail == "" {
			errorDetail = strings.TrimSpace(proc.Stderr)
		}
		if errorDetail != "" {
			return res, fmt.Errorf("codex exited %d: %s: %w", proc.ExitCode, errorDetail, proc.Err)
		}
		return res, fmt.Errorf("codex: %w", proc.Err)
	}
	if turnFailed {
		return res, fmt.Errorf("codex turn failed: %s", errorDetail)
	}

	// Preferred: the schema-validated final message written by -o.
	if r.OutFile != "" {
		if data, err := os.ReadFile(r.OutFile); err == nil {
			res.Final = string(data)
			if raw, err := contracts.ExtractJSON(string(data)); err == nil {
				res.Structured = raw
				return res, nil
			}
		}
	}
	// Fallback: scan the event stream for the last agent message.
	if raw, text := lastAgentMessage(res.Events); raw != nil {
		res.Structured = raw
		res.Final = text
		return res, nil
	}
	r.Observe.Status(Executor, "warning: no structured report from codex; continuing with diff")
	return res, nil
}

// buildArgs assembles the `codex exec` arguments up to (but excluding) any
// extra args and the prompt itself.
func buildArgs(r Request) []string {
	args := []string{"exec", "--json", "-m", r.Model}
	if r.Dir != "" {
		args = append(args, "-C", r.Dir)
	}
	args = append(args, "--skip-git-repo-check")
	switch {
	case r.Bypass:
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case r.ApproveForMe:
		// --approve-for-me is mutually exclusive with -s; it manages its own
		// workspace-write sandbox and auto-reviews approvals.
		args = append(args, "--approve-for-me")
	default:
		if r.Sandbox != "" {
			args = append(args, "-s", r.Sandbox)
		}
	}
	// Variant is the reasoning effort (codex model_reasoning_effort).
	if r.Variant != "" {
		args = append(args, "-c", "model_reasoning_effort="+r.Variant)
	}
	if r.SchemaFile != "" {
		args = append(args, "--output-schema", r.SchemaFile)
	}
	if r.OutFile != "" {
		args = append(args, "-o", r.OutFile)
	}
	return args
}

// lastAgentMessage best-effort extracts the final assistant text from JSONL.
func lastAgentMessage(events []json.RawMessage) (json.RawMessage, string) {
	var lastText string
	for _, ev := range events {
		var obj map[string]any
		if json.Unmarshal(ev, &obj) != nil {
			continue
		}
		if t := findString(obj, "agent_message", "text"); t != "" {
			lastText = t
		}
	}
	if lastText == "" {
		return nil, ""
	}
	raw, err := contracts.ExtractJSON(lastText)
	if err != nil {
		return nil, lastText
	}
	return raw, lastText
}

// findString walks decoded JSON for the first string value stored under any of
// the given keys.
func findString(v any, key string, sub string) string {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == key {
				if m, ok := val.(map[string]any); ok {
					if s, ok := m[sub].(string); ok {
						return s
					}
				}
			}
			if s := findString(val, key, sub); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range t {
			if s := findString(item, key, sub); s != "" {
				return s
			}
		}
	}
	return ""
}
