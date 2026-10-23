// Package gateway turns verified GitHub webhooks into queued review runs. It
// never calls GitHub or creates Jobs; the controller does that.
package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// maxPayloadBytes is GitHub's documented webhook payload cap.
const maxPayloadBytes = 25 << 20

// Recorder persists review triggers; *store.Store implements it.
type Recorder interface {
	RecordTrigger(ctx context.Context, deliveryID string, nr store.NewRun) (id string, created bool, err error)
}

// Handler serves POST /webhook.
type Handler struct {
	Secret    []byte
	Recorder  Recorder
	BudgetUSD float64
	Log       *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayloadBytes))
	if err != nil {
		http.Error(w, "payload too large or unreadable", http.StatusBadRequest)
		return
	}
	if !githubapp.VerifySignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	trigger, ok, err := githubapp.TriggerFromEvent(r.Header.Get("X-GitHub-Event"), body)
	if err != nil {
		h.Log.Warn("rejected webhook", "delivery", delivery, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if delivery == "" {
		http.Error(w, "missing X-GitHub-Delivery", http.StatusBadRequest)
		return
	}
	log := h.Log.With("delivery", delivery, "repo", trigger.Repo, "pr", trigger.PRNumber, "head_sha", trigger.HeadSHA)
	id, created, err := h.Recorder.RecordTrigger(r.Context(), delivery, store.NewRun{
		Repo:           trigger.Repo,
		PRNumber:       trigger.PRNumber,
		HeadSHA:        trigger.HeadSHA,
		InstallationID: trigger.InstallationID,
		BudgetUSD:      h.BudgetUSD,
	})
	if err != nil {
		// GitHub does not retry failed deliveries; they can be redelivered by hand.
		log.Error("record trigger", "err", err)
		http.Error(w, "could not record trigger", http.StatusServiceUnavailable)
		return
	}
	if created {
		log.Info("review queued", "run_id", id)
	} else {
		log.Info("duplicate trigger ignored")
	}
	w.WriteHeader(http.StatusAccepted)
}
