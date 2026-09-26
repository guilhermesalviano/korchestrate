package models

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilhermesalviano/korchestrate/internal/config"
)

func TestParseCodexCache(t *testing.T) {
	data := []byte(`{
  "models": [
    {"slug": "gpt-6-sol", "default_reasoning_level": "medium",
     "supported_reasoning_levels": [{"effort": "low"}, {"effort": "medium"}, {"effort": "high"}]},
    {"slug": "gpt-6-luna", "default_reasoning_level": "low",
     "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}]}
  ]
}`)
	info, err := ParseCodexCache(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Models) != 2 {
		t.Fatalf("want 2 models, got %d", len(info.Models))
	}
	m := info.Models[0]
	if m.ID != "gpt-6-sol" || m.DefaultEffort != "medium" {
		t.Fatalf("unexpected first model: %+v", m)
	}
	if strings.Join(m.Efforts, ",") != "low,medium,high" {
		t.Fatalf("unexpected efforts: %v", m.Efforts)
	}
}

func TestDiscoverCodexFallback(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	info := DiscoverCodex()
	if info.Agent != "codex" || len(info.Models) == 0 {
		t.Fatalf("fallback catalog missing: %+v", info)
	}
	if info.Models[0].ID != "gpt-6-sol" || len(info.Models[0].Efforts) == 0 {
		t.Fatalf("fallback model incomplete: %+v", info.Models[0])
	}
}

func TestDiscoverCodexFromCache(t *testing.T) {
	home := t.TempDir()
	data := `{"models": [{"slug": "my-model", "default_reasoning_level": "high",
	  "supported_reasoning_levels": [{"effort": "high"}]}]}`
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	info := DiscoverCodex()
	if len(info.Models) != 1 || info.Models[0].ID != "my-model" {
		t.Fatalf("cache not used: %+v", info.Models)
	}
	if info.Models[0].DefaultEffort != "high" {
		t.Fatalf("default effort not parsed: %+v", info.Models[0])
	}
}

func TestParseOpencodeVerbose(t *testing.T) {
	out := "\x1b[1mopencode\x1b[0m models\n" +
		"opencode-go/deepseek-v4-flash\n" +
		"{\n" +
		"  \"id\": \"deepseek-v4-flash\",\n" +
		"  \"providerID\": \"opencode-go\",\n" +
		"  \"variants\": {}\n" +
		"}\n" +
		"zai/glm-5.2\n" +
		"{\n" +
		"  \"id\": \"glm-5.2\",\n" +
		"  \"providerID\": \"zai\",\n" +
		"  \"variants\": {\n" +
		"    \"max\": {\"reasoningEffort\": \"max\"},\n" +
		"    \"high\": {\"reasoningEffort\": \"high\"}\n" +
		"  }\n" +
		"}\n" +
		"some trailing note\n"
	info, err := ParseOpencodeVerbose([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Models) != 2 {
		t.Fatalf("want 2 models, got %d: %+v", len(info.Models), info.Models)
	}
	if info.Models[0].ID != "opencode-go/deepseek-v4-flash" || len(info.Models[0].Efforts) != 0 {
		t.Fatalf("unexpected first model: %+v", info.Models[0])
	}
	if info.Models[1].ID != "zai/glm-5.2" {
		t.Fatalf("unexpected second model: %+v", info.Models[1])
	}
	if strings.Join(info.Models[1].Efforts, ",") != "high,max" {
		t.Fatalf("variants not sorted: %v", info.Models[1].Efforts)
	}
}

func TestCatalogLookup(t *testing.T) {
	c := &Catalog{Agents: []AgentInfo{
		{Agent: "codex", Models: []ModelInfo{{ID: "gpt-6-sol", Efforts: []string{"low", "high"}}}},
	}}
	if c.Find("codex", "gpt-6-sol") == nil {
		t.Fatal("expected to find gpt-6-sol")
	}
	if c.Find("codex", "nope") != nil || c.Find("claude", "opus") != nil {
		t.Fatal("lookup should miss")
	}
	if eff := c.Efforts("codex", "gpt-6-sol"); len(eff) != 2 {
		t.Fatalf("unexpected efforts: %v", eff)
	}
	if eff := c.Efforts("codex", "nope"); eff != nil {
		t.Fatalf("expected nil efforts, got %v", eff)
	}
	var nilCatalog *Catalog
	if nilCatalog.Agent("codex") != nil || nilCatalog.Find("codex", "x") != nil {
		t.Fatal("nil catalog must be safe")
	}
}

func TestDiscoverIncludesAllProviders(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	c := Discover()
	for _, name := range ProviderOrder() {
		if c.Agent(name) == nil {
			t.Errorf("provider %q missing from catalog", name)
		}
	}
	if c.DefaultModel("codex") == "" || c.DefaultModel("claude") == "" || c.DefaultModel("opencode") == "" {
		t.Errorf("default models must resolve: %+v", c.Agents)
	}
}

func TestChoicesApply(t *testing.T) {
	cfg := config.Default()
	cfg.Models.Executor.Sandbox = "workspace-write"
	cfg.Models.Reviewer.SubAgent = "plan"
	cfg.Loop.MaxIterations = 3

	out := Choices{
		Planner:  Choice{Agent: "codex", Model: "gpt-6-astra", Variant: "high"},
		Executor: Choice{Agent: "codex", Model: "gpt-6-luna", Variant: "max"},
		Reviewer: Choice{Agent: "zai", Model: "zai/glm-5.2", Variant: "highspeed"},
	}.Apply(cfg)

	if out == cfg {
		t.Fatal("Apply must return a copy")
	}
	if out.Models.Planner.Agent != "codex" || out.Models.Planner.Model != "gpt-6-astra" || out.Models.Planner.Variant != "high" {
		t.Fatalf("planner not applied: %+v", out.Models.Planner)
	}
	if out.Models.Executor.Model != "gpt-6-luna" || out.Models.Executor.Variant != "max" {
		t.Fatalf("executor not applied: %+v", out.Models.Executor)
	}
	if out.Models.Executor.Sandbox != "workspace-write" || !out.Models.Executor.ApproveForMe {
		t.Fatalf("executor knobs the picker does not own must be kept: %+v", out.Models.Executor)
	}
	if out.Models.Reviewer.SubAgent != "plan" {
		t.Fatalf("reviewer subagent must be kept: %+v", out.Models.Reviewer)
	}
	if out.Loop.MaxIterations != 3 || out.Repo != cfg.Repo {
		t.Fatal("unrelated config must be carried over")
	}
	// The source config is untouched.
	if cfg.Models.Executor.Model == "gpt-6-luna" {
		t.Fatal("Apply must not mutate the source config")
	}
}

func TestChoicesApplyEmptyKeepsConfig(t *testing.T) {
	cfg := config.Default()
	out := Choices{}.Apply(cfg)
	if out.Models.Planner.Model != cfg.Models.Planner.Model ||
		out.Models.Executor.Model != cfg.Models.Executor.Model ||
		out.Models.Reviewer.Model != cfg.Models.Reviewer.Model {
		t.Fatalf("empty choices must keep the config models: %+v", out.Models)
	}
}
