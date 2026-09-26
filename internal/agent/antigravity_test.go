package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAntigravityArgsByRole(t *testing.T) {
	plan := antigravityArgs(Request{Model: "m", SchemaInline: "{}", System: "sys", Prompt: "do"}, Planner)
	if !slices.Contains(plan, "plan") || slices.Contains(plan, "--dangerously-skip-permissions") {
		t.Fatalf("planner must be read-only: %v", plan)
	}
	if last := plan[len(plan)-1]; last != "--print=sys\n\ndo"+readOnlyNote {
		t.Fatalf("prompt = %q", last)
	}
	if last := executorPrompt(t); strings.Contains(last, "read-only") {
		t.Fatalf("executor must not be told it is read-only: %q", last)
	}

	exec := antigravityArgs(Request{Model: "m", SchemaFile: "/s.json", Sandbox: "workspace-write"}, Executor)
	for _, want := range []string{"--dangerously-skip-permissions", "--sandbox", "/s.json"} {
		if !slices.Contains(exec, want) {
			t.Fatalf("executor args missing %q: %v", want, exec)
		}
	}
	if full := antigravityArgs(Request{SchemaFile: "/s.json", Bypass: true}, Executor); slices.Contains(full, "--sandbox") {
		t.Fatalf("bypass must drop --sandbox: %v", full)
	}
	if antigravityRole(Request{}) != Reviewer {
		t.Fatal("request without schema should be a review")
	}
	// An explicit role wins: reviews also carry an inline schema.
	if antigravityRole(Request{Role: Reviewer, SchemaInline: "{}"}) != Reviewer {
		t.Fatal("explicit reviewer role must not be inferred as planner")
	}
}

func executorPrompt(t *testing.T) string {
	t.Helper()
	args := antigravityArgs(Request{Prompt: "do", SchemaFile: "/s.json"}, Executor)
	return args[len(args)-1]
}

func TestAntigravityExplainsDeniedActions(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"status\":\"SUCCESS\",\"response\":\"\",\"denied_actions\":[{\"action\":\"command\",\"display_name\":\"RunCommand\"}]}'\n"
	if err := os.WriteFile(filepath.Join(dir, AntigravityBin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := (Antigravity{}).Run(context.Background(), Request{Role: Planner, SchemaInline: "{}", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "blocked RunCommand") {
		t.Fatalf("want a denied-action error, got %v", err)
	}
}

func TestAntigravityParsesEnvelope(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"conversation_id\":\"c\",\"status\":\"SUCCESS\",\"response\":\"done {\\\"verdict\\\":\\\"approve\\\"}\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"thinking_tokens\":3}}'\n"
	if err := os.WriteFile(filepath.Join(dir, AntigravityBin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	res, err := (Antigravity{}).Run(context.Background(), Request{Model: "m", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Structured), "approve") {
		t.Fatalf("structured = %s", res.Structured)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if !Installed("antigravity") || !slices.Contains(Available(), "antigravity") {
		t.Fatal("agy on PATH should be available")
	}
	t.Setenv("PATH", t.TempDir())
	if slices.Contains(Available(), "antigravity") {
		t.Fatal("antigravity offered without agy")
	}
}

// fakeAgy installs an agy that is blocked until it has been resumed
// blockedRuns times, logging each invocation's arguments to calls.
func fakeAgy(t *testing.T, blockedRuns int) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s ' "$@" | tr '\n' ' ' >> ` + calls + `
echo >> ` + calls + `
n=$(wc -l < ` + calls + `)
if [ "$n" -le ` + strconv.Itoa(blockedRuns) + ` ]; then
  echo '{"status":"SUCCESS","conversation_id":"conv-1","response":"","denied_actions":[{"display_name":"RunCommand"}],"usage":{"input_tokens":4}}'
else
  echo '{"status":"SUCCESS","conversation_id":"conv-1","response":"","structured_output":{"summary":"ok"},"usage":{"input_tokens":6}}'
fi
`
	if err := os.WriteFile(filepath.Join(dir, AntigravityBin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestAntigravityResumesAfterBlockedCommand(t *testing.T) {
	calls := fakeAgy(t, 1)
	res, err := (Antigravity{}).Run(context.Background(), Request{Role: Planner, SchemaInline: "{}", Prompt: "plan it", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Structured) != `{"summary":"ok"}` || res.Usage.InputTokens != 10 {
		t.Fatalf("result = %s usage %+v", res.Structured, res.Usage)
	}
	data, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "--conversation") {
		t.Fatalf("calls = %q", lines)
	}
	for _, want := range []string{"--conversation conv-1", "--mode plan", "--json-schema {}", "RunCommand"} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("resume call missing %q: %s", want, lines[1])
		}
	}
	if strings.Contains(lines[1], "--dangerously-skip-permissions") {
		t.Fatal("resuming must stay read-only")
	}
}

func TestAntigravityGivesUpAfterRepeatedBlocks(t *testing.T) {
	calls := fakeAgy(t, 10)
	_, err := (Antigravity{}).Run(context.Background(), Request{Role: Reviewer, SchemaInline: "{}", Prompt: "review"})
	if err == nil || !strings.Contains(err.Error(), "blocked RunCommand") {
		t.Fatalf("want a blocked error, got %v", err)
	}
	data, _ := os.ReadFile(calls)
	if n := strings.Count(string(data), "\n"); n != 1+maxBlockedResumes {
		t.Fatalf("agy ran %d times, want %d", n, 1+maxBlockedResumes)
	}
}
