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

// Antigravity adapts the optional `agy --print` CLI. It can fill any role;
// planning and review run read-only in plan mode.
type Antigravity struct{}

func (Antigravity) Name() string { return "antigravity" }
func (Antigravity) Kind() Kind   { return Executor }

// maxBlockedResumes bounds how often a read-only session that stopped at a
// denied command is continued.
const maxBlockedResumes = 2

func (c Antigravity) Run(ctx context.Context, r Request) (*Result, error) {
	kind := antigravityRole(r)
	args := antigravityArgs(r, kind)
	r.Observe.Status(kind, "antigravity "+string(kind)+" started ("+r.Model+")")

	total := &Result{}
	for resumes := 0; ; resumes++ {
		res, out, err := c.runOnce(ctx, r, kind, args)
		total.ExitCode, total.Stderr, total.Final, total.Structured = res.ExitCode, res.Stderr, res.Final, res.Structured
		total.Events = append(total.Events, res.Events...)
		total.Duration += res.Duration
		total.Usage.InputTokens += res.Usage.InputTokens
		total.Usage.OutputTokens += res.Usage.OutputTokens
		if err != nil {
			return total, err
		}
		blocked := out.blocked()
		if blocked == "" {
			return total, nil
		}
		// A denied command ends the print-mode turn, discarding the work so
		// far. Continuing the conversation keeps what was already read.
		if out.ConversationID == "" || resumes == maxBlockedResumes {
			return total, fmt.Errorf("antigravity stopped without a result after read-only mode blocked %s", blocked)
		}
		r.Observe.Status(kind, "antigravity: read-only mode blocked "+blocked+"; continuing without it")
		args = antigravityResumeArgs(r, kind, out.ConversationID, blocked)
	}
}

// agyOutput is the `agy --output-format json` envelope.
type agyOutput struct {
	Status           string          `json:"status"`
	ConversationID   string          `json:"conversation_id"`
	Response         string          `json:"response"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	DeniedActions    []struct {
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
	Usage struct {
		InputTokens     int `json:"input_tokens"`
		OutputTokens    int `json:"output_tokens"`
		ThinkingTokens  int `json:"thinking_tokens"`
		CacheReadTokens int `json:"cache_read_tokens"`
	} `json:"usage"`
}

// blocked names the denied actions when the session ended on one with an
// empty response, or returns "" otherwise.
func (o *agyOutput) blocked() string {
	if o == nil || len(o.DeniedActions) == 0 || strings.TrimSpace(o.Response) != "" {
		return ""
	}
	names := make([]string, len(o.DeniedActions))
	for i, a := range o.DeniedActions {
		names[i] = a.DisplayName
	}
	return strings.Join(names, ", ")
}

// runOnce invokes agy and parses its envelope; out is nil when stdout was not
// an envelope but still carried JSON.
func (c Antigravity) runOnce(ctx context.Context, r Request, kind Kind, args []string) (*Result, *agyOutput, error) {
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
		return res, nil, fmt.Errorf("antigravity exited %d: %s", proc.ExitCode, firstLine(proc.Stderr))
	}

	var out agyOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		raw, exErr := contracts.ExtractJSON(proc.Stdout)
		if exErr != nil {
			return res, nil, fmt.Errorf("antigravity: cannot parse output: %w", err)
		}
		res.Final = proc.Stdout
		res.Structured = raw
		return res, nil, nil
	}

	res.Final = out.Response
	res.Usage = Usage{
		InputTokens:  out.Usage.InputTokens + out.Usage.CacheReadTokens,
		OutputTokens: out.Usage.OutputTokens + out.Usage.ThinkingTokens,
	}
	if out.Status != "" && !strings.EqualFold(out.Status, "SUCCESS") {
		return res, &out, fmt.Errorf("antigravity finished with status %s: %s", out.Status, firstLine(out.Response))
	}
	if len(out.StructuredOutput) > 0 && string(out.StructuredOutput) != "null" {
		res.Structured = out.StructuredOutput
	} else if raw, err := contracts.ExtractJSON(out.Response); err == nil {
		res.Structured = raw
	}
	return res, &out, nil
}

// antigravityResumeArgs continues a conversation that stopped at a denied
// action, with the same model, mode and schema.
func antigravityResumeArgs(r Request, kind Kind, conversation, blocked string) []string {
	r.System = ""
	r.Prompt = "Your last step tried " + blocked + ", which this session does not allow, so it was denied. " +
		"Do not try it again. Continue from what you have already read and return your result now."
	args := antigravityArgs(r, kind)
	last := len(args) - 1
	return append(append(args[:last:last], "--conversation", conversation), args[last])
}

// readOnlyNote steers read-only sessions to agy's file tools: plan mode denies
// shell commands, and auto-approving them would also allow file edits.
const readOnlyNote = "\n\nThis session is read-only: shell and terminal commands are unavailable and will be denied. " +
	"Inspect the repository only with your file viewing, directory listing and search tools, then return your result."

// antigravityRole uses the requested role, else infers it from the request
// shape.
func antigravityRole(r Request) Kind {
	switch {
	case r.Role != "":
		return r.Role
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

	prompt := r.promptWithSystem()
	if kind != Executor {
		prompt += readOnlyNote
	}
	// Attach the prompt to the flag so a leading "-" is never read as a flag.
	return append(args, "--print="+prompt)
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
