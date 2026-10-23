// Command gateway receives GitHub webhooks, verifies them, and records review
// triggers. It never calls GitHub or creates Jobs; the controller does that.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
)

// maxPayloadBytes is GitHub's documented webhook payload cap.
const maxPayloadBytes = 25 << 20

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		log.Error("GITHUB_WEBHOOK_SECRET is required")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /webhook", webhookHandler(log, []byte(secret)))

	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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

func webhookHandler(log *slog.Logger, secret []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayloadBytes))
		if err != nil {
			http.Error(w, "payload too large or unreadable", http.StatusBadRequest)
			return
		}
		if !githubapp.VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		delivery := r.Header.Get("X-GitHub-Delivery")
		trigger, ok, err := githubapp.TriggerFromEvent(r.Header.Get("X-GitHub-Event"), body)
		if err != nil {
			log.Warn("rejected webhook", "delivery", delivery, "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// M1: insert a queued review_run keyed by (repo, pr, head_sha), deduplicated by delivery ID.
		log.Info("review triggered", "delivery", delivery, "repo", trigger.Repo, "pr", trigger.PRNumber, "head_sha", trigger.HeadSHA)
		w.WriteHeader(http.StatusAccepted)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
