package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/preview"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

const previewLabel = "ai-pr-review.cloud/preview"

// PreviewConfig is installed only when cluster preview has explicitly been enabled.
type PreviewConfig struct {
	Kube   *kube.Client
	locks  sync.Map
	CAFile string
}

type previewReply struct {
	Enabled    bool   `json:"enabled"`
	Namespace  string `json:"namespace,omitempty"`
	Status     string `json:"status,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

func (c *Controller) handlePreview(w http.ResponseWriter, req *http.Request) {
	r, ok := c.callerRun(w, req)
	if !ok {
		return
	}
	if r.State != run.StateRunning && r.State != run.StatePreviewing {
		http.Error(w, "run no longer active", 409)
		return
	}
	var bundle preview.Bundle
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 2<<20)).Decode(&bundle); err != nil {
		http.Error(w, "invalid preview bundle", 400)
		return
	}
	cfg, err := preview.Parse([]byte(bundle.Files[".ai-pr-review.yaml"]))
	if err != nil {
		http.Error(w, err.Error(), 422)
		return
	}
	if !cfg.Enabled {
		_ = json.NewEncoder(w).Encode(previewReply{})
		return
	}
	if c.cfg.Preview == nil {
		http.Error(w, "preview requested but disabled by operator", 422)
		return
	}
	// Serialization also prevents a delayed old attempt from replacing a newer preview.
	// The deployment is bounded and independent of an interrupted HTTP connection.
	unlock, err := c.cfg.Preview.lock(req.Context(), preview.Namespace(r.Repo, r.PRNumber))
	if err != nil {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 12*time.Minute)
	defer cancel()
	current, err := c.store.Get(ctx, r.ID)
	if err != nil || current.Attempt != r.Attempt || (current.State != run.StateRunning && current.State != run.StatePreviewing) {
		http.Error(w, "run no longer active", 409)
		return
	}
	pull, err := c.gh.Client(r.InstallationID).GetPull(ctx, r.Repo, r.PRNumber)
	if err != nil {
		http.Error(w, "cannot check current PR", 503)
		return
	}
	if pull.State != "open" || pull.Head.SHA != r.HeadSHA {
		http.Error(w, "PR closed or head changed", 409)
		return
	}
	if current.State == run.StateRunning {
		if err := c.store.TransitionAttempt(ctx, r.ID, r.Attempt, run.StateRunning, run.StatePreviewing); err != nil {
			http.Error(w, "cannot start preview", 409)
			return
		}
	}
	reply, err := c.preparePreview(ctx, r, bundle)
	// A concurrent supersede wins; never revive a terminal run.
	transitionErr := c.store.TransitionAttempt(ctx, r.ID, r.Attempt, run.StatePreviewing, run.StateRunning)
	if transitionErr != nil && !errors.Is(transitionErr, store.ErrConflict) {
		c.runLog(r).Error("finish preview", "err", transitionErr)
	}
	if err != nil {
		c.runLog(r).Error("prepare preview", "err", err)
		status := http.StatusServiceUnavailable
		if errors.Is(err, errPreviewInput) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, "preview deployment failed: "+err.Error(), status)
		return
	}
	if transitionErr != nil {
		http.Error(w, "preview attempt no longer active", 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func obj(api, kind, name string, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["apiVersion"], fields["kind"] = api, kind
	fields["metadata"] = map[string]any{"name": name}
	return fields
}

func (c *Controller) preparePreview(ctx context.Context, r store.Run, bundle preview.Bundle) (previewReply, error) {
	k := c.cfg.Preview.Kube
	ns := preview.Namespace(r.Repo, r.PRNumber)
	renderCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	objects, cfg, err := preview.Render(renderCtx, bundle, r.HeadSHA, ns)
	cancel()
	if err != nil {
		return previewReply{}, fmt.Errorf("%w: %v", errPreviewInput, err)
	}
	namespace := obj("v1", "Namespace", ns, nil)
	namespace["metadata"] = map[string]any{"name": ns, "labels": map[string]string{previewLabel: k.Namespace, "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.34"}, "annotations": map[string]string{"repo": r.Repo, "pr": strconv.Itoa(r.PRNumber), "installation": strconv.FormatInt(r.InstallationID, 10), "run": r.ID, "head": r.HeadSHA, "created": time.Now().UTC().Format(time.RFC3339)}}
	if err := k.Request(ctx, "POST", "/api/v1/namespaces", namespace, nil); err != nil {
		if !errors.Is(err, kube.ErrAlreadyExists) {
			return previewReply{}, err
		}
		var existing struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := k.Request(ctx, "GET", "/api/v1/namespaces/"+ns, nil, &existing); err != nil {
			return previewReply{}, err
		}
		if existing.Metadata.Labels[previewLabel] != k.Namespace || existing.Metadata.Annotations["repo"] != r.Repo || existing.Metadata.Annotations["pr"] != strconv.Itoa(r.PRNumber) {
			return previewReply{}, fmt.Errorf("%w: namespace is not owned by this preview", errPreviewInput)
		}
		if existing.Metadata.Annotations["head"] != r.HeadSHA {
			if err := k.Request(ctx, "DELETE", "/api/v1/namespaces/"+ns, nil, nil); err != nil {
				return previewReply{}, err
			}
			for {
				err := k.Request(ctx, "GET", "/api/v1/namespaces/"+ns, nil, nil)
				if errors.Is(err, kube.ErrNotFound) {
					break
				}
				if err != nil {
					return previewReply{}, err
				}
				select {
				case <-ctx.Done():
					return previewReply{}, ctx.Err()
				case <-time.After(time.Second):
				}
			}
			if err := k.Request(ctx, "POST", "/api/v1/namespaces", namespace, nil); err != nil {
				return previewReply{}, err
			}
		}
	}
	apply := func(group, resource string, o map[string]any) error {
		return k.ApplyPreview(ctx, ns, group, resource, o["metadata"].(map[string]any)["name"].(string), o)
	}
	quota := obj("v1", "ResourceQuota", "preview", map[string]any{"spec": map[string]any{"hard": map[string]string{"requests.cpu": "2", "requests.memory": "2Gi", "limits.cpu": "4", "limits.memory": "4Gi", "pods": "10", "services": "5", "configmaps": "10"}}})
	policy := obj("networking.k8s.io/v1", "NetworkPolicy", "preview-isolation", map[string]any{"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress", "Egress"}, "ingress": []any{}, "egress": []any{}}})
	for _, item := range []struct {
		group, resource string
		object          map[string]any
	}{{"api/v1", "resourcequotas", quota}, {"apis/networking.k8s.io/v1", "networkpolicies", policy}, {"api/v1", "serviceaccounts", obj("v1", "ServiceAccount", "preview-reader", nil)}} {
		if err := apply(item.group, item.resource, item.object); err != nil {
			return previewReply{}, err
		}
	}
	rules := []any{map[string]any{"apiGroups": []string{""}, "resources": []string{"pods", "pods/log", "events"}, "verbs": []string{"get", "list", "watch"}}, map[string]any{"apiGroups": []string{"apps"}, "resources": []string{"deployments", "replicasets"}, "verbs": []string{"get", "list", "watch"}}}
	role := obj("rbac.authorization.k8s.io/v1", "Role", "preview-reader", map[string]any{"rules": rules})
	binding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", "preview-reader", map[string]any{"roleRef": map[string]string{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "preview-reader"}, "subjects": []any{map[string]string{"kind": "ServiceAccount", "name": "preview-reader", "namespace": ns}}})
	if err := apply("apis/rbac.authorization.k8s.io/v1", "roles", role); err != nil {
		return previewReply{}, err
	}
	if err := apply("apis/rbac.authorization.k8s.io/v1", "rolebindings", binding); err != nil {
		return previewReply{}, err
	}
	deployments := []string{}
	for _, o := range objects {
		group, resource := "api/v1", "configmaps"
		switch o["kind"] {
		case "Deployment":
			group, resource = "apis/apps/v1", "deployments"
			deployments = append(deployments, o["metadata"].(map[string]any)["name"].(string))
		case "Service":
			resource = "services"
		}
		if err := apply(group, resource, o); err != nil {
			return previewReply{}, err
		}
	}
	if len(deployments) == 0 {
		return previewReply{}, fmt.Errorf("%w: preview chart must include a Deployment", errPreviewInput)
	}
	duration, _ := time.ParseDuration(cfg.ReadyTimeout)
	deadline := time.Now().Add(duration)
	status := "timeout: inspect pods, events and logs to determine the cause"
	for time.Now().Before(deadline) {
		ready := true
		for _, name := range deployments {
			var d struct {
				Metadata struct {
					Generation int64 `json:"generation"`
				} `json:"metadata"`
				Spec struct {
					Replicas int `json:"replicas"`
				} `json:"spec"`
				Status struct {
					Observed int64 `json:"observedGeneration"`
					Ready    int   `json:"readyReplicas"`
					Updated  int   `json:"updatedReplicas"`
				} `json:"status"`
			}
			if err := k.Request(ctx, "GET", "/apis/apps/v1/namespaces/"+ns+"/deployments/"+name, nil, &d); err != nil {
				return previewReply{}, err
			}
			if d.Spec.Replicas < 1 || d.Status.Observed < d.Metadata.Generation || d.Status.Ready < d.Spec.Replicas || d.Status.Updated < d.Spec.Replicas {
				ready = false
			}
		}
		if ready {
			status = "ready"
			break
		}
		select {
		case <-ctx.Done():
			return previewReply{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		current, err := c.store.Get(ctx, r.ID)
		if err != nil {
			return previewReply{}, err
		}
		if current.State != run.StatePreviewing || current.Attempt != r.Attempt {
			return previewReply{}, fmt.Errorf("run no longer previewing")
		}
	}
	// Short-lived credentials can only read this namespace; never use controller credentials in the runner.
	var token struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	request := map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest", "spec": map[string]any{"expirationSeconds": 3600}}
	if err := k.Request(ctx, "POST", kube.PreviewPath(ns, "serviceaccounts", "preview-reader")+"/token", request, &token); err != nil {
		return previewReply{}, err
	}
	caFile := c.cfg.Preview.CAFile
	if caFile == "" {
		caFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return previewReply{}, err
	}
	config := map[string]any{"apiVersion": "v1", "kind": "Config", "current-context": "preview", "clusters": []any{map[string]any{"name": "preview", "cluster": map[string]string{"server": k.Host, "certificate-authority-data": base64.StdEncoding.EncodeToString(ca)}}}, "users": []any{map[string]any{"name": "preview", "user": map[string]string{"token": token.Status.Token}}}, "contexts": []any{map[string]any{"name": "preview", "context": map[string]string{"cluster": "preview", "user": "preview", "namespace": ns}}}}
	encoded, _ := json.Marshal(config)
	return previewReply{Enabled: true, Namespace: ns, Status: status, Kubeconfig: string(encoded)}, nil
}

// CleanPreviews polls PR state so cleanup survives missed close webhooks and controller restarts.
func (c *Controller) CleanPreviews(ctx context.Context) {
	if c.cfg.Preview == nil {
		return
	}
	k := c.cfg.Preview.Kube
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.Request(ctx, "GET", "/api/v1/namespaces?labelSelector="+previewLabel+"="+k.Namespace, nil, &list); err != nil {
		c.log.Warn("list previews", "err", err)
		return
	}
	for _, item := range list.Items {
		c.cleanPreview(ctx, k, item.Metadata.Name, item.Metadata.Annotations)
	}
}

func (c *Controller) cleanPreview(ctx context.Context, k *kube.Client, name string, annotations map[string]string) {
	unlock, err := c.cfg.Preview.lock(ctx, name)
	if err != nil {
		return
	}
	defer unlock()
	a := annotations
	number, e1 := strconv.Atoi(a["pr"])
	installation, e2 := strconv.ParseInt(a["installation"], 10, 64)
	if e1 != nil || e2 != nil || name != preview.Namespace(a["repo"], number) {
		return
	}
	pull, err := c.gh.Client(installation).GetPull(ctx, a["repo"], number)
	remove := err == nil && pull.State == "closed"
	created, _ := time.Parse(time.RFC3339, a["created"])
	if !remove && !created.IsZero() && c.now().Sub(created) > 24*time.Hour {
		runs, listErr := c.store.Active(ctx)
		if listErr != nil {
			return
		}
		active := false
		for _, r := range runs {
			if r.Repo == a["repo"] && r.PRNumber == number && (r.State == run.StateRunning || r.State == run.StatePreviewing || r.State == run.StateReviewed) {
				active = true
			}
		}
		remove = !active && (err == nil || githubStatusGone(err))
	}
	if remove {
		if err := k.Request(ctx, "DELETE", "/api/v1/namespaces/"+name, nil, nil); err != nil {
			c.log.Warn("delete preview", "namespace", name, "err", err)
		}
	}
}

func githubStatusGone(err error) bool { s := githubapp.StatusOf(err); return s == 404 || s == 410 }

var errPreviewInput = errors.New("invalid preview")

// Different PRs can deploy concurrently; retries and close cleanup for one PR serialize.
func (p *PreviewConfig) lock(ctx context.Context, namespace string) (func(), error) {
	value, _ := p.locks.LoadOrStore(namespace, make(chan struct{}, 1))
	semaphore := value.(chan struct{})
	select {
	case semaphore <- struct{}{}:
		return func() { <-semaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
