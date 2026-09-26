package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAntigravityArgsByRole(t *testing.T) {
	plan := antigravityArgs(Request{Model: "m", SchemaInline: "{}", System: "sys", Prompt: "do"}, Planner)
	if !slices.Contains(plan, "plan") || slices.Contains(plan, "--dangerously-skip-permissions") {
		t.Fatalf("planner must be read-only: %v", plan)
	}
	if last := plan[len(plan)-1]; last != "--print=sys\n\ndo" {
		t.Fatalf("prompt = %q", last)
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
