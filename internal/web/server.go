// Package web serves a phone-friendly dashboard using the same pipeline as the TUI.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/agent"
	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	cfg      *config.Config
	token    string
	opts     pipeline.Options
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	sessions map[string]*session
	wg       sync.WaitGroup
	handler  http.Handler
	// These hooks use the command's existing plan/config resolution.
	PlanFromPrompt func(string) (*contracts.Plan, string, error)
	LoadRunConfig  func(*artifact.Run) (*config.Config, error)
	execute        func(context.Context, *pipeline.Pipeline) error
}

func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func New(ctx context.Context, cfg *config.Config, opts pipeline.Options, token string) (*Server, error) {
	if len(token) < 24 {
		return nil, fmt.Errorf("web access token must have at least 24 characters")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{cfg: cfg, token: token, opts: opts, ctx: ctx, cancel: cancel, sessions: make(map[string]*session), execute: func(ctx context.Context, p *pipeline.Pipeline) error { return p.Execute(ctx) }}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("GET /api/runs", s.list)
	mux.HandleFunc("POST /api/runs", s.start)
	mux.HandleFunc("GET /api/runs/{id}", s.detail)
	mux.HandleFunc("POST /api/runs/{id}/answer", s.answer)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.stop)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, http.StatusNotFound, "endpoint not found") })
	static, _ := fs.Sub(assets, "static")
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		files.ServeHTTP(w, r)
	})
	s.handler = s.secure(mux)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Shutdown cancels agents and gates; callers can bound how long cleanup takes.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
				fail(w, http.StatusUnauthorized, "Open the access link printed by kor web, or enter its token.")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if err != nil || u.Host != r.Host || u.Scheme != scheme {
					fail(w, http.StatusForbidden, "cross-origin request denied")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	send(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		fail(w, 415, "expected application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		fail(w, 400, "invalid request: "+err.Error())
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, 400, "expected one JSON object")
		return false
	}
	return true
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	send(w, 200, map[string]any{"repo": s.cfg.Repo, "models": map[string]string{
		"planner":  s.cfg.Models.Planner.Agent + " / " + s.cfg.Models.Planner.Model,
		"executor": s.cfg.Models.Executor.Agent + " / " + s.cfg.Models.Executor.Model,
		"reviewer": s.cfg.Models.Reviewer.Agent + " / " + s.cfg.Models.Reviewer.Model,
	}})
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	runs, err := artifact.List(s.cfg.ArtifactsDir)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	views := []snapshot{}
	seen := map[string]bool{}
	s.mu.Lock()
	for _, session := range s.sessions {
		v := session.view(false)
		views = append(views, v)
		if v.Run != nil {
			seen[v.Run.ID] = true
		}
	}
	s.mu.Unlock()
	for _, run := range runs {
		if filepath.Clean(run.Repo) != filepath.Clean(s.cfg.Repo) || seen[run.ID] {
			continue
		}
		views = append(views, history(run))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.After(views[j].CreatedAt) })
	send(w, 200, views)
}

func history(run *artifact.Run) snapshot {
	return snapshot{ID: run.ID, Prompt: run.Prompt, Status: string(run.State), Error: run.Error, Run: run, CreatedAt: run.CreatedAt}
}

func (s *Server) find(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// Exact IDs from this repository only; never interpret user input as a path.
func (s *Server) load(id string) (*artifact.Run, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return nil, os.ErrNotExist
	}
	run, err := artifact.Load(filepath.Join(s.cfg.ArtifactsDir, id))
	if err != nil {
		return nil, err
	}
	if run.ID != id || filepath.Clean(run.Repo) != filepath.Clean(s.cfg.Repo) {
		return nil, os.ErrNotExist
	}
	// Artifact paths come from the configured storage root, not the record.
	run.Dir = filepath.Join(s.cfg.ArtifactsDir, id)
	return run, nil
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	var v snapshot
	if session := s.find(r.PathValue("id")); session != nil {
		v = session.view(true)
	} else {
		run, err := s.load(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "run not found")
			return
		}
		v = history(run)
	}
	if v.Run != nil {
		if run, err := s.load(v.Run.ID); err == nil {
			v.Run = run
		}
		v.Plan = readArtifact(v.Run, "plan.json")
		var plan contracts.Plan
		if json.Unmarshal([]byte(v.Plan), &plan) == nil {
			v.Plan = ui.RenderPlan(&plan)
		}
		v.Review = readArtifact(v.Run, "review.json")
		var review contracts.Review
		if json.Unmarshal([]byte(v.Review), &review) == nil {
			v.Review = ui.RenderReview(&review)
		}
		v.Diff = readArtifact(v.Run, "diff.patch")
	}
	send(w, 200, v)
}

func readArtifact(run *artifact.Run, name string) string {
	f, err := os.Open(run.Path(name))
	if err != nil {
		return ""
	}
	defer f.Close()
	const limit = 512 << 10
	b, _ := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > limit {
		return string(b[:limit]) + "\n[truncated; full artifact is on the host]"
	}
	return string(b)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt    string     `json:"prompt"`
		Name      string     `json:"name"`
		Autopilot bool       `json:"autopilot"`
		RunID     string     `json:"run_id"`
		From      agent.Kind `json:"from"`
	}
	if !decode(w, r, &req) {
		return
	}
	opts := s.opts
	opts.Repo, opts.Prompt, opts.Name, opts.Autopilot = s.cfg.Repo, strings.TrimSpace(req.Prompt), strings.TrimSpace(req.Name), req.Autopilot
	// Preserve work on cancellation and rejection from a remote device.
	opts.KeepWorktree = true
	cfg := s.cfg
	var run *artifact.Run
	if req.RunID != "" {
		var err error
		run, err = s.load(req.RunID)
		if err != nil {
			fail(w, 404, "run not found")
			return
		}
		switch req.From {
		case agent.Planner, agent.Executor, agent.Reviewer:
		default:
			fail(w, 400, "choose planner, executor or reviewer")
			return
		}
		if s.LoadRunConfig != nil {
			cfg, err = s.LoadRunConfig(run)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		opts.Prompt, opts.From, opts.Autopilot = run.Prompt, req.From, run.Autopilot
	} else {
		if opts.Prompt == "" {
			fail(w, 400, "a prompt is required")
			return
		}
		if s.PlanFromPrompt != nil {
			var err error
			opts.Plan, opts.Prompt, err = s.PlanFromPrompt(opts.Prompt)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
	}
	copyCfg := *cfg
	copyCfg.Repo, copyCfg.ArtifactsDir = s.cfg.Repo, s.cfg.ArtifactsDir
	if err := copyCfg.Validate(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	id, err := NewToken()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		fail(w, 503, "server is shutting down")
		return
	}
	for _, existing := range s.sessions {
		if existing.view(false).Active {
			s.mu.Unlock()
			fail(w, 409, "another run is active; finish or cancel it first")
			return
		}
	}
	// Retain recent live logs without unbounded memory growth. History stays on disk.
	if len(s.sessions) >= 20 {
		var oldest *session
		for _, existing := range s.sessions {
			if oldest == nil || existing.CreatedAt.Before(oldest.CreatedAt) {
				oldest = existing
			}
		}
		delete(s.sessions, oldest.ID)
	}
	if run != nil {
		for key, existing := range s.sessions {
			v := existing.view(false)
			if v.Run != nil && v.Run.ID == run.ID {
				delete(s.sessions, key)
			}
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	session := &session{snapshot: snapshot{ID: id, Prompt: opts.Prompt, Active: true, Status: "starting", CreatedAt: time.Now()}, cancel: cancel}
	if run != nil {
		session.RunUpdated(*run)
	}
	s.sessions[id] = session
	s.wg.Add(1)
	s.mu.Unlock()
	p := &pipeline.Pipeline{Cfg: &copyCfg, Opts: opts, Run: run, Gate: session}
	go func() {
		defer s.wg.Done()
		defer cancel()
		err := s.execute(ctx, p)
		if errors.Is(err, context.Canceled) {
			session.Info("Run cancelled; checkout and edits retained.")
		}
		session.finish(err, p.Run)
	}()
	send(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GateID int    `json:"gate_id"`
		Value  string `json:"value"`
	}
	if !decode(w, r, &req) {
		return
	}
	session := s.find(r.PathValue("id"))
	if session == nil {
		fail(w, 404, "live run not found")
		return
	}
	if err := session.answer(req.GateID, req.Value); err != nil {
		fail(w, 409, err.Error())
		return
	}
	send(w, 200, map[string]bool{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	session := s.find(r.PathValue("id"))
	if session == nil {
		fail(w, 404, "live run not found")
		return
	}
	session.stop()
	send(w, 200, map[string]bool{"ok": true})
}
