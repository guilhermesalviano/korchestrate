package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanFiles(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(valid, []byte(`{"summary":"s","steps":[{"id":"1","description":"d"}],"acceptance_criteria":["c"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, prompt, err := planFiles("implement the plan", []string{valid})
	if err != nil {
		t.Fatalf("load valid plan: %v", err)
	}
	if plan.Summary != "s" || len(plan.Steps) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if prompt != "implement the plan" {
		t.Fatalf("unexpected prompt: %q", prompt)
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"summary":"s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planFiles("implement the plan", []string{invalid}); err == nil {
		t.Fatal("expected invalid plan error (missing steps/criteria)")
	}
	if _, _, err := planFiles("implement the plan", []string{filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("expected missing file error")
	}
}
