package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/guilhermesalviano/korchestrate/internal/contracts"
)

// AntigravityBin is the Antigravity CLI binary.
const AntigravityBin = "agy"

// Antigravity adapts the optional `agy --print` CLI. It can fill any role; the
// role is inferred from the request (planner: inline schema, executor: schema
// file, reviewer: neither).
type Antigravity struct{}

func (Antigravity) Name() string { return "antigravity" }
func (Antigravity) Kind() Kind   { return Executor }

func (c Antigravity) Run(ctx context.Context, r Request) (*Result, error) {
	kind := antigravityRole(r)
	args := antigravityArgs(r, kind)

	r.Observe.Status(kind, "antigravity "+string(kind)+" started ("+r.Model+")")
	proc := Exec(ctx, ProcSpec{
		Bin:     AntigravityBin,
		Args:    args,
		Dir:     r.Dir,
		Env:     r.Env,
		Timeout: r.Timeout,
		OnLine:  lineObserver(r.Observe, kind),
	})

	res := &Result{
		ExitCode: proc.ExitCode,
		Stderr:   proc.Stderr,
		Duration: proc.Duration,
	}
	stdout := strings.TrimSpace(proc.Stdout)
	if stdout != "" && json.Valid([]byte(stdout)) {
		res.Events = append(res.Events, json.RawMessage(stdout))
	}
	if proc.Err != nil && proc.ExitCode != 0 {
		return res, fmt.Errorf("antigravity exited %d: %s", proc.ExitCode, firstLine(proc.Stderr))
	}

	var out struct {
		Status           string          `json:"status"`
		Response         string          `json:"response"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            struct {
			InputTokens     int `json:"input_tokens"`
			OutputTokens    int `json:"output_tokens"`
			ThinkingTokens  int `json:"thinking_tokens"`
			CacheReadTokens int `json:"cache_read_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		raw, exErr := contracts.ExtractJSON(proc.Stdout)
		if exErr != nil {
			return res, fmt.Errorf("antigravity: cannot parse output: %w", err)
		}
		res.Final = proc.Stdout
		res.Structured = raw
		return res, nil
	}

	res.Final = out.Response
	res.Usage = Usage{
		InputTokens:  out.Usage.InputTokens + out.Usage.CacheReadTokens,
		OutputTokens: out.Usage.OutputTokens + out.Usage.ThinkingTokens,
	}
	if out.Status != "" && !strings.EqualFold(out.Status, "SUCCESS") {
		return res, fmt.Errorf("antigravity finished with status %s: %s", out.Status, firstLine(out.Response))
	}
	if len(out.StructuredOutput) > 0 && string(out.StructuredOutput) != "null" {
		res.Structured = out.StructuredOutput
		return res, nil
	}
	if raw, err := contracts.ExtractJSON(out.Response); err == nil {
		res.Structured = raw
	}
	return res, nil
}

// antigravityRole infers the pipeline role from the request shape.
func antigravityRole(r Request) Kind {
	switch {
	case r.SchemaInline != "":
		return Planner
	case r.SchemaFile != "":
		return Executor
	default:
		return Reviewer
	}
}

// antigravityArgs assembles the `agy` arguments. The CLI has no system-prompt
// flag, so the system prompt is prepended to the user prompt.
func antigravityArgs(r Request, kind Kind) []string {
	args := []string{"--output-format", "json"}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Variant != "" {
		args = append(args, "--effort", r.Variant)
	}
	switch {
	case r.SchemaInline != "":
		args = append(args, "--json-schema", r.SchemaInline)
	case r.SchemaFile != "":
		args = append(args, "--json-schema", r.SchemaFile)
	}
	if kind == Executor {
		// Print mode cannot prompt for approvals, so the executor must
		// auto-approve; keep terminal restrictions unless full access is asked.
		args = append(args, "--dangerously-skip-permissions")
		if !r.Bypass && r.Sandbox != "danger-full-access" {
			args = append(args, "--sandbox")
		}
	} else {
		// Planning and review are strictly read-only.
		args = append(args, "--mode", "plan")
	}
	args = append(args, r.ExtraArgs...)

	// Attach the prompt to the flag so a leading "-" is never read as a flag.
	return append(args, "--print="+r.promptWithSystem())
}

// Installed reports whether an adapter's CLI can be invoked. The core
// adapters are always offered (a missing CLI surfaces when the stage runs);
// optional adapters such as antigravity are offered only when present.
func Installed(name string) bool {
	switch name {
	case "antigravity":
		_, err := exec.LookPath(AntigravityBin)
		return err == nil
	default:
		return true
	}
}

// Available lists the known adapters whose CLI is installed.
func Available() []string {
	var out []string
	for _, n := range Known() {
		if Installed(n) {
			out = append(out, n)
		}
	}
	return out
}
