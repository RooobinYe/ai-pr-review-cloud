package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /apis/batch/v1/namespaces/ns/jobs":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &created)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"j","uid":"u1"},"spec":{"template":{"spec":{"containers":[]}}}}`))
		case "POST /api/v1/namespaces/ns/secrets":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"secrets \"s\" already exists"}`))
		case "GET /api/v1/namespaces/ns/secrets/s":
			_, _ = w.Write([]byte(`{"metadata":{"name":"s"},"data":{"RUN_TOKEN":"dG9r"}}`))
		case "GET /apis/batch/v1/namespaces/ns/jobs/j":
			_, _ = w.Write([]byte(`{"metadata":{"name":"j"},"spec":{"template":{"spec":{"containers":[]}}},
				"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded"}]}}`))
		case "GET /api/v1/namespaces/ns/pods":
			if r.URL.Query().Get("labelSelector") != "job-name=j" {
				t.Errorf("selector %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"p"},"status":{"initContainerStatuses":[
				{"name":"fetch","state":{"terminated":{"exitCode":1,"message":"boom"}}}]}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{Host: srv.URL, Namespace: "ns", HTTP: srv.Client(), Token: func() (string, error) { return "sa-token", nil }}
	ctx := context.Background()

	job, err := c.CreateJob(ctx, Job{Metadata: ObjectMeta{Name: "j"}})
	if err != nil || job.Metadata.UID != "u1" {
		t.Fatalf("CreateJob = %+v, %v", job, err)
	}
	if created["apiVersion"] != "batch/v1" || created["kind"] != "Job" {
		t.Errorf("created %v", created)
	}
	if _, ok := created["status"]; ok {
		t.Error("empty status sent on create")
	}
	if err := c.CreateSecret(ctx, Secret{Metadata: ObjectMeta{Name: "s"}}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("CreateSecret err %v, want ErrAlreadyExists", err)
	}
	job, err = c.GetJob(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := job.Status.Condition(JobFailed); !ok || cond.Reason != "DeadlineExceeded" {
		t.Errorf("failed condition %+v %v", cond, ok)
	}
	if _, err := c.GetJob(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetJob missing: %v", err)
	}
	if _, err := c.GetSecret(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSecret missing: %v", err)
	}
	if s, err := c.GetSecret(ctx, "s"); err != nil || string(s.Data["RUN_TOKEN"]) != "tok" {
		t.Errorf("GetSecret = %+v, %v", s, err)
	}
	pods, err := c.ListPods(ctx, "job-name=j")
	if err != nil || len(pods) != 1 || pods[0].Status.InitContainerStatuses[0].State.Terminated.Message != "boom" {
		t.Errorf("ListPods = %+v, %v", pods, err)
	}
}
