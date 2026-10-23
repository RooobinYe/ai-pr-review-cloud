package controller

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// lifecycleKube uses real Jobs, Pods, Secrets and GC, but substitutes a sleeping
// process for the model/fetch. This test exercises orchestration, not model quality.
type lifecycleKube struct {
	*kube.Client
	image string
}

func (k lifecycleKube) CreateJob(ctx context.Context, j kube.Job) (kube.Job, error) {
	j.Spec.Template.Spec.InitContainers = nil
	j.Spec.Template.Spec.Containers[0].Image = k.image
	j.Spec.Template.Spec.Containers[0].ImagePullPolicy = "IfNotPresent"
	// postgres' entrypoint delegates non-postgres commands directly to exec.
	j.Spec.Template.Spec.Containers[0].Args = []string{"sleep", "300"}
	return k.Client.CreateJob(ctx, j)
}

// Run only against a disposable namespace via an authenticated kubectl proxy.
// TEST_KUBE_URL=http://127.0.0.1:18082 TEST_KUBE_NAMESPACE=m4-acceptance
func TestKindApprovalModes(t *testing.T) {
	host, namespace := os.Getenv("TEST_KUBE_URL"), os.Getenv("TEST_KUBE_NAMESPACE")
	if host == "" || namespace == "" {
		t.Skip("TEST_KUBE_URL and disposable TEST_KUBE_NAMESPACE not set")
	}
	image := os.Getenv("TEST_KUBE_IMAGE")
	if image == "" {
		image = "postgres:17"
	}
	k := lifecycleKube{Client: &kube.Client{Host: host, Namespace: namespace, HTTP: &http.Client{Timeout: 10 * time.Second}, Token: func() (string, error) { return "", nil }}, image: image}
	ctx := context.Background()
	wait := func(t *testing.T, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatal("Kubernetes lifecycle did not converge within 60 seconds")
	}
	for _, mode := range []string{"vote", "auto", "manual"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness()
			h.gh.config = []byte("approvalMode: " + mode)
			h.c.kube = k
			h.reconcile()
			r := h.state()
			if r.State != run.StateRunning {
				t.Fatalf("run %+v", r)
			}
			t.Cleanup(func() {
				_ = k.DeleteJob(ctx, r.JobName)
				wait(t, func() bool { pods, err := k.ListPods(ctx, "job-name="+r.JobName); return err == nil && len(pods) == 0 })
			})
			wait(t, func() bool {
				pods, err := k.ListPods(ctx, "job-name="+r.JobName)
				return err == nil && len(pods) == 1 && pods[0].Status.Phase == "Running"
			})
			secret, err := k.GetSecret(ctx, runSecretName(r.JobName))
			if err != nil {
				t.Fatal(err)
			}
			if status := h.postWithToken(result(review.VerdictApprove), string(secret.Data["RUN_TOKEN"])); status != 204 {
				t.Fatalf("result status %d", status)
			}
			h.reconcile()
			if mode == "vote" {
				if h.state().State != run.StateDone || len(h.gh.merges) != 0 {
					t.Fatal("vote merged")
				}
				return
			}
			wait(t, func() bool { pods, err := k.ListPods(ctx, "job-name="+r.JobName); return err == nil && len(pods) == 0 })
			if mode == "manual" {
				if h.state().State != run.StateAwaitingApproval {
					t.Fatal(h.state().State)
				}
				h.restart()
				h.c.kube = k
				h.reconcile()
				if h.state().State != run.StateAwaitingApproval {
					t.Fatal("waiting not recovered")
				}
				h.gh.writable = true
				h.store.signals = map[string][]store.MergeSignal{runID: {{DeliveryID: "confirmation", Actor: "writer"}}}
				h.reconcile()
			}
			h.reconcile()
			if h.state().State != run.StateDone || len(h.gh.merges) != 1 {
				t.Fatalf("run %+v merges %v", h.state(), h.gh.merges)
			}
		})
	}
}
