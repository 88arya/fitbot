package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/88arya/spotter/internal/config"
	"github.com/88arya/spotter/internal/db"
	"github.com/88arya/spotter/internal/db/store"
	"github.com/88arya/spotter/internal/llm"
	"github.com/88arya/spotter/internal/server"
	"github.com/88arya/spotter/internal/telnyx"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	var sms *server.SMS
	if cfg.Telnyx.APIKey == "" {
		slog.Warn("TELNYX_API_KEY not set; texting is disabled")
	} else {
		tc, err := telnyx.New(cfg.Telnyx.APIKey, cfg.Telnyx.PublicKey, cfg.Telnyx.FromNumber)
		if err != nil {
			return err
		}
		sms = &server.SMS{
			Store:           store.New(pool),
			Telnyx:          tc,
			AllowedSenders:  cfg.AllowedSenders,
			DefaultTimezone: cfg.DefaultTimezone.String(),
		}
	}
	if cfg.OpenAI.APIKey == "" {
		slog.Warn("OPENAI_API_KEY not set; only fast-path replies will work")
	} else if sms != nil {
		sms.LLM = llm.New(cfg.OpenAI.BaseURL, cfg.OpenAI.APIKey, cfg.OpenAI.Model, cfg.OpenAI.InputPrice, cfg.OpenAI.OutputPrice)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.New(pool, sms),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
