package controller

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func TestRetryUsesRemainingBudget(t *testing.T) {
	for _, spent := range []float64{0.2, 0.5, 0.6} {
		t.Run(strconv.FormatFloat(spent, 'f', 2, 64), func(t *testing.T) {
			h := newHarness()
			h.reconcile()
			oldJob := h.state().JobName
			if err := h.store.PutUsage(context.Background(), runID, store.Usage{Turn: 1, CostUSD: spent}); err != nil {
				t.Fatal(err)
			}
			h.losePod()
			h.reconcile()
			if len(h.keys.live) != 0 {
				t.Fatal("retry left the old attempt key live after deleting its Secret")
			}
			if _, exists := h.kube.jobs[oldJob]; exists {
				t.Fatal("retry left the old Job")
			}
			h.reconcile()
			if spent >= 0.5 {
				if h.state().State != run.StateFailed || !strings.Contains(h.state().Error, BudgetExceeded) || len(h.keys.issued) != 1 {
					t.Fatalf("exhausted retry: state=%s error=%s keys=%d", h.state().State, h.state().Error, len(h.keys.issued))
				}
			} else if len(h.keys.issued) != 2 || math.Abs(h.keys.issued[1].MaxBudgetUSD-(0.5-spent)) > 1e-9 {
				t.Fatalf("retry key budgets: %+v", h.keys.issued)
			}
		})
	}
}

func TestTimeoutRevokesBeforeDeletingSecret(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.clock = h.clock.Add(h.c.attemptDeadline() + time.Second)
	h.reconcile()
	if h.state().State != run.StateFailed || len(h.keys.live) != 0 || len(h.kube.jobs) != 0 {
		t.Fatalf("timeout left resources: state=%s keys=%d jobs=%d", h.state().State, len(h.keys.live), len(h.kube.jobs))
	}
}

func TestFailedRunRevokesKeyAfterControllerRestart(t *testing.T) {
	h := newHarness()
	h.reconcile()
	if err := h.store.Transition(context.Background(), runID, run.StateRunning, run.StateFailed, "controller stopped after persisting failure"); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.reconcile()
	if len(h.keys.live) != 0 || !h.state().Done(store.ActionCheckRunComplete) {
		t.Fatalf("failed run recovery: live keys=%d, actions=%v", len(h.keys.live), h.state().Actions)
	}
}

func TestLostSecretResponseKeepsUsableKey(t *testing.T) {
	h := newHarness()
	h.reconcile()
	r := h.state()
	clear(h.kube.secrets)
	h.kube.createSecretResponseErr = context.DeadlineExceeded
	err := h.c.createRunSecret(context.Background(), r, h.kube.jobs[r.JobName])
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create secret: %v, want deadline exceeded", err)
	}
	key := string(h.kube.secrets[runSecretName(r.JobName)].Data["LLM_API_KEY"])
	if _, live := h.keys.live[key]; !live {
		t.Fatal("persisted Secret references a revoked key")
	}
	issued := len(h.keys.issued)
	h.kube.createSecretResponseErr = nil
	h.reconcile()
	if len(h.keys.issued) != issued {
		t.Fatal("reconciliation minted another key despite the persisted Secret")
	}
}

func TestLostSecretRaceDeletesUnusedKey(t *testing.T) {
	h := newHarness()
	h.reconcile()
	r := h.state()
	kept := string(h.kube.secrets[runSecretName(r.JobName)].Data["LLM_API_KEY"])
	// Another pass created the Secret first; this one's key must not linger.
	if err := h.c.createRunSecret(context.Background(), r, h.kube.jobs[r.JobName]); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.keys.live[kept]; !ok || len(h.keys.live) != 1 || len(h.keys.issued) != 2 {
		t.Errorf("live keys %v after %d issued, want only %s", h.keys.live, len(h.keys.issued), kept)
	}
}
