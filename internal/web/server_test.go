package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Repo, cfg.ArtifactsDir = t.TempDir(), t.TempDir()
	s, err := New(context.Background(), cfg, pipeline.Options{}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func request(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func startTest(t *testing.T, s *Server, body string) string {
	t.Helper()
	w := request(s, "POST", "/api/runs", body)
	if w.Code != 202 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result["id"]
}

func await(t *testing.T, s *Server, id string, predicate func(snapshot) bool) snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := request(s, "GET", "/api/runs/"+id, "")
		var v snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if predicate(v) {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for run %s", id)
	return snapshot{}
}

func answerTest(t *testing.T, s *Server, id string, g *gate, value string, status int) {
	t.Helper()
	w := request(s, "POST", "/api/runs/"+id+"/answer", fmt.Sprintf(`{"gate_id":%d,"value":%q}`, g.ID, value))
	if w.Code != status {
		t.Fatalf("answer: %d %s; want %d", w.Code, w.Body.String(), status)
	}
}

func TestAuthenticationAndOrigin(t *testing.T) {
	s := testServer(t)
	for _, test := range []struct {
		path, token, origin string
		status              int
	}{
		{"/", "", "", 200}, {"/app.js", "", "", 200}, {"/style.css", "", "", 200},
		{"/api/runs", "", "", 401}, {"/api/config", "bad-token", "", 401},
		{"/api/runs", testToken, "http://evil.test", 403},
		{"/api/runs", testToken, "https://example.com", 403},
		{"/api/runs", testToken, "http://example.com", 200},
	} {
		r := httptest.NewRequest("GET", test.path, nil)
		r.Header.Set("Authorization", "Bearer "+test.token)
		r.Header.Set("Origin", test.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Errorf("%+v: got %d", test, w.Code)
		}
		if strings.Contains(w.Body.String(), testToken) {
			t.Error("token leaked into response")
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Error("missing CSP")
		}
	}
}

func TestDecisionsSurviveReconnectAndRejectStaleAnswers(t *testing.T) {
	s := testServer(t)
	s.execute = func(ctx context.Context, p *pipeline.Pipeline) error {
		if _, err := p.Gate.PlanGate(ctx, &contracts.Plan{Summary: "A plan"}, ""); err != nil {
			return err
		}
		_, err := p.Gate.CommitGate(ctx, "main", "")
		return err
	}
	id := startTest(t, s, `{"prompt":"hello"}`)
	v := await(t, s, id, func(v snapshot) bool { return v.Gate != nil })
	if w := request(s, "POST", "/api/runs", `{"prompt":"second"}`); w.Code != 409 {
		t.Fatalf("concurrent start: %d", w.Code)
	}
	answerTest(t, s, id, v.Gate, "commit", 409)
	answerTest(t, s, id, v.Gate, "approve", 200)
	next := await(t, s, id, func(v snapshot) bool { return v.Gate != nil && v.Gate.Kind == "commit" })
	answerTest(t, s, id, v.Gate, "approve", 409)
	answerTest(t, s, id, next.Gate, "stop", 200)
	v = await(t, s, id, func(v snapshot) bool { return !v.Active })
	if v.Error != "" {
		t.Fatal(v.Error)
	}
}

func TestCancelAndShutdownUnblockPendingGates(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			s := testServer(t)
			s.execute = func(ctx context.Context, p *pipeline.Pipeline) error {
				_, err := p.Gate.PlanGate(ctx, &contracts.Plan{}, "")
				return err
			}
			id := startTest(t, s, `{"prompt":"wait"}`)
			await(t, s, id, func(v snapshot) bool { return v.Gate != nil })
			if shutdown {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := s.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
				if w := request(s, "POST", "/api/runs", `{"prompt":"again"}`); w.Code != 503 {
					t.Fatalf("closed start: %d", w.Code)
				}
			} else if w := request(s, "POST", "/api/runs/"+id+"/cancel", "{}"); w.Code != 200 {
				t.Fatalf("cancel: %d", w.Code)
			}
			v := await(t, s, id, func(v snapshot) bool { return !v.Active })
			if v.Gate != nil || v.Error != context.Canceled.Error() {
				t.Fatalf("cancelled snapshot: %+v", v)
			}
		})
	}
}

func TestInputAndHistoryIsolation(t *testing.T) {
	s := testServer(t)
	other, err := artifact.New(s.cfg.ArtifactsDir, t.TempDir(), "other repository")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"prompt":" "}`, `{"prompt":"x","unexpected":true}`, `{"prompt":"x"} {}`, `{"prompt":"` + strings.Repeat("x", 65536) + `"}`} {
		if w := request(s, "POST", "/api/runs", body); w.Code != 400 {
			t.Fatalf("bad request accepted: %d", w.Code)
		}
	}
	if w := request(s, "GET", "/api/runs", ""); strings.Contains(w.Body.String(), other.ID) {
		t.Fatal("other repository visible")
	}
	if w := request(s, "GET", "/api/runs/"+other.ID, ""); w.Code != 404 {
		t.Fatal("other repository readable")
	}
	if w := request(s, "POST", "/api/runs", fmt.Sprintf(`{"run_id":%q,"from":"planner"}`, other.ID)); w.Code != 404 {
		t.Fatal("other repository resumable")
	}
	for _, id := range []string{"../outside", ".", "..", "a/b", `a\b`} {
		if _, err := s.load(id); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("path %q accepted: %v", id, err)
		}
	}
}

func TestFallbackRetryAndReviewFixControls(t *testing.T) {
	s := testServer(t)
	s.execute = func(ctx context.Context, p *pipeline.Pipeline) error {
		pick, err := p.Gate.SelectAgent(ctx, agent.Planner, "claude", []string{"codex"}, "codex", errors.New("agent unavailable"))
		if err != nil {
			return err
		}
		if pick != "codex" {
			return fmt.Errorf("unexpected fallback %q", pick)
		}
		again, err := p.Gate.(*session).RetryGate(ctx, "executor", errors.New("temporary failure"))
		if err != nil {
			return err
		}
		if !again {
			return errors.New("retry was not delivered")
		}
		decision, err := p.Gate.ReviewGate(ctx, &contracts.Review{Verdict: "fail", Summary: "needs a fix"}, "")
		if err != nil {
			return err
		}
		if decision != ui.Fix {
			return fmt.Errorf("unexpected decision %v", decision)
		}
		return nil
	}
	id := startTest(t, s, `{"prompt":"retry test"}`)
	for _, step := range []struct{ kind, answer string }{{"agent", "codex"}, {"retry", "retry"}, {"review", "fix"}} {
		v := await(t, s, id, func(v snapshot) bool { return v.Gate != nil && v.Gate.Kind == step.kind })
		if step.kind == "review" {
			answerTest(t, s, id, v.Gate, "approve", 409)
		}
		answerTest(t, s, id, v.Gate, step.answer, 200)
	}
	v := await(t, s, id, func(v snapshot) bool { return !v.Active })
	if v.Error != "" {
		t.Fatal(v.Error)
	}
}

type fakeAgent struct {
	name    string
	systems *systemLog
}

// systemLog records the base prompt each fake agent received.
type systemLog struct {
	mu   sync.Mutex
	seen map[string]string
}

func (l *systemLog) get(name string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[name]
}

func (f fakeAgent) Name() string     { return f.name }
func (f fakeAgent) Kind() agent.Kind { return agent.Planner }
func (f fakeAgent) Run(_ context.Context, req agent.Request) (*agent.Result, error) {
	if f.systems != nil {
		f.systems.mu.Lock()
		f.systems.seen[f.name] = req.System
		f.systems.mu.Unlock()
	}
	switch f.name {
	case "claude":
		return &agent.Result{Structured: json.RawMessage(`{"summary":"add feature","steps":[{"id":"1","description":"write feature.txt"}],"acceptance_criteria":["feature.txt exists"]}`)}, nil
	case "codex":
		if err := os.WriteFile(filepath.Join(req.Dir, "feature.txt"), []byte("hello from web\n"), 0644); err != nil {
			return nil, err
		}
		return &agent.Result{}, nil
	default:
		return &agent.Result{Structured: json.RawMessage(`{"verdict":"pass","summary":"looks good"}`)}, nil
	}
}

func TestRealPipelineThroughHTTPAndResume(t *testing.T) {
	s := testServer(t)
	repo := s.cfg.Repo
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "test"}, {"config", "user.email", "test@example.com"}, {"commit", "--allow-empty", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	// Preflight probes only these tiny binaries, never the host's real agents.
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex", "opencode"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho test-agent\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	systems := &systemLog{seen: map[string]string{}}
	s.execute = func(ctx context.Context, p *pipeline.Pipeline) error {
		p.AgentFactory = func(name string) (agent.Agent, error) { return fakeAgent{name, systems}, nil }
		return p.Execute(ctx)
	}
	id := startTest(t, s, `{"prompt":"add feature","prompts":{"planner":"custom plan","executor":"","reviewer":"custom review"}}`)
	for _, step := range []struct{ kind, answer string }{{"plan", "approve"}, {"review", "approve"}, {"commit", "stop"}} {
		v := await(t, s, id, func(v snapshot) bool { return v.Gate != nil && v.Gate.Kind == step.kind })
		answerTest(t, s, id, v.Gate, step.answer, 200)
	}
	v := await(t, s, id, func(v snapshot) bool { return !v.Active })
	if v.Error != "" || v.Run == nil || v.Run.State != artifact.StateDone || !strings.Contains(v.Diff, "hello from web") {
		t.Fatalf("pipeline result: %+v", v)
	}
	if v.Plan == "" || v.Review == "" {
		t.Fatal("missing plan/review")
	}
	// Blank prompts fall back to the built-in instructions.
	if systems.get("claude") != "custom plan" || systems.get("codex") != contracts.ExecutorPrompt || systems.get("opencode") != "custom review" {
		t.Fatalf("base prompts not passed to agents: %+v", systems.seen)
	}
	// Saved prompts are shown for the finished run and can be edited on resume.
	detail := request(s, "GET", "/api/runs/"+v.Run.ID, "")
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `"planner":"custom plan"`) {
		t.Fatalf("saved prompts missing from detail: %d %s", detail.Code, detail.Body.String())
	}
	id = startTest(t, s, fmt.Sprintf(`{"run_id":%q,"from":"reviewer","prompts":{"planner":"custom plan","executor":"","reviewer":"edited review"}}`, v.Run.ID))
	for _, step := range []struct{ kind, answer string }{{"review", "approve"}, {"commit", "stop"}} {
		v := await(t, s, id, func(v snapshot) bool { return v.Gate != nil && v.Gate.Kind == step.kind })
		answerTest(t, s, id, v.Gate, step.answer, 200)
	}
	v = await(t, s, id, func(v snapshot) bool { return !v.Active })
	if v.Error != "" {
		t.Fatal(v.Error)
	}
	if systems.get("opencode") != "edited review" {
		t.Fatalf("resume did not use edited reviewer prompt: %q", systems.get("opencode"))
	}
}

func TestWorktreesListLinkedRunAndChanges(t *testing.T) {
	s := testServer(t)
	repo := s.cfg.Repo
	wt := filepath.Join(t.TempDir(), "wt")
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "test"}, {"config", "user.email", "test@example.com"}, {"commit", "--allow-empty", "-m", "initial"}, {"worktree", "add", "-b", "feature", wt}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	run, err := artifact.New(s.cfg.ArtifactsDir, repo, "feature work")
	if err != nil {
		t.Fatal(err)
	}
	run.Worktree = wt
	if err := run.Save(); err != nil {
		t.Fatal(err)
	}

	w := request(s, "GET", "/api/worktrees", "")
	var list []worktreeView
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil {
		t.Fatalf("worktrees: %d %s", w.Code, w.Body.String())
	}
	if len(list) != 2 || !list[0].Main || list[0].RunID != "" {
		t.Fatalf("unexpected main worktree: %+v", list)
	}
	if list[1].Branch != "feature" || list[1].Changes != 1 || list[1].RunID != run.ID {
		t.Fatalf("unexpected linked worktree: %+v", list[1])
	}
}
