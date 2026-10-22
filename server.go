package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server exposes the HTTP API. Config changes are serialized by mu and applied with RELOAD.
type Server struct {
	mu      sync.Mutex
	cfg     *Config
	admin   *Admin
	token   string
	running func() bool
}

func NewServer(cfg *Config, admin *Admin, token string, running func() bool) *Server {
	return &Server{cfg: cfg, admin: admin, token: token, running: running}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("GET /config", s.getConfig)

	mux.Handle("POST /switch", s.auth(s.switchTarget))
	mux.Handle("PUT /targets/{name}", s.auth(s.putTarget))
	mux.Handle("PATCH /settings", s.auth(s.patchSettings))
	mux.Handle("POST /reload", s.auth(s.simple("RELOAD")))
	mux.Handle("POST /resume", s.auth(s.simple("RESUME")))
	mux.Handle("POST /reconnect", s.auth(s.simple("RECONNECT")))
	mux.Handle("POST /pause", s.auth(s.pause))
	return logRequests(mux)
}

// healthz checks PgBouncer itself only. It must not depend on the backend,
// otherwise the orchestrator restarts PgBouncer while the backend is blocked during a cutover.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if !s.running() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "error": "pgbouncer is not running"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows, err := s.admin.Query(ctx, "SHOW VERSION")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "error": err.Error()})
		return
	}
	version := ""
	if len(rows) > 0 && rows[0]["version"] != nil {
		version = *rows[0]["version"]
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
}

var statsSections = map[string]string{
	"stats":     "SHOW STATS",
	"pools":     "SHOW POOLS",
	"databases": "SHOW DATABASES",
	"lists":     "SHOW LISTS",
	"state":     "SHOW STATE",
}

// stats returns SHOW results as JSON. It answers 503 when the admin console is unreachable,
// so it also works as a health check. ?sections=pools,stats limits the output.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	sections := []string{"state", "stats", "pools", "databases", "lists"}
	if q := r.URL.Query().Get("sections"); q != "" {
		sections = strings.Split(q, ",")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	out := map[string]Rows{}
	for _, name := range sections {
		cmd, ok := statsSections[name]
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unknown section %q", name))
			return
		}
		rows, err := s.admin.Query(ctx, cmd)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		out[name] = rows
	}
	writeJSON(w, http.StatusOK, out)
}

type configView struct {
	ActiveTarget string            `json:"active_target"`
	Targets      map[string]Target `json:"targets"`
	Settings     map[string]string `json:"settings"`
}

func (s *Server) view() configView {
	return configView{
		ActiveTarget: s.cfg.ActiveTarget,
		Targets:      maps.Clone(s.cfg.Targets),
		Settings:     maps.Clone(s.cfg.Settings),
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.view())
}

// apply runs mutate on a copy of the config, writes the files and reloads PgBouncer.
// The in-memory config only changes when the reload succeeds.
func (s *Server) apply(ctx context.Context, mutate func(c *Config) error) (configView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := *s.cfg
	next.Targets = maps.Clone(s.cfg.Targets)
	next.Settings = maps.Clone(s.cfg.Settings)
	if err := mutate(&next); err != nil {
		return configView{}, &httpError{http.StatusBadRequest, err}
	}

	write := func(c *Config) error {
		if err := writeFileAtomic(c.path("databases.ini"), c.RenderDatabases()); err != nil {
			return err
		}
		return writeFileAtomic(c.IniPath(), c.RenderIni())
	}
	if err := write(&next); err != nil {
		return configView{}, err
	}
	if _, err := s.admin.Query(ctx, "RELOAD"); err != nil {
		// Put the previous files back so a later restart or RELOAD does not pick up a half-applied change.
		if rerr := write(s.cfg); rerr != nil {
			slog.Error("restore config after failed reload", "error", rerr)
		}
		return configView{}, fmt.Errorf("reload: %w", err)
	}
	s.cfg = &next
	return s.view(), nil
}

type switchRequest struct {
	Target string `json:"target"`
}

func (s *Server) switchTarget(w http.ResponseWriter, r *http.Request) {
	var req switchRequest
	if !decode(w, r, &req) {
		return
	}
	var from string
	v, err := s.apply(r.Context(), func(c *Config) error {
		if _, ok := c.Targets[req.Target]; !ok {
			return fmt.Errorf("unknown target %q", req.Target)
		}
		from = c.ActiveTarget
		c.ActiveTarget = req.Target
		return nil
	})
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	slog.Info("switched target", "from", from, "to", v.ActiveTarget)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) putTarget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var t Target
	if !decode(w, r, &t) {
		return
	}
	if t.Port == 0 {
		t.Port = 5432
	}
	v, err := s.apply(r.Context(), func(c *Config) error {
		if err := validateTarget(name, t); err != nil {
			return err
		}
		c.Targets[name] = t
		return nil
	})
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	slog.Info("updated target", "name", name, "host", t.Host, "port", t.Port)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if !decode(w, r, &req) {
		return
	}
	v, err := s.apply(r.Context(), func(c *Config) error {
		for k, val := range req {
			if err := validateSetting(k, val); err != nil {
				return err
			}
			c.Settings[k] = val
		}
		return nil
	})
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	slog.Info("updated settings", "settings", req)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) simple(cmd string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.admin.Query(r.Context(), cmd); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"result": cmd})
	}
}

// pause issues PAUSE. PgBouncer answers only after every server connection is released,
// which in session mode means after all backend sessions end. The command keeps running
// in the background; if it has not finished within ?timeout (default 10s) this returns 202
// and the progress can be followed with GET /stats?sections=state,pools.
func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	timeout := 10 * time.Second
	if q := r.URL.Query().Get("timeout"); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("timeout: %w", err))
			return
		}
		timeout = d
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_, err := s.admin.Query(ctx, "PAUSE")
		if err != nil {
			slog.Error("pause", "error", err)
		} else {
			slog.Info("paused")
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"state": "paused"})
	case <-time.After(timeout):
		writeJSON(w, http.StatusAccepted, map[string]string{"state": "pausing"})
	}
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}
		h(w, r)
	})
}

type httpError struct {
	status int
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }
func (e *httpError) Unwrap() error { return e.err }

func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return http.StatusInternalServerError
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logRequests logs every request except successful health checks, which run every few seconds.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		if r.Method == http.MethodGet && rec.status < 400 {
			return
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}
