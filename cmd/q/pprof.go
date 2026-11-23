package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"
)

// EnvPprof names a loopback address at which `q daemon run` serves Go's
// profiler, for measuring a daemon under its real load:
//
//	Q_PPROF=127.0.0.1:6061 q daemon restart
//	go tool pprof -top 'http://127.0.0.1:6061/debug/pprof/profile?seconds=30'
//
// It is off unless set, and refuses anything but a loopback address because
// the profiler is unauthenticated.
const EnvPprof = "Q_PPROF"

// servePprof starts the profiler when EnvPprof asks for it. It never fails the
// daemon: a bad address is logged and ignored.
func servePprof(ctx context.Context, logger *slog.Logger) {
	addr := strings.TrimSpace(os.Getenv(EnvPprof))
	if addr == "" {
		return
	}

	if err := loopback(addr); err != nil {
		logger.Warn("not serving the profiler", "addr", addr, "error", err)

		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	go func() {
		logger.Info("serving the profiler", "addr", addr)

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("serving the profiler", "addr", addr, "error", err)
		}
	}()
}

func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	if host == "localhost" {
		return nil
	}

	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("the profiler only listens on a loopback address")
	}

	return nil
}
