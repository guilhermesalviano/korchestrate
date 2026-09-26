package contracts

import (
	"encoding/json"
	"testing"
)

func TestExecReportSchemaRequiresEveryProperty(t *testing.T) {
	var schema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal([]byte(ExecReportSchema), &schema); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]bool)
	for _, name := range schema.Required {
		required[name] = true
	}
	for name := range schema.Properties {
		if !required[name] {
			t.Errorf("Codex strict output requires property %q in required", name)
		}
	}
	if schema.AdditionalProperties {
		t.Error("Codex strict output must forbid additional properties")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", `{"a":1}`, `{"a":1}`, true},
		{"prose", "here you go:\n{\"a\":1}\ndone", `{"a":1}`, true},
		{"fenced", "```json\n{\"a\":{\"b\":2}}\n```", `{"a":{"b":2}}`, true},
		{"braces in string", `{"s":"}{|","n":1}`, `{"s":"}{|","n":1}`, true},
		{"escaped quote", `{"s":"a\"}b","n":1}`, `{"s":"a\"}b","n":1}`, true},
		{"none", "no json here", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractJSON(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("expected error, got %s", got)
				}
				return
			}
			var a, b any
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatalf("invalid json returned: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &b); err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(a, b) {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func TestPickJSONObjectPrefersSchemaMatch(t *testing.T) {
	text := "I inspected package.json:\n{\"name\":\"demo\",\"version\":\"1\"}\n\n" +
		"Final review:\n{\"verdict\":\"pass\",\"summary\":\"ok\",\"acceptance\":[{\"criterion\":\"c\",\"met\":true}]}\n"
	got, err := PickJSONObject(text, ReviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	var r Review
	if err := json.Unmarshal(got, &r); err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "pass" || r.Summary != "ok" {
		t.Fatalf("picked the wrong object: %s", got)
	}
}

func TestPickJSONObjectFallsBackToLast(t *testing.T) {
	got, err := PickJSONObject("first {\"a\":1} then {\"b\":2}", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"b":2}` {
		t.Fatalf("got %s, want the last object", got)
	}
}

func TestPickJSONObjectNoJSON(t *testing.T) {
	if _, err := PickJSONObject("no json here", ReviewSchema); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExtractJSONObjectsNested(t *testing.T) {
	objs := ExtractJSONObjects(`{"verdict":"pass","summary":"ok","issues":[{"description":"d"}]}`)
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want the review and its nested issue: %v", len(objs), objs)
	}
}

func TestPlanValidate(t *testing.T) {
	if err := (&Plan{}).Validate(); err == nil {
		t.Fatal("empty plan should be invalid")
	}
	p := &Plan{Summary: "s", Steps: []PlanStep{{ID: "1", Description: "d"}}, AcceptanceCriteria: []string{"c"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestReviewValidateAndPass(t *testing.T) {
	r := &Review{Verdict: "maybe", Summary: "x"}
	if err := r.Validate(); err == nil {
		t.Fatal("bad verdict should fail")
	}
	r = &Review{Verdict: "pass", Summary: "ok"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if !r.Pass() {
		t.Fatal("expected pass")
	}
	r = &Review{Verdict: "FAIL", Summary: "no"}
	if r.Pass() {
		t.Fatal("expected not pass")
	}
}
