package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/preview"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
)

func previewBundle(t *testing.T) preview.Bundle {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is needed for preview render integration")
	}
	root := "../../.."
	files := map[string]string{".ai-pr-review.yaml": "preview:\n  enabled: true\n  helm:\n    chart: demo/preview/chart\n  readyTimeout: 1s\n"}
	for _, p := range []string{"demo/preview/chart/Chart.yaml", "demo/preview/chart/values.yaml", "demo/preview/chart/templates/deployment.yaml"} {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		files[p] = string(b)
	}
	return preview.Bundle{Files: files}
}

func TestPreviewReadyAndTimeoutKeepReadOnlyEvidenceAccess(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			h := newHarness()
			h.reconcile()
			var resources []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "POST" && r.URL.Path == "/api/v1/namespaces":
					w.WriteHeader(201)
				case r.Method == "PATCH":
					resources = append(resources, r.URL.Path)
					var object map[string]any
					if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
						t.Error(err)
					}
					if strings.HasSuffix(r.URL.Path, "/roles/preview-reader") {
						data, _ := json.Marshal(object["rules"])
						if strings.Contains(string(data), "secrets") || strings.Contains(string(data), "create") {
							t.Error("reader can access credentials or write")
						}
					}
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/deployments/preview-demo"):
					count := 0
					if ready {
						count = 1
					}
					fmt.Fprintf(w, `{"metadata":{"generation":2},"spec":{"replicas":1},"status":{"observedGeneration":2,"readyReplicas":%d,"updatedReplicas":1}}`, count)
				case strings.HasSuffix(r.URL.Path, "/serviceaccounts/preview-reader/token"):
					fmt.Fprint(w, `{"status":{"token":"test-only-reader"}}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			ca := filepath.Join(t.TempDir(), "ca")
			if err := os.WriteFile(ca, []byte("test CA"), 0600); err != nil {
				t.Fatal(err)
			}
			h.c.cfg.Preview = &PreviewConfig{Kube: &kube.Client{Host: server.URL, Namespace: "reviews", HTTP: server.Client()}, CAFile: ca, ImageRepositories: map[string]string{"o/r": "ghcr.io/rooobinye/ai-pr-review-cloud-preview"}}
			body, _ := json.Marshal(previewBundle(t))
			rec := h.call("POST", "/runs/"+runID+"/preview", h.runToken(), body)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var reply previewReply
			if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			if (reply.Status == "ready") != ready || reply.Namespace != "pr-o-r-7" || !strings.Contains(reply.Kubeconfig, "test-only-reader") {
				t.Fatal("incorrect preview outcome")
			}
			if len(resources) != 6 || h.state().State != run.StateRunning {
				t.Fatalf("resources %v; state %s", resources, h.state().State)
			}
		})
	}
}

func TestPreviewRejectsUnauthenticatedDisabledAndStaleRequests(t *testing.T) {
	h := newHarness()
	h.reconcile()
	body, _ := json.Marshal(previewBundle(t))
	if got := h.call("POST", "/runs/"+runID+"/preview", "wrong", body).Code; got != 401 {
		t.Fatal(got)
	}
	if got := h.call("POST", "/runs/"+runID+"/preview", h.runToken(), body).Code; got != 422 {
		t.Fatal(got)
	}
	h.c.cfg.Preview = &PreviewConfig{ImageRepositories: map[string]string{"o/r": "ghcr.io/rooobinye/ai-pr-review-cloud-preview"}}
	h.gh.head = oldSHA
	if got := h.call("POST", "/runs/"+runID+"/preview", h.runToken(), body).Code; got != 409 {
		t.Fatal(got)
	}
	if h.state().State != run.StateRunning {
		t.Fatal("stale request changed state")
	}
}

func TestPreviewingPodFailureRetries(t *testing.T) {
	h := newHarness()
	h.reconcile()
	h.store.runs[runID].State = run.StatePreviewing
	h.losePod()
	h.reconcile()
	if h.state().State != run.StateQueued {
		t.Fatal(h.state().State)
	}
	h.reconcile()
	if h.state().Attempt != 2 {
		t.Fatal(h.state().Attempt)
	}
}

func TestCleanupClosedAndOrphanPreviews(t *testing.T) {
	for _, tt := range []struct {
		name, state    string
		age            time.Duration
		active, remove bool
	}{{"open", "open", time.Hour, false, false}, {"closed", "closed", time.Hour, false, true}, {"orphan", "open", 25 * time.Hour, false, true}, {"active", "open", 25 * time.Hour, true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness()
			h.gh.state = tt.state
			if tt.active {
				h.reconcile()
			}
			deleted := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					deleted = true
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "pr-o-r-7", "annotations": map[string]string{"repo": "o/r", "pr": "7", "installation": "42", "created": h.now().Add(-tt.age).Format(time.RFC3339)}}}}})
			}))
			defer server.Close()
			h.c.cfg.Preview = &PreviewConfig{Kube: &kube.Client{Host: server.URL, Namespace: "reviews", HTTP: server.Client()}}
			h.c.CleanPreviews(context.Background())
			if deleted != tt.remove {
				t.Fatalf("deleted=%v want %v", deleted, tt.remove)
			}
		})
	}
}
