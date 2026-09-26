// Package web serves a phone-friendly dashboard using the same pipeline as the TUI.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
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
	"github.com/guilhermesalviano/korchestrate/internal/models"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/ui"
	"github.com/guilhermesalviano/korchestrate/internal/worktree"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	cfg      *config.Config
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

	// The model catalog is discovered once, on first use, since it probes
	// the agent CLIs.
	discover    func() *models.Catalog
	catalogOnce sync.Once
	catalog     *models.Catalog
}

func New(ctx context.Context, cfg *config.Config, opts pipeline.Options) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{cfg: cfg, opts: opts, ctx: ctx, cancel: cancel, sessions: make(map[string]*session), execute: func(ctx context.Context, p *pipeline.Pipeline) error { return p.Execute(ctx) }, discover: models.Discover}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("GET /api/runs", s.list)
	mux.HandleFunc("GET /api/worktrees", s.worktrees)
	mux.HandleFunc("GET /api/models", s.listModels)
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

// Active reports whether a browser-started run is still in flight.
func (s *Server) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.sessions {
		if existing.view(false).Active {
			return true
		}
	}
	return false
}

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

func newID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// secure limits the dashboard to this computer and the private LAN. There is no
// token, so it also rejects DNS-rebound host names and cross-origin API calls,
// which would otherwise let any web page drive the agents.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !localClient(r.RemoteAddr) {
			fail(w, http.StatusForbidden, "kor web only accepts connections from this computer or your local network")
			return
		}
		if !localHost(r.Host) {
			fail(w, http.StatusForbidden, "open kor web by its IP address, as printed by kor web")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
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

// localClient reports whether addr is loopback or a private LAN address.
func localClient(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// localHost accepts only IP literals and localhost, so a public name that is
// rebound to a LAN address cannot reach the API from a browser.
func localHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
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
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
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
	send(w, 200, map[string]any{"repo": s.cfg.Repo, "prompts": s.cfg.Prompts.Resolved(), "default_prompts": config.DefaultPrompts(), "models": models.ChoicesFromConfig(s.cfg)})
}

// listModels lists the installed providers and the models and efforts they offer.
func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	s.catalogOnce.Do(func() { s.catalog = s.discover() })
	installed := map[string]bool{}
	for _, name := range models.ProviderOrder() {
		installed[name] = true
	}
	agents := []models.AgentInfo{}
	for _, info := range s.catalog.Agents {
		// Like the TUI picker, providers without models are not offered.
		if installed[info.Agent] && len(info.Models) > 0 {
			agents = append(agents, info)
		}
	}
	send(w, 200, models.Catalog{Agents: agents})
}

// validChoices rejects providers that are not installed and values that the
// agent CLIs could read as flags.
func validChoices(c models.Choices) error {
	installed := map[string]bool{}
	for _, name := range models.ProviderOrder() {
		installed[name] = true
	}
	for _, stage := range []struct {
		name   string
		choice models.Choice
	}{{"planner", c.Planner}, {"executor", c.Executor}, {"reviewer", c.Reviewer}} {
		if !installed[stage.choice.Agent] {
			return fmt.Errorf("%s provider %q is not installed", stage.name, stage.choice.Agent)
		}
		if stage.choice.Model == "" {
			return fmt.Errorf("choose a %s model", stage.name)
		}
		for _, value := range []string{stage.choice.Model, stage.choice.Variant} {
			if len(value) > 200 || strings.HasPrefix(value, "-") || strings.ContainsFunc(value, func(r rune) bool { return r < ' ' || r == 0x7f }) {
				return fmt.Errorf("invalid %s model or effort %q", stage.name, value)
			}
		}
	}
	return nil
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

// worktreeView is a repository worktree with its pending changes and the
// newest run that used it, if any.
type worktreeView struct {
	worktree.Info
	Changes   int    `json:"changes"`
	RunID     string `json:"run_id,omitempty"`
	RunStatus string `json:"run_status,omitempty"`
}

func (s *Server) worktrees(w http.ResponseWriter, _ *http.Request) {
	list, err := worktree.List(s.cfg.Repo)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	runs, _ := artifact.List(s.cfg.ArtifactsDir)
	views := make([]worktreeView, 0, len(list))
	for _, wt := range list {
		v := worktreeView{Info: wt}
		if !wt.Prunable {
			files, _ := worktree.ChangedFiles(wt.Path)
			v.Changes = len(files)
		}
		// The main checkout hosts every in-place run, so only linked
		// worktrees point at a single run. Runs are listed newest first.
		if !wt.Main {
			for _, run := range runs {
				if filepath.Clean(run.Worktree) == filepath.Clean(wt.Path) {
					v.RunID, v.RunStatus = run.ID, string(run.State)
					break
				}
			}
		}
		views = append(views, v)
	}
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
		if v.Prompts == nil || v.Models == nil {
			cfg, err := s.runConfig(v.Run)
			if err != nil {
				fail(w, 500, "could not load saved run settings: "+err.Error())
				return
			}
			prompts, choices := cfg.Prompts.Resolved(), models.ChoicesFromConfig(cfg)
			v.Prompts, v.Models = &prompts, &choices
		}
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
		Prompt    string          `json:"prompt"`
		Name      string          `json:"name"`
		Autopilot bool            `json:"autopilot"`
		RunID     string          `json:"run_id"`
		From      agent.Kind      `json:"from"`
		Prompts   *config.Prompts `json:"prompts"`
		Models    *models.Choices `json:"models"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Prompt) > 50000 || len(req.Name) > 200 {
		fail(w, 400, "request or branch name is too long")
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
		cfg, err = s.runConfig(run)
		if err != nil {
			fail(w, 400, err.Error())
			return
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
	if req.Models != nil {
		if err := validChoices(*req.Models); err != nil {
			fail(w, 400, err.Error())
			return
		}
		cfg = req.Models.Apply(cfg)
	}
	copyCfg := *cfg
	if req.Prompts != nil {
		copyCfg.Prompts = *req.Prompts
	}
	copyCfg.Prompts = copyCfg.Prompts.Resolved()
	copyCfg.Repo, copyCfg.ArtifactsDir = s.cfg.Repo, s.cfg.ArtifactsDir
	if err := copyCfg.Validate(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	id, err := newID()
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
	prompts, choices := copyCfg.Prompts, models.ChoicesFromConfig(&copyCfg)
	session.Prompts, session.Models = &prompts, &choices
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

func (s *Server) runConfig(run *artifact.Run) (*config.Config, error) {
	if s.LoadRunConfig != nil {
		return s.LoadRunConfig(run)
	}
	cfg, err := config.Load(run.Path("config.resolved.yaml"))
	if err != nil {
		return nil, err
	}
	cfg.Repo = run.Repo
	return cfg, nil
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
