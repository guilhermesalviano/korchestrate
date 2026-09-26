package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/worktree"
)

const prTimeout = 2 * time.Minute

// openPR opens a pull request for the pushed branch with the GitHub CLI and
// returns its URL. An open PR for the branch (a retried run pushing again) is
// reused. Tests replace it so no real gh runs.
var openPR = func(ctx context.Context, dir, branch, title, body string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, prTimeout)
	defer cancel()
	if url, err := gh(ctx, dir, "pr", "view", branch, "--json", "url,state", "--jq", `select(.state == "OPEN") | .url`); err == nil && url != "" {
		return url, nil
	}
	out, err := gh(ctx, dir, "pr", "create", "--head", branch, "--title", title, "--body", body)
	if err != nil {
		return "", err
	}
	// gh prints progress before the URL; the URL is the last line.
	lines := strings.Split(out, "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func gh(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := firstLine(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", args[0]+" "+args[1], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// prTitle is the commit message's subject line.
func prTitle(message string) string {
	title, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return strings.TrimSpace(title)
}

// prBody describes the change from the run's request, plan and review.
func prBody(run *artifact.Run, plan *contracts.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Request\n%s\n", strings.TrimSpace(run.Prompt))
	if plan != nil && strings.TrimSpace(plan.Summary) != "" {
		fmt.Fprintf(&b, "\n## Plan\n%s\n", strings.TrimSpace(plan.Summary))
		for _, c := range plan.AcceptanceCriteria {
			fmt.Fprintf(&b, "- [x] %s\n", c)
		}
	}
	if data, err := run.Read("review.json"); err == nil {
		var review contracts.Review
		if contracts.DecodeObject(data, &review) == nil && strings.TrimSpace(review.Summary) != "" {
			fmt.Fprintf(&b, "\n## Review\n%s\n", strings.TrimSpace(review.Summary))
		}
	}
	fmt.Fprintf(&b, "\n---\nOpened by kor autopilot · run `%s`\n", run.ID)
	return b.String()
}

// EndMessage is the closing summary of an autopilot run: what was committed,
// whether it reached origin and the pull request, or why it stopped.
func EndMessage(run *artifact.Run) string {
	if run == nil {
		return "autopilot stopped before the run started"
	}
	switch run.State {
	case artifact.StateDone:
	case artifact.StateFailed, artifact.StateAborted:
		msg := "autopilot stopped: " + string(run.State)
		if run.Error != "" {
			msg += " — " + firstLine(run.Error)
		}
		return msg + "; press t to retry"
	default:
		return "autopilot interrupted while " + strings.ReplaceAll(string(run.State), "_", " ")
	}
	if run.Commit == "" {
		return "autopilot finished: review passed, but there was nothing to commit on " + run.Branch
	}
	parts := []string{"committed " + shortSHA(run.Commit) + " on " + run.Branch}
	switch {
	case !run.Pushed:
		parts = append(parts, "push failed (press p to retry)")
	case run.PR != "":
		parts = append(parts, "pushed", "PR "+run.PR)
	case !worktree.SeparateBranch(run.Worktree, run.Branch):
		parts = append(parts, "pushed", "no PR: "+run.Branch+" is the default branch")
	default:
		parts = append(parts, "pushed", "no PR opened")
	}
	return "autopilot finished: " + strings.Join(parts, " · ")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
