package contracts

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanMarkdownRoundTrip(t *testing.T) {
	plan := &Plan{
		Summary:            "Add rate limiting to the API",
		Assumptions:        []string{"Redis is available"},
		Files:              []string{"api/limit.go"},
		Steps:              []PlanStep{{ID: "1", Description: "Add a limiter"}, {ID: "2", Description: "Wire it into the router"}},
		AcceptanceCriteria: []string{"returns 429 after 10 requests"},
		OutOfScope:         []string{"per-user quotas"},
	}
	md := plan.Markdown()
	got, err := PlanFromMarkdown("plan.md", []byte(md))
	if err != nil {
		t.Fatalf("parse rendered plan: %v\n%s", err, md)
	}
	got.Steps[0].Files, got.Steps[1].Files = nil, nil
	if !reflect.DeepEqual(got, plan) {
		t.Fatalf("round trip changed the plan:\ngot  %+v\nwant %+v\n%s", got, plan, md)
	}
}

func TestPlanMarkdownKeepsStepDetails(t *testing.T) {
	plan := &Plan{Summary: "s", Steps: []PlanStep{{ID: "1", Description: "Edit handler", Files: []string{"a.go", "b.go"}, Verification: "go test ./..."}}, AcceptanceCriteria: []string{"c"}}
	md := plan.Markdown()
	for _, want := range []string{"1. Edit handler", "Files: `a.go`, `b.go`", "Check: go test ./..."} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in:\n%s", want, md)
		}
	}
	got, err := PlanFromMarkdown("plan.md", []byte(md))
	if err != nil || len(got.Steps) != 1 {
		t.Fatalf("details must stay within their step: %v %+v", err, got)
	}
}
