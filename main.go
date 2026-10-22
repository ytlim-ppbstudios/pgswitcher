// pgswitcher 는 PgBouncer 를 자식 프로세스로 띄우고,
// 백엔드 교체와 설정 변경을 런타임에 하는 HTTP API 를 연다.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("logger", "pgswitcher"))
	os.Exit(run())
}

func run() int {
	cfg, err := LoadConfig(os.Getenv, os.Environ())
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	token := os.Getenv("PGSW_TOKEN")
	if token == "" {
		slog.Error("PGSW_TOKEN is required")
		return 1
	}
	if err := cfg.WriteAll(); err != nil {
		slog.Error("write pgbouncer config", "error", err)
		return 1
	}

	cmd := exec.Command(orDefault(os.Getenv("PGSW_PGBOUNCER_BIN"), "/usr/bin/pgbouncer"), cfg.IniPath())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("start pgbouncer", "error", err)
		return 1
	}
	slog.Info("started pgbouncer", "pid", cmd.Process.Pid, "backend", cfg.Backend.Redacted())

	var running atomic.Bool
	running.Store(true)
	exited := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		running.Store(false)
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = 1
		}
		slog.Info("pgbouncer exited", "code", code, "error", err)
		exited <- code
	}()

	srv := &http.Server{
		Addr:              orDefault(os.Getenv("PGSW_LISTEN"), ":8080"),
		Handler:           NewServer(cfg, NewAdmin(cfg.ListenPort, cfg.AdminPassword), token, running.Load).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "error", err)
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	// 종료/리로드 시그널은 PgBouncer 에 넘기고, PgBouncer 가 끝나면 같이 끝낸다.
	// 오케스트레이터가 PgBouncer 생존 여부를 그대로 보게 하려는 것.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	for {
		select {
		case sig := <-sigs:
			slog.Info("forwarding signal", "signal", sig.String())
			_ = cmd.Process.Signal(sig)
		case code := <-exited:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = srv.Shutdown(ctx)
			cancel()
			return code
		}
	}
}
