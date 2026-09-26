package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/contracts"
)

// OpenCode adapts the `opencode run` CLI. It is used as the reviewer.
type OpenCode struct{}

func (OpenCode) Name() string { return "opencode" }
func (OpenCode) Kind() Kind   { return Reviewer }

func (c OpenCode) Run(ctx context.Context, r Request) (*Result, error) {
	schema := r.SchemaInline
	if schema == "" && r.SchemaFile != "" {
		if data, err := os.ReadFile(r.SchemaFile); err == nil {
			schema = string(data)
		}
	}
	prompt := r.promptWithSystem()
	if schema != "" {
		prompt += "\n\nReturn ONE JSON object validating against this JSON Schema:\n```json\n" + schema + "\n```"
	}

	args := []string{"run", "-m", r.Model, "--format", "json"}
	if r.Dir != "" {
		args = append(args, "--dir", r.Dir)
	}
	if r.Agent != "" {
		args = append(args, "--agent", r.Agent)
	}
	if r.Variant != "" {
		args = append(args, "--variant", r.Variant)
	}
	args = append(args, r.ExtraArgs...)
	args = append(args, prompt)

	r.Observe.Status(Reviewer, "opencode review started ("+r.Model+")")
	proc := Exec(ctx, ProcSpec{
		Bin:     "opencode",
		Args:    args,
		Dir:     r.Dir,
		Env:     r.Env,
		Timeout: r.Timeout,
		OnLine:  lineObserver(r.Observe, Reviewer),
	})

	res := &Result{
		ExitCode: proc.ExitCode,
		Stderr:   proc.Stderr,
		Duration: proc.Duration,
	}
	var text strings.Builder
	for _, line := range strings.Split(proc.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		res.Events = append(res.Events, json.RawMessage(line))
		var ev struct {
			Type string `json:"type"`
			Part struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Reason string `json:"reason"`
				Tokens struct {
					Input  int `json:"input"`
					Output int `json:"output"`
				} `json:"tokens"`
				Cost float64 `json:"cost"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "text":
			if ev.Part.Text != "" {
				text.WriteString(ev.Part.Text)
				text.WriteByte('\n')
			}
		case "step_finish":
			res.Usage.InputTokens += ev.Part.Tokens.Input
			res.Usage.OutputTokens += ev.Part.Tokens.Output
			res.Usage.CostUSD += ev.Part.Cost
		}
	}
	if proc.Err != nil && proc.ExitCode != 0 {
		return res, fmt.Errorf("opencode exited %d: %s", proc.ExitCode, firstLine(proc.Stderr))
	}

	res.Final = text.String()
	raw, err := contracts.PickJSONObject(res.Final, schema)
	if err != nil {
		// Last resort: try the raw stdout (some versions print plain text).
		// JSONL event output must not be scanned: its first event object
		// would be mistaken for the verdict.
		if len(res.Events) > 0 {
			return res, fmt.Errorf("opencode: no JSON verdict in output: %w", err)
		}
		if raw2, err2 := contracts.PickJSONObject(proc.Stdout, schema); err2 == nil {
			res.Structured = raw2
			return res, nil
		}
		return res, fmt.Errorf("opencode: no JSON verdict in output: %w", err)
	}
	res.Structured = raw
	return res, nil
}
