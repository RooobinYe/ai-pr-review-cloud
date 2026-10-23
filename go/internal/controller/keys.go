package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/litellm"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// Keys issues and revokes the runners' LiteLLM virtual keys (*litellm.Client).
type Keys interface {
	Generate(ctx context.Context, k litellm.Key) (string, error)
	Delete(ctx context.Context, key string) error
}

// keyGrace is how long a virtual key outlives its Job's deadline. Expiry backs
// up revocation: a key the controller failed to delete still stops working.
const keyGrace = 5 * time.Minute

var errBudgetExceeded = errors.New(BudgetExceeded)

// mintKey issues the virtual key for one attempt of a run, capped at the
// run's budget and limited to the configured model.
func (c *Controller) mintKey(ctx context.Context, r store.Run) (string, error) {
	spent, err := c.store.RunCostUSD(ctx, r.ID)
	if err != nil {
		return "", err
	}
	remaining := r.BudgetUSD - spent
	if remaining <= 0 {
		return "", fmt.Errorf("%w: reported cost $%.4f exhausted the run's $%.4f budget", errBudgetExceeded, spent, r.BudgetUSD)
	}
	var models []string
	if m := c.cfg.Job.LLMEnv["LLM_MODEL"]; m != "" {
		models = []string{m}
	}
	return c.keys.Generate(ctx, litellm.Key{
		Alias:        r.JobName,
		Models:       models,
		MaxBudgetUSD: remaining,
		Duration:     time.Duration(c.cfg.Job.ActiveDeadlineSeconds)*time.Second + keyGrace,
		Metadata:     map[string]string{"run_id": r.ID, "repo": r.Repo, "pr": strconv.Itoa(r.PRNumber), "head_sha": r.HeadSHA},
	})
}

// revokeKey deletes the run's virtual key once its runner is done with it.
// Failures are logged rather than retried, since the key expires on its own.
func (c *Controller) revokeKey(ctx context.Context, r store.Run) {
	if r.JobName == "" {
		return
	}
	secret, err := c.kube.GetSecret(ctx, runSecretName(r.JobName))
	if errors.Is(err, kube.ErrNotFound) {
		return // never created, or garbage-collected with its Job
	}
	if key := secret.Data["LLM_API_KEY"]; err == nil && len(key) > 0 {
		err = c.keys.Delete(ctx, string(key))
	}
	if err != nil {
		c.runLog(r).Warn("revoke virtual key; it expires on its own", "err", err)
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
