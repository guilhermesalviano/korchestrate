package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilhermesalviano/korchestrate/internal/contracts"
)

func TestPromptsResolvedKeepsBuiltInsForBlankFields(t *testing.T) {
	p := Prompts{Planner: "custom", Executor: "  "}.Resolved()
	if p.Planner != "custom" || p.Executor != contracts.ExecutorPrompt || p.Reviewer != contracts.ReviewerPrompt {
		t.Fatalf("unexpected resolved prompts: %+v", p)
	}
}

func TestPromptsValidateRejectsOversizedPrompt(t *testing.T) {
	p := Prompts{Reviewer: strings.Repeat("x", MaxBasePromptBytes+1)}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("want reviewer size error, got %v", err)
	}
}

func TestLoadPromptOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kor.yaml")
	if err := os.WriteFile(path, []byte("repo: /tmp/x\nprompts:\n  planner: my planner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Prompts.Resolved()
	if p.Planner != "my planner" || p.Executor != contracts.ExecutorPrompt {
		t.Fatalf("unexpected prompts: %+v", p)
	}
}
