package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server 는 HTTP API. 설정 변경은 mu 로 직렬화하고 RELOAD 로 반영한다.
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

	mux.Handle("PUT /backend", s.auth(s.putBackend))
	mux.Handle("PATCH /settings", s.auth(s.patchSettings))
	mux.Handle("POST /reload", s.auth(s.simple("RELOAD")))
	mux.Handle("POST /resume", s.auth(s.simple("RESUME")))
	mux.Handle("POST /reconnect", s.auth(s.simple("RECONNECT")))
	mux.Handle("POST /pause", s.auth(s.pause))
	return logRequests(mux)
}

// healthz 는 PgBouncer 자신만 확인한다. 백엔드를 보면 전환 중 백엔드를 막아 둔 구간에
// 오케스트레이터가 PgBouncer 를 재시작해 버린다.
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

// stats 는 SHOW 결과를 JSON 으로 준다. 관리 콘솔이 응답하지 않으면 503 이라 헬스체크로도 쓸 수 있다.
// ?sections=pools,stats 로 골라 받는다.
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
	Backend  Backend           `json:"backend"`
	Settings map[string]string `json:"settings"`
}

func (s *Server) view() configView {
	return configView{
		Backend:  s.cfg.Backend.Redacted(),
		Settings: maps.Clone(s.cfg.Settings),
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.view())
}

// apply 는 설정 사본에 mutate 를 적용하고 파일을 쓴 뒤 RELOAD 한다.
// 메모리의 설정은 RELOAD 가 성공했을 때만 바뀐다.
func (s *Server) apply(ctx context.Context, mutate func(c *Config) error) (configView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := *s.cfg
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
		// 이후 재시작이나 RELOAD 가 반쯤 적용된 변경을 읽지 않도록 이전 파일로 되돌린다.
		if rerr := write(s.cfg); rerr != nil {
			slog.Error("restore config after failed reload", "error", rerr)
		}
		return configView{}, fmt.Errorf("reload: %w", err)
	}
	s.cfg = &next
	return s.view(), nil
}

// putBackend 는 모든 DB 가 향하는 백엔드를 교체한다. 본문은 백엔드 JSON 객체 그대로.
// 기존 서버 커넥션은 반납되는 대로 닫히고, 새 커넥션은 새 백엔드로 붙는다.
func (s *Server) putBackend(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var from Backend
	v, err := s.apply(r.Context(), func(c *Config) error {
		b, err := ParseBackend(body)
		if err != nil {
			return err
		}
		from = c.Backend
		c.Backend = b
		return nil
	})
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	slog.Info("changed backend", "from", from.Redacted(), "to", v.Backend)
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

// pause 는 PAUSE 를 보낸다. PgBouncer 는 서버 커넥션이 모두 반납돼야 응답하는데,
// session 모드에선 백엔드 세션이 다 끝나야 한다는 뜻이다. 명령은 백그라운드에서 계속 기다리고,
// ?timeout(기본 10s) 안에 끝나지 않으면 202 를 준다. 진행 상황은 GET /stats?sections=state,pools 로 본다.
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

// logRequests 는 요청을 로그로 남긴다. 몇 초마다 오는 성공한 GET(헬스체크 등)은 뺀다.
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
