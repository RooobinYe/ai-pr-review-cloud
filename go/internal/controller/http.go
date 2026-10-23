package controller

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// maxResultBytes bounds a posted result; 15 comments of Markdown fit easily.
const maxResultBytes = 1 << 20

// Routes registers the runner callback API on mux.
// M2 adds a per-run RUN_TOKEN that these handlers verify.
func (c *Controller) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /runs/{id}/result", c.handleResult)
}

// matchResult checks that a result belongs to the run it was posted for.
func matchResult(r store.Run, res review.Result) error {
	want := review.PR{Repo: r.Repo, Number: r.PRNumber, HeadSHA: r.HeadSHA}
	if res.PR != want {
		return fmt.Errorf("result is for %s#%d@%s, run is for %s#%d@%s",
			res.PR.Repo, res.PR.Number, res.PR.HeadSHA, want.Repo, want.Number, want.HeadSHA)
	}
	return nil
}

func (c *Controller) handleResult(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxResultBytes))
	if err != nil {
		http.Error(w, "result too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	res, err := review.Parse(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := req.Context()
	r, err := c.store.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	if err != nil {
		c.log.Error("load run for result", "run_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := matchResult(r, res); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if r.State == run.StateRunning {
		err = c.store.SubmitResult(ctx, id, res)
		if errors.Is(err, store.ErrConflict) {
			r, err = c.store.Get(ctx, id)
		} else if err == nil {
			c.runLog(r).Info("result received", "verdict", res.Verdict, "comments", len(res.Comments))
			c.Wake()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			c.log.Error("store result", "run_id", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	// A retried POST after the first one landed is fine; anything else is late.
	if r.Verdict != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, fmt.Sprintf("run is %s and no longer accepts results", r.State), http.StatusConflict)
}
