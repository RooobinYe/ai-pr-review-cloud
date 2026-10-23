package controller

import (
	"crypto/subtle"
	"encoding/json"
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
	mux.HandleFunc("PUT /runs/{id}/usage", c.handleUsage)
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
	if !ok || got == "" || r.JobName == "" {
		return false, nil
	}
	secret, err := c.kube.GetSecret(req.Context(), runSecretName(r.JobName))
	if errors.Is(err, kube.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	want := secret.Data["RUN_TOKEN"]
	return len(want) > 0 && subtle.ConstantTimeCompare([]byte(got), want) == 1, nil
}

// authorizedRun loads the run named in the path and checks the request's run
// token, answering the request itself when either fails.
func (c *Controller) authorizedRun(w http.ResponseWriter, req *http.Request) (store.Run, bool) {
	id := req.PathValue("id")
	if !uuidPattern.MatchString(id) {
		http.Error(w, "unknown run", http.StatusNotFound)
		return store.Run{}, false
	}
	r, err := c.store.Get(req.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown run", http.StatusNotFound)
		return store.Run{}, false
	}
	if err != nil {
		c.log.Error("load run", "run_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Run{}, false
	}
	if ok, err := c.authorized(req, r); err != nil {
		c.log.Error("check run token", "run_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Run{}, false
	} else if !ok {
		http.Error(w, "missing or wrong run token", http.StatusUnauthorized)
		return store.Run{}, false
	}
	return r, true
}

// maxUsageBytes bounds a usage report, a handful of numbers.
const maxUsageBytes = 4 << 10

// ValidUsage checks a usage report before it is stored.
func ValidUsage(u store.Usage) error {
	switch {
	case u.Turn < 1:
		return fmt.Errorf("turn must be at least 1, got %d", u.Turn)
	case u.Model == "" || len(u.Model) > 200:
		return errors.New("model must be 1 to 200 bytes")
	case u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0:
		return errors.New("token counts must not be negative")
	case !(u.CostUSD >= 0 && u.CostUSD < 1000): // also rejects NaN
		return fmt.Errorf("cost_usd must be in [0, 1000), got %v", u.CostUSD)
	}
	return nil
}

// handleUsage records one turn of model usage for the metrics (§7.1).
func (c *Controller) handleUsage(w http.ResponseWriter, req *http.Request) {
	r, ok := c.authorizedRun(w, req)
	if !ok {
		return
	}
	var u store.Usage
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxUsageBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		http.Error(w, "invalid usage: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := ValidUsage(u); err != nil {
		http.Error(w, "invalid usage: "+err.Error(), http.StatusBadRequest)
		return
	}
	if r.State != run.StateRunning && r.State != run.StatePreviewing {
		http.Error(w, fmt.Sprintf("run is %s and no longer accepts usage", r.State), http.StatusConflict)
		return
	}
	if err := c.store.RecordUsage(req.Context(), r.ID, u); err != nil {
		c.log.Error("store usage", "run_id", r.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Controller) handleResult(w http.ResponseWriter, req *http.Request) {
	r, ok := c.authorizedRun(w, req)
	if !ok {
		return
	}
	id, ctx := r.ID, req.Context()
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
