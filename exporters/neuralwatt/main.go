package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type app struct {
	cfg      config
	log      *slog.Logger
	st       *state
	registry *prometheus.Registry
	poller   *poller
}

func newApp(cfg config, log *slog.Logger, clk clock) (*app, error) {
	client, err := newQuotaClient(cfg, clk)
	if err != nil {
		return nil, err
	}
	st := newState()
	reg := prometheus.NewRegistry()
	// process_start_time_seconds（起動時刻）を出すため、標準の process collector と Go の collector も登録する。
	// turtton/infra のアラート NeuralwattQuotaStale が「一度も成功していない」状態の猶予の起点に使う。
	if err := registerAll(reg,
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
		newCollector(st, cfg.MonthlyBudgetUSD),
	); err != nil {
		return nil, err
	}
	return &app{cfg: cfg, log: log, st: st, registry: reg, poller: newPoller(client, st, clk, cfg.PollInterval, log)}, nil
}

func registerAll(reg *prometheus.Registry, cs ...prometheus.Collector) error {
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(a.registry, promhttp.HandlerOpts{}))
	// プロセスが生きていれば 200。API の失敗では落とさない（落とすと再起動しても直らない）。
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

func run(ctx context.Context, getenv func(string) string, stderr io.Writer) error {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	a, err := newApp(cfg, log, realClock{})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           a.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Info("starting", "listen", cfg.ListenAddress, "base_url", cfg.BaseURL, "poll_interval", cfg.PollInterval.String(), "budget_configured", cfg.MonthlyBudgetUSD != nil)

	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		a.poller.run(pollCtx)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		stopPoll()
		<-pollDone
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	stopPoll()
	<-pollDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stderr); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("exiting", "error", err.Error())
		os.Exit(1)
	}
}
