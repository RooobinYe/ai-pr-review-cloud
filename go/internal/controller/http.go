package controller

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// uuidPattern matches run IDs; anything else would be rejected by PostgreSQL.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// maxResultBytes bounds a posted result; 15 comments of Markdown fit easily.
const maxResultBytes = 1 << 20

// Routes registers the runner callback API on mux. Each call must carry the
// run's RUN_TOKEN as a bearer token; it lives in the run's Secret, which only
// the controller and that run's runner container can read.
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

// authorized reports whether the request carries the run's callback token.
func (c *Controller) authorized(req *http.Request, r store.Run) (bool, error) {
	got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false, nil
	}
	return c.tokenMatches(req.Context(), r, "RUN_TOKEN", got)
}

// tokenMatches compares got with the token stored under key in the run's Secret.
func (c *Controller) tokenMatches(ctx context.Context, r store.Run, key, got string) (bool, error) {
	if got == "" || r.JobName == "" {
		return false, nil
	}
	secret, err := c.kube.GetSecret(ctx, runSecretName(r.JobName))
	if errors.Is(err, kube.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	want := secret.Data[key]
	return len(want) > 0 && subtle.ConstantTimeCompare([]byte(got), want) == 1, nil
}

func (c *Controller) handleResult(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if !uuidPattern.MatchString(id) {
		http.Error(w, "unknown run", http.StatusNotFound)
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
	if ok, err := c.authorized(req, r); err != nil {
		c.log.Error("check run token", "run_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "missing or wrong run token", http.StatusUnauthorized)
		return
	}
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
