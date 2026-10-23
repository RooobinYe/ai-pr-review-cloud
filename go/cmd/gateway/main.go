// Command gateway receives GitHub webhooks, verifies them, and records review
// triggers. It never calls GitHub or creates Jobs; the controller does that.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/gateway"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		log.Error("GITHUB_WEBHOOK_SECRET is required")
		os.Exit(1)
	}
	budget, err := strconv.ParseFloat(envOr("DEFAULT_BUDGET_USD", "0.5"), 64)
	if err != nil {
		log.Error("DEFAULT_BUDGET_USD", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("POST /webhook", &gateway.Handler{Secret: []byte(secret), Recorder: db, BudgetUSD: budget, Log: log})

	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("gateway listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
