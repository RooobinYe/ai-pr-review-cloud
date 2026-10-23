package controller

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
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

// maxSessionBytes bounds a session snapshot. Tool output is capped per call,
// so even a long review stays in the low megabytes.
const maxSessionBytes = 32 << 20

// sessionTurnHeader carries a snapshot's turn number on GET /runs/{id}/session.
const sessionTurnHeader = "X-Session-Turn"

// Routes registers the runner callback API on mux. Each call must carry the
// run's RUN_TOKEN as a bearer token; it lives in the run's Secret, which only
// the controller and the current attempt's runner container can read.
func (c *Controller) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /runs/{id}/result", c.handleResult)
	mux.HandleFunc("PUT /runs/{id}/session", c.handlePutSession)
	mux.HandleFunc("GET /runs/{id}/session", c.handleGetSession)
	mux.HandleFunc("PUT /runs/{id}/usage", c.handlePutUsage)
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

// callerRun loads the run named in the path and checks the caller's token. On
// failure it has written the response and returns ok=false.
func (c *Controller) callerRun(w http.ResponseWriter, req *http.Request) (r store.Run, ok bool) {
	id := req.PathValue("id")
	if !uuidPattern.MatchString(id) {
		http.Error(w, "unknown run", http.StatusNotFound)
		return r, false
	}
	r, err := c.store.Get(req.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown run", http.StatusNotFound)
		return r, false
	}
	if err != nil {
		c.log.Error("load run", "run_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return r, false
	}
	if ok, err := c.authorized(req, r); err != nil {
		c.log.Error("check run token", "run_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return r, false
	} else if !ok {
		http.Error(w, "missing or wrong run token", http.StatusUnauthorized)
		return r, false
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

func (c *Controller) handleResult(w http.ResponseWriter, req *http.Request) {
	r, ok := c.callerRun(w, req)
	if !ok {
		return
	}
	ctx, id := req.Context(), r.ID
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

// handlePutSession stores the runner's session snapshot after a turn. The
// next attempt resumes from it if this one fails (docs/architecture.md §4.3).
func (c *Controller) handlePutSession(w http.ResponseWriter, req *http.Request) {
	r, ok := c.callerRun(w, req)
	if !ok {
		return
	}
	turn, err := strconv.Atoi(req.URL.Query().Get("turn"))
	if err != nil || turn < 0 {
		http.Error(w, "turn must be a non-negative integer", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxSessionBytes))
	if err != nil {
		http.Error(w, "session too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	switch err := c.store.PutSession(req.Context(), r.ID, turn, body); {
	case errors.Is(err, store.ErrConflict):
		http.Error(w, fmt.Sprintf("run is %s and no longer takes snapshots", r.State), http.StatusConflict)
	case err != nil:
		c.runLog(r).Error("store session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGetSession returns the run's latest snapshot, or 404 if it has none.
func (c *Controller) handleGetSession(w http.ResponseWriter, req *http.Request) {
	r, ok := c.callerRun(w, req)
	if !ok {
		return
	}
	jsonl, turn, err := c.store.GetSession(req.Context(), r.ID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no session snapshot", http.StatusNotFound)
		return
	}
	if err != nil {
		c.runLog(r).Error("load session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c.runLog(r).Info("resuming from session snapshot", "attempt", r.Attempt, "turn", turn)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set(sessionTurnHeader, strconv.Itoa(turn))
	_, _ = w.Write(jsonl)
}

// handlePutUsage records one turn's model usage.
func (c *Controller) handlePutUsage(w http.ResponseWriter, req *http.Request) {
	r, ok := c.callerRun(w, req)
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
	if err := c.store.PutUsage(req.Context(), r.ID, u); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown run", http.StatusNotFound)
			return
		}
		c.runLog(r).Error("record usage", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
